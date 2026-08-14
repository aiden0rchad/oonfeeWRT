package api

import (
	"context"
	"net/http"
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

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.now()

	var since int64
	if r.URL.Query().Get("all") != "1" {
		since = now.Add(-24 * time.Hour).Unix()
	}
	limit := queryInt(r, "limit", 500, 1, 5000)

	clients, err := s.Store.Clients(ctx, since, limit)
	if handleStoreErr(w, err, "clients") {
		return
	}

	// One pass over the recent RSSI series to find which MACs are associated,
	// and where. Done as a single query rather than per client: a 40-client
	// network would otherwise issue 40 round trips to render one screen.
	rf := s.recentStations(ctx, now)

	out := make([]clientView, 0, len(clients))
	onlineCutoff := now.Add(-clientActiveWindow).Unix()
	for _, c := range clients {
		v := clientView{Client: c, Connection: "unknown"}
		v.Online = c.LastSeen != nil && *c.LastSeen >= onlineCutoff
		if st, ok := rf[c.MAC]; ok {
			v.Connection = "wireless"
			sig := st.signal
			v.Signal = &sig
			v.DeviceID = &st.deviceID
			if st.retry != nil {
				v.RetryPct = st.retry
			}
		}
		out = append(out, v)
	}
	// Count the scopes here so the UI's filter counts do not depend on the page
	// it happened to receive, and so an empty local list is legible: "0 local,
	// 8 upstream" is an answer, an empty grid on its own is not.
	scopes := map[string]int{}
	for _, c := range out {
		scopes[c.Scope]++
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"clients": out,
		"scopes":  scopes,
		// Says plainly why most rows have no RF data, so the UI can explain it
		// rather than leaving a column mysteriously empty.
		"note": "signal and retry data come from the focused poll tier, so they " +
			"are present only for devices a screen is currently watching",
		// The scoping caveat, in the response rather than only in the UI, so an
		// API consumer gets it too.
		"scope_note": "clients are scoped by which of the device's own IPv4 " +
			"subnets their address falls in; \"upstream\" means the interface " +
			"carrying the default route, i.e. a neighbour on the uplink rather " +
			"than a client of this network",
	})
}

type stationRF struct {
	deviceID int64
	signal   int
	retry    *float64
}

// recentStations reads the newest RSSI (and retry) rollup per station MAC.
//
// The window is deliberately short. A station series persists for the full
// retention period, so without a recency bound this would report a client as
// wireless-and-at-−52-dBm two weeks after it left.
func (s *Server) recentStations(ctx context.Context, now time.Time) map[string]stationRF {
	out := map[string]stationRF{}
	cutoff := now.Add(-clientActiveWindow).Unix()

	rows, err := s.Store.SQL().QueryContext(ctx, `
SELECT se.key, se.device_id, se.kind, r.avg
  FROM rollup_5m r
  JOIN series se ON se.id = r.series_id
 WHERE se.kind IN (?, ?) AND r.ts >= ?
 ORDER BY r.ts`,
		string(telemetry.KindStaRSSI), string(telemetry.KindStaRetry), cutoff)
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
		// Ordered by ts, so later rows overwrite earlier ones and the last write
		// wins — which is the most recent reading.
		e := out[mac]
		e.deviceID = deviceID
		switch telemetry.Kind(kind) {
		case telemetry.KindStaRSSI:
			e.signal = int(avg)
		case telemetry.KindStaRetry:
			v := avg
			e.retry = &v
		}
		out[mac] = e
	}
	return out
}
