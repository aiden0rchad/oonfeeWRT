package daemon

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/telemetry"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// StartCollector begins polling every adopted device in the inventory.
//
// Devices that are not adopted are skipped rather than failed: a pending device
// has no credential to unseal, and polling it would produce a stream of
// authentication errors that look like a broken device instead of one that has
// simply not been set up yet.
func (d *Daemon) StartCollector(ctx context.Context, opts collector.Options) error {
	if opts.Log == nil {
		opts.Log = d.Log
	}
	c := collector.New(d.sink(), opts)

	devices, err := d.Store.Devices(ctx)
	if err != nil {
		return err
	}
	adopted := 0
	for _, dev := range devices {
		if !dev.Adopted() {
			continue
		}
		c.Add(d.target(dev))
		adopted++
	}
	c.Start(ctx)

	// Once, at startup: collect anything a previous run left behind (a device
	// removed while the daemon was down, or a crash between the cascade and the
	// sweep). One scan at boot, not one every five minutes.
	if err := d.Store.SweepOrphans(ctx); err != nil {
		d.Log.Error("could not sweep orphaned telemetry at startup", "err", err)
	}

	d.mu.Lock()
	d.collector = c
	d.mu.Unlock()
	d.Log.Info("collector started", "devices", adopted, "skipped", len(devices)-adopted)
	return nil
}

// target builds the collector's view of a device.
//
// Connect is a closure rather than a pre-opened client because the collector
// calls it again after dropping a session, and because a device that is offline
// when the daemon starts must not prevent the daemon from starting.
func (d *Daemon) target(dev *store.Device) collector.Target {
	id, mac, name := dev.ID, dev.MAC, dev.Name
	return collector.Target{
		DeviceID: id, MAC: mac, Name: name,
		Connect: func(ctx context.Context) (*ubus.Client, error) {
			// Re-read the row each time: the address, the certificate pin and
			// the credential can all have changed since the collector started,
			// and a stale copy would keep dialling somewhere that has moved.
			fresh, err := d.Store.DeviceByMAC(ctx, mac)
			if err != nil {
				return nil, err
			}
			return d.Connect(ctx, fresh)
		},
	}
}

// Track brings a newly adopted device into the poll loop.
func (d *Daemon) Track(dev *store.Device) {
	if c := d.collectorRef(); c != nil && dev.Adopted() {
		c.Add(d.target(dev))
	}
}

