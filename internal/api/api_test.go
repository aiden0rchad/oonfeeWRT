package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
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
	clients  map[int64]*int
	overhead map[int64]collector.Overhead
	degraded map[int64][]collector.Degradation
	// aps is what each device is broadcasting. Nil for a device means no poll
	// has looked, which the API must report differently from "no BSS".
	aps map[int64][]collector.AP
	// sections maps a device's interfaces to their UCI wifi-iface sections.
	// Absent means no poll has read it, which must not read as "foreign".
	sections map[int64]map[string]string
	// modes is each interface's configured mode. Absent means no poll has read
	// it, which must never be taken as "they are all APs".
	modes map[int64]map[string]string
}

func (f *stubFleet) IfaceModes(id int64) (map[string]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.modes[id]
	return v, ok
}

func (f *stubFleet) IfaceSections(id int64) (map[string]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.sections[id]
	return v, ok
}

func (f *stubFleet) Broadcasting(id int64) ([]collector.AP, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.aps[id]
	return v, ok
}

func newStubFleet() *stubFleet {
	return &stubFleet{focused: map[int64]int{}, tier: map[int64]collector.Tier{},
		quiesced: map[int64]bool{}, clients: map[int64]*int{},
		overhead: map[int64]collector.Overhead{},
		degraded: map[int64][]collector.Degradation{}}
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

func (f *stubFleet) LiveClients(deviceID int64) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.clients[deviceID]
	if !ok || n == nil {
		return 0, false
	}
	return *n, true
}

func (f *stubFleet) setClients(deviceID int64, n *int) {
	f.mu.Lock()
	f.clients[deviceID] = n
	f.mu.Unlock()
}

func (f *stubFleet) Degraded(deviceID int64) ([]collector.Degradation, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.degraded[deviceID]
	return d, ok
}

func (f *stubFleet) setDegraded(deviceID int64, d []collector.Degradation) {
	f.mu.Lock()
	f.degraded[deviceID] = d
	f.mu.Unlock()
}

