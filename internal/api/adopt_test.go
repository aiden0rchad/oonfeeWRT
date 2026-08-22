package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// stubEnroller records what the handler passed down, so the tests can assert on
// validation and on what does NOT reach the orchestration.
type stubEnroller struct {
	mu         sync.Mutex
	inspected  []InspectRequest
	adopted    []AdoptRequest
	refreshed  []RefreshACLRequest
	lldp       []LLDPCapabilityRequest
	unadopt    []UnadoptRequest
	adoptErr   error
	unaErr     error
	result     *AdoptResult
	unaRes     *UnadoptResult
	inspectErr error
	inspectRes *InspectResult
	refreshErr error
	refreshRes *RefreshACLResult
	lldpErr    error
	lldpRes    *LLDPCapabilityResult
}

func (e *stubEnroller) LLDPCapability(_ context.Context, req LLDPCapabilityRequest) (*LLDPCapabilityResult, error) {
	e.mu.Lock()
	e.lldp = append(e.lldp, req)
	e.mu.Unlock()
	if e.lldpErr != nil {
		return nil, e.lldpErr
	}
	if e.lldpRes != nil {
		return e.lldpRes, nil
	}
	return &LLDPCapabilityResult{DeviceID: req.DeviceID, State: "installed"}, nil
}

func (e *stubEnroller) RefreshACL(_ context.Context, req RefreshACLRequest) (*RefreshACLResult, error) {
	e.mu.Lock()
	e.refreshed = append(e.refreshed, req)
	e.mu.Unlock()
	if e.refreshErr != nil {
		return nil, e.refreshErr
	}
	if e.refreshRes != nil {
		return e.refreshRes, nil
	}
	return &RefreshACLResult{DeviceID: req.DeviceID, ACLUpdated: true, ControllerVerified: true}, nil
}

func (e *stubEnroller) Inspect(_ context.Context, req InspectRequest) (*InspectResult, error) {
	e.mu.Lock()
	e.inspected = append(e.inspected, req)
	e.mu.Unlock()
	if e.inspectErr != nil {
		return nil, e.inspectErr
	}
	if e.inspectRes != nil {
		return e.inspectRes, nil
	}
	return &InspectResult{Model: req.Host}, nil
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
		{"no host", map[string]any{"username": "root", "password": "x", "acknowledge_router_changes": true}, http.StatusBadRequest},
		{"blank host", map[string]any{"host": "   ", "username": "root", "acknowledge_router_changes": true}, http.StatusBadRequest},
		{"no username", map[string]any{"host": "192.168.1.1", "acknowledge_router_changes": true}, http.StatusBadRequest},
		{"bad scheme", map[string]any{"host": "h", "username": "root", "scheme": "ftp", "acknowledge_router_changes": true}, http.StatusBadRequest},
		{"bad port", map[string]any{"host": "h", "username": "root", "port": 99999, "acknowledge_router_changes": true}, http.StatusBadRequest},
		{"valid", map[string]any{"host": "192.168.1.1", "username": "root", "password": "p", "acknowledge_router_changes": true}, http.StatusCreated},
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

func TestAdoptRequiresExplicitRouterChangeAcknowledgementBeforeEnroller(t *testing.T) {
	// Enroller is the handler's only path to the daemon, SSH dialer, and router
	// mutations. No recorded call therefore proves this boundary is pre-write.
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"missing", map[string]any{"host": "192.0.2.1", "username": "root"}},
		{"false", map[string]any{"host": "192.0.2.1", "username": "root", "acknowledge_router_changes": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, e := harnessWithEnroller(t)
			w := h.do(http.MethodPost, "/api/v1/devices/adopt", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "acknowledge_router_changes") {
				t.Fatalf("rejection does not name the required acknowledgement: %s", w.Body.String())
			}
			if len(e.adopted) != 0 {
				t.Fatalf("unacknowledged adoption reached the enroller: %+v", e.adopted)
			}
		})
	}
}

