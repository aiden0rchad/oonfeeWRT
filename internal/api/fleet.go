package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/telemetry"
)

// deviceView is one row of the Devices list.
//
// LastSeen and Class are pointers because "never seen" and "class unknown" are
// real states that a zero would misreport — the first as the epoch, the second
// as a class the device may not be in.
type deviceView struct {
	ID        int64   `json:"id"`
	MAC       string  `json:"mac"`
	Name      string  `json:"name"`
	Host      string  `json:"host"`
	Role      string  `json:"role"`
	Adopted   bool    `json:"adopted"`
	AdoptedAt *int64  `json:"adopted_at"`
	Class     *string `json:"class"`
	FWRelease string  `json:"firmware"`
	LastSeen  *int64  `json:"last_seen"`
	PollState string  `json:"poll_state"`

	// Status is derived here rather than stored, so it cannot go stale: a device
	// is only "online" relative to the moment someone asks.
	Status string `json:"status"`

	// Tier and Quiesced come from the live collector, not the database. They
	// describe what the controller is doing right now, which is what the
	// Management Overhead readout is for.
	Tier     string `json:"tier,omitempty"`
	Quiesced bool   `json:"quiesced,omitempty"`
}

// offlineAfter is how long without a poll makes a device offline. Two baseline
// intervals plus slack: one missed poll is a blip, and marking a device down for
// a single dropped request would make the list flicker.
const offlineAfter = 150 * time.Second

