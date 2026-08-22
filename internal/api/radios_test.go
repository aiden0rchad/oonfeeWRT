package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/radio"
)

type radioFleetStub struct {
	*stubFleet
	states   map[int64][]radio.LiveState
	statuses map[int64]radio.CollectionStatus
}

func (f *radioFleetStub) RadioStatus(deviceID int64) (radio.CollectionStatus, bool) {
	status, ok := f.statuses[deviceID]
	return status, ok
}

func (f *radioFleetStub) Radios(deviceID int64) ([]radio.LiveState, bool) {
	rows, ok := f.states[deviceID]
	return rows, ok
}

type radioScannerStub struct {
	deviceID int64
	radioKey string
	calls    int
}

func (s *radioScannerStub) ScanRadio(_ context.Context, deviceID int64, key string) (model.RadioScan, []model.RadioScanBSS, error) {
	s.deviceID, s.radioKey, s.calls = deviceID, key, s.calls+1
	finished := int64(20)
	return model.RadioScan{ID: 9, Radio: model.RadioKey{DeviceID: deviceID, Section: key},
			StartedAt: 10, FinishedAt: &finished, Status: model.RadioScanCompleted},
		[]model.RadioScanBSS{{ScanID: 9, BSSID: "00:11:22:33:44:55", MHz: 5180, Channel: 36}}, nil
}

func TestRadiosPreserveUnknownDFSAndUseStableKeys(t *testing.T) {
	h := newHarness(t)
	h.setup()
	device := h.seedDevice("radio-ap", true, nil)
	caps := capability.NewRegistry()
	caps.Set(capability.FeatRadioScan, capability.Present)
	if err := h.db.SetCapabilities(context.Background(), device.ID, caps, "A"); err != nil {
		t.Fatal(err)
	}
	no, yes := false, true
	mhz, channel := 5180, 36
	const observedAt = int64(1_787_100_000_000)
	h.srv.Now = func() time.Time { return time.UnixMilli(observedAt + 60_000) }
	h.srv.Fleet = &radioFleetStub{stubFleet: h.fleet,
		statuses: map[int64]radio.CollectionStatus{device.ID: {
			ObservedAt: observedAt, LastPollAt: observedAt, LastPollOK: true,
		}}, states: map[int64][]radio.LiveState{
			device.ID: {{InventoryRadio: radio.InventoryRadio{Key: "radio0", Band: "5g",
				CurrentMHz: &mhz, CurrentChannel: &channel, Interfaces: []radio.Interface{{Name: "phy0-ap0", Mode: "ap"}}},
				InventoryObservedAt: observedAt, FrequenciesObservedAt: observedAt,
				FrequenciesKnown: true, Frequencies: []radio.Frequency{
					{Band: "5g", Channel: 36, MHz: 5180, Restricted: &no, Active: &yes, Flags: []string{}},
					{Band: "5g", Channel: 44, MHz: 5220, Restricted: &no, Flags: []string{}},
					{Band: "5g", Channel: 52, MHz: 5260, Restricted: &yes, Flags: []string{"NO-IR"}},
				}},
			}}}

	scan := &model.RadioScan{Radio: model.RadioKey{DeviceID: device.ID, Section: "radio0"}, StartedAt: observedAt - 1_000}
	if err := h.db.CreateRadioScan(context.Background(), scan); err != nil {
		t.Fatal(err)
	}
	signal := -45
	if err := h.db.FinishRadioScan(context.Background(), scan.ID, model.RadioScanCompleted, observedAt, nil,
		[]model.RadioScanBSS{{BSSID: "00:11:22:33:44:55", MHz: 5180, Channel: 36, Signal: &signal}}); err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, "/api/v1/radios", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("radios: %d %s", w.Code, w.Body.String())
	}
	var response radiosResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Devices) != 1 || len(response.Devices[0].Radios) != 1 {
		t.Fatalf("response = %+v", response)
	}
	got := response.Devices[0].Radios[0]
	if got.Key != "radio0" || got.Channels[0].State != "in-use" ||
		got.Channels[1].State != "enabled" || got.Channels[2].State != "restricted" {
		t.Fatalf("channel plan = %+v", got)
	}
	for _, row := range got.Channels {
		if row.DFS != nil || row.Excluded != nil {
			t.Fatalf("unknown DFS/exclusion became a verdict: %+v", row)
		}
	}
	if got.Suggested == nil || got.Suggested.Channel != 44 || got.ScanCapability != "present" {
		t.Fatalf("suggestion/capability = %+v", got)
	}
	if response.GeneratedAt == got.InventoryObservedAt || got.Stale || len(response.Gaps) != 0 {
		t.Fatalf("generation/source freshness collapsed: response=%+v radio=%+v", response, got)
	}
}

