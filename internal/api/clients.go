package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/telemetry"
)

// clientView is one row of the Client Devices grid.
//
// The RF fields are pointers and are absent rather than zero when no focused
// poll has covered this client. That is the honest state for most rows most of
// the time: the inventory comes from the cheap baseline sources, while signal
// and rate come from iwinfo, which is ~92% of a focused poll and therefore only
// runs while somebody is looking. A grid that showed −0 dBm for every idle
// client would be worse than one that shows nothing.
type clientView struct {
	store.Client

	// Connection is "wireless" when a focused poll has seen this MAC
	// associated, and "unknown" otherwise — NOT "wired". Absence of wireless
	// evidence is not evidence of a cable.
	Connection string `json:"connection"`

	// Online is derived from last_seen against the same threshold the device
	// list uses, so the two screens cannot disagree.
	Online bool `json:"online"`
}

// clientActiveWindow is how far back a client counts as current. Longer than
// the device threshold: ARP entries age out slowly, and a phone that is asleep
// is still on the network in every sense a user cares about.
const clientActiveWindow = 15 * time.Minute

// wirelessKinds are the series whose presence means a MAC was associated to a
// radio. One list, used both by the SQL that facets and pages the grid and by
// recentStations, which enriches the rows it returns — two definitions of
// "wireless" would put a row in the list that its own rail does not count.
var wirelessKinds = []string{
	string(telemetry.KindStaRSSI), string(telemetry.KindStaRetry),
}

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.now()

	var since int64
	if r.URL.Query().Get("all") != "1" {
		since = now.Add(-24 * time.Hour).Unix()
	}
	onlineCutoff := now.Add(-clientActiveWindow).Unix()
	limit := queryInt(r, "limit", 500, 1, 5000)
	offset := queryInt(r, "offset", 0, 0, 1<<30)

	// Filters, paging and facet counts all go to the database. The grid used to
	// receive every client and narrow them in the browser, which is correct
	// only while the page is the whole table — and the rail counted the page it
	// held, so on a paged fleet it would report "4 wireless" from a page of 100
	// while the table held four hundred.
	page, err := s.Store.ClientsPage(ctx, store.ClientFilter{
		SeenSince:     since,
		ActiveSince:   onlineCutoff,
		WirelessKinds: wirelessKinds,
		Presence:      r.URL.Query().Get("presence"),
		Connection:    r.URL.Query().Get("connection"),
		Scope:         r.URL.Query().Get("scope"),
		Limit:         limit,
		Offset:        offset,
	})
	if handleStoreErr(w, err, "clients") {
		return
	}

	// One pass over the recent RSSI series to find which MACs are associated,
	// and where. Done as a single query rather than per client: a 40-client
	// network would otherwise issue 40 round trips to render one screen.
	rf := s.recentStations(ctx, now)
	// What is associated RIGHT NOW, over the top of the rollups.
	//
	// recentStations reads rollup_5m, which only exists after the five-minute
	// flush and only gets written at all while a focused poll is running. So a
	// client that is associated this second showed as "unknown" on every column
	// until somebody opened a device screen and then waited out a flush.
	//
	// hostapd's get_clients runs at the baseline rate and already carries every
	// MAC and its RSSI. Live wins where both exist: the rollup is an average
	// over a five-minute bucket, and the question the grid asks is about now.
	for mac, st := range s.liveStations(ctx) {
		e := rf[mac]
		e.deviceID = st.deviceID
		if st.signal != nil {
			e.signal = *st.signal
			e.haveSignal = true
		}
		e.live = true
		rf[mac] = e
	}

	out := make([]clientView, 0, len(page.Clients))
	for _, c := range page.Clients {
		v := clientView{Client: c, Connection: "unknown"}
		v.Online = c.LastSeen != nil && *c.LastSeen >= onlineCutoff
		if st, ok := rf[strings.ToLower(c.MAC)]; ok {
			v.Connection = "wireless"
			// Signal is reported only when something actually measured one. A
			// station hostapd lists without an RSSI is associated and unmeasured,
			// and 0 dBm would draw as a perfect signal.
			if st.haveSignal {
				sig := st.signal
				v.Signal = &sig
			}
			v.DeviceID = &st.deviceID
			if st.retry != nil {
				v.RetryPct = st.retry
			}
		}
		out = append(out, v)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"clients": out,
		"total":   page.Total,
		"limit":   limit,
		"offset":  offset,
		// Counted over the whole filtered table, each rail with the OTHER
		// filters applied but not its own — so every option answers "how many
		// would I get if I clicked that instead?".
		"facets": map[string]any{
			"presence":   page.Presence,
			"connection": page.Connection,
			"scope":      page.Scope,
		},
		// Says plainly why most rows have no RF data, so the UI can explain it
		// rather than leaving a column mysteriously empty.
		//
		// Names every column it covers. It listed only signal and retry until
		// the access-point column was added and not added here, and an empty
		// column beside an explanation that does not mention it reads as a
		// column that is broken — which is how it was first reported.
		"note": s.rfNote(ctx),
		// The scoping caveat, in the response rather than only in the UI, so an
		// API consumer gets it too.
		"scope_note": "clients are scoped by which of the device's own IPv4 " +
			"subnets their address falls in; \"upstream\" means the interface " +
			"carrying the default route, i.e. a neighbour on the uplink rather " +
			"than a client of this network",
	})
}