func (f *stubFleet) Overhead(deviceID int64) (collector.Overhead, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.overhead[deviceID]
	return o, ok
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
	srv := New(db, fleet, nil, quiet())
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
	// One AP answered with a count; the other could not be asked at all. The
	// counts are LIVE state from the last poll, not rollups — asking the rollup
	// table would report "unknown" for the first five minutes of every run.
	five := 5
	h.fleet.setClients(a.ID, &five)
	h.fleet.setClients(b.ID, nil)

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
	if d.WirelessClients != nil {
		t.Errorf("wireless_clients = %d, want null — device %q has no readable count",
			*d.WirelessClients, b.Name)
	}
	if len(d.ClientsUnsure) != 1 || d.ClientsUnsure[0] != b.Name {
		t.Errorf("clients_unknown_on = %v, want [%s]", d.ClientsUnsure, b.Name)
	}

	// Once every AP can be read, the total is reported.
	four := 4
	h.fleet.setClients(b.ID, &four)
	w = h.do(http.MethodGet, "/api/v1/dashboard", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.WirelessClients == nil || *d.WirelessClients != 9 {
		t.Fatalf("wireless_clients = %v, want 9", d.WirelessClients)
	}

	// The LAN inventory is a different question and must not be conflated with
	// associated stations — and it counts THIS network, not everything the
	// device can see. A gateway's neighbour tables also cover its uplink.
	if err := h.db.UpsertClients(ctx, []store.SeenClient{
		{MAC: "11:11:11:11:11:11", Name: "wired-nas", IPv4: "192.168.1.20",
			Scope: store.ScopeLocal},
		{MAC: "22:22:22:22:22:22", Name: "upstream-router", IPv4: "10.7.46.1",
			Scope: store.ScopeUpstream},
		{MAC: "33:33:33:33:33:33", Name: "unplaced", Scope: store.ScopeUnknown},
	}, now.Unix()); err != nil {
		t.Fatal(err)
	}
	w = h.do(http.MethodGet, "/api/v1/dashboard", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.KnownDevices != 1 || d.ActiveDevices != 1 {
		t.Errorf("known/active devices = %d/%d, want 1/1 — the upstream "+
			"neighbour and the unplaced host are not on this network",
			d.KnownDevices, d.ActiveDevices)
	}
	if d.UpstreamDevices != 1 || d.UnscopedDevices != 1 {
		t.Errorf("upstream/unscoped = %d/%d, want 1/1 — the excluded hosts must "+
			"still be reported, or the headline is just quietly smaller",
			d.UpstreamDevices, d.UnscopedDevices)
	}
	if d.WirelessClients == nil || *d.WirelessClients != 9 {
		t.Errorf("the LAN inventory changed the wireless total: %v", d.WirelessClients)
	}

	// The client list's own scope counts must agree with the dashboard's: they
	// are the same question and used to be computed two different ways. Asked
	// with all=1 so both cover the same set — the grid's default view is the
	// last 24 hours, the dashboard's totals are all-time.
	w = h.do(http.MethodGet, "/api/v1/clients?all=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("clients: %d %s", w.Code, w.Body.String())
	}
	var cl struct {
		Facets struct {
			Scope []store.Facet `json:"scope"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cl); err != nil {
		t.Fatal(err)
	}
	scopes := map[string]int{}
	for _, f := range cl.Facets.Scope {
		scopes[f.Value] = f.Count
	}
	if scopes[store.ScopeLocal] != d.KnownDevices {
		t.Errorf("the client list says %d local, the dashboard says %d",
			scopes[store.ScopeLocal], d.KnownDevices)
	}
	if scopes[store.ScopeUpstream] != 1 || scopes[store.ScopeUnknown] != 1 {
		t.Errorf("client scope counts = %v, want 1 upstream and 1 unknown", scopes)
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

// ---- clients ----

func TestClientsGrid(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	now := time.Now()
	h.srv.Now = func() time.Time { return now }

	dev := h.seedDevice("ap-c", true, nil)
	if err := h.db.UpsertClients(ctx, []store.SeenClient{
		{MAC: "aa:bb:cc:11:22:33", Name: "laptop", IPv4: "192.168.1.130"},
		{MAC: "aa:bb:cc:44:55:66", Name: "iot-plug", IPv4: "192.168.1.131"},
	}, now.Unix()); err != nil {
		t.Fatal(err)
	}
	// One of them has been seen by a focused poll; the other has not.
	base := now.Truncate(5 * time.Minute).Unix()
	if err := h.db.WriteRollups(ctx, []store.RollupRow{
		{DeviceID: dev.ID, Kind: "sta_rssi", Key: "aa:bb:cc:11:22:33",
			TS: base, Avg: -52, Cnt: 12},
		{DeviceID: dev.ID, Kind: "sta_retry_pct", Key: "aa:bb:cc:11:22:33",
			TS: base, Avg: 4.5, Cnt: 12},
	}); err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, "/api/v1/clients", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("clients: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Clients []clientView `json:"clients"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Clients) != 2 {
		t.Fatalf("got %d clients, want 2", len(resp.Clients))
	}
	byMAC := map[string]clientView{}
	for _, c := range resp.Clients {
		byMAC[c.MAC] = c
	}

	seen := byMAC["aa:bb:cc:11:22:33"]
	if seen.Connection != "wireless" {
		t.Errorf("a client with RSSI telemetry is %q, want wireless", seen.Connection)
	}
	if seen.Signal == nil || *seen.Signal != -52 {
		t.Errorf("signal = %v, want -52", seen.Signal)
	}
	if seen.RetryPct == nil {
		t.Error("retry percentage missing for a client with the series")
	}
	if !seen.Online {
		t.Error("a client seen just now is not online")
	}

	// The one with no focused-poll data must report unknown and carry NO signal
	// — not "wired", and certainly not 0 dBm.
	unseen := byMAC["aa:bb:cc:44:55:66"]
	if unseen.Connection != "unknown" {
		t.Errorf("connection = %q; absence of wireless evidence is not evidence "+
			"of a cable", unseen.Connection)
	}
	if unseen.Signal != nil {
		t.Errorf("signal = %v for a client no focused poll has covered", *unseen.Signal)
	}
}

// The rail's idea of "wireless" and the row's must be the same idea.
//
// They are computed in two places — the facet and filter in SQL, the per-row
// Connection field in Go from the station series — because one of them has to
// survive paging and the other carries the signal and retry values. Two
// definitions is exactly how a grid ends up listing a row its own rail did not
// count, so this asserts they agree on the same data.
func TestClientsFilterAndRowsAgreeOnWireless(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	now := time.Now()
	h.srv.Now = func() time.Time { return now }

	dev := h.seedDevice("ap-e", true, nil)
	if err := h.db.UpsertClients(ctx, []store.SeenClient{
		{MAC: "aa:bb:cc:00:00:aa", Name: "phone", Scope: store.ScopeLocal},
		{MAC: "aa:bb:cc:00:00:bb", Name: "nas", Scope: store.ScopeLocal},
	}, now.Unix()); err != nil {
		t.Fatal(err)
	}
	base := now.Truncate(5 * time.Minute).Unix()
	if err := h.db.WriteRollups(ctx, []store.RollupRow{
		{DeviceID: dev.ID, Kind: "sta_rssi", Key: "aa:bb:cc:00:00:aa",
			TS: base, Avg: -61, Cnt: 9},
	}); err != nil {
		t.Fatal(err)
	}

	var resp struct {
		Clients []clientView `json:"clients"`
		Total   int          `json:"total"`
		Facets  struct {
			Connection []store.Facet `json:"connection"`
		} `json:"facets"`
	}
	w := h.do(http.MethodGet, "/api/v1/clients?connection=wireless", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("clients: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 || len(resp.Clients) != 1 {
		t.Fatalf("wireless filter returned %d of %d, want 1 of 1",
			len(resp.Clients), resp.Total)
	}
	// The row the SQL selected must also render as wireless. If these two
	// disagree the grid shows a row labelled "unknown" under a filter that says
	// wireless, and nothing in the response admits which one is wrong.
	if got := resp.Clients[0]; got.Connection != "wireless" ||
		got.MAC != "aa:bb:cc:00:00:aa" {
		t.Errorf("the wireless filter selected %s rendered as %q",
			got.MAC, got.Connection)
	}
	counts := map[string]int{}
	for _, f := range resp.Facets.Connection {
		counts[f.Value] = f.Count
	}
	if counts["wireless"] != 1 || counts["unknown"] != 1 {
		t.Errorf("connection facet = %v, want 1 wireless and 1 unknown even "+
			"while filtered to wireless", counts)
	}
}

// A station series outlives the client's visit by the whole retention period, so
// without a recency bound the grid would report a laptop as connected at
// -52 dBm two weeks after it left.
func TestClientsIgnoreStaleStationTelemetry(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	now := time.Now()
	h.srv.Now = func() time.Time { return now }

	dev := h.seedDevice("ap-d", true, nil)
	if err := h.db.UpsertClients(ctx, []store.SeenClient{
		{MAC: "aa:bb:cc:11:22:33", Name: "laptop"},
	}, now.Unix()); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-3 * time.Hour).Truncate(5 * time.Minute).Unix()
	if err := h.db.WriteRollups(ctx, []store.RollupRow{
		{DeviceID: dev.ID, Kind: "sta_rssi", Key: "aa:bb:cc:11:22:33",
			TS: old, Avg: -52, Cnt: 12},
	}); err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, "/api/v1/clients", nil)
	var resp struct {
		Clients []clientView `json:"clients"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Clients) != 1 {
		t.Fatalf("got %d clients", len(resp.Clients))
	}
	if resp.Clients[0].Signal != nil {
		t.Errorf("three-hour-old RSSI was reported as current: %v", *resp.Clients[0].Signal)
	}
	if resp.Clients[0].Connection != "unknown" {
		t.Errorf("connection = %q from stale telemetry", resp.Clients[0].Connection)
	}
}

func TestClientNameIsNotOverwrittenByAnEmptyOne(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	now := time.Now().Unix()

	if err := h.db.UpsertClients(ctx,
		[]store.SeenClient{{MAC: "aa:bb", Name: "laptop"}}, now); err != nil {
		t.Fatal(err)
	}
	// A later poll where reverse DNS failed must not erase the name.
	if err := h.db.UpsertClients(ctx,
		[]store.SeenClient{{MAC: "aa:bb", Name: ""}}, now+60); err != nil {
		t.Fatal(err)
	}
	got, err := h.db.Clients(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "laptop" {
		t.Fatalf("client = %+v, want the name preserved", got)
	}
	if got[0].LastSeen == nil || *got[0].LastSeen != now+60 {
		t.Errorf("last_seen was not advanced: %v", got[0].LastSeen)
	}
	if got[0].FirstSeen == nil || *got[0].FirstSeen != now {
		t.Errorf("first_seen changed: %v", got[0].FirstSeen)
	}
}

// ---- fixes from the adversarial review ----

// The CSRF token cannot protect /setup and /login — there is no session to
// carry one yet. /setup is the sharp case: on a fresh controller it creates the
// administrator account, so a cross-site POST could claim the install.
func TestUnauthenticatedMutationsRequireSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site (a sibling subdomain)", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"foreign Origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"foreign Referer", map[string]string{"Referer": "https://evil.example/page"}, http.StatusForbidden},
		{"same-origin metadata", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusCreated},
		{"user-initiated navigation", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusCreated},
		{"no browser headers at all (curl)", nil, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/setup",
				strings.NewReader(`{"username":"admin","password":"`+testPassword+`"}`))
			req.Host = "controller.local"
			req.RemoteAddr = "192.0.2.10:1"
			req.Header.Set("Content-Type", "application/json")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.mux.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// An HTML form can only send urlencoded, multipart or text/plain, so insisting
// on JSON blocks a cross-site form post outright.
func TestNonJSONContentTypeIsRejected(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup",
		strings.NewReader(`{"username":"admin","password":"`+testPassword+`"}`))
	req.RemoteAddr = "192.0.2.10:1"
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d, want 415", w.Code)
	}
}

// Identical bodies are not enough if the clock differs. An unknown username
// that skips argon2 entirely answers in microseconds where a known one takes
// tens of milliseconds, which is the account enumeration the identical
// responses were meant to prevent.
func TestLoginSpendsTheSameWorkOnUnknownUsernames(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.cookies, h.csrf = nil, ""

	measure := func(username string) time.Duration {
		// Best of three: the floor is the signal, scheduler noise only adds.
		best := time.Hour
		for range 3 {
			start := time.Now()
			w := h.do(http.MethodPost, "/api/v1/login",
				map[string]string{"username": username, "password": "wrong-but-long-enough"})
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s: got %d, want 401", username, w.Code)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	known := measure("admin")
	unknown := measure("no-such-operator")

	// The unknown path must not be dramatically faster. A factor of two is
	// generous — before the fix it was three orders of magnitude.
	if unknown*2 < known {
		t.Fatalf("unknown username answered in %v against %v for a known one; "+
			"the timing distinguishes them", unknown, known)
	}
	t.Logf("known %v, unknown %v", known, unknown)
}

// The count and the insert are separated by an argon2id derivation, which is
// ample room for a second request to pass the same check — and two different
// usernames would both insert cleanly, since only `username` is unique.
func TestConcurrentSetupCreatesExactlyOneAdmin(t *testing.T) {
	h := newHarness(t)
	const n = 6
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/setup",
				strings.NewReader(fmt.Sprintf(
					`{"username":"admin%d","password":"%s"}`, i, testPassword)))
			req.RemoteAddr = "192.0.2.10:1"
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.mux.ServeHTTP(w, req)
			codes <- w.Code
		}(i)
	}
	wg.Wait()
	close(codes)

	created := 0
	for c := range codes {
		if c == http.StatusCreated {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d of %d concurrent setups reported success, want exactly 1", created, n)
	}
	got, err := h.db.AdminCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("%d administrator accounts exist, want 1", got)
	}
}

// The throttle bounds the RATE of completed attempts, not concurrent ones: it
// records a failure only after the hash finishes. Each derivation allocates a
// 64 MiB arena, so unbounded concurrency is a memory-exhaustion primitive on an
// unauthenticated endpoint.
func TestConcurrentLoginsAreBounded(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.cookies, h.csrf = nil, ""

	const n = 12
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/login",
				strings.NewReader(`{"username":"admin","password":"wrong-but-long-enough"}`))
			req.RemoteAddr = "192.0.2.10:1"
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.mux.ServeHTTP(w, req)
			codes <- w.Code
		}()
	}
	wg.Wait()
	close(codes)

	shed := 0
	for c := range codes {
		switch c {
		case http.StatusServiceUnavailable:
			shed++
		case http.StatusUnauthorized, http.StatusTooManyRequests:
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	// Not asserting an exact number — the point is that the server sheds load
	// instead of admitting every caller into a 64 MiB allocation.
	t.Logf("%d of %d concurrent logins shed with 503", shed, n)
	if h.srv.hashing == nil || cap(h.srv.hashing) != hashSlots {
		t.Fatalf("hashing semaphore is not in place (cap=%d)", cap(h.srv.hashing))
	}
}

// Filtering a page the database already truncated selects from the newest N
// events overall rather than the newest N matching, so a view filtered to
// "error" can come back empty while errors exist.
func TestEventFilterAppliesBeforeTheLimit(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()

	// One old error, then plenty of newer routine events.
	if err := h.db.LogEvent(ctx, store.Event{
		TS: 1000, Category: "device", Severity: "error", Event: "device.unreachable",
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		if err := h.db.LogEvent(ctx, store.Event{
			TS: int64(2000 + i), Category: "device", Severity: "info", Event: "device.reachable",
		}); err != nil {
			t.Fatal(err)
		}
	}

	var resp struct {
		Events []store.Event `json:"events"`
	}
	w := h.do(http.MethodGet, "/api/v1/events?severity=error&limit=10", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range resp.Events {
		if e.Severity != "error" {
			t.Errorf("filter let %q through", e.Severity)
		}
		if e.Event == "device.unreachable" {
			found = true
		}
	}
	if !found {
		t.Fatal("an error buried under 50 newer routine events was reported as " +
			"absent; the filter ran after the LIMIT")
	}
}

// An unrecognised role is refused at the API, before anything reaches a device.
//
// It used to be stored verbatim and compared later with an exact string match,
// so "Gateway" adopted a router as an access point: no addressing, no DHCP, no
// firewall zone, and nothing anywhere saying why. The rejection has to name the
// valid roles, because an operator who typed "router" needs to be told the word
// is "gateway".
func TestAdoptRefusesAnUnknownRole(t *testing.T) {
	h := newHarness(t)
	h.setup()
	e := &stubEnroller{}
	h.srv.Enroll = e

	w := h.do(http.MethodPost, "/api/v1/devices/adopt", map[string]any{
		"host": "192.0.2.1", "username": "root", "password": "x", "role": "router",
	})
	if w.Code == http.StatusOK {
		t.Fatal("an unknown role was accepted")
	}
	body := w.Body.String()
	for _, want := range []string{"gateway", "ap", "switch"} {
		if !strings.Contains(body, want) {
			t.Errorf("the rejection %q does not name %q as a valid role", body, want)
		}
	}
	if len(e.adopted) != 0 {
		t.Error("the device was contacted despite an invalid role; validation " +
			"must happen before anything is written")
	}
}

// A limitation the controller knows about must be readable somewhere.
//
// Degradations are logged at debug — deliberately, because they are a standing
// property of a device's ACL rather than an event and logging them per poll
// would bury everything else. That reasoning is right and it leaves the
// operator with no way to see them at all. The device detail is where they go.
func TestDeviceDetailReportsWhatThePollCouldNotRead(t *testing.T) {
	h := newHarness(t)
	h.setup()
	dev := h.seedDevice("ap-x", true, nil)
	h.fleet.setDegraded(dev.ID, []collector.Degradation{
		{Object: "luci-rpc", Method: "getWirelessDevices",
			Err: "Permission denied", Permanent: true},
		{Object: "iwinfo", Method: "survey", Err: "busy"},
	})

	w := h.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", dev.ID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("device: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Degraded []struct {
			Call      string `json:"call"`
			Err       string `json:"error"`
			Permanent bool   `json:"permanent"`
			Costs     string `json:"costs"`
		} `json:"degraded"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Degraded) != 2 {
		t.Fatalf("degraded = %+v, want two", got.Degraded)
	}
	first := got.Degraded[0]
	if first.Call != "luci-rpc.getWirelessDevices" || !first.Permanent {
		t.Errorf("first degradation = %+v", first)
	}
	// The consequence, not just the call name. "luci-rpc.getWirelessDevices:
	// Permission denied" tells an operator nothing about what they lost.
	if !strings.Contains(first.Costs, "mesh") || !strings.Contains(first.Costs, "clients") {
		t.Errorf("costs = %q; it does not say what the missing grant costs",
			first.Costs)
	}
}

// A device the collector has never polled reports no degradations rather than
// an empty list, which would read as "everything is fine".
func TestDeviceDetailOmitsDegradationsBeforeAnyPoll(t *testing.T) {
	h := newHarness(t)
	h.setup()
	dev := h.seedDevice("ap-y", true, nil)

	w := h.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", dev.ID), nil)
	if strings.Contains(w.Body.String(), `"degraded"`) {
		t.Errorf("an unpolled device reported a degradation list: %s", w.Body.String())
	}
}

// An uplink is validated against the whole site before it is stored, not after.
//
// It is only meaningful in relation to a WLAN — whether that network is
// enabled, accepts bridges, and is published on the requested band — so storing
// one that cannot work would put a row in the site model whose only effect is a
// rendered station that never associates. That failure looks exactly like a
// driver refusing 4-address frames, which sends an operator to the wrong place.
func TestUplinkIsRefusedBeforeItIsStored(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()

	n := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}
	if err := h.db.SaveNetwork(ctx, n); err != nil {
		t.Fatal(err)
	}
	g := &model.APGroup{Name: "all"}
	if err := h.db.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	// A WLAN that does NOT accept wireless bridges — the half people forget.
	w := &model.WLAN{SSID: "roam", NetworkID: n.ID, GroupID: g.ID,
		Bands: []model.Band{model.Band5G}, Enabled: true}
	if err := h.db.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}

	rec := h.do("POST", "/api/v1/site/uplinks", map[string]any{
		"device_id": 1, "wlan_id": w.ID, "band": "5g", "enabled": true,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 — an unusable uplink was stored", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "does not accept wireless bridges") {
		t.Errorf("the refusal does not name the missing half: %s", rec.Body.String())
	}

	site, err := h.db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(site.Uplinks) != 0 {
		t.Errorf("a refused uplink was stored anyway: %+v", site.Uplinks)
	}
}

// Both hazards are on the responses, every time, rather than only in
// documentation. The controller cannot see the far end of a cable, and these
// are the two things it cannot check for the operator.
func TestUplinkResponsesCarryBothHazards(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()

	n := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}
	_ = h.db.SaveNetwork(ctx, n)
	g := &model.APGroup{Name: "all"}
	_ = h.db.SaveGroup(ctx, g)
	w := &model.WLAN{SSID: "roam", NetworkID: n.ID, GroupID: g.ID,
		Bands: []model.Band{model.Band5G}, Enabled: true,
		Options: model.WLANOptions{AllowUplink: true}}
	_ = h.db.SaveWLAN(ctx, w)
	at := int64(1)
	dev := &store.Device{MAC: "60:38:e0:00:0f:01", Host: "192.168.1.9",
		Name: "no-cable", Scheme: "http", AdoptedAt: &at}
	if err := h.db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}

	rec := h.do("POST", "/api/v1/site/uplinks", map[string]any{
		"device_id": dev.ID, "wlan_id": w.ID, "band": "5g", "enabled": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "layer-2 loop") {
		t.Errorf("creating an uplink did not warn about the loop: %s", rec.Body.String())
	}

	// The site view carries it, so a screen can render it without a second call.
	site := h.json(h.do("GET", "/api/v1/site", nil))
	ups, _ := site["uplinks"].([]any)
	if len(ups) != 1 {
		t.Fatalf("the site view does not carry the uplink: %v", site["uplinks"])
	}

	// And the delete warns about the OTHER hazard: on a device with no cable
	// this station IS the route the controller reaches it through.
	created := h.json(rec)["uplink"].(map[string]any)
	id := int(created["id"].(float64))
	rec = h.do("DELETE", fmt.Sprintf("/api/v1/site/uplinks/%d", id), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "acknowledge") {
		t.Errorf("deleting an uplink did not warn that it is the route: %s",
			rec.Body.String())
	}
}

