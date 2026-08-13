package telemetry

import (
	"context"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
)

// Observe turns one poll into samples. This is the sampling map: every series
// the controller keeps is produced here and nowhere else, so "where does this
// number come from" has exactly one answer.
//
// A failed poll contributes nothing and — importantly — does NOT reset any
// counter baseline. A missed poll is a gap, not a restart: the next successful
// poll computes its rate over the longer interval, which is the correct average
// for the time that actually passed. Rebasing on every hiccup would silently
// drop a sample each time the network stuttered.
func (s *Store) Observe(_ context.Context, snap collector.Snapshot) {
	if !snap.OK() {
		return
	}
	ts := snap.At.Unix()
	dev := snap.DeviceID

	s.mu.Lock()
	defer s.mu.Unlock()

	// Uptime first: everything below depends on knowing whether the counters
	// underneath it survived since the last poll.
	rebooted := s.observeUptime(dev, snap.Uptime, ts)
	if rebooted {
		s.forgetCounters(dev)
	}

	gauge := func(kind Kind, key string, v float64) {
		s.appendLocked(SeriesKey{DeviceID: dev, Kind: kind, Key: key}, ts, v)
	}
	rate := func(kind Kind, key string, counter uint64, recreated bool) {
		k := SeriesKey{DeviceID: dev, Kind: kind, Key: key}
		if v, ok := s.rate(k, ts, counter, rebooted, recreated); ok {
			s.appendLocked(k, ts, v)
		}
	}

	gauge(KindLoad1, "", snap.Load[0])
	if snap.Memory.Total > 0 {
		used := snap.Memory.Used()
		gauge(KindMemUsed, "", float64(used))
		gauge(KindMemPct, "", float64(used)*100/float64(snap.Memory.Total))
	}

	for name, iface := range snap.Interfaces {
		if skipInterface(name) {
			continue
		}
		// An interface that was down and is up again was recreated by netifd,
		// and its counters started over. Tracked per interface rather than per
		// device, because one link flapping says nothing about the others.
		recreated := s.ifaceCameBack(dev, name, iface.Up)
		rate(KindIfaceRx, name, uint64(iface.Stats.RxBytes), recreated)
		rate(KindIfaceTx, name, uint64(iface.Stats.TxBytes), recreated)
	}

	for _, ap := range snap.APs {
		// Only when it is known. A missing client count is not zero clients, and
		// a series that records the difference as data cannot be un-recorded.
		if ap.Clients != nil {
			gauge(KindAPClients, ap.Iface, float64(*ap.Clients))
		}
		if ap.Airtime != nil {
			gauge(KindAPAirtime, ap.Iface, ap.Airtime.UtilizationPercent())
		}
	}

	for _, sv := range snap.Surveys {
		if sv.ActiveTime > 0 {
			gauge(KindChanBusy, sv.Iface, sv.BusyPercent())
		}
		// The noise floor is deliberately not a series. It is gated per radio by
		// the capability probe, and on the reference device's 2.4 GHz radio it
		// swings 40+ dB between consecutive reads from either source. Averaging
		// that into a rollup would produce a smooth, stable, meaningless line —
		// the most convincing kind of wrong.
	}

	for _, st := range snap.Stations {
		mac := strings.ToLower(st.MAC)
		if mac == "" {
			continue
		}
		gauge(KindStaRSSI, mac, float64(st.Signal))
		rate(KindStaRx, mac, uint64(st.RX.Bytes), false)
		rate(KindStaTx, mac, uint64(st.TX.Bytes), false)
		if st.TX.Packets > 0 {
			// Retries as a share of packets actually sent. The raw counter grows
			// forever, so the ratio is the only form that means anything at a
			// glance — and it is computed here, never on the device.
			gauge(KindStaRetry, mac, float64(st.TX.Retries)*100/
				float64(st.TX.Packets+st.TX.Retries))
		}
	}
}

// skipInterface drops the ones nobody graphs. Loopback is noise, and the
// bridge's own counters double-count its members.
func skipInterface(name string) bool {
	return name == "lo"
}

// ifaceCameBack reports an interface transitioning from down to up, which means
// netifd rebuilt it and its counters restarted.
func (s *Store) ifaceCameBack(deviceID int64, name string, up bool) bool {
	k := SeriesKey{DeviceID: deviceID, Kind: "iface_up", Key: name}
	st := s.counters[k]
	if st == nil {
		st = &counterState{}
		s.counters[k] = st
	}
	was, had := st.last == 1, st.valid
	st.last, st.valid = 0, true
	if up {
		st.last = 1
	}
	return had && !was && up
}
