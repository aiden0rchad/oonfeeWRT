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

	gauge := func(kind Kind, key string, v float64) {
		s.appendLocked(SeriesKey{DeviceID: dev, Kind: kind, Key: key}, ts, v)
	}
	observeWAN := func() {
		if snap.WAN == nil {
			return
		}
		up := 0.0
		if snap.WAN.Up {
			up = 1
		}
		gauge(KindSiteWANUp, "", up)
		gauge(KindSiteWANLoss, "", snap.WAN.LossPct)
		if snap.WAN.LatencyMS != nil {
			gauge(KindSiteWANLatency, "", *snap.WAN.LatencyMS)
		}
	}
	if snap.WANOnly {
		observeWAN()
		return
	}

	// Uptime first for full snapshots: everything below depends on knowing
	// whether the counters underneath it survived since the last full poll.
	// WAN-only snapshots carry no uptime and returned above, so their zero can
	// never masquerade as a reboot and erase counter baselines.
	rebooted := s.observeUptime(dev, snap.Uptime, ts)
	if rebooted {
		s.forgetCounters(dev)
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
	observeWAN()

	for name, iface := range snap.Interfaces {
		if skipInterface(name) {
			continue
		}
		// An interface that was down and is up again was recreated by netifd,
		// and its counters started over. Tracked per interface rather than per
		// device, because one link flapping says nothing about the others.
		recreated := s.ifaceCameBack(dev, ts, name, iface.Up)
		// Successful production decodes set per-direction presence. Hand-built
		// snapshots predating that metadata have NetDevsFresh=false and retain
		// their historical test/helper semantics.
		if iface.Stats.RxBytesKnown || !snap.NetDevsFresh {
			rate(KindIfaceRx, name, uint64(iface.Stats.RxBytes), recreated)
		}
		if iface.Stats.TxBytesKnown || !snap.NetDevsFresh {
			rate(KindIfaceTx, name, uint64(iface.Stats.TxBytes), recreated)
		}
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

	// Current frequency is measured by hostapd per BSS or the fresh sanitized
	// iwinfo inventory. Survey may include off-channel rows; only a row matching
	// current frequency may touch even the per-interface counter baseline.
	stableMapping := snap.RadiosKnown && !snap.RadiosStale
	radioMHz := map[string]int{}
	ambiguousMHz := map[string]bool{}
	ifaceMHz := map[string]int{}
	ambiguousIfaceMHz := map[string]bool{}
	if stableMapping {
		for _, observed := range snap.Radios {
			if observed.Key == "" {
				continue
			}
			if observed.CurrentAmbiguous {
				ambiguousMHz[observed.Key] = true
				continue
			}
			if observed.CurrentMHz == nil || *observed.CurrentMHz <= 0 {
				continue
			}
			radioMHz[observed.Key] = *observed.CurrentMHz
			for _, iface := range observed.Interfaces {
				if previous := ifaceMHz[iface.Name]; previous != 0 && previous != *observed.CurrentMHz {
					ambiguousIfaceMHz[iface.Name] = true
				}
				ifaceMHz[iface.Name] = *observed.CurrentMHz
			}
		}
		for iface, radioKey := range snap.IfaceRadios {
			if current := radioMHz[radioKey]; current > 0 {
				if previous := ifaceMHz[iface]; previous != 0 && previous != current {
					ambiguousIfaceMHz[iface] = true
				}
				ifaceMHz[iface] = current
			}
		}
	}
	for _, ap := range snap.APs {
		if ap.Freq <= 0 {
			continue
		}
		if previous := ifaceMHz[ap.Iface]; previous != 0 && previous != ap.Freq {
			ambiguousIfaceMHz[ap.Iface] = true
		}
		ifaceMHz[ap.Iface] = ap.Freq
		if stableMapping {
			radioKey := snap.IfaceRadios[ap.Iface]
			if radioKey == "" {
				continue
			}
			if previous := radioMHz[radioKey]; previous != 0 && previous != ap.Freq {
				ambiguousMHz[radioKey] = true
			}
			radioMHz[radioKey] = ap.Freq
		}
	}
	selectedIfaceSurveys := map[string]collector.Survey{}

	for _, sv := range snap.Surveys {
		requiredKnown := !sv.PresenceKnown ||
			(sv.MHzKnown && sv.ActiveTimeKnown && sv.BusyTimeKnown)
		if !requiredKnown || ambiguousIfaceMHz[sv.Iface] || ifaceMHz[sv.Iface] <= 0 ||
			sv.MHz != ifaceMHz[sv.Iface] {
			continue
		}
		if _, exists := selectedIfaceSurveys[sv.Iface]; exists {
			continue
		}
		selectedIfaceSurveys[sv.Iface] = sv
	}
	for iface, sv := range selectedIfaceSurveys {
		// Utilization is the ratio of two counter DELTAS, never of the absolute
		// values: busy_time and active_time do not share an epoch. See
		// Store.ratio — dividing the absolutes reads 25.9% on a radio that is
		// really at 73.3%.
		k := SeriesKey{DeviceID: dev, Kind: KindChanBusy, Key: iface}
		if v, ok := s.ratio(k, ts, uint64(sv.BusyTime), uint64(sv.ActiveTime), rebooted); ok {
			s.appendLocked(k, ts, v)
		}
	}
	selectedSurveys := map[string]collector.Survey{}
	if stableMapping {
		for _, sv := range selectedIfaceSurveys {
			radioKey := snap.IfaceRadios[sv.Iface]
			if radioKey == "" || ambiguousMHz[radioKey] || radioMHz[radioKey] <= 0 ||
				sv.MHz != radioMHz[radioKey] {
				continue
			}
			// Survey is per PHY but iwinfo exposes it per interface. Pick one row,
			// deterministically, so two SSIDs do not double-weight one radio.
			if existing, ok := selectedSurveys[radioKey]; !ok || sv.Iface < existing.Iface {
				selectedSurveys[radioKey] = sv
			}
			// The noise floor is deliberately not a series. It is gated per radio by
			// the capability probe, and on the reference device's 2.4 GHz radio it
			// swings 40+ dB between consecutive reads from either source. Averaging
			// that into a rollup would produce a smooth, stable, meaningless line —
			// the most convincing kind of wrong.
		}
	}
	for radioKey, survey := range selectedSurveys {
		k := SeriesKey{DeviceID: dev, Kind: KindRadioUtilization, Key: radioKey}
		utilization, utilizationOK := s.ratio(k, ts, uint64(survey.BusyTime), uint64(survey.ActiveTime), rebooted)
		if utilizationOK {
			s.appendLocked(k, ts, utilization)
		}
		airtimeFieldsKnown := !survey.PresenceKnown ||
			(survey.RxTimeKnown && survey.TxTimeKnown)
		if !snap.AirtimeSplit || !airtimeFieldsKnown {
			continue
		}
		rxKey := SeriesKey{DeviceID: dev, Kind: KindRadioRXAirtime, Key: radioKey}
		rx, rxOK := s.ratio(rxKey, ts, survey.RxTime, uint64(survey.ActiveTime), rebooted)
		if rxOK {
			s.appendLocked(rxKey, ts, rx)
		}
		txKey := SeriesKey{DeviceID: dev, Kind: KindRadioTXAirtime, Key: radioKey}
		tx, txOK := s.ratio(txKey, ts, survey.TxTime, uint64(survey.ActiveTime), rebooted)
		if txOK {
			s.appendLocked(txKey, ts, tx)
		}
		// Interference is the part of measured busy airtime not accounted for by
		// this radio receiving or transmitting. If the independently sampled
		// counters disagree, omit it instead of clamping a fabricated value.
		if utilizationOK && rxOK && txOK && utilization >= rx+tx {
			gauge(KindRadioInterference, radioKey, utilization-rx-tx)
		}
	}

	type radioStationAggregate struct {
		stations, quality, valid int64
		packets, retries, failed int64
		signalTotal              int64
		signalCount              int64
	}
	radioStations := map[string]*radioStationAggregate{}
	radioAssocComplete := map[string]bool{}
	assocMappingComplete := true
	if stableMapping && snap.AssocAsked != nil {
		for iface := range snap.AssocAsked {
			radioKey := snap.IfaceRadios[iface]
			if radioKey == "" {
				assocMappingComplete = false
				continue
			}
			if _, ok := radioAssocComplete[radioKey]; !ok {
				radioAssocComplete[radioKey] = true
			}
			if !snap.AssocAnswered[iface] {
				radioAssocComplete[radioKey] = false
			}
		}
	}

	for _, st := range snap.Stations {
		mac := strings.ToLower(st.MAC)
		if mac == "" {
			continue
		}
		signalKnown := st.SignalKnown || (!st.PresenceKnown && st.Signal != 0)
		radioKey := ""
		if stableMapping {
			radioKey = snap.IfaceRadios[st.Iface]
		}
		var aggregate *radioStationAggregate
		if radioKey != "" {
			aggregate = radioStations[radioKey]
			if aggregate == nil {
				aggregate = &radioStationAggregate{}
				radioStations[radioKey] = aggregate
			}
			aggregate.stations++
			if signalKnown {
				aggregate.signalTotal += int64(st.Signal)
				aggregate.signalCount++
			}
		}
		if signalKnown {
			gauge(KindStaRSSI, mac, float64(st.Signal))
		}
		if st.RX.BytesKnown || !st.PresenceKnown {
			rate(KindStaRx, mac, uint64(st.RX.Bytes), false)
		}
		if st.TX.BytesKnown || !st.PresenceKnown {
			rate(KindStaTx, mac, uint64(st.TX.Bytes), false)
		}
		qualityKnown := st.TXQualityKnown || (!st.PresenceKnown &&
			(st.TX.Packets != 0 || st.TX.Retries != 0 || st.TX.Failed != 0))
		if !qualityKnown {
			continue
		}
		if aggregate != nil {
			aggregate.quality++
		}
		delta, ok := s.observeStationQualityCounters(dev, mac, stationQualityCounters{
			Iface: st.Iface, ConnectedTime: st.ConnectedTime,
			Packets: st.TX.Packets, Retries: st.TX.Retries, Failed: st.TX.Failed,
		}, ts)
		if ok {
			retry := float64(delta.PacketsRetry) * 100 / float64(delta.Packets+delta.PacketsRetry)
			failed := float64(delta.PacketsFailed) * 100 / float64(delta.Packets+delta.PacketsFailed)
			gauge(KindStaRetryDelta, mac, retry)
			gauge(KindStaTXFailDelta, mac, failed)
			if experience, valid := WiFiExperienceV1(st.Signal, &retry, &failed); signalKnown && valid {
				gauge(KindStaExperienceWiFiV1, mac, experience)
			}
			if aggregate != nil {
				aggregate.valid++
				aggregate.packets += delta.Packets
				aggregate.retries += delta.PacketsRetry
				aggregate.failed += delta.PacketsFailed
			}
		}
	}
	for radioKey, aggregate := range radioStations {
		if snap.AssocAsked != nil && (!assocMappingComplete || !radioAssocComplete[radioKey]) {
			continue
		}
		if aggregate.signalCount == aggregate.stations && aggregate.signalCount > 0 {
			gauge(KindRadioSignalAvg, radioKey,
				float64(aggregate.signalTotal)/float64(aggregate.signalCount))
		}
		// A partial denominator is not a radio metric. If one associated station
		// omitted its counters, rebased, roamed, or sent no packets, omit both.
		if aggregate.quality != aggregate.stations || aggregate.valid != aggregate.quality ||
			aggregate.valid == 0 {
			continue
		}
		gauge(KindRadioRetryDelta, radioKey,
			float64(aggregate.retries)*100/float64(aggregate.packets+aggregate.retries))
		gauge(KindRadioTXFailDelta, radioKey,
			float64(aggregate.failed)*100/float64(aggregate.packets+aggregate.failed))
	}
}

// skipInterface drops the ones nobody graphs. Loopback is noise, and the
// bridge's own counters double-count its members.
func skipInterface(name string) bool {
	return name == "lo"
}

// ifaceCameBack reports an interface transitioning from down to up, which means
// netifd rebuilt it and its counters restarted.
func (s *Store) ifaceCameBack(deviceID int64, ts int64, name string, up bool) bool {
	k := SeriesKey{DeviceID: deviceID, Kind: "iface_up", Key: name}
	st := s.counters[k]
	if st == nil {
		st = &counterState{}
		s.counters[k] = st
	}
	was, had := st.last == 1, st.valid
	st.last, st.valid = 0, true
	st.lastTS = ts
	if up {
		st.last = 1
	}
	return had && !was && up
}