func TestRefreshACLRequiresExplicitAdministratorCredentialAndPassesNoSecretsToResponse(t *testing.T) {
	h, e := harnessWithEnroller(t)
	if w := h.do(http.MethodPost, "/api/v1/devices/7/refresh-acl", map[string]any{
		"username": " ", "password": "sentinel-password", "acknowledge_router_changes": true,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("blank username status=%d body=%s", w.Code, w.Body.String())
	}
	if len(e.refreshed) != 0 {
		t.Fatalf("invalid request reached enroller: %+v", e.refreshed)
	}
	w := h.do(http.MethodPost, "/api/v1/devices/7/refresh-acl", map[string]any{
		"username": "root", "password": "sentinel-password", "private_key": "sentinel-key",
		"acknowledge_router_changes": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(e.refreshed) != 1 || e.refreshed[0].DeviceID != 7 ||
		e.refreshed[0].Username != "root" || e.refreshed[0].Password != "sentinel-password" ||
		e.refreshed[0].PrivateKey != "sentinel-key" || !e.refreshed[0].AcknowledgeRouterChanges {
		t.Fatalf("refresh request=%+v", e.refreshed)
	}
	if strings.Contains(w.Body.String(), "sentinel-password") || strings.Contains(w.Body.String(), "sentinel-key") {
		t.Fatalf("refresh response exposed operator credential: %s", w.Body.String())
	}
}

func TestRefreshACLRequiresExplicitRouterChangeAcknowledgementBeforeEnroller(t *testing.T) {
	// As above, reaching Enroller is what can open SSH and replace the ACL file.
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"missing", map[string]any{"username": "root"}},
		{"false", map[string]any{"username": "root", "acknowledge_router_changes": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, e := harnessWithEnroller(t)
			w := h.do(http.MethodPost, "/api/v1/devices/7/refresh-acl", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "acknowledge_router_changes") {
				t.Fatalf("rejection does not name the required acknowledgement: %s", w.Body.String())
			}
			if len(e.refreshed) != 0 {
				t.Fatalf("unacknowledged ACL refresh reached the enroller: %+v", e.refreshed)
			}
		})
	}
}

func TestLLDPCapabilityRequiresSeparatePlanAndMutationAcknowledgements(t *testing.T) {
	h, e := harnessWithEnroller(t)
	for _, body := range []map[string]any{
		{"action": "diagnose", "username": "root"},
		{"action": "plan_configure", "username": "root"},
		{"action": "plan_install", "username": "root"},
		{"action": "install", "username": "root", "plan_hash": "abc"},
		{"action": "install", "username": "root", "plan_hash": "abc", "acknowledge_router_changes": true},
		{"action": "configure", "username": "root", "plan_hash": "abc"},
		{"action": "remove", "username": "root", "acknowledge_router_changes": true},
	} {
		w := h.do(http.MethodPost, "/api/v1/devices/7/capabilities/lldp", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body=%v status=%d response=%s", body, w.Code, w.Body.String())
		}
	}
	if len(e.lldp) != 0 {
		t.Fatalf("unacknowledged action reached enroller: %+v", e.lldp)
	}
	w := h.do(http.MethodPost, "/api/v1/devices/7/capabilities/lldp", map[string]any{
		"action": "install", "username": "root", "password": "sentinel-password",
		"plan_hash": "abc", "acknowledge_router_changes": true, "acknowledge_package_index_refresh": true,
	})
	if w.Code != http.StatusOK || len(e.lldp) != 1 || e.lldp[0].Password != "sentinel-password" ||
		!e.lldp[0].AcknowledgePackageIndexRefresh {
		t.Fatalf("status=%d response=%s request=%+v", w.Code, w.Body.String(), e.lldp)
	}
	if strings.Contains(w.Body.String(), "sentinel-password") {
		t.Fatalf("response exposed credential: %s", w.Body.String())
	}
	w = h.do(http.MethodPost, "/api/v1/devices/7/capabilities/lldp", map[string]any{
		"action": "diagnose", "username": "root", "password": "diagnostic-password",
		"acknowledge_read_only_diagnostics": true,
	})
	if w.Code != http.StatusOK || len(e.lldp) != 2 || !e.lldp[1].AcknowledgeReadOnlyDiagnostics {
		t.Fatalf("status=%d response=%s request=%+v", w.Code, w.Body.String(), e.lldp)
	}
	if strings.Contains(w.Body.String(), "diagnostic-password") {
		t.Fatalf("response exposed diagnostic credential: %s", w.Body.String())
	}
}

func TestInspectValidatesInputAndNeverEchoesCredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"no host", map[string]any{"username": "root"}, http.StatusBadRequest},
		{"no username", map[string]any{"host": "192.0.2.1"}, http.StatusBadRequest},
		{"bad scheme", map[string]any{"host": "h", "username": "root", "scheme": "ssh"}, http.StatusBadRequest},
		{"bad port", map[string]any{"host": "h", "username": "root", "port": -1}, http.StatusBadRequest},
		{"valid", map[string]any{"host": " 192.0.2.1 ", "username": "root", "password": "inspect-secret"}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, e := harnessWithEnroller(t)
			w := h.do(http.MethodPost, "/api/v1/devices/inspect", tc.body)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "inspect-secret") {
				t.Fatal("inspection echoed the device administrator password")
			}
			if tc.name == "valid" {
				e.mu.Lock()
				got := e.inspected[0]
				e.mu.Unlock()
				if got.Host != "192.0.2.1" || got.Password != "inspect-secret" {
					t.Fatalf("inspection request was not normalised/passed through: %+v", got)
				}
			}
		})
	}
}

