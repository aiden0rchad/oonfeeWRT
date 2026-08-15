package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
)

type stubReprober struct {
	res  *ReprobeResult
	err  error
	last int64
}

func (s *stubReprober) Reprobe(_ context.Context, id int64) (*ReprobeResult, error) {
	s.last = id
	return s.res, s.err
}

// "A probe is already running" is not a device failure, and the status code has
// to say so. A 502 tells an operator their router is unreachable; the truth is
// that they pressed the button twice.
func TestReprobeBusyIs429NotAGatewayError(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.srv.Reprobe = &stubReprober{err: ErrReprobeBusy}

	w := h.do(http.MethodPost, "/api/v1/devices/1/reprobe", nil)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 — a probe already running is a "+
			"\"not now\", not a broken device", w.Code)
	}
}

// A genuine device failure is a 502: the caller did nothing wrong.
func TestReprobeDeviceFailureIs502(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.srv.Reprobe = &stubReprober{err: context.DeadlineExceeded}

	w := h.do(http.MethodPost, "/api/v1/devices/1/reprobe", nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

// Without a Reprober the endpoint says the feature is unavailable rather than
// panicking or reporting a device problem.
func TestReprobeWithoutABackendIsUnavailable(t *testing.T) {
	h := newHarness(t)
	h.setup()

	w := h.do(http.MethodPost, "/api/v1/devices/1/reprobe", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// The response must let a client tell "the device changed" from "we can see
// less of it" without re-deriving the classification.
//
// This is the whole point of the endpoint. A client that counted `changes` and
// warned would warn about a narrowed ACL as though a radio had been removed.
func TestReprobeSeparatesVisibilityChangesFromRealOnes(t *testing.T) {
	h := newHarness(t)
	h.setup()

	old := NewRegistryFor(map[capability.Feature]capability.State{
		capability.FeatDSA:            capability.Present,
		capability.FeatHostapdControl: capability.Present,
	})
	new := NewRegistryFor(map[capability.Feature]capability.State{
		capability.FeatDSA:            capability.Absent,        // really lost
		capability.FeatHostapdControl: capability.NotObservable, // just hidden
	})
	changes := capability.Diff(old, new)
	h.srv.Reprobe = &stubReprober{res: &ReprobeResult{
		DeviceID: 1, Name: "ap", Summary: new.Summary(),
		Changes: changes, Registry: new,
	}}

	w := h.do(http.MethodPost, "/api/v1/devices/1/reprobe", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Changes    []capability.Change `json:"changes"`
		Actionable int                 `json:"actionable"`
		Unchanged  bool                `json:"unchanged"`
		Note       string              `json:"note"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(got.Changes))
	}
	if got.Actionable != 1 {
		t.Errorf("actionable = %d, want 1 — only the DSA loss is real; the "+
			"hostapd-control change is a refused check", got.Actionable)
	}
	if got.Unchanged {
		t.Error("unchanged is true with two changes reported")
	}
	if got.Note == "" {
		t.Error("the response does not explain the distinction it just drew")
	}
}

// A probe that found nothing must say so, or an empty list reads as a failure.
func TestReprobeReportsNoChangeAsAResult(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.srv.Reprobe = &stubReprober{res: &ReprobeResult{
		DeviceID: 1, Name: "ap", Summary: "class A WRT3200ACM", Unchanged: true,
	}}

	w := h.do(http.MethodPost, "/api/v1/devices/1/reprobe", nil)
	var got struct {
		Unchanged bool   `json:"unchanged"`
		Summary   string `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Unchanged || got.Summary == "" {
		t.Errorf("a no-change probe reported %+v; the UI cannot distinguish "+
			"this from a failure without both fields", got)
	}
}

// NewRegistryFor is a test helper: a registry with the given feature states.
func NewRegistryFor(feats map[capability.Feature]capability.State) *capability.Registry {
	r := capability.NewRegistry()
	for f, s := range feats {
		r.Set(f, s)
	}
	return r
}