func TestCurrentChannelDoesNotEraseUnknownAvailability(t *testing.T) {
	mhz := 5745
	got := viewChannel(radio.Frequency{Band: "5g", Channel: 149, MHz: mhz}, &mhz)
	if got.State != "in-use" || !got.InUse || got.Availability != "unknown" || got.Restricted != nil {
		t.Fatalf("current unknown channel collapsed availability: %+v", got)
	}
}

func TestRadiosSerializeUnconfiguredInterfacesAsAnEmptyArray(t *testing.T) {
	h := newHarness(t)
	h.setup()
	device := h.seedDevice("disabled-radio-ap", true, nil)
	const observedAt = int64(1_787_100_000_000)
	h.srv.Fleet = &radioFleetStub{stubFleet: h.fleet,
		statuses: map[int64]radio.CollectionStatus{device.ID: {ObservedAt: observedAt, LastPollOK: true}},
		states: map[int64][]radio.LiveState{device.ID: {{
			InventoryRadio:      radio.InventoryRadio{Key: "radio0", Interfaces: nil},
			InventoryObservedAt: observedAt,
		}}}}

	w := h.do(http.MethodGet, "/api/v1/radios", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("radios: %d %s", w.Code, w.Body.String())
	}
	var response radiosResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := response.Devices[0].Radios[0].Interfaces; got == nil || len(got) != 0 {
		t.Fatalf("interfaces = %#v, want non-nil empty array", got)
	}
	if !strings.Contains(w.Body.String(), `"interfaces":[]`) {
		t.Fatalf("response flattened empty interfaces to null: %s", w.Body.String())
	}
}

func TestRadiosDoNotTurnAnOldScanIntoACurrentSuggestion(t *testing.T) {
	h := newHarness(t)
	now := time.Unix(1_800_000_000, 0)
	h.srv.Now = func() time.Time { return now }
	h.setup()
	device := h.seedDevice("old-scan-ap", true, nil)
	no := false
	observedAt := now.Add(-25 * time.Hour).UnixMilli()
	h.srv.Fleet = &radioFleetStub{stubFleet: h.fleet,
		statuses: map[int64]radio.CollectionStatus{device.ID: {
			ObservedAt: now.UnixMilli(), LastPollAt: now.UnixMilli(), LastPollOK: true,
		}}, states: map[int64][]radio.LiveState{device.ID: {{
			InventoryRadio:      radio.InventoryRadio{Key: "radio0", Band: "5g"},
			InventoryObservedAt: now.UnixMilli(), FrequenciesObservedAt: now.UnixMilli(),
			FrequenciesKnown: true, Frequencies: []radio.Frequency{
				{Band: "5g", Channel: 36, MHz: 5180, Restricted: &no},
			},
		}}}}
	scan := &model.RadioScan{Radio: model.RadioKey{DeviceID: device.ID, Section: "radio0"},
		StartedAt: observedAt - 1_000}
	if err := h.db.CreateRadioScan(context.Background(), scan); err != nil {
		t.Fatal(err)
	}
	signal := -50
	if err := h.db.FinishRadioScan(context.Background(), scan.ID, model.RadioScanCompleted,
		observedAt, nil, []model.RadioScanBSS{{BSSID: "00:11:22:33:44:55",
			MHz: 5180, Channel: 36, Signal: &signal}}); err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, "/api/v1/radios", nil)
	var response radiosResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || response.Devices[0].Radios[0].Suggested != nil || len(response.Gaps) == 0 {
		t.Fatalf("old scan rendered as current suggestion: status=%d response=%+v", w.Code, response)
	}
}

