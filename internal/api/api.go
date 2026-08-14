// Package api is the controller's HTTP surface: REST under /api/v1, session
// cookie auth, and the read-only fleet view that Phase 1 exists to deliver.
//
// Two things it deliberately does not do.
//
// It does not compute in handlers. Everything a screen shows is either a row
// the collector already wrote or a rollup the maintenance tick already
// aggregated, so a slow query is a schema problem rather than something to be
// cached around later.
//
// It does not invent numbers. Where the device could not answer — a client
// count that was denied, a noise floor the driver cannot be trusted for — the
// JSON carries null and says why in a sibling field, rather than a zero that a
// chart will draw as a fact.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// Fleet is what the API needs from the daemon. An interface rather than the
// daemon itself, so the API can be tested without a keyring, a listener and a
// device — and so the dependency runs one way.
type Fleet interface {
	// Focus raises a device to the focused poll rate for as long as a screen is
	// showing it. The returned function releases it and is safe to call twice.
	Focus(deviceID int64) func()
	// Tier reports how a device is currently polled, for the Management
	// Overhead readout.
	Tier(deviceID int64) (collector.Tier, bool)
	// Quiesced reports polling suspended for an apply.
	Quiesced(deviceID int64) bool

	// Overhead reports what the controller is costing this device.
	Overhead(deviceID int64) (collector.Overhead, bool)

	// LiveClients reports the most recent associated-station count for a
	// device, and whether it is known at all.
	//
	// From the last poll, not from the rollup table. "How many clients are
	// connected" is a question about now, and the rollups only exist after the
	// five-minute flush — asking them made a freshly started controller report
	// "unknown" for five minutes while it was polling successfully the whole
	// time. Unknown must mean we could not find out, not that we have not
	// written it down yet.
	LiveClients(deviceID int64) (int, bool)
}

// Server serves /api/v1.
type Server struct {
	Store  *store.DB
	Fleet  Fleet
	Enroll Enroller
	Scan   Scanner
	Hub    *Hub
	Log    *slog.Logger

	// Now is injectable for tests.
	Now func() time.Time

	sessions *sessions
	throttle *throttle

	// hashing bounds concurrent argon2id derivations; see hashSlots.
	hashing chan struct{}

	// dummyHash is verified against when an account does not exist, so the
	// unknown-username path costs the same as the known one.
	dummyHash string
}