func TestAdoptCanonicalisesIndependentFunctionsAndLegacyRole(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          map[string]any
		wantRole      string
		wantFunctions string
	}{
		{"legacy gateway", map[string]any{"role": "Gateway"}, "gateway", "gateway,ap,switch"},
		{"gateway only", map[string]any{"role": "ap", "functions": []string{"gateway"}}, "gateway", "gateway"},
		{"AP and switch", map[string]any{"role": "gateway", "functions": []string{"switch", "AP"}}, "ap", "ap,switch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, e := harnessWithEnroller(t)
			body := map[string]any{"host": "192.0.2.1", "username": "root", "acknowledge_router_changes": true}
			for k, v := range tc.body {
				body[k] = v
			}
			w := h.do(http.MethodPost, "/api/v1/devices/adopt", body)
			if w.Code != http.StatusCreated {
				t.Fatalf("adopt: %d %s", w.Code, w.Body.String())
			}
			e.mu.Lock()
			got := e.adopted[0]
			e.mu.Unlock()
			if got.Role != tc.wantRole || strings.Join(got.Functions, ",") != tc.wantFunctions {
				t.Fatalf("role=%q functions=%v, want %q %q", got.Role, got.Functions, tc.wantRole, tc.wantFunctions)
			}
		})
	}
}