// Untrack removes a device from the poll loop, for un-adoption or deletion.
//
// It also sweeps telemetry whose series row the device cascade took with it.
// That sweep is a full scan of both rollup tables, which is why it lives here
// rather than on the five-minute tick: it should run when something can
// actually be orphaned, not every five minutes to find nothing.
func (d *Daemon) Untrack(deviceID int64) {
	if c := d.collectorRef(); c != nil {
		c.Remove(deviceID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Store.SweepOrphans(ctx); err != nil {
		d.Log.Error("could not sweep orphaned telemetry", "err", err)
	}
}

// Focus raises a device to the focused poll rate while a UI screen shows it.
// The returned function releases it and is safe to call more than once.
func (d *Daemon) Focus(deviceID int64) func() {
	if c := d.collectorRef(); c != nil {
		return c.Focus(deviceID)
	}
	return func() {}
}

func (d *Daemon) collectorRef() *collector.Collector {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.collector
}

// sink records what the poll loop learns.
//
// Phase 0 keeps this deliberately small — last_seen, poll state, and the
// transitions worth an event. Telemetry rollups are a later concern; what
// matters now is that reachability changes are recorded once, on the edge,
// rather than every interval. A device that has been down for a day should
// appear in the log once, not 1,440 times.
func (d *Daemon) sink() collector.Sink {
	var (
		mu    sync.Mutex
		up    = map[int64]bool{}
		known = map[int64]bool{}
		fw    = map[int64]string{}
	)
	return collector.SinkFunc(func(ctx context.Context, s collector.Snapshot) {
		mu.Lock()
		wasUp, seen := up[s.DeviceID], known[s.DeviceID]
		up[s.DeviceID], known[s.DeviceID] = s.OK(), true
		lastFW := fw[s.DeviceID]
		if s.Board != nil {
			fw[s.DeviceID] = s.Board.Release.Description
		}
		mu.Unlock()

		id := s.DeviceID
		switch {
		case !s.OK():
			if !seen || wasUp {
				d.Log.Warn("device unreachable", "device", s.MAC, "err", s.Err)
				_ = d.Store.LogEvent(ctx, store.Event{
					DeviceID: &id, Category: "device", Severity: "warning",
					Event:  "device.unreachable",
					Detail: map[string]any{"error": s.Err.Error(), "tier": string(s.Tier)},
				})
				_ = d.Store.SetLastSeen(ctx, id, s.At.Unix(), "backoff")
			}
			return
		case !wasUp && seen:
			d.Log.Info("device reachable again", "device", s.MAC)
			_ = d.Store.LogEvent(ctx, store.Event{
				DeviceID: &id, Category: "device", Severity: "info",
				Event: "device.reachable",
			})
		}

		if err := d.Store.SetLastSeen(ctx, id, s.At.Unix(), string(s.Tier)); err != nil {
			d.Log.Error("could not record last_seen", "device", s.MAC, "err", err)
		}

		// Into the ring. Nothing reaches the database here: raw samples are
		// drained on the five-minute tick, in one transaction (decision D4).
		d.Samples.Observe(ctx, s)
		d.recordClients(ctx, s)
		d.recordLiveClients(s)
		d.publishLive(s)

		// Record the firmware every time the board is re-read. Without this the
		// column stays empty forever: nothing else writes it after adoption.
		if s.Board != nil {
			if err := d.Store.SetFirmware(ctx, id, s.Board.Release.Description); err != nil {
				d.Log.Debug("could not record firmware", "device", s.MAC, "err", err)
			}
		}

		// A firmware change invalidates the capability snapshot: a new build can
		// add or remove ubus objects, and rendering against a stale registry is
		// how a screen offers a control the device no longer has.
		if s.Board != nil && lastFW != "" && s.Board.Release.Description != lastFW {
			d.Log.Warn("device firmware changed; its capability snapshot is stale",
				"device", s.MAC, "from", lastFW, "to", s.Board.Release.Description)
			_ = d.Store.LogEvent(ctx, store.Event{
				DeviceID: &id, Category: "device", Severity: "warning",
				Event: "device.firmware_changed",
				Detail: map[string]any{
					"from": lastFW, "to": s.Board.Release.Description,
				},
			})
		}

		// Degradations are logged at debug: they are a standing property of a
		// device's ACL or driver, not an event, and logging them per poll would
		// bury everything else.
		for _, deg := range s.Degraded {
			d.Log.Debug("poll degraded", "device", s.MAC, "detail", deg.String(),
				"permanent", deg.Permanent)
		}
	})
}

// StartMaintenance begins the telemetry tick: drain the ring into rollups, fold
// the hourly ladder, prune to retention.
//
// Separate from StartCollector so a caller can poll without persisting — which
// the integration tests do, and which is the honest split anyway: collecting and
// keeping are different decisions.
func (d *Daemon) StartMaintenance(ctx context.Context) {
	m := telemetry.NewMaintainer(d.Store, d.Samples, d.Log)
	if d.api != nil {
		// Idle sessions and lapsed login lockouts expire on the same cadence.
		// They want a periodic sweep, not a timer each.
		m.AfterTick = d.api.Sweep
	}
	done := make(chan struct{})

	// Its own cancellation, derived from the caller's. Shutdown must be able to
	// trigger the final flush directly rather than relying on whoever owns ctx
	// to have cancelled it first — Close is called on paths where nobody has.
	mctx, cancel := context.WithCancel(ctx)

	d.mu.Lock()
	if d.maint != nil {
		d.mu.Unlock()
		cancel()
		return
	}
	d.maint, d.maintDone, d.maintStop = m, done, cancel
	d.mu.Unlock()

	go func() {
		defer close(done)
		m.Run(mctx)
	}()
	d.Log.Info("telemetry maintenance started", "interval", m.Interval)
}

// stopMaintainer runs the final flush and waits for it.
//
// Waiting is the point: Run performs one last drain when its context is
// cancelled, and returning before that lands would defeat the whole reason for
// having it.
func (d *Daemon) stopMaintainer() {
	d.mu.Lock()
	m, done, cancel := d.maint, d.maintDone, d.maintStop
	d.maint, d.maintDone, d.maintStop = nil, nil, nil
	d.mu.Unlock()
	if m == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		d.Log.Error("the final telemetry flush did not finish; this window is lost")
	}
}

// recordClients writes the client inventory a poll saw.
//
// This is a database write per poll, which the rollup path deliberately avoids —
// and it is justified differently. The inventory is small (one row per host,
// updated in place), it is what makes the Client Devices screen exist at all
// between focused polls, and it is bounded by the number of things on the LAN
// rather than by time. Telemetry is unbounded in time, which is why that goes
// through the ring instead.
func (d *Daemon) recordClients(ctx context.Context, s collector.Snapshot) {
	if len(s.Hosts) == 0 {
		return
	}
	// The device's own interfaces show up in its ARP table. Listing a router as
	// a client of itself is confusing, and it would be counted in the fleet
	// client total.
	own := map[string]bool{}
	for _, iface := range s.Interfaces {
		if iface.MAC != "" {
			own[strings.ToLower(iface.MAC)] = true
		}
	}
	seen := make([]store.SeenClient, 0, len(s.Hosts))
	for _, h := range s.Hosts {
		if own[h.MAC] {
			continue
		}
		c := store.SeenClient{MAC: h.MAC, Name: h.Name, Scope: store.ScopeUnknown}
		if len(h.IPv4) > 0 {
			c.IPv4 = h.IPv4[0]
			// Scoped here, at ingest, rather than in the API handler: the
			// snapshot is the only place that has both the host and the
			// device's own subnets, and this package's rule is that handlers
			// read what was already worked out.
			c.Scope = s.Scope(c.IPv4)
		}
		seen = append(seen, c)
	}
	if err := d.Store.UpsertClients(ctx, seen, s.At.Unix()); err != nil {
		d.Log.Error("could not record clients", "device", s.MAC, "err", err)
	}
}

// liveClients reports the associated-station count from the most recent poll.
//
// Kept in memory rather than read back from the rollups: this is a question
// about right now, and the rollups only exist after the five-minute flush.
// Reading them made a freshly started controller answer "unknown" for five
// minutes while it was polling successfully the whole time — which is exactly
// the confusion between "could not find out" and "have not written it down"
// that the three-state rule exists to prevent.
func (d *Daemon) liveClients(deviceID int64) (int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, ok := d.lastClients[deviceID]
	if !ok || n == nil {
		return 0, false
	}
	return *n, true
}

// recordLiveClients stores what a poll learned, including that it could not
// find out — a nil entry is the "asked and was refused" state, distinct from
// having no entry at all.
func (d *Daemon) recordLiveClients(s collector.Snapshot) {
	n, ok := s.ClientCount()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastClients == nil {
		d.lastClients = map[int64]*int{}
	}
	if !ok {
		d.lastClients[s.DeviceID] = nil
		return
	}
	v := n
	d.lastClients[s.DeviceID] = &v
}

// publishLive pushes a completed poll to any browser watching this device.
//
// It sends what a screen shows rather than the whole snapshot: the raw poll
// carries interface counters and station byte totals, which are inputs to
// derived series and meaningless on their own. Sending them would invite a
// client to do the derivation itself, differently, and DEVICE-BUDGET §4.3 is
// clear that derivation happens in one place.
func (d *Daemon) publishLive(s collector.Snapshot) {
	d.mu.Lock()
	api := d.api
	d.mu.Unlock()
	if api == nil || api.Hub == nil {
		return
	}
	if !s.OK() {
		api.Hub.Publish(s.DeviceID, map[string]any{
			"type": "device.unreachable", "device_id": s.DeviceID,
			"ts": s.At.Unix(), "error": s.Err.Error(),
		})
		return
	}

	aps := make([]map[string]any, 0, len(s.APs))
	for _, ap := range s.APs {
		e := map[string]any{"iface": ap.Iface, "ssid": ap.SSID,
			"channel": ap.Channel, "freq": ap.Freq}
		// nil, not zero: "we could not ask" and "nobody is connected" are
		// different answers, and JSON null is how that survives the wire.
		e["clients"] = ap.Clients
		if ap.Airtime != nil {
			e["airtime_pct"] = ap.Airtime.UtilizationPercent()
		}
		aps = append(aps, e)
	}
	stations := make([]map[string]any, 0, len(s.Stations))
	for _, st := range s.Stations {
		stations = append(stations, map[string]any{
			"mac": st.MAC, "iface": st.Iface, "signal": st.Signal,
			"rx_kbit": st.RX.Rate, "tx_kbit": st.TX.Rate,
			"connected_seconds": st.ConnectedTime,
		})
	}
	clients, clientsKnown := s.ClientCount()
	msg := map[string]any{
		"type": "stats", "device_id": s.DeviceID, "ts": s.At.Unix(),
		"tier": string(s.Tier), "uptime": s.Uptime, "load1": s.Load[0],
		"poll_ms": s.Duration.Milliseconds(),
		"aps":     aps, "stations": stations,
		"degraded": len(s.Degraded),
	}
	if s.Memory.Total > 0 {
		msg["mem_pct"] = float64(s.Memory.Used()) * 100 / float64(s.Memory.Total)
	}
	if clientsKnown {
		msg["clients"] = clients
	} else {
		msg["clients"] = nil
	}
	api.Hub.Publish(s.DeviceID, msg)
}
