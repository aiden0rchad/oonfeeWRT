package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
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
	baselineSeconds := max(0, dev.PollInterval)
	if baselineSeconds > int((15*time.Minute)/time.Second) {
		// Older databases may contain values accepted before the API capped the
		// control. Keep radio/topology refresh truthful without requiring a data
		// rewrite merely to normalize a scheduling option.
		baselineSeconds = int((15 * time.Minute) / time.Second)
	}
	baseline := time.Duration(baselineSeconds) * time.Second
	airtimeSplit := false
	if caps, err := deviceCaps(dev); err == nil {
		airtimeSplit = caps.State(capability.FeatAirtimeSplit).Buildable()
	}
	return collector.Target{
		DeviceID: id, MAC: mac, Name: name, Class: dev.Class,
		Gateway:       deviceFunctions(dev).Routes(),
		AirtimeSplit:  airtimeSplit,
		ConnectionKey: deviceConnectionKey(dev),
		Baseline:      baseline,
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

func deviceConnectionKey(dev *store.Device) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%q\x00%q\x00%d\x00%q\x00%q\x00",
		dev.MAC, dev.Host, dev.Port, dev.Scheme, dev.CertFP)
	_, _ = h.Write(dev.CredEnc)
	return hex.EncodeToString(h.Sum(nil))
}

// Track brings a newly adopted device into the poll loop.
func (d *Daemon) Track(dev *store.Device) {
	if c := d.collectorRef(); c != nil && dev.Adopted() {
		c.Add(d.target(dev))
	}
}

// registerDevice makes inventory identity creation and collector registration
// atomic with deletion's post-commit purge. Without this boundary SQLite can
// reuse the deleted ID while deleteDevice still holds old process state, and
// that purge can erase the replacement's samples, ownership, subscriptions,
// and freshly registered poller.
func (d *Daemon) registerDevice(ctx context.Context, dev *store.Device) error {
	d.telemetryLifecycle.Lock()
	defer d.telemetryLifecycle.Unlock()
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		return err
	}
	d.Track(dev)
	return nil
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
	d.purgeDeviceState(deviceID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Store.SweepOrphans(ctx); err != nil {
		d.Log.Error("could not sweep orphaned telemetry", "err", err)
	}
}

// purgeDeviceState is the single identity boundary for process-local state.
// SQLite may reuse an INTEGER PRIMARY KEY immediately after deletion, so every
// cache keyed by that number must become unknown before a replacement can be
// observed. The collector is removed first; Remove is an emission barrier, so
// an old in-flight poll cannot repopulate these maps after this returns.
func (d *Daemon) purgeDeviceState(deviceID int64) {
	d.telemetryLifecycle.Lock()
	defer d.telemetryLifecycle.Unlock()
	d.purgeDeviceStateLocked(deviceID)
}

