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
	"net/http"
	"strconv"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
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
}

// Server serves /api/v1.
type Server struct {
	Store *store.DB
	Fleet Fleet
	Log   *slog.Logger

	// Now is injectable for tests.
	Now func() time.Time

	sessions *sessions
	throttle *throttle
}

// New builds a Server.
func New(db *store.DB, fleet Fleet, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		Store: db, Fleet: fleet, Log: log, Now: time.Now,
		sessions: newSessions(),
		throttle: newThrottle(),
	}
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
	mux.HandleFunc("POST /api/v1/setup", s.handleSetup)
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)

	private := http.NewServeMux()
	private.HandleFunc("POST /api/v1/logout", s.handleLogout)
	private.HandleFunc("GET /api/v1/session", s.handleSession)
	private.HandleFunc("POST /api/v1/session/password", s.handleChangePassword)

	private.HandleFunc("GET /api/v1/devices", s.handleDevices)
	private.HandleFunc("GET /api/v1/devices/{id}", s.handleDevice)
	private.HandleFunc("GET /api/v1/devices/{id}/series", s.handleDeviceSeries)
	private.HandleFunc("POST /api/v1/devices/{id}/focus", s.handleFocus)
	private.HandleFunc("GET /api/v1/stats/{kind}", s.handleStats)
	private.HandleFunc("GET /api/v1/events", s.handleEvents)
	private.HandleFunc("GET /api/v1/dashboard", s.handleDashboard)

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