// rfNote explains why the radio columns are empty, from the reason they
// actually are.
//
// It used to be one fixed sentence — "comes from the focused poll tier, so all
// three are present only for devices a screen is currently watching" — with
// the UI appending "Open a device to populate them."
//
// That is one of three possible causes, stated as though it were the only one.
// Reported by the operator against a fleet where every managed radio had ZERO
// associated stations: the clients in the grid were on other access points
// entirely, arriving through ARP and DHCP rather than through any radio we
// poll. Opening a device would have run a focused poll, read an empty
// assoclist, and changed nothing — advice that cannot work, for a cause that
// was not the cause.
//
// The distinction costs nothing to make. hostapd's get_clients runs at the
// BASELINE rate, so the associated-station count is already known for every
// device on every poll; only the per-station RSSI needs the focused tier.
//
// Three-state, like everything else here: clients present, none present, and
// no poll has said.
func (s *Server) rfNote(ctx context.Context) string {
	const focused = "which access point a client is on, along with its signal " +
		"and retry rate, comes from the focused poll tier, so all three are " +
		"present only for devices a screen is currently watching. Open a " +
		"device to populate them"

	if s.Fleet == nil {
		return focused
	}
	devs, err := s.Store.Devices(ctx)
	if err != nil {
		return focused
	}
	var associated, known int
	for _, d := range devs {
		if !d.Adopted() {
			continue
		}
		if n, ok := s.Fleet.LiveClients(d.ID); ok {
			known++
			associated += n
		}
	}
	switch {
	case known == 0:
		return "no poll has reported how many clients are associated to this " +
			"controller's access points yet, so whether any of these are on " +
			"them is unknown"
	case associated == 0:
		return "no client is associated to any access point this controller " +
			"manages. The rows below are hosts seen on the network through ARP " +
			"and DHCP, and their signal, access point and retry rate belong to " +
			"whatever is actually serving them — opening a device will not fill " +
			"these in"
	}
	return focused
}

type stationRF struct {
	deviceID int64
	signal   int
	// haveSignal separates "no RSSI was reported" from a reading of 0 dBm,
	// which is a real value and an implausible one.
	haveSignal bool
	retry      *float64
	// live marks an entry from the current poll rather than a rollup bucket.
	live bool
}

// liveStation is one associated client from the baseline poll.
type liveStation struct {
	deviceID int64
	signal   *int
}