func TestAdoptRejectsEmptyOrUnknownFunctionsBeforeEnroller(t *testing.T) {
	for _, functions := range [][]string{{}, {"router"}, {"ap", "mesh"}} {
		h, e := harnessWithEnroller(t)
		w := h.do(http.MethodPost, "/api/v1/devices/adopt", map[string]any{
			"host": "192.0.2.1", "username": "root", "functions": functions,
			"acknowledge_router_changes": true,
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("functions %v: got %d want 400: %s", functions, w.Code, w.Body.String())
		}
		e.mu.Lock()
		calls := len(e.adopted)
		e.mu.Unlock()
		if calls != 0 {
			t.Fatalf("functions %v reached the enroller", functions)
		}
	}
}

func TestAdoptRejectsExplicitNullOrNonArrayFunctions(t *testing.T) {
	for name, functions := range map[string]any{
		"null":   nil,
		"string": "gateway",
		"object": map[string]any{"gateway": true},
	} {
		t.Run(name, func(t *testing.T) {
			h, e := harnessWithEnroller(t)
			w := h.do(http.MethodPost, "/api/v1/devices/adopt", map[string]any{
				"host": "192.0.2.1", "username": "root", "functions": functions,
				"acknowledge_router_changes": true,
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("got %d want 400: %s", w.Code, w.Body.String())
			}
			e.mu.Lock()
			calls := len(e.adopted)
			e.mu.Unlock()
			if calls != 0 {
				t.Fatal("invalid functions reached the enroller")
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

func TestInspectRequiresAuthAndCSRF(t *testing.T) {
	h, _ := harnessWithEnroller(t)
	good := h.csrf
	body := map[string]any{"host": "h", "username": "root"}

	h.csrf = ""
	if w := h.do(http.MethodPost, "/api/v1/devices/inspect", body); w.Code != http.StatusForbidden {
		t.Fatalf("without CSRF: %d, want 403", w.Code)
	}
	h.csrf = good
	h.cookies = nil
	if w := h.do(http.MethodPost, "/api/v1/devices/inspect", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("without a session: %d, want 401", w.Code)
	}
}

func TestRefreshACLRequiresAuthAndCSRF(t *testing.T) {
	h, _ := harnessWithEnroller(t)
	good := h.csrf
	body := map[string]any{"username": "root", "password": "placeholder"}

	h.csrf = ""
	if w := h.do(http.MethodPost, "/api/v1/devices/1/refresh-acl", body); w.Code != http.StatusForbidden {
		t.Fatalf("without CSRF: %d, want 403", w.Code)
	}
	h.csrf = good
	h.cookies = nil
	if w := h.do(http.MethodPost, "/api/v1/devices/1/refresh-acl", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("without a session: %d, want 401", w.Code)
	}
}

// The device's administrator credential is used for one transaction and must
// not survive it — not in the response, not in the event log.
func TestAdoptDoesNotEchoOrLogTheOperatorCredential(t *testing.T) {
	h, e := harnessWithEnroller(t)
	const (
		secret = "the-routers-admin-password"
		key    = "-----BEGIN OPENSSH PRIVATE KEY-----\nprivate-key-sentinel\n-----END OPENSSH PRIVATE KEY-----"
	)

	w := h.do(http.MethodPost, "/api/v1/devices/adopt", map[string]any{
		"host": "192.168.1.1", "username": "root", "password": secret,
		"private_key": key, "name": "ap1", "acknowledge_router_changes": true,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("adopt: %d %s", w.Code, w.Body.String())
	}
	for name, value := range map[string]string{"password": secret, "private key": key} {
		if strings.Contains(w.Body.String(), value) {
			t.Errorf("the response echoed the device's administrator %s", name)
		}
	}
	// It did reach the orchestration — that is the whole point of the call.
	e.mu.Lock()
	gotPassword := e.adopted[0].Password
	gotKey := e.adopted[0].PrivateKey
	e.mu.Unlock()
	if gotPassword != secret || gotKey != key {
		t.Fatalf("the credentials did not reach the enroller: password=%q key=%q",
			gotPassword, gotKey)
	}

	events, err := h.db.RecentEvents(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		blob, _ := json.Marshal(ev)
		for name, value := range map[string]string{"password": secret, "private key": key} {
			if strings.Contains(string(blob), value) {
				t.Fatalf("event %q recorded the device's administrator %s", ev.Event, name)
			}
		}
	}
}

// A failure mid-adoption is shown to an operator who needs to know which step
// broke, so the message is passed through — but it must never carry the
// credential with it.
func TestAdoptFailureIsReportedWithoutTheCredential(t *testing.T) {
	h, e := harnessWithEnroller(t)
	var logs strings.Builder
	h.srv.Log = slog.New(slog.NewTextHandler(&logs, nil))
	e.adoptErr = errors.New("could not sign in to 192.168.1.1: access denied")
	const key = "private-key-sentinel-on-failure"

	w := h.do(http.MethodPost, "/api/v1/devices/adopt", map[string]any{
		"host": "192.168.1.1", "username": "root", "password": "hunter2",
		"private_key": key, "acknowledge_router_changes": true,
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
	if strings.Contains(w.Body.String(), key) {
		t.Error("the failure response leaked the SSH private key")
	}
	for name, value := range map[string]string{"password": "hunter2", "private key": key} {
		if strings.Contains(logs.String(), value) {
			t.Errorf("the failure log leaked the operator %s", name)
		}
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
	const key = "private-key-sentinel-for-unadopt"

	w := h.do(http.MethodPost, "/api/v1/devices/"+itoa(int(dev.ID))+"/unadopt",
		map[string]any{"username": "root", "password": "p", "private_key": key})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), key) {
		t.Fatal("the un-adopt response echoed the SSH private key")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.unadopt) != 1 || e.unadopt[0].DeviceID != dev.ID ||
		e.unadopt[0].PrivateKey != key {
		t.Fatalf("device id or private key did not reach the enroller: %+v", e.unadopt)
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
		map[string]any{"host": "192.168.1.1", "username": "root", "password": "p", "acknowledge_router_changes": true})
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
		map[string]any{"host": "192.168.1.1", "username": "root", "password": "p", "acknowledge_router_changes": true})
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