func (s *Server) viewDevice(d *store.Device, now time.Time) deviceView {
	v := deviceView{
		ID: d.ID, MAC: d.MAC, Name: d.Name, Host: d.Host, Role: d.Role,
		Adopted: d.Adopted(), AdoptedAt: d.AdoptedAt,
		FWRelease: d.FWRelease, LastSeen: d.LastSeen, PollState: d.PollState,
	}
	if d.Class != "" {
		c := d.Class
		v.Class = &c
	}
	switch {
	case !d.Adopted():
		v.Status = "pending"
	case d.LastSeen == nil:
		v.Status = "unknown" // adopted but never successfully polled
	case now.Sub(time.Unix(*d.LastSeen, 0)) > offlineAfter:
		v.Status = "offline"
	default:
		v.Status = "online"
	}
	if s.Fleet != nil {
		if tier, ok := s.Fleet.Tier(d.ID); ok {
			v.Tier = string(tier)
		}
		v.Quiesced = s.Fleet.Quiesced(d.ID)
	}
	return v
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.Store.Devices(r.Context())
	if handleStoreErr(w, err, "devices") {
		return
	}
	now := s.now()
	wantStatus := r.URL.Query().Get("status")
	out := make([]deviceView, 0, len(devices))
	for _, d := range devices {
		v := s.viewDevice(d, now)
		if wantStatus != "" && v.Status != wantStatus {
			continue
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// deviceDetail is the device slide-over: the row, plus its capability record
// and the series it actually has.
type deviceDetail struct {
	deviceView
	Capabilities *capability.Registry `json:"capabilities"`

	// Interfaces and Radios list the series keys that exist for this device, so
	// a screen can offer a picker without guessing what was collected.
	Interfaces []string `json:"interfaces"`
	Radios     []string `json:"radios"`
	Stations   []string `json:"stations"`

	// Degraded is what the last poll could not read on this device, and what
	// each gap costs. A standing property of the device's ACL or driver rather
	// than an event — which is exactly why it needs somewhere to be READ. A
	// limitation the controller knows about and never shows is one an operator
	// discovers from a number being quietly wrong.
	Degraded []degradation `json:"degraded,omitempty"`

	// Broadcasting is every SSID this device is actually putting on the air,
	// including the ones oonfeeWRT does not manage.
	//
	// The controller deliberately never touches config it did not write, so an
	// AP adopted with SSIDs already on it keeps broadcasting them — correctly,
	// and until now invisibly. Not showing them made the device screen answer
	// "what is this AP broadcasting?" with only half the truth, and the missing
	// half is the half nobody is administering.
	Broadcasting []broadcastView `json:"broadcasting,omitempty"`

	// BroadcastKnown separates "the last poll saw no BSS" from "no poll has
	// looked". Without it an empty list would claim the radios are silent.
	BroadcastKnown bool `json:"broadcast_known"`
}

// Provenance is who wrote the UCI section behind one BSS.
//
// Three states, because the second and third are different facts and only one
// of them is a defect.
type Provenance string

const (
	// ProvOurs means the interface came from a section in owned_sections: this
	// controller wrote it, and un-adopt can put it back.
	ProvOurs Provenance = "ours"
	// ProvForeign means a section this controller did not write. Nobody is
	// administering it from here.
	ProvForeign Provenance = "foreign"
	// ProvUnknown means the device did not say which section created this
	// interface, or no poll has read the interface list. NOT foreign: a check
	// that could not run must not return a verdict, and calling an operator's
	// own SSID foreign because we failed to ask is the worse error.
	ProvUnknown Provenance = "unknown"
)

// broadcastView is one BSS on the air.
type broadcastView struct {
	SSID  string `json:"ssid"`
	Iface string `json:"iface"`
	BSSID string `json:"bssid,omitempty"`
	// Section is the wifi-iface that created this interface, when the device
	// said. Shown because it is what an operator needs to act on it.
	Section string `json:"section,omitempty"`
	// Brief is what it would take to bring a foreign SSID under management,
	// and what that would cost. Present only for foreign sections, because it
	// is the only case where the question arises.
	Brief *foreignSection `json:"brief,omitempty"`

	// Origin is decided from the SECTION, not from the SSID.
	//
	// It was decided by SSID for exactly one hour, and that was wrong in a way
	// worth recording: creating a site WLAN with the same name as a foreign
	// SSID flipped the still-foreign, still-broadcasting BSS to "managed" and
	// took its warning away — while the controller still did not own the
	// section and still could not change or remove it. A correct value under
	// the wrong question.
	Origin Provenance `json:"origin"`
}

// degradation is one call the poll could not use, with the consequence spelled
// out rather than left as an object and method name.
type degradation struct {
	Call string `json:"call"`
	Err  string `json:"error"`
	// Permanent marks a refusal retrying cannot fix — an ACL gap rather than a
	// device that was busy. The two need different responses from a human.
	Permanent bool   `json:"permanent"`
	Costs     string `json:"costs,omitempty"`
}

// degradationCost explains what a missing call takes away.
//
// Only for the ones whose consequence is not obvious from the call name. The
// point of the field is that "luci-rpc.getWirelessDevices: Permission denied"
// tells an operator nothing about what they lost.
func degradationCost(object, method string) string {
	switch object + "." + method {
	case "luci-rpc.getWirelessDevices":
		return "the poll cannot tell a mesh point from an access point, so a " +
			"mesh backhaul's peers are counted as clients"
	case "luci-rpc.getHostHints", "luci-rpc.getDHCPLeases":
		return "the client inventory is incomplete: hosts this device can see " +
			"will not appear in the client list"
	case "network.interface.dump":
		return "clients cannot be scoped, so the list cannot separate this " +
			"network's devices from neighbours on the uplink"
	case "iwinfo.survey":
		return "channel utilization is unavailable for this radio"
	case "iwinfo.assoclist":
		return "signal and rate are unavailable for clients on this radio"
	}
	return ""
}

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	d, err := s.deviceByID(r, id)
	if handleStoreErr(w, err, "device") {
		return
	}

	detail := deviceDetail{deviceView: s.viewDevice(d, s.now())}
	if s.Fleet != nil {
		if degs, ok := s.Fleet.Degraded(id); ok {
			for _, g := range degs {
				detail.Degraded = append(detail.Degraded, degradation{
					Call:      g.Object + "." + g.Method,
					Err:       g.Err,
					Permanent: g.Permanent,
					Costs:     degradationCost(g.Object, g.Method),
				})
			}
		}
	}
	if d.CapsJSON != "" && d.CapsJSON != "{}" {
		var caps capability.Registry
		if err := json.Unmarshal([]byte(d.CapsJSON), &caps); err == nil {
			detail.Capabilities = &caps
		} else {
			// A capability record that will not parse must not be reported as
			// "no capabilities" — that is the difference between a device that
			// cannot do something and one we failed to ask about.
			s.Log.Error("device has an unreadable capability record",
				"device", d.MAC, "err", err)
		}
	}
	ctx := r.Context()
	if s.Fleet != nil {
		if aps, ok := s.Fleet.Broadcasting(id); ok {
			detail.BroadcastKnown = true
			// Provenance comes from the UCI section, joined against what we
			// recorded writing. sectionsKnown is the three-state: without it,
			// a device whose ACL refuses getWirelessDevices would have every
			// BSS called foreign, including ours.
			sections, sectionsKnown := s.Fleet.IfaceSections(id)
			ours := map[string]bool{}
			if owned, err := s.Store.OwnedSections(ctx, id); err == nil {
				for _, o := range owned {
					if o.Config == "wireless" {
						ours[o.Section] = true
					}
				}
			} else {
				// Could not read our own claims, so nothing can be called ours
				// without guessing. Everything reports unknown.
				sectionsKnown = false
				s.Log.Debug("could not read ownership claims", "device", id, "err", err)
			}
			modes, modesKnown := s.Fleet.IfaceModes(id)
			notes, notesErr := s.Store.ForeignNotes(ctx, id)
			if notesErr != nil {
				// An unreadable ledger is not an empty one. Rendering it as
				// "nobody has decided about this" invites a second decision
				// about a section somebody already settled.
				s.Log.Warn("could not read recorded decisions about foreign SSIDs",
					"device", id, "err", notesErr)
			}
			elsewhere, elsewhereKnown := s.wouldAlsoBroadcast(ctx, id)
			for _, ap := range aps {
				if ap.SSID == "" {
					continue
				}
				v := broadcastView{SSID: ap.SSID, Iface: ap.Iface, BSSID: ap.BSSID,
					Origin: ProvUnknown}
				if sectionsKnown {
					if sec, ok := sections[ap.Iface]; ok {
						v.Section = sec
						if ours[sec] {
							v.Origin = ProvOurs
						} else {
							v.Origin = ProvForeign
						}
					}
				}
				if v.Origin == ProvForeign {
					brief := buildBrief(v, modes[ap.Iface], modesKnown,
						elsewhere, elsewhereKnown)
					if n, ok := notes[v.Section]; ok {
						brief.Note, brief.DecidedBy, brief.DecidedAt = n.Note, n.DecidedBy, n.DecidedAt
					}
					v.Brief = &brief
				}
				detail.Broadcasting = append(detail.Broadcasting, v)
			}
			sort.Slice(detail.Broadcasting, func(i, j int) bool {
				if detail.Broadcasting[i].SSID != detail.Broadcasting[j].SSID {
					return detail.Broadcasting[i].SSID < detail.Broadcasting[j].SSID
				}
				return detail.Broadcasting[i].Iface < detail.Broadcasting[j].Iface
			})
		}
	}
	detail.Interfaces, _ = s.Store.SeriesKeys(ctx, id, string(telemetry.KindIfaceRx))
	detail.Radios, _ = s.Store.SeriesKeys(ctx, id, string(telemetry.KindChanBusy))
	detail.Stations, _ = s.Store.SeriesKeys(ctx, id, string(telemetry.KindStaRSSI))
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) deviceByID(r *http.Request, id int64) (*store.Device, error) {
	return s.Store.DeviceByID(r.Context(), id)
}

// handleDeviceSeries lists what a device has recorded, which is the honest
// answer to "what can I chart" — it reflects what was collected rather than
// what the code can in principle produce.
func (s *Server) handleDeviceSeries(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	kinds := []telemetry.Kind{
		telemetry.KindLoad1, telemetry.KindMemPct,
		telemetry.KindIfaceRx, telemetry.KindIfaceTx,
		telemetry.KindAPClients, telemetry.KindAPAirtime, telemetry.KindChanBusy,
		telemetry.KindStaRSSI, telemetry.KindStaRx, telemetry.KindStaTx,
		telemetry.KindStaRetry,
	}
	out := map[string][]string{}
	for _, k := range kinds {
		keys, err := s.Store.SeriesKeys(r.Context(), id, string(k))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not list series")
			return
		}
		if len(keys) > 0 {
			out[string(k)] = keys
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": out})
}

// handleStats serves one series over a time range.
//
// The resolution is chosen by the store from the range, not requested by the
// caller: asking for 5-minute points across a year returns 105,000 points that
// the client will immediately throw away, and beyond 14 days the 5-minute table
// cannot answer completely anyway. The response says which resolution it used.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !knownKind(kind) {
		writeErr(w, http.StatusBadRequest, "unknown series kind")
		return
	}
	deviceID, err := strconv.ParseInt(r.URL.Query().Get("device_id"), 10, 64)
	if err != nil || deviceID <= 0 {
		writeErr(w, http.StatusBadRequest, "device_id is required")
		return
	}
	now := s.now()
	from := queryTime(r, "from", now.Add(-6*time.Hour))
	to := queryTime(r, "to", now)
	if !to.After(from) {
		writeErr(w, http.StatusBadRequest, "to must be after from")
		return
	}
	if to.Sub(from) > 400*24*time.Hour {
		writeErr(w, http.StatusBadRequest, "range exceeds the retention window")
		return
	}

	series, err := s.Store.QuerySeries(r.Context(), deviceID, kind,
		r.URL.Query().Get("key"), from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read the series")
		return
	}
	writeJSON(w, http.StatusOK, series)
}

func knownKind(k string) bool {
	switch telemetry.Kind(k) {
	case telemetry.KindLoad1, telemetry.KindMemUsed, telemetry.KindMemPct,
		telemetry.KindIfaceRx, telemetry.KindIfaceTx,
		telemetry.KindAPClients, telemetry.KindAPAirtime, telemetry.KindChanBusy,
		telemetry.KindStaRSSI, telemetry.KindStaRx, telemetry.KindStaTx,
		telemetry.KindStaRetry:
		return true
	}
	return false
}

// handleFocus raises a device to the focused poll rate for a bounded time.
//
// Bounded, and released by a timer rather than by a matching call. A caller that
// goes away does not get to run cleanup code, so a focus held until an explicit
// release would leak — and a leaked focus means a router polled every five
// seconds forever because somebody closed a laptop lid.
//
// The UI does NOT use this: it subscribes on the live channel, where the
// connection's lifetime is the focus's lifetime and the release is exact. This
// stays for clients that cannot hold a WebSocket — a script, a probe, a curl —
// where a lease is the only honest option.
func (s *Server) handleFocus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if s.Fleet == nil {
		writeErr(w, http.StatusServiceUnavailable, "the collector is not running")
		return
	}
	if _, err := s.deviceByID(r, id); handleStoreErr(w, err, "device") {
		return
	}
	seconds := queryInt(r, "seconds", 30, 5, 300)
	release := s.Fleet.Focus(id)
	time.AfterFunc(time.Duration(seconds)*time.Second, release)
	writeJSON(w, http.StatusOK, map[string]any{
		"focused_for_seconds": seconds,
	})
}

