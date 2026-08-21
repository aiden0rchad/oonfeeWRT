package radio

import "math"

// Suggestion is a transparent least-occupied-channel score. It is not DFS
// advice and does not override a restricted/unknown availability state.
type Suggestion struct {
	Channel int     `json:"channel"`
	MHz     int     `json:"mhz"`
	Score   float64 `json:"score"`
	Basis   string  `json:"basis"`
}

const (
	SuggestionBasis         = "scan-v1: RSSI-weighted 20 MHz spectral overlap; unknown BSS width is conservatively 40 MHz; higher is better"
	candidateWidthMHz       = 20.0
	unknownObservedWidthMHz = 40.0
)

// ScoreChannels scores only frequencies proved unrestricted. A missing
// Restricted field is unknown, not enabled. Unknown RSSI contributes a modest
// penalty rather than becoming zero-strength evidence.
func ScoreChannels(frequencies []Frequency, observations []ScanBSS) []Suggestion {
	out := []Suggestion{}
	for _, frequency := range frequencies {
		if frequency.Restricted == nil || *frequency.Restricted {
			continue
		}
		penalty := 0.0
		for _, bss := range observations {
			overlap := spectralOverlap(frequency.MHz, bss)
			if overlap == 0 {
				continue
			}
			strength := 25.0
			if bss.Signal != nil {
				strength = math.Max(0, math.Min(100, float64(*bss.Signal+100)*2))
			}
			penalty += strength * overlap
		}
		score := 100 / (1 + penalty/100)
		out = append(out, Suggestion{Channel: frequency.Channel, MHz: frequency.MHz,
			Score: math.Round(score*10) / 10, Basis: SuggestionBasis})
	}
	return out
}

func spectralOverlap(candidateMHz int, observed ScanBSS) float64 {
	width := unknownObservedWidthMHz
	if observed.Width != nil {
		width = math.Max(candidateWidthMHz, math.Min(320, float64(*observed.Width)))
	}
	candidateLow, candidateHigh := float64(candidateMHz)-candidateWidthMHz/2,
		float64(candidateMHz)+candidateWidthMHz/2
	observedLow, observedHigh := float64(observed.MHz)-width/2, float64(observed.MHz)+width/2
	overlapMHz := math.Min(candidateHigh, observedHigh) - math.Max(candidateLow, observedLow)
	if overlapMHz <= 0 {
		return 0
	}
	return math.Min(1, overlapMHz/candidateWidthMHz)
}
