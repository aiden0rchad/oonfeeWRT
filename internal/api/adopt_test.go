package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// stubEnroller records what the handler passed down, so the tests can assert on
// validation and on what does NOT reach the orchestration.
type stubEnroller struct {
	mu       sync.Mutex
	adopted  []AdoptRequest
	unadopt  []UnadoptRequest
	adoptErr error
	unaErr   error
	result   *AdoptResult
	unaRes   *UnadoptResult
}

func (e *stubEnroller) Adopt(_ context.Context, req AdoptRequest) (*AdoptResult, error) {
	e.mu.Lock()
	e.adopted = append(e.adopted, req)
	e.mu.Unlock()
	if e.adoptErr != nil {
		return nil, e.adoptErr
	}
	if e.result != nil {
		return e.result, nil
	}
	return &AdoptResult{DeviceID: 1, MAC: "aa:bb:cc:dd:ee:ff", Name: req.Name}, nil
}

func (e *stubEnroller) Unadopt(_ context.Context, req UnadoptRequest) (*UnadoptResult, error) {
	e.mu.Lock()
	e.unadopt = append(e.unadopt, req)
	e.mu.Unlock()
	if e.unaErr != nil {
		return e.unaRes, e.unaErr
	}
	if e.unaRes != nil {
		return e.unaRes, nil
	}
	return &UnadoptResult{Removed: true}, nil
}

func harnessWithEnroller(t *testing.T) (*harness, *stubEnroller) {
	t.Helper()
	h := newHarness(t)
	e := &stubEnroller{}
	h.srv.Enroll = e
	h.mux = h.srv.Routes()
	h.setup()
	return h, e
}

func TestAdoptValidatesInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"no host", map[string]any{"username": "root", "password": "x"}, http.StatusBadRequest},
		{"blank host", map[string]any{"host": "   ", "username": "root"}, http.StatusBadRequest},
		{"no username", map[string]any{"host": "192.168.1.1"}, http.StatusBadRequest},
		{"bad scheme", map[string]any{"host": "h", "username": "root", "scheme": "ftp"}, http.StatusBadRequest},
		{"bad port", map[string]any{"host": "h", "username": "root", "port": 99999}, http.StatusBadRequest},
		{"valid", map[string]any{"host": "192.168.1.1", "username": "root", "password": "p"}, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := harnessWithEnroller(t)
			w := h.do(http.MethodPost, "/api/v1/devices/adopt", tc.body)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// Adoption is a mutating call on an authenticated route, so it needs both.
func TestAdoptRequiresAuthAndCSRF(t *testing.T) {
	h, _ := harnessWithEnroller(t)
	good := h.csrf

	h.csrf = ""
	if w := h.do(http.MethodPost, "/api/v1/devices/adopt",
		map[string]any{"host": "h", "username": "root"}); w.Code != http.StatusForbidden {
		t.Fatalf("without CSRF: %d, want 403", w.Code)
	}
	h.csrf = good
	h.cookies = nil
	if w := h.do(http.MethodPost, "/api/v1/devices/adopt",
		map[string]any{"host": "h", "username": "root"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("without a session: %d, want 401", w.Code)
	}
}

// The device's administrator credential is used for one transaction and must
// not survive it — not in the response, not in the event log.
func TestAdoptDoesNotEchoOrLogTheOperatorCredential(t *testing.T) {
	h, e := harnessWithEnroller(t)
	const secret = "the-routers-admin-password"

	w := h.do(http.MethodPost, "/api/v1/devices/adopt", map[string]any{
		"host": "192.168.1.1", "username": "root", "password": secret, "name": "ap1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("adopt: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Error("the response echoed the device's administrator password")
	}
	// It did reach the orchestration — that is the whole point of the call.
	e.mu.Lock()
	got := e.adopted[0].Password
	e.mu.Unlock()
	if got != secret {
		t.Fatalf("the password did not reach the enroller: %q", got)
	}

	events, err := h.db.RecentEvents(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		blob, _ := json.Marshal(ev)
		if strings.Contains(string(blob), secret) {
			t.Fatalf("event %q recorded the device's administrator password", ev.Event)
		}
	}
}

// A failure mid-adoption is shown to an operator who needs to know which step
// broke, so the message is passed through — but it must never carry the
// credential with it.
func TestAdoptFailureIsReportedWithoutTheCredential(t *testing.T) {
	h, e := harnessWithEnroller(t)
	e.adoptErr = errors.New("could not sign in to 192.168.1.1: access denied")

	w := h.do(http.MethodPost, "/api/v1/devices/adopt", map[string]any{
		"host": "192.168.1.1", "username": "root", "password": "hunter2",
	})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "could not sign in") {
		t.Errorf("the reason was not passed through: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hunter2") {
		t.Error("the failure response leaked the password")
	}
}

// Phase 1 runs under the controller's own login; phase 2 cannot, and its
// absence is a documented degradation rather than a failure. The client has to
// be able to tell that state from both success and a real error.
func TestUnadoptWithoutOperatorCredentialReports409AndTheResidue(t *testing.T) {
	h, e := harnessWithEnroller(t)
	e.unaErr = ErrOperatorRequired
	e.unaRes = &UnadoptResult{
		RevertedSections: 3,
		FootprintRemains: true,
		NeedsOperator:    true,
		Residue: []string{
			"/usr/share/rpcd/acl.d/oonfeewrt.json",
			"config login 'oonfeewrt' in /etc/config/rpcd",
		},
	}
	dev := h.seedDevice("ap-u", true, nil)

	w := h.do(http.MethodPost, "/api/v1/devices/"+itoa(int(dev.ID))+"/unadopt", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", w.Code, w.Body.String())
	}
	var res UnadoptResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.NeedsOperator {
		t.Error("the response does not say an operator credential is needed")
	}
	if res.RevertedSections != 3 {
		t.Errorf("phase 1's work was not reported: %+v", res)
	}
	if len(res.Residue) != 2 {
		t.Errorf("the residue was not listed: %v", res.Residue)
	}
	if res.Removed {
		t.Error("the device was removed from the inventory while a footprint remains")
	}
}

// A failed un-adopt that still produced a report must send the report.
//
// The worst case is the forced removal whose phase 2 connected and then could
// not commit: the inventory row is GONE and Residue is the only surviving
// record of what is installed on that device. Sending `{"error": "..."}` and
// dropping the body destroyed exactly that — and no client could recover it,
// because the row it described no longer exists to ask about.
func TestAFailedUnadoptStillReturnsItsReport(t *testing.T) {
	h, e := harnessWithEnroller(t)
	e.unaErr = errors.New("adoption: un-adopt completed with 1 error(s)")
	e.unaRes = &UnadoptResult{
		Removed:          true, // forced: the row is already gone
		FootprintRemains: true,
		Errors:           []string{"uci commit rpcd: Read-only file system"},
		Residue: []string{
			"/usr/share/rpcd/acl.d/oonfeewrt.json",
			"config login 'oonfeewrt' in /etc/config/rpcd",
		},
	}
	dev := h.seedDevice("ap-w", true, nil)

	w := h.do(http.MethodPost, "/api/v1/devices/"+itoa(int(dev.ID))+"/unadopt",
		map[string]any{"username": "root", "password": "p", "force": true})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502: %s", w.Code, w.Body.String())
	}
	var res UnadoptResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Residue) != 2 {
		t.Fatalf("the residue did not survive the error response: %+v", res)
	}
	if !res.Removed {
		t.Error("the response does not say the row was removed, which is what " +
			"makes the residue list irrecoverable")
	}
	if len(res.Errors) != 1 {
		t.Errorf("the specific failure was not carried: %v", res.Errors)
	}
	// And a generic client that knows nothing about un-adopt still finds a
	// message where every other endpoint puts one.
	if !strings.Contains(res.Error, "1 error(s)") {
		t.Errorf("no top-level error for a generic client: %q", res.Error)
	}
}

// With no report there is nothing to preserve, so the ordinary error body is
// still the right answer — a bare `{"error": ...}` rather than an empty report
// that would render as "nothing was removed and nothing remains".
func TestAnUnadoptFailureWithNoReportStaysAPlainError(t *testing.T) {
	h, e := harnessWithEnroller(t)
	e.unaErr = errors.New("store: not found")
	e.unaRes = nil
	dev := h.seedDevice("ap-x", true, nil)

	w := h.do(http.MethodPost, "/api/v1/devices/"+itoa(int(dev.ID))+"/unadopt",
		map[string]any{"username": "root", "password": "p"})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["removed_from_inventory"]; ok {
		t.Errorf("an absent report was sent as an empty one: %v", body)
	}
	if s, _ := body["error"].(string); !strings.Contains(s, "not found") {
		t.Errorf("error not reported: %v", body)
	}
}

func TestUnadoptPassesTheDeviceID(t *testing.T) {
	h, e := harnessWithEnroller(t)
	dev := h.seedDevice("ap-v", true, nil)

	w := h.do(http.MethodPost, "/api/v1/devices/"+itoa(int(dev.ID))+"/unadopt",
		map[string]any{"username": "root", "password": "p"})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.unadopt) != 1 || e.unadopt[0].DeviceID != dev.ID {
		t.Fatalf("device id did not reach the enroller: %+v", e.unadopt)
	}
}