// handleOverhead reports what the controller costs one device.
//
// DEVICE-BUDGET §7 asks for this to be shown, not merely measured. The numbers
// are the ones the budget is actually written in — requests per minute and the
// interval currently in force — rather than the configured interval, which
// would understate a device we have deliberately backed off from.
func (s *Server) handleOverhead(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.deviceByID(r, id); handleStoreErr(w, err, "device") {
		return
	}
	if s.Fleet == nil {
		writeErr(w, http.StatusServiceUnavailable, "the collector is not running")
		return
	}
	o, ok := s.Fleet.Overhead(id)
	if !ok {
		// Adopted but not polled: an honest "nothing yet", not zero cost.
		writeErr(w, http.StatusNotFound, "this device is not being polled")
		return
	}
	dev, err := s.deviceByID(r, id)
	if handleStoreErr(w, err, "device") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"overhead": o,
		// DEVICE-BUDGET §7's remaining two fields.
		//
		// Packages is empty and will stay empty until something installs one.
		// It is reported rather than omitted because "we installed nothing on
		// your router" is the claim ARCHITECTURE §0 makes, and a field that
		// only appears once it is non-empty cannot be used to check it.
		"packages": []string{},
		"packages_note": "the controller installs no packages. Its entire " +
			"device-side footprint is one ACL file and one login, both listed " +
			"by un-adopt",
		"poll_interval_s": dev.PollInterval,
		"poll_interval_note": "0 uses the controller default. An override can " +
			"only make polling less frequent — a per-device knob that could " +
			"raise the rate would turn the budget into a suggestion",
	})
}

