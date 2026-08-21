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
	"context"
	"encoding/json"
	"errors"
	"io"
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

	// Degraded reports what the last poll of this device could not read.
	// Standing limitations rather than events — see collector.Degraded.
	Degraded(deviceID int64) ([]collector.Degradation, bool)

	// Broadcasting reports every BSS the last poll saw on a device, including
	// ones this controller does not manage.
	Broadcasting(deviceID int64) ([]collector.AP, bool)

	// IfaceSections maps a device's wireless interfaces to the UCI section that
	// created each. False means no poll has read it — never "none have one".
	IfaceSections(deviceID int64) (map[string]string, bool)

	// IfaceModes is each wireless interface's configured mode. False means no
	// poll has read it, never "they are all APs".
	IfaceModes(deviceID int64) (map[string]string, bool)

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

	// LiveStations is every BSS association the last poll saw on a device,
	// grouped by lower-case MAC. Multiple observations for one MAC are retained
	// because choosing one would invent an AP during a roam or stale driver read.
	//
	// From hostapd's get_clients, which runs at the BASELINE rate and already
	// carries every MAC and its RSSI — the collector used to keep only the
	// count. False means the read failed, never "nobody is associated".
	LiveStations(deviceID int64) (collector.LiveStationSet, bool)

	// LivePresence is the latest authoritative reachability proof per client
	// MAC. Inventory-only host hints and DHCP leases are never included.
	LivePresence(deviceID int64) (collector.ClientPresenceState, bool)
}