func TestAdoptUnavailableWithoutAnEnroller(t *testing.T) {
	h := newHarness(t) // Enroll is nil
	h.setup()
	w := h.do(http.MethodPost, "/api/v1/devices/adopt",
		map[string]any{"host": "h", "username": "root"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
}

// One action, one audit event. The Enroller writes the success event because it
// knows the device id, MAC, model and class; logging it in the handler too
// would double every adoption in the audit trail.
func TestAdoptLogsExactlyOneOutcome(t *testing.T) {
	h, _ := harnessWithEnroller(t)
	ctx := context.Background()

	before, err := h.db.RecentEvents(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	w := h.do(http.MethodPost, "/api/v1/devices/adopt",
		map[string]any{"host": "192.168.1.1", "username": "root", "password": "p"})
	if w.Code != http.StatusCreated {
		t.Fatalf("adopt: %d", w.Code)
	}
	after, err := h.db.RecentEvents(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	// The stub Enroller writes nothing, so a successful adopt must add NO event
	// from the handler at all.
	n := 0
	for _, e := range after {
		if e.Event == "device.adopted" {
			n++
		}
	}
	if n != 0 {
		t.Errorf("the handler logged %d device.adopted event(s); that is the "+
			"Enroller's job and doing both doubles the audit trail", n)
	}
	if len(after) != len(before) {
		t.Errorf("a successful adopt added %d handler event(s)", len(after)-len(before))
	}

	// A failure IS logged here, because the Enroller returns early.
	e := h.srv.Enroll.(*stubEnroller)
	e.adoptErr = errors.New("nope")
	h.do(http.MethodPost, "/api/v1/devices/adopt",
		map[string]any{"host": "192.168.1.1", "username": "root", "password": "p"})
	failed, err := h.db.RecentEvents(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range failed {
		if ev.Event == "device.adopt_failed" {
			found = true
		}
	}
	if !found {
		t.Error("a failed adoption was not recorded")
	}
}