// purgeDeviceStateLocked requires telemetryLifecycle. deleteDevice already
// holds it across the database identity boundary and calls this form directly.
func (d *Daemon) purgeDeviceStateLocked(deviceID int64) {
	if d.Samples != nil {
		d.Samples.ForgetDevice(deviceID)
	}

	d.mu.Lock()
	delete(d.lastClients, deviceID)
	delete(d.lastStations, deviceID)
	delete(d.lastPresence, deviceID)
	apiServer := d.api
	d.mu.Unlock()

	d.meshes.forget(deviceID)
	d.reprobes.forget(deviceID)

	d.sinkMu.Lock()
	delete(d.sinkUp, deviceID)
	delete(d.sinkKnown, deviceID)
	delete(d.sinkFirmware, deviceID)
	logs, topologyIngest, cascadeIngest := d.logIngest, d.topologyIngest, d.cascadeIngest
	d.sinkMu.Unlock()
	if logs != nil {
		logs.forgetDevice(deviceID)
	}
	if topologyIngest != nil {
		topologyIngest.forgetDevice(deviceID)
	}
	if cascadeIngest != nil {
		cascadeIngest.forgetDevice(deviceID)
	}

	// The last neighbour result embeds a fleet roster. Invalidate the whole
	// cycle rather than trying to edit a historical result into a new identity.
	d.nbrMu.Lock()
	d.lastNeighbourRun = nil
	d.nbrMu.Unlock()

	if apiServer != nil && apiServer.Hub != nil {
		apiServer.Hub.ForgetDevice(deviceID)
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
	d.sinkMu.Lock()
	if d.sinkUp == nil {
		d.sinkUp = map[int64]bool{}
		d.sinkKnown = map[int64]bool{}
		d.sinkFirmware = map[int64]string{}
	}
	if d.logIngest == nil {
		d.logIngest = newLogIngestor(d.Store)
	}
	if d.topologyIngest == nil {
		d.topologyIngest = newTopologyIngestor(d.Store)
	}
	if d.cascadeIngest == nil {
		d.cascadeIngest = newCascadeGrouper(
			d.Store, d.Log, cascadeOfflineWindow, cascadeOnlineWindow, true)
	}
	logs, topologyIngest, cascadeIngest := d.logIngest, d.topologyIngest, d.cascadeIngest
	d.sinkMu.Unlock()
	return collector.SinkFunc(func(ctx context.Context, s collector.Snapshot) {
		if s.WANOnly || s.LogOnly {
			// A minute auxiliary poll carries no fresh uptime, load, clients or
			// topology. Consume only the payloads explicitly marked present.
			if s.WANOnly {
				d.Samples.Observe(ctx, s)
			}
			if s.LogOnly && s.LogsFresh {
				if err := logs.record(ctx, s.DeviceID, s.At, s.LogEpoch, s.Logs); err != nil {
					d.Log.Error("could not ingest auxiliary OpenWrt log page", "device", s.MAC, "err", err)
				}
			}
			for _, deg := range s.Degraded {
				d.Log.Debug("auxiliary poll degraded", "device", s.MAC,
					"detail", deg.String(), "permanent", deg.Permanent)
			}
			return
		}
		d.sinkMu.Lock()
		wasUp, seen := d.sinkUp[s.DeviceID], d.sinkKnown[s.DeviceID]
		d.sinkUp[s.DeviceID], d.sinkKnown[s.DeviceID] = s.OK(), true
		lastFW := d.sinkFirmware[s.DeviceID]
		if s.Board != nil {
			d.sinkFirmware[s.DeviceID] = s.Board.Release.Description
		}
		d.sinkMu.Unlock()

		d.meshes.put(s)

		id := s.DeviceID
		switch {
		case !s.OK():
			if err := topologyIngest.unavailable(ctx, s); err != nil {
				d.Log.Error("could not mark topology sources unavailable", "device", s.MAC, "err", err)
			}
			if !seen || wasUp {
				d.Log.Warn("device unreachable", "device", s.MAC, "err", s.Err)
				if err := d.Store.LogEvent(ctx, store.Event{
					DeviceID: &id, Category: "device", Severity: "warning",
					Event:  "device.unreachable",
					Detail: map[string]any{"error": s.Err.Error(), "tier": string(s.Tier)},
				}); err != nil {
					d.Log.Error("could not persist device unreachable event", "device", s.MAC, "err", err)
				}
				if err := cascadeIngest.observe(ctx, cascadeOffline, id, s.At); err != nil {
					d.Log.Error("could not group device unreachable event", "device", s.MAC, "err", err)
				}
				_ = d.Store.SetLastSeen(ctx, id, s.At.Unix(), "backoff")
			}
			return
		case !wasUp && seen:
			d.Log.Info("device reachable again", "device", s.MAC)
			if err := d.Store.LogEvent(ctx, store.Event{
				DeviceID: &id, Category: "device", Severity: "info",
				Event: "device.reachable",
			}); err != nil {
				d.Log.Error("could not persist device reachable event", "device", s.MAC, "err", err)
			}
			if err := cascadeIngest.observe(ctx, cascadeOnline, id, s.At); err != nil {
				d.Log.Error("could not group device reachable event", "device", s.MAC, "err", err)
			}
		}

		lastSeenRecorded := true
		if err := d.Store.SetLastSeen(ctx, id, s.At.Unix(), string(s.Tier)); err != nil {
			lastSeenRecorded = false
			d.Log.Error("could not record last_seen", "device", s.MAC, "err", err)
		}
		if s.LogsFresh && lastSeenRecorded {
			if err := logs.record(ctx, id, s.At, s.LogEpoch, s.Logs); err != nil {
				d.Log.Error("could not ingest OpenWrt log page", "device", s.MAC, "err", err)
			}
		}
		if lastSeenRecorded {
			if err := topologyIngest.record(ctx, s); err != nil {
				d.Log.Error("could not persist topology observation", "device", s.MAC, "err", err)
			}
		}

		// Into the ring. Nothing reaches the database here: raw samples are
		// drained on the five-minute tick, in one transaction (decision D4).
		d.Samples.Observe(ctx, s)
		d.recordClients(ctx, s)
		d.recordLiveClients(s)
		// Publish timestamped presence before the untimestamped station map.
		// An API read may otherwise observe a newly associated MAC without the
		// poll time that proves it current. The reverse intermediate state is
		// safe: evidence can exist briefly before RF enrichment catches up.
		d.recordLivePresence(s)
		d.recordLiveStations(s)
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
		// how a screen offers a control the device no longer has. So this does
		// not merely say so — it re-probes, in the background, because the poll
		// callback must return and a probe is dozens of calls.
		if s.Board != nil && lastFW != "" && s.Board.Release.Description != lastFW {
			d.Log.Warn("device firmware changed; re-probing its capabilities",
				"device", s.MAC, "from", lastFW, "to", s.Board.Release.Description)
			_ = d.Store.LogEvent(ctx, store.Event{
				DeviceID: &id, Category: "device", Severity: "warning",
				Event: "device.firmware_changed",
				Detail: map[string]any{
					"from": lastFW, "to": s.Board.Release.Description,
				},
			})
			d.reprobeAfterFirmwareChange(id, s.MAC, lastFW,
				s.Board.Release.Description)
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

func (d *Daemon) stopCascadeEvents(ctx context.Context) error {
	d.sinkMu.Lock()
	grouper := d.cascadeIngest
	d.sinkMu.Unlock()
	if grouper == nil {
		return nil
	}
	return grouper.stopAndFlush(ctx)
}

// StartMaintenance begins the telemetry tick: drain the ring into rollups, fold
// the hourly ladder, prune to retention.
//
// Separate from StartCollector so a caller can poll without persisting — which
// the integration tests do, and which is the honest split anyway: collecting and
// keeping are different decisions.
func (d *Daemon) StartMaintenance(ctx context.Context) {
	m := telemetry.NewMaintainer(d.Store, d.Samples, d.Log)
	m.Lifecycle = &d.telemetryLifecycle
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
func (d *Daemon) stopMaintainer() error {
	d.mu.Lock()
	m, done, cancel := d.maint, d.maintDone, d.maintStop
	d.maint, d.maintDone, d.maintStop = nil, nil, nil
	d.mu.Unlock()
	if m == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-time.After(30 * time.Second):
		d.Log.Error("the final telemetry flush did not finish; this window is lost")
		return errors.New("daemon: final telemetry flush did not finish")
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

// recordLiveStations stores which clients the last poll saw associated, so the
// clients grid can answer "which AP is this on" from the baseline rate rather
// than waiting for a focused poll and a five-minute rollup flush.
func (d *Daemon) recordLiveStations(s collector.Snapshot) {
	m, ok := s.LiveStations()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastStations == nil {
		d.lastStations = map[int64]collector.LiveStationSet{}
	}
	if !ok {
		// Nil is "we could not find out", which the API must not read as
		// "nobody is associated" — the same rule lastClients follows.
		d.lastStations[s.DeviceID] = nil
		return
	}
	d.lastStations[s.DeviceID] = m
}

// liveStations reports the last poll's associated stations for one device.
func (d *Daemon) liveStations(deviceID int64) (collector.LiveStationSet, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m, ok := d.lastStations[deviceID]
	if !ok || m == nil {
		return nil, false
	}
	return m, true
}

// recordLivePresence retains the last proof per MAC, not the last time an
// inventory source repeated the MAC. The retention bound matches the durable
// client inventory, so randomized addresses cannot grow this map forever.
func (d *Daemon) recordLivePresence(s collector.Snapshot) {
	observations := s.ClientPresenceObservations()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastPresence == nil {
		d.lastPresence = map[int64]*clientPresenceCache{}
	}
	state := d.lastPresence[s.DeviceID]
	if state == nil {
		state = &clientPresenceCache{
			sources: map[string]collector.ClientPresence{}, lastSeen: collector.ClientPresence{},
		}
	}
	cutoff := s.At.Add(-telemetry.DefaultClientTTL).Unix()
	for source, clients := range state.sources {
		for mac, at := range clients {
			if at < cutoff {
				delete(clients, mac)
			}
		}
		if len(clients) == 0 {
			delete(state.sources, source)
		}
	}
	for mac, at := range state.lastSeen {
		if at < cutoff {
			delete(state.lastSeen, mac)
		}
	}
	for _, observation := range observations {
		clients := make(collector.ClientPresence, len(observation.Clients))
		for mac, at := range observation.Clients {
			clients[mac] = at
			if at > state.lastSeen[mac] {
				state.lastSeen[mac] = at
			}
		}
		// Replacement is load-bearing: a successful known-empty source clears
		// its previous current set. Failed/unasked sources emit no observation
		// and retain their timestamp until the API freshness window expires.
		state.sources[observation.Source] = clients
	}
	d.lastPresence[s.DeviceID] = state
}

func (d *Daemon) livePresence(deviceID int64) (collector.ClientPresenceState, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.lastPresence[deviceID]
	if !ok {
		return collector.ClientPresenceState{}, false
	}
	out := collector.ClientPresenceState{
		Active: collector.ClientPresence{}, LastSeen: collector.ClientPresence{},
	}
	for _, clients := range state.sources {
		for mac, at := range clients {
			if at > out.Active[mac] {
				out.Active[mac] = at
			}
		}
	}
	for mac, at := range state.lastSeen {
		out.LastSeen[mac] = at
	}
	return out, true
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
		var signal *int
		if st.SignalKnown || (!st.PresenceKnown && st.Signal != 0) {
			value := st.Signal
			signal = &value
		}
		var rx, tx *int64
		if st.RX.RateKnown || !st.PresenceKnown {
			value := st.RX.Rate
			rx = &value
		}
		if st.TX.RateKnown || !st.PresenceKnown {
			value := st.TX.Rate
			tx = &value
		}
		stations = append(stations, map[string]any{
			"mac": st.MAC, "iface": st.Iface, "signal": signal,
			"rx_kbit": rx, "tx_kbit": tx,
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