// New builds a Server.
func New(db *store.DB, fleet Fleet, enroll Enroller, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	srv := &Server{
		Store: db, Fleet: fleet, Enroll: enroll, Log: log, Now: time.Now,
		sessions: newSessions(),
		throttle: newThrottle(),
		hashing:  make(chan struct{}, hashSlots),
	}
	srv.Hub = NewHub(fleet, log)
	// One derivation at startup, with the shipped parameters, so that verifying
	// an unknown username costs exactly what verifying a known one costs.
	h, err := secrets.HashPassword([]byte("oonfeewrt-timing-equaliser"), secrets.DefaultParams())
	if err != nil {
		// Only reachable if the parameters themselves are invalid, which would
		// mean no password could ever be hashed. Fail loudly rather than run
		// with a login path that leaks which accounts exist.
		panic("api: cannot derive the timing-equaliser hash: " + err.Error())
	}
	srv.dummyHash = h
	return srv
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Sweep expires idle sessions and lapsed login lockouts. The daemon calls it on
// the maintenance tick rather than running a timer per table.
func (s *Server) Sweep() {
	now := s.now()
	s.sessions.sweep(now)
	s.throttle.sweep(now)
}

// Routes returns the API handler, to be mounted under /api/v1/.
//
// Only three routes are unauthenticated, and each is a deliberate exception:
// the setup-state probe (one bit, no data), first-run enrolment (which stops
// working the moment an account exists), and login itself.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/setup", s.handleSetupState)
	// These two are unauthenticated, so the CSRF token cannot protect them —
	// there is no session to carry one yet. A same-origin gate stands in.
	// /setup especially: on a fresh controller it creates the administrator
	// account, and a cross-site POST could claim the install.
	mux.HandleFunc("POST /api/v1/setup", requireSameOrigin(s.handleSetup))
	mux.HandleFunc("POST /api/v1/login", requireSameOrigin(s.handleLogin))

	private := http.NewServeMux()
	private.HandleFunc("POST /api/v1/logout", s.handleLogout)
	private.HandleFunc("GET /api/v1/session", s.handleSession)
	private.HandleFunc("POST /api/v1/session/password", s.handleChangePassword)

	private.HandleFunc("GET /api/v1/devices", s.handleDevices)
	private.HandleFunc("GET /api/v1/devices/{id}", s.handleDevice)
	private.HandleFunc("GET /api/v1/devices/{id}/series", s.handleDeviceSeries)
	private.HandleFunc("GET /api/v1/devices/{id}/overhead", s.handleOverhead)
	private.HandleFunc("POST /api/v1/devices/{id}/focus", s.handleFocus)
	private.HandleFunc("POST /api/v1/devices/adopt", s.handleAdopt)
	private.HandleFunc("POST /api/v1/devices/{id}/unadopt", s.handleUnadopt)
	private.HandleFunc("GET /api/v1/discovery", s.handleScanPlan)
	// A POST because it makes the controller emit traffic across a subnet.
	// requireAuth enforces the CSRF token on it for that reason: a GET would be
	// reachable from any page the operator has open.
	private.HandleFunc("POST /api/v1/discovery/scan", s.handleScan)
	private.HandleFunc("GET /api/v1/stats/{kind}", s.handleStats)
	private.HandleFunc("GET /api/v1/clients", s.handleClients)
	private.HandleFunc("GET /api/v1/events", s.handleEvents)
	private.HandleFunc("GET /api/v1/dashboard", s.handleDashboard)
	// Behind requireAuth like everything else. The upgrade is a GET, so the
	// CSRF token does not apply — handleLive checks the Origin itself, which
	// is what actually stops cross-site WebSocket hijacking.
	private.HandleFunc("GET /api/v1/live", s.handleLive)

	mux.Handle("/api/v1/", s.requireAuth(private))
	return noStore(mux)
}

// noStore keeps API responses out of caches. Everything here is either live
// fleet state or scoped to one signed-in operator, and neither should survive
// in a shared cache or a browser's back-forward store.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so this cannot become an error
		// response. Logging is all that is left, and silence would be worse.
		slog.Default().Debug("api: could not write response body", "err", err)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// maxBody bounds a request body. Every endpoint here takes a small JSON object,
// and an unbounded reader on an unauthenticated route is a memory exhaustion
// primitive.
const maxBody = 64 << 10

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	// Requiring a JSON content type is complementary CSRF hardening, not
	// pedantry: an HTML form can only send urlencoded, multipart or text/plain,
	// so a cross-site form post cannot reach any handler that insists on JSON.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeErr(w, http.StatusUnsupportedMediaType,
				"request body must be application/json")
			return false
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request body")
		return false
	}
	return true
}

// pathID reads a numeric path segment.
func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid "+name)
		return 0, false
	}
	return id, true
}

// notFound distinguishes a missing row from a broken query, so a client can
// tell "this device was removed" from "the controller is unwell".
func handleStoreErr(w http.ResponseWriter, err error, what string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, what+" not found")
		return true
	}
	writeErr(w, http.StatusInternalServerError, "could not read "+what)
	return true
}

func itoa(n int) string { return strconv.Itoa(n) }

// queryInt reads an optional integer query parameter.
func queryInt(r *http.Request, name string, def, minV, maxV int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

// queryTime reads a unix-seconds query parameter.
func queryTime(r *http.Request, name string, def time.Time) time.Time {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	sec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return time.Unix(sec, 0)
}