// Which AP a client is on must be decided by the radio, not by which collector
// happened to write its row second.
//
// Two APs report the same MAC in one five-minute bucket on every roam, and also
// whenever an operator opens two device pages within five minutes — both get
// focused, and a focused poll is the only thing that produces station
// telemetry. The old query scanned flat and let the last row win, so the
// evidence never entered into it: holding the readings fixed and reversing only
// the write order moved the client to the other AP.
func TestClientAPAttributionIgnoresWriteOrder(t *testing.T) {
	for _, rightLast := range []bool{false, true} {
		name := "left-written-last"
		if rightLast {
			name = "right-written-last"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.setup()
			ctx := context.Background()
			now := time.Now()
			h.srv.Now = func() time.Time { return now }

			left := h.seedDevice("ap-left", true, nil).ID
			right := h.seedDevice("ap-right", true, nil).ID
			const mac = "aa:bb:cc:de:ad:01"
			if err := h.db.UpsertClients(ctx, []store.SeenClient{
				{MAC: mac, Name: "roamer", IPv4: "192.168.9.99"},
			}, now.Unix()); err != nil {
				t.Fatal(err)
			}

			// The truth is identical in both runs: the client is associated to
			// ap-left, which hears it at -40 across nine samples. ap-right
			// merely overhears it at -88. Only the write order differs.
			base := now.Truncate(5 * time.Minute).Unix()
			onLeft := []store.RollupRow{
				{DeviceID: left, Kind: "sta_rssi", Key: mac, TS: base, Avg: -40, Cnt: 9},
				{DeviceID: left, Kind: "sta_retry_pct", Key: mac, TS: base, Avg: 3, Cnt: 9},
			}
			onRight := []store.RollupRow{
				{DeviceID: right, Kind: "sta_rssi", Key: mac, TS: base, Avg: -88, Cnt: 2},
				{DeviceID: right, Kind: "sta_retry_pct", Key: mac, TS: base, Avg: 61, Cnt: 2},
			}
			order := append(append([]store.RollupRow{}, onLeft...), onRight...)
			if rightLast {
				order = append(append([]store.RollupRow{}, onRight...), onLeft...)
			}
			if err := h.db.WriteRollups(ctx, order); err != nil {
				t.Fatal(err)
			}

			w := h.do(http.MethodGet, "/api/v1/clients", nil)
			if w.Code != http.StatusOK {
				t.Fatalf("clients: %d %s", w.Code, w.Body.String())
			}
			var resp struct {
				Clients []clientView `json:"clients"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			var got *clientView
			for i := range resp.Clients {
				if resp.Clients[i].MAC == mac {
					got = &resp.Clients[i]
				}
			}
			if got == nil {
				t.Fatal("the roaming client is missing from the grid entirely")
			}
			if got.DeviceID == nil {
				t.Fatal("no AP attributed to a client two APs reported")
			}
			if *got.DeviceID != left {
				t.Errorf("attributed to the AP that merely overhears it " +
					"(-88, 2 samples) rather than the one it is on " +
					"(-40, 9 samples); write order decided this, not the radio")
			}
			// The metrics must come from the SAME AP as the attribution. Each
			// field used to be overwritten independently, so a client could be
			// shown on ap-left at ap-right's signal and ap-right's retry rate —
			// three fields, three different sources, one plausible-looking row.
			if got.Signal == nil {
				t.Error("no signal for a client two APs reported")
			} else if *got.Signal != -40 {
				t.Errorf("signal %d is not the attributed AP's reading (-40)", *got.Signal)
			}
			if got.RetryPct == nil {
				t.Error("no retry rate for a client two APs reported")
			} else if *got.RetryPct != 3 {
				t.Errorf("retry %.0f%% is not the attributed AP's reading (3%%); "+
					"it belongs to the other AP", *got.RetryPct)
			}
		})
	}
}

// An AP adopted with SSIDs already on it keeps broadcasting them — correctly,
// because the controller never touches config it did not write. Until now it
// never showed them either, so the device screen answered "what is this AP
// broadcasting?" with only half the truth, and the missing half was the half
// nobody is administering.
//
// Reported from the lab: the Archer C6 was on the air with oonfee-c6-2g and
// oonfee-c6-5g alongside the managed SSID.
//
// Provenance is decided from the SECTION, never the SSID. This test carries a
// foreign section whose SSID is IDENTICAL to a managed one, because that is the
// case an SSID-keyed answer gets wrong: it reported the still-foreign,
// still-broadcasting BSS as managed and withdrew its warning, while the
// controller still did not own the section and could not touch it.
func TestBroadcastProvenanceComesFromTheSectionNotTheSSID(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	dev := h.seedDevice("ap-c6", true, nil)

	if err := h.db.RecordOwned(ctx, []store.OwnedSection{
		{DeviceID: dev.ID, Config: "wireless", Section: "oowrt_wlan1_radio0",
			RenderedHash: "h1"},
	}); err != nil {
		t.Fatal(err)
	}

	h.fleet.mu.Lock()
	h.fleet.aps = map[int64][]collector.AP{dev.ID: {
		{Iface: "phy0-ap1", SSID: "oonfee-roam", BSSID: "aa:bb:cc:00:00:01"},
		{Iface: "phy0-ap0", SSID: "oonfee-c6-5g", BSSID: "aa:bb:cc:00:00:02"},
		// Same SSID as the managed one, from a section we do not own.
		{Iface: "phy1-ap0", SSID: "oonfee-roam", BSSID: "aa:bb:cc:00:00:03"},
	}}
	h.fleet.sections = map[int64]map[string]string{dev.ID: {
		"phy0-ap1": "oowrt_wlan1_radio0",
		"phy0-ap0": "default_radio0",
		"phy1-ap0": "default_radio1",
	}}
	h.fleet.mu.Unlock()

	w := h.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", dev.ID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("device detail: %d %s", w.Code, w.Body.String())
	}
	var detail deviceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if !detail.BroadcastKnown {
		t.Fatal("a poll that saw BSSes reported the list as unknown")
	}
	byIface := map[string]broadcastView{}
	for _, b := range detail.Broadcasting {
		byIface[b.Iface] = b
	}
	if got := byIface["phy0-ap1"].Origin; got != ProvOurs {
		t.Errorf("a section in owned_sections reported %q, want ours", got)
	}
	if got := byIface["phy0-ap0"].Origin; got != ProvForeign {
		t.Errorf("a section we never wrote reported %q, want foreign", got)
	}
	// The one that matters: identical SSID, foreign section.
	if got := byIface["phy1-ap0"].Origin; got != ProvForeign {
		t.Errorf("a FOREIGN section sharing a managed SSID reported %q; "+
			"provenance was decided by the name on the air rather than by who "+
			"wrote the config", got)
	}
	if byIface["phy0-ap0"].Section != "default_radio0" {
		t.Error("the section is not reported, so an operator cannot act on it")
	}
}

// A device whose interface list could not be read must report unknown, never
// foreign. Calling an operator's own SSID foreign because we failed to ask is
// the worse of the two errors.
func TestUnreadableSectionsReportUnknownNotForeign(t *testing.T) {
	h := newHarness(t)
	h.setup()
	dev := h.seedDevice("ap-acl-denied", true, nil)

	h.fleet.mu.Lock()
	h.fleet.aps = map[int64][]collector.AP{dev.ID: {
		{Iface: "phy0-ap0", SSID: "somebody-elses", BSSID: "aa:bb:cc:00:00:09"},
	}}
	h.fleet.sections = nil // getWirelessDevices refused
	h.fleet.mu.Unlock()

	w := h.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", dev.ID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("device detail: %d", w.Code)
	}
	var detail deviceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	for _, b := range detail.Broadcasting {
		if b.Origin != ProvUnknown {
			t.Errorf("origin %q with no section data; a check that could not "+
				"run must not return a verdict", b.Origin)
		}
	}
}

// An empty list must not claim the radios are silent. No poll having looked and
// a poll having found nothing are different answers, and only one of them is
// about the device.
func TestDeviceDetailSeparatesNoBSSFromNotLookedAt(t *testing.T) {
	h := newHarness(t)
	h.setup()
	dev := h.seedDevice("ap-unpolled", true, nil)

	w := h.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", dev.ID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("device detail: %d", w.Code)
	}
	var detail deviceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.BroadcastKnown {
		t.Error("no poll has looked, but the empty list was reported as known")
	}
}

// The brief must never carry the operator's passphrase, and that must be
// provable rather than asserted.
//
// Marshals the WHOLE device-detail response and searches the bytes for the real
// lab passphrase. A field-by-field check would pass on a payload that leaked it
// through some field nobody thought to look at.
func TestTheTakeoverBriefNeverCarriesThePassphrase(t *testing.T) {
	// A fabricated string, never a real one.
	//
	// This test originally used the lab C6's ACTUAL passphrase, read off the
	// device and committed to a public repository — in the test whose entire
	// purpose is proving passphrases do not leak. Nothing about the test needed
	// a real secret: the controller never reads key material for a foreign
	// section, so any sentinel does the same work.
	//
	// The load-bearing assertion is the one below it, not this constant. A
	// sentinel can only catch a leak of itself, so the test also asserts the
	// response contains no "key" field at all — the brief must have nowhere for
	// a passphrase to live, which is a property of the type rather than of a
	// value anybody remembered to strip.
	const neverALeakedKey = "not-a-real-key-2f8Qv1xLpZ"

	h := newHarness(t)
	h.setup()
	dev := h.seedDevice("ap-c6", true, nil)

	h.fleet.mu.Lock()
	h.fleet.aps = map[int64][]collector.AP{dev.ID: {
		{Iface: "phy0-ap0", SSID: "oonfee-c6-5g", BSSID: "aa:bb:cc:00:00:02"},
	}}
	h.fleet.sections = map[int64]map[string]string{dev.ID: {"phy0-ap0": "default_radio0"}}
	h.fleet.modes = map[int64]map[string]string{dev.ID: {"phy0-ap0": "ap"}}
	h.fleet.mu.Unlock()

	w := h.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", dev.ID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("device detail: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), neverALeakedKey) {
		t.Fatal("the response carries the foreign network's passphrase")
	}
	// And nothing that would let one be added later without noticing.
	if strings.Contains(strings.ToLower(w.Body.String()), `"key"`) {
		t.Error("the response has a \"key\" field; the brief must have nowhere " +
			"for a passphrase to live")
	}

	var detail deviceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	var brief *foreignSection
	for _, b := range detail.Broadcasting {
		if b.Brief != nil {
			brief = b.Brief
		}
	}
	if brief == nil {
		t.Fatal("no brief for a foreign section")
	}
	if !brief.SafeToDisable || len(brief.Recipe) == 0 {
		t.Fatalf("a plain AP got no recipe: %+v", brief)
	}
	// The reload is the step that actually takes the BSS off the air. A recipe
	// that stops at `uci commit` leaves someone believing an SSID is gone while
	// it keeps transmitting.
	var reloads bool
	for _, step := range brief.Recipe {
		if strings.Contains(step, "wifi reload") {
			reloads = true
		}
	}
	if !reloads {
		t.Errorf("the recipe never reloads wifi, so it would not take the BSS "+
			"off the air: %v", brief.Recipe)
	}
}

// A section that is not a plain access point must get no disable instructions
// at all. It may be the only way the device reaches the network, and the
// controller cannot tell from here.
func TestTheBriefRefusesToAdviseDisablingANonAP(t *testing.T) {
	for _, mode := range []string{"sta", "mesh"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			h.setup()
			dev := h.seedDevice("ap-repeater", true, nil)

			h.fleet.mu.Lock()
			h.fleet.aps = map[int64][]collector.AP{dev.ID: {
				{Iface: "phy0-sta0", SSID: "upstream-wisp", BSSID: "aa:bb:cc:00:00:07"},
			}}
			h.fleet.sections = map[int64]map[string]string{dev.ID: {"phy0-sta0": "wwan"}}
			h.fleet.modes = map[int64]map[string]string{dev.ID: {"phy0-sta0": mode}}
			h.fleet.mu.Unlock()

			w := h.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", dev.ID), nil)
			if w.Code != http.StatusOK {
				t.Fatalf("device detail: %d", w.Code)
			}
			var detail deviceDetail
			if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
				t.Fatal(err)
			}
			for _, b := range detail.Broadcasting {
				if b.Brief == nil {
					continue
				}
				if b.Brief.SafeToDisable {
					t.Errorf("%s mode was marked safe to disable", mode)
				}
				if len(b.Brief.Recipe) != 0 {
					t.Errorf("instructions offered for a %s interface, which "+
						"may be the device's only path to the network: %v",
						mode, b.Brief.Recipe)
				}
				if b.Brief.Refusal == "" {
					t.Error("refused without saying why")
				}
			}
		})
	}
}

// An unknown mode must be refused too. "No poll has read it" is not "it is an
// access point", and guessing wrong here costs someone their device.
func TestTheBriefRefusesWhenTheModeIsUnknown(t *testing.T) {
	h := newHarness(t)
	h.setup()
	dev := h.seedDevice("ap-unread", true, nil)

	h.fleet.mu.Lock()
	h.fleet.aps = map[int64][]collector.AP{dev.ID: {
		{Iface: "phy0-ap0", SSID: "mystery", BSSID: "aa:bb:cc:00:00:08"},
	}}
	h.fleet.sections = map[int64]map[string]string{dev.ID: {"phy0-ap0": "default_radio0"}}
	h.fleet.modes = nil // getWirelessDevices never answered
	h.fleet.mu.Unlock()

	w := h.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", dev.ID), nil)
	var detail deviceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	for _, b := range detail.Broadcasting {
		if b.Brief != nil && b.Brief.SafeToDisable {
			t.Error("advised disabling an interface whose mode was never read")
		}
	}
}

// The 802.11k card must be able to say what the AUTOMATIC cycle did, without
// running one.
//
// It used to render nothing until an operator pressed "Distribute now" — on a
// feature whose own description says it runs every fifteen minutes. So the only
// way to learn whether 802.11k was working was to trigger it, which is not an
// observation, and every automatic cycle that had been running left no trace
// anywhere a user looks.
func TestTheLastNeighbourCycleCanBeReadWithoutRunningOne(t *testing.T) {
	h := newHarness(t)
	h.setup()

	var ran int
	h.srv.Neighbours = func(context.Context) (*NeighbourResult, error) {
		ran++
		return &NeighbourResult{Updated: 2, Unchanged: 1}, nil
	}
	var last *NeighbourResult
	var lastAt time.Time
	var haveLast bool
	h.srv.LastNeighbours = func() (*NeighbourResult, string, time.Time, bool) {
		return last, "", lastAt, haveLast
	}

	// Before any cycle: a real answer, and NOT "nothing needed doing".
	w := h.do(http.MethodGet, "/api/v1/roaming/neighbours", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET last neighbours: %d %s", w.Code, w.Body.String())
	}
	var before struct {
		Ran    bool             `json:"ran"`
		Result *NeighbourResult `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if before.Ran {
		t.Error("reported a cycle before any had run")
	}
	if ran != 0 {
		t.Fatalf("reading the last cycle TRIGGERED %d; observing it must not "+
			"change it", ran)
	}

	// After one.
	last, lastAt, haveLast = &NeighbourResult{Updated: 2, Unchanged: 1}, time.Now(), true
	w = h.do(http.MethodGet, "/api/v1/roaming/neighbours", nil)
	var after struct {
		Ran    bool             `json:"ran"`
		At     int64            `json:"at"`
		Result *NeighbourResult `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if !after.Ran || after.Result == nil || after.Result.Updated != 2 {
		t.Errorf("the completed cycle was not reported back: %+v", after)
	}
	if after.At == 0 {
		t.Error("no timestamp, so the reader cannot tell a fresh run from a stale one")
	}
	if ran != 0 {
		t.Errorf("reading still triggered %d cycle(s)", ran)
	}
}

// "with no errors" must not be claimed from a response that cannot support it.
//
// DistributeNeighbours returns a nil error when the CYCLE ran and individual
// devices failed — their reasons land in res.Devices[].Error — so a screen
// reading only the top-level error reports a clean run for one in which half
// the fleet was unreachable.
func TestTheLastNeighbourRunCountsPerDeviceFailures(t *testing.T) {
	h := newHarness(t)
	h.setup()

	res := &NeighbourResult{
		Updated: 1, Unchanged: 1,
		Devices: []NeighbourDevice{
			{DeviceID: 1, Name: "ok-ap", Updated: 1},
			{DeviceID: 2, Name: "dead-ap", Error: "could not reach this device"},
			// A standing reason is NOT a failure and must not be counted.
			{DeviceID: 3, Name: "old-acl", Skipped: "its ACL predates neighbour reports"},
		},
	}
	h.srv.LastNeighbours = func() (*NeighbourResult, string, time.Time, bool) {
		return res, "", time.Now(), true
	}

	w := h.do(http.MethodGet, "/api/v1/roaming/neighbours", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Ran           bool `json:"ran"`
		DevicesFailed int  `json:"devices_failed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Ran {
		t.Fatal("a completed cycle reported as not run")
	}
	if got.DevicesFailed != 1 {
		t.Errorf("devices_failed=%d, want 1: one device errored and one was "+
			"skipped for a standing reason, which is not a failure",
			got.DevicesFailed)
	}
}