// handlePollInterval loosens (never tightens) one device's poll rate.
func (s *Server) handlePollInterval(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	dev, err := s.deviceByID(r, id)
	if handleStoreErr(w, err, "device") {
		return
	}
	var req struct {
		Seconds int `json:"seconds"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// An hour is already far past useful; beyond it a device would be reported
	// offline for most of the time it is fine.
	if req.Seconds < 0 || req.Seconds > 3600 {
		writeErr(w, http.StatusBadRequest,
			"the poll interval must be between 0 (controller default) and 3600 seconds")
		return
	}
	if err := s.Store.SetPollInterval(r.Context(), id, req.Seconds); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Re-register so the change takes effect without a restart.
	if s.Retrack != nil {
		s.Retrack(id)
	}
	devID := id
	_ = s.Store.LogEvent(r.Context(), store.Event{
		DeviceID: &devID, Category: "audit", Severity: "info",
		Event:  "device.poll_interval_set",
		Detail: map[string]any{"seconds": req.Seconds, "mac": dev.MAC},
	})
	writeJSON(w, http.StatusOK, map[string]any{"poll_interval_s": req.Seconds})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100, 1, 1000)
	offset := queryInt(r, "offset", 0, 0, 1<<30)
	category := r.URL.Query().Get("category")
	severity := r.URL.Query().Get("severity")

	// Filters go to the database, not to the page it returned. Filtering
	// afterwards selects from the newest N events overall rather than the
	// newest N matching, so a view filtered to "error" can come back empty
	// while errors exist.
	events, err := s.Store.QueryEventsPage(r.Context(), category, severity, limit, offset)
	if handleStoreErr(w, err, "events") {
		return
	}
	if events == nil {
		events = []store.Event{}
	}

	// The filter counts and the total come from an aggregate over the whole
	// table, per UI-SPEC §5. Counting the returned page instead would report "3
	// errors" from a page of 100 while the table holds three hundred — and
	// report it in exactly the same typeface as a true number.
	cats, sevs, total, err := s.Store.EventFacets(r.Context(), category, severity)
	if handleStoreErr(w, err, "events") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"total":  total,
		"limit":  limit,
		"offset": offset,
		"facets": map[string]any{"category": cats, "severity": sevs},
	})
}

// dashboard is the fleet summary: counts a human reads at a glance, plus what
// the controller is costing the devices.
type dashboard struct {
	Devices struct {
		Total   int `json:"total"`
		Online  int `json:"online"`
		Offline int `json:"offline"`
		Pending int `json:"pending"`
		Unknown int `json:"unknown"`
	} `json:"devices"`

	// WirelessClients counts stations ASSOCIATED to the fleet's radios, from
	// hostapd. It is nil when it cannot be totalled — if any AP's count was
	// unreadable, summing the rest reports a dip that means "a radio did not
	// answer". ClientsUnsure names the devices responsible.
	WirelessClients *int     `json:"wireless_clients"`
	ClientsUnsure   []string `json:"wireless_clients_unknown_on,omitempty"`

	// KnownDevices counts hosts on THIS network — wireless, wired and whatever
	// else answers ARP — and ActiveDevices is those seen recently. It is a
	// different question from WirelessClients and is deliberately a separate
	// number: showing one labelled "clients" next to a grid listing the other is
	// how a dashboard gets quietly distrusted.
	//
	// Both are scoped to store.ScopeLocal. A gateway's neighbour tables cover
	// every interface, so an unscoped count includes the neighbours on its
	// uplink — on the reference device that was 11 of 14, none of them anything
	// the operator owns. UpstreamDevices and UnscopedDevices carry the
	// remainder so the headline is smaller *and* legible, rather than smaller
	// for no visible reason.
	KnownDevices    int `json:"known_devices"`
	ActiveDevices   int `json:"active_devices"`
	UpstreamDevices int `json:"upstream_devices"`
	UnscopedDevices int `json:"unscoped_devices"`

	Focused  int           `json:"focused_devices"`
	Quiesced int           `json:"quiesced_devices"`
	Events   []store.Event `json:"recent_events"`
	Series   int           `json:"series_count"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	devices, err := s.Store.Devices(ctx)
	if handleStoreErr(w, err, "devices") {
		return
	}
	now := s.now()

	var d dashboard
	total := 0
	known := true
	for _, dev := range devices {
		v := s.viewDevice(dev, now)
		d.Devices.Total++
		switch v.Status {
		case "online":
			d.Devices.Online++
		case "offline":
			d.Devices.Offline++
		case "pending":
			d.Devices.Pending++
		default:
			d.Devices.Unknown++
		}
		if v.Tier == "focused" {
			d.Focused++
		}
		if v.Quiesced {
			d.Quiesced++
		}
		if v.Status != "online" {
			continue
		}
		n, ok := s.liveClientCount(dev.ID)
		if !ok {
			known = false
			d.ClientsUnsure = append(d.ClientsUnsure, dev.Name)
			continue
		}
		total += n
	}
	if known {
		d.WirelessClients = &total
	}

	// Counted in SQL and by scope, using the same call the client grid's filter
	// rail uses — see store.ClientCounts for why both go through one place.
	if counts, err := s.Store.ClientCounts(ctx, 0,
		now.Add(-clientActiveWindow).Unix()); err == nil {
		d.KnownDevices = counts[store.ScopeLocal].Total
		d.ActiveDevices = counts[store.ScopeLocal].Active
		d.UpstreamDevices = counts[store.ScopeUpstream].Active
		d.UnscopedDevices = counts[store.ScopeUnknown].Active
	} else {
		s.Log.Debug("could not count clients", "err", err)
	}

	if events, err := s.Store.RecentEvents(ctx, 20); err == nil {
		d.Events = events
	}
	if err := s.Store.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM series`).Scan(&d.Series); err != nil {
		s.Log.Debug("could not count series", "err", err)
	}
	writeJSON(w, http.StatusOK, d)
}

// liveClientCount asks the collector what the last poll saw.
//
// ok=false means the count is genuinely unknown — no poll has succeeded, or the
// call that would have counted them was refused. It never means "we have not
// flushed yet", which is why this reads live state rather than the rollups.
func (s *Server) liveClientCount(deviceID int64) (int, bool) {
	if s.Fleet == nil {
		return 0, false
	}
	return s.Fleet.LiveClients(deviceID)
}

// wouldAlsoBroadcast names the OTHER devices that would start transmitting a
// WLAN recreated on this device's groups.
//
// The site model has no per-device WLANs: a WLAN belongs to an AP group and
// fans out to every device in it. Recreating a foreign SSID therefore does not
// bring one network under management — it starts that network on every other AP
// in the group. Named as devices rather than as a group, because "all-aps"
// reads as a label and "wrt3200acm would start broadcasting it" reads as a
// consequence.
func (s *Server) wouldAlsoBroadcast(ctx context.Context, deviceID int64) ([]string, bool) {
	site, err := s.Store.Site(ctx)
	if err != nil {
		// NOT "no other AP is affected". The brief is about to tell somebody
		// what it costs to recreate a network, and a silent nil there is the
		// most expensive kind of reassurance.
		s.Log.Debug("could not read the site model for the fan-out warning", "err", err)
		return nil, false
	}
	devs, err := s.Store.Devices(ctx)
	if err != nil {
		s.Log.Debug("could not read the device list for the fan-out warning", "err", err)
		return nil, false
	}
	nameOf := map[int64]string{}
	for _, d := range devs {
		nameOf[d.ID] = d.Name
	}
	seen := map[int64]bool{}
	var out []string
	for _, g := range site.Groups {
		var carries bool
		for _, member := range g.DeviceIDs {
			if member == deviceID {
				carries = true
			}
		}
		if !carries {
			continue
		}
		for _, member := range g.DeviceIDs {
			if member == deviceID || seen[member] {
				continue
			}
			seen[member] = true
			if n := nameOf[member]; n != "" {
				out = append(out, n)
			}
		}
	}
	return sortedNames(out), true
}
