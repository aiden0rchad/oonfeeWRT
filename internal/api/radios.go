package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/radio"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

var (
	ErrRadioScanUnavailable = errors.New("explicit RF scan is unavailable")
	ErrRadioNotFound        = errors.New("radio was not found")
)

// RadioScanner performs the one deliberately disruptive radio operation. The
// API requires an explicit acknowledgement before calling it.
type RadioScanner interface {
	ScanRadio(context.Context, int64, string) (model.RadioScan, []model.RadioScanBSS, error)
}

// radioFleet is optional so the existing fleet interface and its test doubles
// do not have to invent radio state. False means no successful inventory poll.
type radioFleet interface {
	Radios(deviceID int64) ([]radio.LiveState, bool)
}

type radioFleetStatus interface {
	RadioStatus(deviceID int64) (radio.CollectionStatus, bool)
}

type channelView struct {
	Band         string   `json:"band,omitempty"`
	Channel      int      `json:"channel"`
	MHz          int      `json:"mhz"`
	State        string   `json:"state"`
	Availability string   `json:"availability"`
	InUse        bool     `json:"in_use"`
	Restricted   *bool    `json:"restricted"`
	DFS          *bool    `json:"dfs"`
	Excluded     *bool    `json:"excluded"`
	Flags        []string `json:"flags"`
}

type radioView struct {
	radio.InventoryRadio
	InventoryObservedAt int64                `json:"inventory_observed_at,omitempty"`
	ChannelsObservedAt  int64                `json:"channels_observed_at,omitempty"`
	Stale               bool                 `json:"stale"`
	Channels            []channelView        `json:"channels"`
	ChannelsKnown       bool                 `json:"channels_known"`
	ScanCapability      string               `json:"scan_capability"`
	LatestScan          *model.RadioScan     `json:"latest_scan,omitempty"`
	LatestObservations  []model.RadioScanBSS `json:"latest_observations"`
	Suggested           *radioSuggestionView `json:"suggested,omitempty"`
}

type radioSuggestionView struct {
	radio.Suggestion
	ScanID     int64 `json:"scan_id"`
	ObservedAt int64 `json:"observed_at"`
}

type radioDeviceView struct {
	DeviceID int64                   `json:"device_id"`
	Name     string                  `json:"name"`
	Status   *radio.CollectionStatus `json:"status,omitempty"`
	Radios   []radioView             `json:"radios"`
}

type radiosResponse struct {
	GeneratedAt int64             `json:"generated_at"`
	Devices     []radioDeviceView `json:"devices"`
	Gaps        []string          `json:"gaps"`
}

func (s *Server) handleRadios(w http.ResponseWriter, r *http.Request) {
	devices, err := s.Store.Devices(r.Context())
	if handleStoreErr(w, err, "radios") {
		return
	}
	now := s.now()
	provider, _ := s.Fleet.(radioFleet)
	statusProvider, _ := s.Fleet.(radioFleetStatus)
	response := radiosResponse{GeneratedAt: now.UnixMilli(), Devices: []radioDeviceView{}, Gaps: []string{}}
	for _, device := range devices {
		if !device.Adopted() || !model.DeviceFunctionsOf(device.Functions, device.Role).Wireless() {
			continue
		}
		row := radioDeviceView{DeviceID: device.ID, Name: device.Name, Radios: []radioView{}}
		if provider == nil {
			response.Gaps = append(response.Gaps, fmt.Sprintf("device:%d: radio inventory has not been observed", device.ID))
			response.Devices = append(response.Devices, row)
			continue
		}
		states, known := provider.Radios(device.ID)
		if !known {
			response.Gaps = append(response.Gaps, fmt.Sprintf("device:%d: radio inventory has not been observed", device.ID))
			response.Devices = append(response.Devices, row)
			continue
		}
		status, statusKnown := radio.CollectionStatus{Stale: true}, false
		if statusProvider != nil {
			status, statusKnown = statusProvider.RadioStatus(device.ID)
		}
		if !statusKnown {
			for _, state := range states {
				if state.InventoryObservedAt > status.ObservedAt {
					status.ObservedAt = state.InventoryObservedAt
				}
			}
			response.Gaps = append(response.Gaps,
				fmt.Sprintf("device:%d: radio freshness is unavailable; values are last-known", device.ID))
		} else if status.Stale {
			reason := fmt.Sprintf("observed_at=%d", status.ObservedAt)
			if status.LastSourceAttemptAt > 0 && !status.LastSourceAttemptOK {
				reason = fmt.Sprintf("latest radio inventory refresh failed at %d", status.LastSourceAttemptAt)
			} else if status.ConsecutiveFailures > 0 {
				reason = fmt.Sprintf("latest poll failed (%d consecutive)", status.ConsecutiveFailures)
			}
			response.Gaps = append(response.Gaps,
				fmt.Sprintf("device:%d: radio state is stale/last-known: %s", device.ID, reason))
		}
		row.Status = &status
		caps := decodeRadioCapabilities(device.CapsJSON)
		for _, state := range states {
			inventory := state.InventoryRadio
			inventory.Interfaces = append([]radio.Interface{}, state.Interfaces...)
			view := radioView{InventoryRadio: inventory, Channels: []channelView{},
				InventoryObservedAt: state.InventoryObservedAt,
				ChannelsObservedAt:  state.FrequenciesObservedAt, Stale: status.Stale || !statusKnown,
				ChannelsKnown: state.FrequenciesKnown, ScanCapability: caps.State(capability.FeatRadioScan).String(),
				LatestObservations: []model.RadioScanBSS{}}
			if !state.FrequenciesKnown {
				response.Gaps = append(response.Gaps, fmt.Sprintf("device:%d/%s: channel list is unknown", device.ID, state.Key))
			}
			for _, frequency := range state.Frequencies {
				view.Channels = append(view.Channels, viewChannel(frequency, state.CurrentMHz))
			}
			scan, observations, err := s.Store.LatestRadioScan(r.Context(), model.RadioKey{DeviceID: device.ID, Section: state.Key})
			if err == nil {
				view.LatestScan, view.LatestObservations = &scan, observations
				if scan.Status == model.RadioScanCompleted && freshRadioScan(now, scan.FinishedAt) &&
					!view.Stale && freshRadioChannelPlan(now, state) {
					suggestions := radio.ScoreChannels(state.Frequencies, scanRows(observations))
					if len(suggestions) > 0 {
						sort.Slice(suggestions, func(i, j int) bool {
							if suggestions[i].Score == suggestions[j].Score {
								return suggestions[i].MHz < suggestions[j].MHz
							}
							return suggestions[i].Score > suggestions[j].Score
						})
						view.Suggested = &radioSuggestionView{Suggestion: suggestions[0],
							ScanID: scan.ID, ObservedAt: *scan.FinishedAt}
					}
				} else if scan.Status == model.RadioScanCompleted {
					response.Gaps = append(response.Gaps, fmt.Sprintf(
						"device:%d/%s: RF suggestion unavailable because the scan, radio state, or channel plan is stale",
						device.ID, state.Key))
				}
			} else if !errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusInternalServerError, "could not read radio scan history")
				return
			}
			row.Radios = append(row.Radios, view)
		}
		sort.Slice(row.Radios, func(i, j int) bool { return row.Radios[i].Key < row.Radios[j].Key })
		response.Devices = append(response.Devices, row)
	}
	writeJSON(w, http.StatusOK, response)
}