// Server serves /api/v1.
type Server struct {
	Store *store.DB
	// Keys produces domain-separated request bindings. Apply idempotency
	// records never persist a preview token or an unkeyed verifier for one.
	Keys   *secrets.Keeper
	Fleet  Fleet
	Enroll Enroller
	Scan   Scanner
	// Provision previews and applies the site model. Optional: the fleet view
	// works without it.
	Provision Provisioner
	// Reprobe re-runs the capability probe. Optional: without it the stored
	// record is whatever adoption found, which is the behaviour that made a
	// firmware upgrade leave a device permanently misdescribed.
	Reprobe Reprober
	// Neighbours distributes 802.11k neighbour lists across the fleet.
	// Optional: without it every AP still advertises that it answers neighbour
	// requests and still answers them with nothing, which is where this
	// project was before the endpoint existed.
	Neighbours func(context.Context) (*NeighbourResult, error)
	// LastNeighbours reports the most recent distribution without running one,
	// so the screen can show what the automatic cycle did rather than only what
	// a button press does. The bool is false when none has run since start —
	// which is a different answer from "nothing needed doing".
	LastNeighbours func() (*NeighbourResult, string, time.Time, bool)
	// MeshHealth reports what every configured backhaul is doing. Optional,
	// and free: it reads no device.
	MeshHealth func(context.Context) (*MeshHealthResult, error)
	// OnAir verifies the fleet is transmitting what it claims, by making each
	// radio scan for the others. Optional, and deliberately not on any timer:
	// a scan takes a radio off-channel.
	OnAir func(context.Context) (*OnAirResult, error)
	// RadioScan runs one acknowledged, persisted RF scan. It is never called by
	// a GET or a timer because the selected serving radio leaves its channel.
	RadioScan RadioScanner
	Hub       *Hub
	Log       *slog.Logger

	// Retrack re-registers a device with the collector after its polling
	// settings change, so an interval override takes effect without a restart.
	// Optional; without it the change lands in the database and applies on the
	// next start, which is worse but not wrong.
	Retrack func(deviceID int64)

	// Now is injectable for tests.
	Now func() time.Time

	sessions *sessions
	throttle *throttle

	// hashing bounds concurrent argon2id derivations; see hashSlots.
	hashing chan struct{}

	// dummyHash is verified against when an account does not exist, so the
	// unknown-username path costs the same as the known one.
	dummyHash string

	// siteMu covers read/merge/write HTTP mutations. Store.siteMu protects
	// persistence invariants, but a partial network handler reads before it
	// writes; two such requests could otherwise each merge against stale state
	// and silently discard the other's field.
	siteMu   siteMutex
	requests *requestGate
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
		requests: newRequestGate(),
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
	private.HandleFunc("POST /api/v1/devices/{id}/poll-interval", s.handlePollInterval)
	private.HandleFunc("POST /api/v1/devices/{id}/name", s.handleRename)
	private.HandleFunc("POST /api/v1/devices/adopt", s.handleAdopt)
	private.HandleFunc("POST /api/v1/devices/inspect", s.handleInspect)
	private.HandleFunc("POST /api/v1/devices/{id}/unadopt", s.handleUnadopt)
	private.HandleFunc("POST /api/v1/devices/{id}/refresh-acl", s.handleRefreshACL)
	private.HandleFunc("POST /api/v1/devices/{id}/reprobe", s.handleReprobe)
	// Records a DECISION about a foreign wireless section. Writes nothing to
	// any device — the controller does not touch config it did not create.
	private.HandleFunc("POST /api/v1/devices/{id}/foreign/{section}/note", s.handleForeignNote)
	private.HandleFunc("POST /api/v1/roaming/neighbours", s.handleNeighbours)
	private.HandleFunc("GET /api/v1/roaming/neighbours", s.handleLastNeighbours)
	private.HandleFunc("GET /api/v1/site/mesh-health", s.handleMeshHealth)
	private.HandleFunc("POST /api/v1/site/verify-on-air", s.handleOnAir)
	// The site model (Phase 2). Editing any of this changes nothing on any
	// device; /site/preview says what it WOULD change and /site/apply does it.
	private.HandleFunc("GET /api/v1/site", s.handleSite)
	private.HandleFunc("POST /api/v1/site/name", s.handleSiteName)
	private.HandleFunc("GET /api/v1/site/wlans/{id}", s.handleGetWLAN)
	private.HandleFunc("POST /api/v1/site/wlans", s.handleSaveWLAN)
	private.HandleFunc("POST /api/v1/site/wlans/{id}", s.handleSaveWLAN)
	private.HandleFunc("DELETE /api/v1/site/wlans/{id}", s.handleDeleteWLAN)
	private.HandleFunc("GET /api/v1/site/meshes/{id}", s.handleGetMesh)
	private.HandleFunc("POST /api/v1/site/meshes", s.handleSaveMesh)
	private.HandleFunc("POST /api/v1/site/meshes/{id}", s.handleSaveMesh)
	private.HandleFunc("DELETE /api/v1/site/meshes/{id}", s.handleDeleteMesh)
	private.HandleFunc("POST /api/v1/site/uplinks", s.handleSaveUplink)
	private.HandleFunc("POST /api/v1/site/uplinks/{id}", s.handleSaveUplink)
	private.HandleFunc("DELETE /api/v1/site/uplinks/{id}", s.handleDeleteUplink)
	private.HandleFunc("POST /api/v1/site/groups", s.handleSaveGroup)
	private.HandleFunc("POST /api/v1/site/groups/{id}", s.handleSaveGroup)
	private.HandleFunc("DELETE /api/v1/site/groups/{id}", s.handleDeleteGroup)
	private.HandleFunc("POST /api/v1/site/networks", s.handleSaveNetwork)
	private.HandleFunc("POST /api/v1/site/networks/{id}", s.handleSaveNetwork)
	private.HandleFunc("DELETE /api/v1/site/networks/{id}", s.handleDeleteNetwork)
	private.HandleFunc("POST /api/v1/site/zones/{name}", s.handleSaveZonePolicy)
	private.HandleFunc("DELETE /api/v1/site/zones/{name}", s.handleDeleteZonePolicy)
	private.HandleFunc("GET /api/v1/site/policies", s.handlePolicies)
	private.HandleFunc("POST /api/v1/site/policies", s.handleSavePolicy)
	private.HandleFunc("POST /api/v1/site/policies/{id}", s.handleSavePolicy)
	private.HandleFunc("DELETE /api/v1/site/policies/{id}", s.handleDeletePolicy)
	private.HandleFunc("POST /api/v1/site/object-manager/compile", s.handleCompileObjects)
	private.HandleFunc("POST /api/v1/clients/{mac}/policy", s.handleSaveClientPolicy)
	private.HandleFunc("POST /api/v1/site/devices/{id}/override", s.handleSetOverride)
	private.HandleFunc("GET /api/v1/site/preview", s.handlePreview)
	private.HandleFunc("POST /api/v1/site/apply", s.handleApply)
	private.HandleFunc("GET /api/v1/site/apply/{operation_id}", s.handleApplyOperationStatus)

	private.HandleFunc("GET /api/v1/discovery", s.handleScanPlan)
	// A POST because it makes the controller emit traffic across a subnet.
	// requireAuth enforces the CSRF token on it for that reason: a GET would be
	// reachable from any page the operator has open.
	private.HandleFunc("POST /api/v1/discovery/scan", s.handleScan)
	private.HandleFunc("GET /api/v1/stats/{kind}", s.handleStats)
	private.HandleFunc("GET /api/v1/clients", s.handleClients)
	private.HandleFunc("GET /api/v1/clients/{mac}/observability", s.handleClientObservability)
	private.HandleFunc("GET /api/v1/events", s.handleEvents)
	private.HandleFunc("GET /api/v1/events/{id}", s.handleEventDetail)
	private.HandleFunc("GET /api/v1/topology", s.handleTopology)
	private.HandleFunc("GET /api/v1/topology/history", s.handleTopologyHistory)
	private.HandleFunc("GET /api/v1/radios", s.handleRadios)
	private.HandleFunc("POST /api/v1/devices/{id}/radios/{radio}/scan", s.handleRadioScan)
	private.HandleFunc("GET /api/v1/dashboard", s.handleDashboard)
	// Behind requireAuth like everything else. The upgrade is a GET, so the
	// CSRF token does not apply — handleLive checks the Origin itself, which
	// is what actually stops cross-site WebSocket hijacking.
	private.HandleFunc("GET /api/v1/live", s.handleLive)

	mux.Handle("/api/v1/", s.requireAuth(private))
	return noStore(s.admitRequests(mux))
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
	if err := dec.Decode(&struct{}{}); err != io.EOF {
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