func TestRadiosRetainFreshScanButDoNotSuggestAfterFailedRadioRefresh(t *testing.T) {
	h := newHarness(t)
	now := time.Unix(1_800_000_000, 0)
	h.srv.Now = func() time.Time { return now }
	h.setup()
	device := h.seedDevice("stale-suggestion-ap", true, nil)
	no := false
	observedAt := now.UnixMilli()
	h.srv.Fleet = &radioFleetStub{stubFleet: h.fleet,
		statuses: map[int64]radio.CollectionStatus{device.ID: {
			ObservedAt: observedAt - 60_000, LastPollAt: observedAt,
			LastPollOK: true, ConsecutiveFailures: 0, LastSourceAttemptAt: observedAt,
			LastSourceAttemptOK: false, Stale: true,
		}}, states: map[int64][]radio.LiveState{device.ID: {{
			InventoryRadio:      radio.InventoryRadio{Key: "radio0", Band: "5g"},
			InventoryObservedAt: observedAt - 60_000, FrequenciesObservedAt: observedAt,
			FrequenciesKnown: true, Frequencies: []radio.Frequency{
				{Band: "5g", Channel: 36, MHz: 5180, Restricted: &no},
			},
		}}}}
	scan := &model.RadioScan{Radio: model.RadioKey{DeviceID: device.ID, Section: "radio0"},
		StartedAt: observedAt - 120_000}
	if err := h.db.CreateRadioScan(context.Background(), scan); err != nil {
		t.Fatal(err)
	}
	signal := -50
	if err := h.db.FinishRadioScan(context.Background(), scan.ID, model.RadioScanCompleted,
		observedAt-60_000, nil, []model.RadioScanBSS{{BSSID: "00:11:22:33:44:55",
			MHz: 5180, Channel: 36, Signal: &signal}}); err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, "/api/v1/radios", nil)
	var response radiosResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	got := response.Devices[0].Radios[0]
	if w.Code != http.StatusOK || got.LatestScan == nil || got.Suggested != nil || len(response.Gaps) == 0 {
		t.Fatalf("stale radio state qualified a suggestion: status=%d response=%+v", w.Code, response)
	}
}

func TestRadiosRetainOfflineLastKnownStateButLabelItStale(t *testing.T) {
	h := newHarness(t)
	h.setup()
	device := h.seedDevice("offline-ap", true, nil)
	observedAt := int64(1_700_000_000_000)
	h.srv.Fleet = &radioFleetStub{stubFleet: h.fleet,
		statuses: map[int64]radio.CollectionStatus{device.ID: {
			ObservedAt: observedAt, LastPollAt: observedAt + 60_000,
			LastPollOK: false, ConsecutiveFailures: 1, Stale: true,
		}}, states: map[int64][]radio.LiveState{device.ID: {{
			InventoryRadio:      radio.InventoryRadio{Key: "radio0", Band: "5g"},
			InventoryObservedAt: observedAt,
		}}}}

	w := h.do(http.MethodGet, "/api/v1/radios", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("radios: %d %s", w.Code, w.Body.String())
	}
	var response radiosResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Devices) != 1 || len(response.Devices[0].Radios) != 1 ||
		!response.Devices[0].Radios[0].Stale || response.Devices[0].Status == nil ||
		response.Devices[0].Status.LastPollOK || len(response.Gaps) == 0 ||
		response.GeneratedAt == observedAt {
		t.Fatalf("offline state claimed fresh: %+v", response)
	}
}

func TestRadioScanRequiresCSRFAndDisruptionAcknowledgement(t *testing.T) {
	h := newHarness(t)
	h.setup()
	device := h.seedDevice("scan-ap", true, nil)
	scanner := &radioScannerStub{}
	h.srv.RadioScan = scanner
	path := fmt.Sprintf("/api/v1/devices/%d/radios/radio0/scan", device.ID)

	csrf := h.csrf
	h.csrf = ""
	w := h.do(http.MethodPost, path, map[string]bool{"acknowledge_disruption": true})
	if w.Code != http.StatusForbidden || scanner.calls != 0 {
		t.Fatalf("missing CSRF: status=%d calls=%d", w.Code, scanner.calls)
	}
	h.csrf = csrf
	w = h.do(http.MethodPost, path, map[string]bool{"acknowledge_disruption": false})
	if w.Code != http.StatusBadRequest || scanner.calls != 0 {
		t.Fatalf("missing acknowledgement: status=%d calls=%d", w.Code, scanner.calls)
	}
	w = h.do(http.MethodPost, path, map[string]bool{"acknowledge_disruption": true})
	if w.Code != http.StatusOK || scanner.calls != 1 || scanner.deviceID != device.ID || scanner.radioKey != "radio0" {
		t.Fatalf("acknowledged scan: status=%d calls=%d target=%d/%s body=%s",
			w.Code, scanner.calls, scanner.deviceID, scanner.radioKey, w.Body.String())
	}
}
