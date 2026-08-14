package api

import (
	"encoding/json"
	"net/http"
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
// Bounded, and released by a timer rather than by a matching call. A browser tab
// that closes does not get to run cleanup code, so a focus held until an
// explicit release would leak — and a leaked focus means a router polled every
// five seconds forever because somebody closed a laptop lid. The UI re-posts
// while the screen is open; silence is how it stops.
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

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100, 1, 1000)
	events, err := s.Store.RecentEvents(r.Context(), limit)
	if handleStoreErr(w, err, "events") {
		return
	}
	category := r.URL.Query().Get("category")
	severity := r.URL.Query().Get("severity")
	out := make([]store.Event, 0, len(events))
	for _, e := range events {
		if category != "" && e.Category != category {
			continue
		}
		if severity != "" && e.Severity != severity {
			continue
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
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

	// KnownDevices counts everything seen on the LAN — wireless, wired and
	// whatever else answers ARP. It is a different question from
	// WirelessClients and is deliberately a separate number: showing one
	// labelled "clients" next to a grid listing the other is how a dashboard
	// gets quietly distrusted.
	KnownDevices  int `json:"known_devices"`
	ActiveDevices int `json:"active_devices"`

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

	if clients, err := s.Store.Clients(ctx, 0, 5000); err == nil {
		d.KnownDevices = len(clients)
		cutoff := now.Add(-clientActiveWindow).Unix()
		for _, c := range clients {
			if c.LastSeen != nil && *c.LastSeen >= cutoff {
				d.ActiveDevices++
			}
		}
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