// liveStations asks every adopted device what it currently has associated.
//
// The baseline poll already knows: hostapd's get_clients carries each MAC and
// its RSSI on every poll of every AP. Nothing here costs a device call.
func (s *Server) liveStations(ctx context.Context) map[string]liveStation {
	out := map[string]liveStation{}
	if s.Fleet == nil {
		return out
	}
	devs, err := s.Store.Devices(ctx)
	if err != nil {
		return out
	}
	for _, d := range devs {
		if !d.Adopted() {
			continue
		}
		m, ok := s.Fleet.LiveStations(d.ID)
		if !ok {
			continue
		}
		for mac, st := range m {
			out[strings.ToLower(mac)] = liveStation{deviceID: d.ID, signal: st.Signal}
		}
	}
	return out
}

// recentStations reads the newest RSSI (and retry) rollup per station MAC, and
// decides which AP a client is on.
//
// The window is deliberately short. A station series persists for the full
// retention period, so without a recency bound this would report a client as
// wireless-and-at-−52-dBm two weeks after it left.
//
// The AP is chosen in SQL, per MAC, before any metric is read. That ordering is
// the point. This used to be a flat scan with last-row-wins, which meant three
// things, none of them intended: a client heard by two APs in the same
// five-minute bucket was attributed to whichever one the collector happened to
// poll second; the signal could come from one AP while the retry rate came from
// another, because each row overwrote its own field independently; and the same
// grid, refreshed, could move a stationary client between APs. Holding the
// evidence fixed and reversing only the write order flipped the answer — so
// nothing about the radio decided it.
//
// Newest bucket first, then strongest signal. A client is heard far better by
// the AP it is associated to than by a neighbour that merely overhears it, so
// within one bucket the stronger reading is the association. device_id last,
// only so that a genuine tie is stable rather than arbitrary.
func (s *Server) recentStations(ctx context.Context, now time.Time) map[string]stationRF {
	out := map[string]stationRF{}
	cutoff := now.Add(-clientActiveWindow).Unix()

	rows, err := s.Store.SQL().QueryContext(ctx, `
WITH sta AS (
  SELECT se.key AS mac, se.device_id AS device_id, se.kind AS kind,
         r.ts AS ts, r.avg AS avg
    FROM rollup_5m r
    JOIN series se ON se.id = r.series_id
   WHERE se.kind IN (?, ?) AND r.ts >= ?
),
-- The AP each MAC is on: strongest recent RSSI wins. Retry rows are excluded
-- from the ranking entirely — a retry percentage says nothing about which
-- radio a client is near, and letting one vote was how a device with no RSSI
-- reading at all could win the attribution.
winner AS (
  SELECT mac, device_id,
         ROW_NUMBER() OVER (
           PARTITION BY mac ORDER BY ts DESC, avg DESC, device_id
         ) AS rn
    FROM sta WHERE kind = ?
)
SELECT s.mac, s.device_id, s.kind, s.avg
  FROM sta s
  JOIN winner w ON w.mac = s.mac AND w.device_id = s.device_id AND w.rn = 1
 ORDER BY s.ts`,
		string(telemetry.KindStaRSSI), string(telemetry.KindStaRetry), cutoff,
		string(telemetry.KindStaRSSI))
	if err != nil {
		s.Log.Debug("could not read station telemetry", "err", err)
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var mac, kind string
		var deviceID int64
		var avg float64
		if err := rows.Scan(&mac, &deviceID, &kind, &avg); err != nil {
			return out
		}
		// Every row here already belongs to the winning AP, so the only thing
		// last-write-wins still decides is which of that AP's buckets to take —
		// and ordered by ts, that is the most recent one.
		e := out[mac]
		e.deviceID = deviceID
		switch telemetry.Kind(kind) {
		case telemetry.KindStaRSSI:
			e.signal = int(avg)
			e.haveSignal = true
		case telemetry.KindStaRetry:
			v := avg
			e.retry = &v
		}
		out[mac] = e
	}
	return out
}