const radioSuggestionMaxAge = 24 * time.Hour
const radioChannelPlanMaxAge = 15 * time.Minute

func freshRadioScan(now time.Time, finishedAt *int64) bool {
	if finishedAt == nil {
		return false
	}
	age := now.Sub(time.UnixMilli(*finishedAt))
	return age >= 0 && age <= radioSuggestionMaxAge
}

func freshRadioChannelPlan(now time.Time, state radio.LiveState) bool {
	if !state.FrequenciesKnown || state.FrequenciesObservedAt <= 0 {
		return false
	}
	age := now.Sub(time.UnixMilli(state.FrequenciesObservedAt))
	return age >= 0 && age <= radioChannelPlanMaxAge
}

func viewChannel(value radio.Frequency, currentMHz *int) channelView {
	availability := "unknown"
	switch {
	case value.Restricted != nil && *value.Restricted:
		availability = "restricted"
	case value.Restricted != nil:
		availability = "enabled"
	}
	inUse := currentMHz != nil && *currentMHz == value.MHz
	state := availability
	if inUse {
		state = "in-use"
	}
	return channelView{Band: value.Band, Channel: value.Channel, MHz: value.MHz,
		State: state, Availability: availability, InUse: inUse,
		Restricted: value.Restricted, DFS: nil, Excluded: nil,
		Flags: append([]string(nil), value.Flags...)}
}

func decodeRadioCapabilities(raw string) *capability.Registry {
	registry := capability.NewRegistry()
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), registry)
	}
	return registry
}

func scanRows(rows []model.RadioScanBSS) []radio.ScanBSS {
	out := make([]radio.ScanBSS, 0, len(rows))
	for _, row := range rows {
		out = append(out, radio.ScanBSS{BSSID: row.BSSID, SSID: row.SSID, MHz: row.MHz,
			Channel: row.Channel, Signal: row.Signal, Width: row.Width})
	}
	return out
}

func (s *Server) handleRadioScan(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	radioKey := r.PathValue("radio")
	if !validRadioPathKey(radioKey) {
		writeErr(w, http.StatusBadRequest, "radio must be a UCI wifi-device section")
		return
	}
	var request struct {
		AcknowledgeDisruption bool `json:"acknowledge_disruption"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if !request.AcknowledgeDisruption {
		writeErr(w, http.StatusBadRequest, "acknowledge_disruption must be true because scanning takes this radio off-channel")
		return
	}
	if s.RadioScan == nil {
		writeErr(w, http.StatusServiceUnavailable, "explicit RF scanning is not available")
		return
	}
	scan, rows, err := s.RadioScan.ScanRadio(r.Context(), deviceID, radioKey)
	if err != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, store.ErrNotFound), errors.Is(err, ErrRadioNotFound):
			status = http.StatusNotFound
		case errors.Is(err, ErrRadioScanUnavailable):
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "scan": scan, "observations": rows})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scan": scan, "observations": rows})
}

func validRadioPathKey(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}
