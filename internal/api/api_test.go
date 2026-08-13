package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// stubFleet stands in for the collector so the API can be tested without a
// keyring, a listener, or a router.
type stubFleet struct {
	mu       sync.Mutex
	focused  map[int64]int
	tier     map[int64]collector.Tier
	quiesced map[int64]bool
}

func newStubFleet() *stubFleet {
	return &stubFleet{focused: map[int64]int{}, tier: map[int64]collector.Tier{},
		quiesced: map[int64]bool{}}
}

func (f *stubFleet) Focus(deviceID int64) func() {
	f.mu.Lock()
	f.focused[deviceID]++
	f.tier[deviceID] = collector.Focused
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.focused[deviceID]--
			if f.focused[deviceID] == 0 {
				f.tier[deviceID] = collector.Baseline
			}
			f.mu.Unlock()
		})
	}
}

func (f *stubFleet) Tier(deviceID int64) (collector.Tier, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tier[deviceID]
	return t, ok
}

func (f *stubFleet) Quiesced(deviceID int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.quiesced[deviceID]
}

func (f *stubFleet) focusCount(deviceID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.focused[deviceID]
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

type harness struct {
	t     *testing.T
	srv   *Server
	mux   http.Handler
	db    *store.DB
	fleet *stubFleet

	cookies []*http.Cookie
	csrf    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := store.Open(context.Background(), "sqlite",
		filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	fleet := newStubFleet()
	srv := New(db, fleet, quiet())
	return &harness{t: t, srv: srv, mux: srv.Routes(), db: db, fleet: fleet}
}

func (h *harness) do(method, path string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		r = bytes.NewReader(blob)
	}
	req := httptest.NewRequest(method, path, r)
	req.RemoteAddr = "192.0.2.10:12345"
	for _, c := range h.cookies {
		req.AddCookie(c)
	}
	if h.csrf != "" {
		req.Header.Set(csrfHeader, h.csrf)
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	// Carry cookies forward the way a browser would.
	if cs := w.Result().Cookies(); len(cs) > 0 {
		h.cookies = cs
	}
	return w
}

func (h *harness) json(w *httptest.ResponseRecorder) map[string]any {
	h.t.Helper()
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		h.t.Fatalf("response is not JSON (%d): %s", w.Code, w.Body.String())
	}
	return v
}

const testPassword = "a-sufficiently-long-password"

// setup enrols the first operator and keeps the session.
func (h *harness) setup() {
	h.t.Helper()
	w := h.do(http.MethodPost, "/api/v1/setup",
		map[string]string{"username": "admin", "password": testPassword})
	if w.Code != http.StatusCreated {
		h.t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	h.csrf, _ = h.json(w)["csrf"].(string)
}

func (h *harness) seedDevice(name string, adopted bool, lastSeen *int64) *store.Device {
	h.t.Helper()
	d := &store.Device{
		MAC:  fmt.Sprintf("aa:bb:cc:00:00:%02d", len(name)),
		Host: "192.0.2.1", Name: name, Role: "ap", LastSeen: lastSeen,
	}
	if adopted {
		at := int64(1)
		d.AdoptedAt = &at
	}
	if err := h.db.UpsertDevice(context.Background(), d); err != nil {
		h.t.Fatalf("seed device: %v", err)
	}
	return d
}

// ---- setup and authentication ----

func TestSetupThenLogin(t *testing.T) {
	h := newHarness(t)

	w := h.do(http.MethodGet, "/api/v1/setup", nil)
	if got := h.json(w)["needs_setup"]; got != true {
		t.Fatalf("needs_setup = %v on a fresh install, want true", got)
	}
	h.setup()

	w = h.do(http.MethodGet, "/api/v1/setup", nil)
	if got := h.json(w)["needs_setup"]; got != false {
		t.Fatalf("needs_setup = %v after enrolment, want false", got)
	}
	// Setup works exactly once: there is no default credential to change later.
	w = h.do(http.MethodPost, "/api/v1/setup",
		map[string]string{"username": "someone", "password": testPassword})
	if w.Code != http.StatusConflict {
		t.Fatalf("second setup: %d, want 409", w.Code)
	}

	w = h.do(http.MethodGet, "/api/v1/session", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("session after setup: %d %s", w.Code, w.Body.String())
	}
	if h.json(w)["username"] != "admin" {
		t.Errorf("session reports %v", h.json(w)["username"])
	}
}

func TestSetupRejectsWeakInput(t *testing.T) {
	for _, tc := range []struct{ name, user, pass string }{
		{"no username", "", testPassword},
		{"short password", "admin", "short"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			w := h.do(http.MethodPost, "/api/v1/setup",
				map[string]string{"username": tc.user, "password": tc.pass})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.cookies, h.csrf = nil, "" // forget the session, like a new browser

	for _, path := range []string{
		"/api/v1/devices", "/api/v1/session", "/api/v1/events",
		"/api/v1/dashboard", "/api/v1/stats/sys_load1?device_id=1",
	} {
		w := h.do(http.MethodGet, path, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", path, w.Code)
		}
	}
}

func TestLoginFailuresDoNotDistinguishUserFromPassword(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.cookies, h.csrf = nil, ""

	wrongUser := h.do(http.MethodPost, "/api/v1/login",
		map[string]string{"username": "nobody", "password": testPassword})
	wrongPass := h.do(http.MethodPost, "/api/v1/login",
		map[string]string{"username": "admin", "password": "wrong-but-long-enough"})

	if wrongUser.Code != http.StatusUnauthorized || wrongPass.Code != http.StatusUnauthorized {
		t.Fatalf("statuses %d/%d, want 401/401", wrongUser.Code, wrongPass.Code)
	}
	// Distinguishing the two hands an attacker free account enumeration.
	if wrongUser.Body.String() != wrongPass.Body.String() {
		t.Errorf("responses differ:\n  unknown user: %s\n  wrong password: %s",
			wrongUser.Body.String(), wrongPass.Body.String())
	}
}

func TestLoginIsThrottledPerAddress(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.cookies, h.csrf = nil, ""

	var last *httptest.ResponseRecorder
	for range 12 {
		last = h.do(http.MethodPost, "/api/v1/login",
			map[string]string{"username": "admin", "password": "wrong-but-long-enough"})
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("after 12 failures: %d, want 429", last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Error("a 429 without Retry-After leaves the client guessing")
	}
	// Even the correct password is refused while locked out — otherwise the
	// limiter is only a speed bump for an online guessing attack.
	w := h.do(http.MethodPost, "/api/v1/login",
		map[string]string{"username": "admin", "password": testPassword})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password during lockout: %d, want 429", w.Code)
	}
}

// Throttling by username would let anyone lock a known operator out.
func TestThrottleIsPerAddressNotPerUser(t *testing.T) {
	tr := newThrottle()
	now := time.Now()
	for range 12 {
		tr.fail("192.0.2.1", now)
	}
	if ok, _ := tr.allow("192.0.2.1", now); ok {
		t.Fatal("the failing address was not locked out")
	}
	if ok, _ := tr.allow("192.0.2.2", now); !ok {
		t.Fatal("a different address was locked out by someone else's failures")
	}
	// A success clears the record.
	tr.succeed("192.0.2.1")
	if ok, _ := tr.allow("192.0.2.1", now); !ok {
		t.Fatal("a successful sign-in did not clear the lockout")
	}
}

func TestCSRFRequiredForMutations(t *testing.T) {
	h := newHarness(t)
	h.setup()
	dev := h.seedDevice("ap1", true, nil)

	good := h.csrf
	h.csrf = ""
	w := h.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/focus", dev.ID), nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("mutation without a CSRF header: %d, want 403", w.Code)
	}
	h.csrf = "not-the-token"
	w = h.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/focus", dev.ID), nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("mutation with a wrong CSRF header: %d, want 403", w.Code)
	}
	h.csrf = good
	w = h.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/focus", dev.ID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("mutation with the right CSRF header: %d %s", w.Code, w.Body.String())
	}
	// Reads never need it.
	h.csrf = ""
	if w := h.do(http.MethodGet, "/api/v1/devices", nil); w.Code != http.StatusOK {
		t.Fatalf("GET with no CSRF header: %d", w.Code)
	}
	h.csrf = good
}

func TestSessionCookieIsHttpOnlyAndStrict(t *testing.T) {
	h := newHarness(t)
	h.setup()

	var session, csrf *http.Cookie
	for _, c := range h.cookies {
		switch c.Name {
		case sessionCookie:
			session = c
		case csrfCookie:
			csrf = c
		}
	}
	if session == nil || csrf == nil {
		t.Fatalf("setup did not set both cookies: %+v", h.cookies)
	}
	if !session.HttpOnly {
		t.Error("the session cookie is readable by script")
	}
	if session.SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie is not SameSite=Strict")
	}
	// The CSRF cookie must be script-readable: the page has to echo it back in
	// a header, and a value it cannot read cannot be echoed.
	if csrf.HttpOnly {
		t.Error("the CSRF cookie is HttpOnly, so the UI cannot send the header")
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	h := newHarness(t)
	h.setup()

	if w := h.do(http.MethodPost, "/api/v1/logout", nil); w.Code != http.StatusOK {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	if w := h.do(http.MethodGet, "/api/v1/devices", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("after logout: %d, want 401", w.Code)
	}
}

func TestPasswordChangeEndsEverySession(t *testing.T) {
	h := newHarness(t)
	h.setup()

	// A second browser, signed in as the same operator.
	other := &harness{t: t, srv: h.srv, mux: h.mux, db: h.db, fleet: h.fleet}
	w := other.do(http.MethodPost, "/api/v1/login",
		map[string]string{"username": "admin", "password": testPassword})
	if w.Code != http.StatusOK {
		t.Fatalf("second login: %d %s", w.Code, w.Body.String())
	}
	other.csrf, _ = other.json(w)["csrf"].(string)

	// The current password is required even though we are already signed in:
	// that is what stops a borrowed session becoming ownership of the account.
	w = h.do(http.MethodPost, "/api/v1/session/password", map[string]string{
		"current_password": "wrong-but-long-enough", "new_password": "another-long-password",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("change with a wrong current password: %d, want 401", w.Code)
	}
	w = h.do(http.MethodPost, "/api/v1/session/password", map[string]string{
		"current_password": testPassword, "new_password": "another-long-password",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", w.Code, w.Body.String())
	}

	// Both sessions are gone, including the other browser's.
	if w := h.do(http.MethodGet, "/api/v1/devices", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("the changing session survived: %d", w.Code)
	}
	if w := other.do(http.MethodGet, "/api/v1/devices", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("another session survived the password change: %d", w.Code)
	}
	// The new password works.
	fresh := &harness{t: t, srv: h.srv, mux: h.mux, db: h.db, fleet: h.fleet}
	w = fresh.do(http.MethodPost, "/api/v1/login",
		map[string]string{"username": "admin", "password": "another-long-password"})
	if w.Code != http.StatusOK {
		t.Fatalf("login with the new password: %d %s", w.Code, w.Body.String())
	}
}

func TestExpiredSessionIsRejectedAndCleared(t *testing.T) {
	h := newHarness(t)
	h.setup()

	now := time.Now()
	h.srv.Now = func() time.Time { return now.Add(sessionIdle + time.Minute) }
	w := h.do(http.MethodGet, "/api/v1/devices", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("idle session: %d, want 401", w.Code)
	}
	// The cookie is cleared so the browser stops presenting a dead token.
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("an expired session did not clear its cookie")
	}
}

// ---- fleet reads ----

func TestDevicesStatusIsDerivedFromLastSeen(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Now()
	h.srv.Now = func() time.Time { return now }

	recent := now.Add(-30 * time.Second).Unix()
	stale := now.Add(-10 * time.Minute).Unix()
	h.seedDevice("online-ap", true, &recent)
	h.seedDevice("offline-ap-x", true, &stale)
	h.seedDevice("never-polled-ap", true, nil)
	h.seedDevice("pending-ap-xyz", false, nil)

	w := h.do(http.MethodGet, "/api/v1/devices", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("devices: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Devices []deviceView `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	byName := map[string]deviceView{}
	for _, d := range resp.Devices {
		byName[d.Name] = d
	}
	want := map[string]string{
		"online-ap": "online", "offline-ap-x": "offline",
		"never-polled-ap": "unknown", "pending-ap-xyz": "pending",
	}
	for name, status := range want {
		got, ok := byName[name]
		if !ok {
			t.Errorf("device %q missing from the list", name)
			continue
		}
		if got.Status != status {
			t.Errorf("%s: status = %q, want %q", name, got.Status, status)
		}
	}
	// "never polled" must not read as the epoch.
	if byName["never-polled-ap"].LastSeen != nil {
		t.Error("a device that was never polled reported a last_seen")
	}

	// Filtering.
	w = h.do(http.MethodGet, "/api/v1/devices?status=online", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Devices) != 1 || resp.Devices[0].Name != "online-ap" {
		t.Fatalf("status filter returned %+v", resp.Devices)
	}
}

func TestDeviceDetailAndSeries(t *testing.T) {
	h := newHarness(t)
	h.setup()
	dev := h.seedDevice("ap1", true, nil)
	ctx := context.Background()

	base := time.Now().Truncate(time.Hour).Unix()
	if err := h.db.WriteRollups(ctx, []store.RollupRow{
		{DeviceID: dev.ID, Kind: "iface_rx_bps", Key: "wan", TS: base, Avg: 100, Cnt: 12},
		{DeviceID: dev.ID, Kind: "chan_busy_pct", Key: "wlan0", TS: base, Avg: 25, Cnt: 12},
		{DeviceID: dev.ID, Kind: "sta_rssi", Key: "aa:bb", TS: base, Avg: -52, Cnt: 12},
	}); err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", dev.ID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("device detail: %d %s", w.Code, w.Body.String())
	}
	var detail deviceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Interfaces) != 1 || detail.Interfaces[0] != "wan" {
		t.Errorf("interfaces = %v, want [wan]", detail.Interfaces)
	}
	if len(detail.Radios) != 1 || detail.Radios[0] != "wlan0" {
		t.Errorf("radios = %v, want [wlan0]", detail.Radios)
	}
	if len(detail.Stations) != 1 {
		t.Errorf("stations = %v", detail.Stations)
	}

	// The series index reflects what was collected, not what the code could
	// in principle produce.
	w = h.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d/series", dev.ID), nil)
	var idx struct {
		Series map[string][]string `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Series) != 3 {
		t.Fatalf("series index = %v, want exactly the three that have data", idx.Series)
	}

	w = h.do(http.MethodGet, "/api/v1/devices/9999", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown device: %d, want 404", w.Code)
	}
}

func TestStatsValidatesItsInput(t *testing.T) {
	h := newHarness(t)
	h.setup()
	dev := h.seedDevice("ap1", true, nil)

	for _, tc := range []struct {
		name, path string
		want       int
	}{
		{"unknown kind", "/api/v1/stats/not_a_kind?device_id=1", http.StatusBadRequest},
		{"no device", "/api/v1/stats/sys_load1", http.StatusBadRequest},
		{"reversed range", fmt.Sprintf(
			"/api/v1/stats/sys_load1?device_id=%d&from=2000&to=1000", dev.ID), http.StatusBadRequest},
		{"beyond retention", fmt.Sprintf(
			"/api/v1/stats/sys_load1?device_id=%d&from=0&to=%d", dev.ID,
			time.Now().Unix()), http.StatusBadRequest},
		{"valid", fmt.Sprintf("/api/v1/stats/sys_load1?device_id=%d", dev.ID), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := h.do(http.MethodGet, tc.path, nil)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}

	// An empty series marshals as [] rather than null, so a chart can render
	// "no data" without a type check.
	w := h.do(http.MethodGet,
		fmt.Sprintf("/api/v1/stats/sys_load1?device_id=%d", dev.ID), nil)
	if !strings.Contains(w.Body.String(), `"points":[]`) {
		t.Errorf("empty series body = %s, want an empty array", w.Body.String())
	}
}

// A browser tab that closes does not run cleanup, so focus must expire on its
// own — a leaked focus means a router polled every five seconds forever.
func TestFocusIsTimeBounded(t *testing.T) {
	h := newHarness(t)
	h.setup()
	dev := h.seedDevice("ap1", true, nil)

	w := h.do(http.MethodPost,
		fmt.Sprintf("/api/v1/devices/%d/focus?seconds=1", dev.ID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("focus: %d %s", w.Code, w.Body.String())
	}
	if got := h.json(w)["focused_for_seconds"]; got != float64(5) {
		// 1 is below the floor, so it is clamped to 5 rather than honoured.
		t.Fatalf("focused_for_seconds = %v, want the clamped minimum of 5", got)
	}
	if n := h.fleet.focusCount(dev.ID); n != 1 {
		t.Fatalf("focus count = %d, want 1", n)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.fleet.focusCount(dev.ID) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("focus was never released; a closed tab would pin the device forever")
}

func TestFocusRejectsUnknownDevice(t *testing.T) {
	h := newHarness(t)
	h.setup()
	if w := h.do(http.MethodPost, "/api/v1/devices/999/focus", nil); w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestEventsFilter(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	for _, e := range []store.Event{
		{Category: "device", Severity: "warning", Event: "device.unreachable"},
		{Category: "device", Severity: "info", Event: "device.reachable"},
		{Category: "audit", Severity: "info", Event: "config.apply"},
	} {
		if err := h.db.LogEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var resp struct {
		Events []store.Event `json:"events"`
	}
	w := h.do(http.MethodGet, "/api/v1/events?category=device", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// The setup itself logged an audit event, so filtering has to actually work.
	for _, e := range resp.Events {
		if e.Category != "device" {
			t.Errorf("category filter let %q through", e.Category)
		}
	}
	if len(resp.Events) != 2 {
		t.Errorf("got %d device events, want 2", len(resp.Events))
	}

	w = h.do(http.MethodGet, "/api/v1/events?severity=warning", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Events) != 1 || resp.Events[0].Severity != "warning" {
		t.Errorf("severity filter returned %+v", resp.Events)
	}
}

// The fleet client total must refuse to be summed when any AP's count is
// unreadable — a partial sum draws a dip that means "a radio did not answer".
func TestDashboardClientTotalRefusesWhenUnknown(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Now()
	h.srv.Now = func() time.Time { return now }
	recent := now.Add(-10 * time.Second).Unix()

	a := h.seedDevice("ap-one", true, &recent)
	b := h.seedDevice("ap-two-x", true, &recent)
	ctx := context.Background()
	base := now.Truncate(5 * time.Minute).Unix()
	if err := h.db.WriteRollups(ctx, []store.RollupRow{
		{DeviceID: a.ID, Kind: "ap_clients", Key: "wlan0", TS: base, Avg: 3, Cnt: 12},
		{DeviceID: a.ID, Kind: "ap_clients", Key: "wlan1", TS: base, Avg: 2, Cnt: 12},
	}); err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, "/api/v1/dashboard", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard: %d %s", w.Code, w.Body.String())
	}
	var d dashboard
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Devices.Total != 2 || d.Devices.Online != 2 {
		t.Errorf("device counts = %+v", d.Devices)
	}
	if d.Clients != nil {
		t.Errorf("clients = %d, want null — device %q has no readable count",
			*d.Clients, b.Name)
	}
	if len(d.ClientsUnsure) != 1 || d.ClientsUnsure[0] != b.Name {
		t.Errorf("clients_unknown_on = %v, want [%s]", d.ClientsUnsure, b.Name)
	}

	// Once every AP can be read, the total is reported.
	if err := h.db.WriteRollups(ctx, []store.RollupRow{
		{DeviceID: b.ID, Kind: "ap_clients", Key: "wlan0", TS: base, Avg: 4, Cnt: 12},
	}); err != nil {
		t.Fatal(err)
	}
	w = h.do(http.MethodGet, "/api/v1/dashboard", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Clients == nil || *d.Clients != 9 {
		t.Fatalf("clients = %v, want 9", d.Clients)
	}
}

func TestResponsesAreNotCacheable(t *testing.T) {
	h := newHarness(t)
	h.setup()
	w := h.do(http.MethodGet, "/api/v1/devices", nil)
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestMalformedBodiesAreRejected(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup",
		strings.NewReader(`{"username":"a","password":"b","extra":1}`))
	req.RemoteAddr = "192.0.2.10:1"
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d, want 400", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(`not json`))
	req.RemoteAddr = "192.0.2.10:1"
	w = httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("garbage body: %d, want 400", w.Code)
	}
}

// The password hash must never leave the server, in any response.
func TestNoPasswordHashEscapes(t *testing.T) {
	h := newHarness(t)
	h.setup()
	admin, err := h.db.AdminByName(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/session", "/api/v1/devices",
		"/api/v1/dashboard", "/api/v1/events"} {
		w := h.do(http.MethodGet, path, nil)
		if strings.Contains(w.Body.String(), admin.PassHash) {
			t.Errorf("%s leaked the password hash", path)
		}
		if strings.Contains(w.Body.String(), "$argon2id$") {
			t.Errorf("%s contains something that looks like a password hash", path)
		}
	}
}

func TestSweepExpiresSessions(t *testing.T) {
	h := newHarness(t)
	h.setup()
	now := time.Now()
	h.srv.Now = func() time.Time { return now.Add(sessionAbsolute + time.Hour) }
	h.srv.Sweep()

	h.srv.sessions.mu.Lock()
	n := len(h.srv.sessions.m)
	h.srv.sessions.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d sessions survived the sweep", n)
	}
}
