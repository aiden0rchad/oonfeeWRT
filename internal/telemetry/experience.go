package telemetry

import "math"

const ExperienceFormula = "wifi-v1"

// WiFiExperienceV1 returns a comparable 0–100 score only when all three
// portable inputs are known. It never renormalizes around missing data.
func WiFiExperienceV1(rssiDBM int, retryPct, txFailPct *float64) (float64, bool) {
	if retryPct == nil || txFailPct == nil ||
		!finitePercent(*retryPct) || !finitePercent(*txFailPct) {
		return 0, false
	}
	rssi := clamp(float64(rssiDBM+90)*2.5, 0, 100) // -90 dBm=0, -50 dBm=100.
	retry := 100 - clamp(*retryPct, 0, 100)
	fail := 100 - clamp(*txFailPct, 0, 100)
	return 0.45*rssi + 0.35*retry + 0.20*fail, true
}

func finitePercent(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type stationQualityCounters struct {
	Iface         string
	ConnectedTime int64
	Packets       int64
	Retries       int64
	Failed        int64
}

type stationQualityKey struct {
	deviceID int64
	mac      string
}

type stationQualityState struct {
	stationQualityCounters
	lastTS int64
}

// stationQualityDelta derives retry/failure percentages from one association.
// A first sample, roam, reset, or zero-packet interval is unavailable.
func stationQualityDelta(prev, cur stationQualityCounters) (retry, failed float64, ok bool) {
	delta, ok := stationQualityCounterDelta(prev, cur)
	if !ok {
		return 0, 0, false
	}
	return float64(delta.PacketsRetry) * 100 / float64(delta.Packets+delta.PacketsRetry),
		float64(delta.PacketsFailed) * 100 / float64(delta.Packets+delta.PacketsFailed), true
}

type stationQualityCounterDeltaResult struct {
	Packets       int64
	PacketsRetry  int64
	PacketsFailed int64
}

func stationQualityCounterDelta(prev, cur stationQualityCounters) (stationQualityCounterDeltaResult, bool) {
	if prev.Iface == "" || cur.Iface != prev.Iface ||
		prev.Packets < 0 || prev.Retries < 0 || prev.Failed < 0 ||
		cur.Packets < 0 || cur.Retries < 0 || cur.Failed < 0 ||
		cur.ConnectedTime < prev.ConnectedTime ||
		cur.Packets < prev.Packets || cur.Retries < prev.Retries || cur.Failed < prev.Failed {
		return stationQualityCounterDeltaResult{}, false
	}
	packets := cur.Packets - prev.Packets
	if packets == 0 {
		return stationQualityCounterDeltaResult{}, false
	}
	return stationQualityCounterDeltaResult{Packets: packets,
		PacketsRetry: cur.Retries - prev.Retries, PacketsFailed: cur.Failed - prev.Failed}, true
}

func (s *Store) observeStationQuality(deviceID int64, mac string, cur stationQualityCounters, ts int64) (float64, float64, bool) {
	delta, ok := s.observeStationQualityCounters(deviceID, mac, cur, ts)
	if !ok {
		return 0, 0, false
	}
	return float64(delta.PacketsRetry) * 100 / float64(delta.Packets+delta.PacketsRetry),
		float64(delta.PacketsFailed) * 100 / float64(delta.Packets+delta.PacketsFailed), true
}

func (s *Store) observeStationQualityCounters(deviceID int64, mac string, cur stationQualityCounters, ts int64) (stationQualityCounterDeltaResult, bool) {
	k := stationQualityKey{deviceID: deviceID, mac: mac}
	prev, seen := s.quality[k]
	s.quality[k] = stationQualityState{stationQualityCounters: cur, lastTS: ts}
	if !seen {
		return stationQualityCounterDeltaResult{}, false
	}
	return stationQualityCounterDelta(prev.stationQualityCounters, cur)
}
