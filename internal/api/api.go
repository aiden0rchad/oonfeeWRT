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
	"sync"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/diagnostics"
	"github.com/aiden0rchad/oonfeewrt/internal/restoreswap"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/speedtest"
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
	// SpeedTests runs bounded HTTP tests from this controller process. It has no
	// Fleet reference and therefore cannot make a router management call.
	SpeedTests *speedtest.Manager
	// Diagnostics packages bounded stored evidence only. The directory and log
	// reader are supplied by the daemon; neither grants router access.
	DiagnosticsDir string
	// BackupsDir holds short-lived encrypted controller exports and their
	// private online-snapshot staging files. It never grants router access.
	BackupsDir string
	// RestoresDir holds encrypted uploads and disposable preview state only.
	// Preview never replaces the live database or contacts a router.
	RestoresDir string
	// RestoreOwnerInstanceID binds a pending restore intent to this opened
	// controller cycle. RequestRestart is signalled only after a successful
	// confirmation response has been written and flushed.
	RestoreOwnerInstanceID string
	RequestRestart         func()
	// RouterWriteSuppression is loaded by the daemon before Routes is built.
	// ResumeRouterWrites must durably remove that fence before returning nil.
	RouterWriteSuppression restoreswap.Suppression
	ResumeRouterWrites     func(context.Context, string) error
	RouterWritesResumed    func()
	ControllerVersion      string
	ControllerStartedAt    time.Time
	ControllerLogTail      func(int) ([]byte, []string, error)
	Hub                    *Hub
	Log                    *slog.Logger

	// Retrack re-registers a device with the collector after its polling
	// settings change, so an interval override takes effect without a restart.
	// Optional; without it the change lands in the database and applies on the
	// next start, which is worse but not wrong.
	Retrack func(deviceID int64)

	// Now is injectable for tests.
	Now func() time.Time
	// afterLoginPasswordVerified coordinates security-race tests. Production
	// servers leave it nil.
	afterLoginPasswordVerified func()

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
	siteMu                     siteMutex
	requests                   *requestGate
	operations                 *operationGate
	instanceID                 string
	diagnostics                *diagnosticManager
	diagnosticGenerate         func(context.Context, string, diagnostics.Input) (diagnostics.Result, error)
	afterDiagnosticGenerated   func(string)
	backups                    *backupManager
	backupCreate               backupCreateFunc
	afterBackupSnapshot        func(string)
	afterBackupCreated         func(string)
	beforeBackupDownloadOpen   func(string)
	restores                   *restoreManager
	restoreInspect             restoreInspectFunc
	restorePrepare             restorePrepareFunc
	restoreCreateIntent        restoreCreateIntentFunc
	restoreAuditWrite          func(context.Context, store.Event) error
	restoreConfirmTimeout      time.Duration
	beforeRestoreUploadPublish func(string, string)
	afterRestoreUploadPublish  func()
	beforeRestorePreviewCheck  func(string)
	afterRestoreInspected      func(string)
	afterRestorePrepared       func()
	suppressionMu              sync.Mutex
}

// New builds a Server.
func New(db *store.DB, fleet Fleet, enroll Enroller, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	instanceID, err := randomToken()
	if err != nil {
		panic("api: cannot generate controller instance identifier: " + err.Error())
	}
	srv := &Server{
		Store: db, Fleet: fleet, Enroll: enroll, Log: log, Now: time.Now,
		ControllerVersion: "dev", ControllerStartedAt: time.Now(),
		sessions:   newSessions(),
		throttle:   newThrottle(),
		hashing:    make(chan struct{}, hashSlots),
		requests:   newRequestGate(),
		operations: &operationGate{},
		instanceID: instanceID,
	}
	srv.Hub = NewHub(fleet, log)
	runner, err := speedtest.NewHTTPRunner(speedtest.DefaultHTTPConfig())
	if err != nil {
		panic("api: invalid built-in speed-test configuration: " + err.Error())
	}
	srv.SpeedTests = speedtest.New(db, runner, func(ctx context.Context, event, severity string, job speedtest.Job) error {
		return db.LogEvent(ctx, store.Event{Category: "audit", Severity: severity,
			Event: event, Detail: map[string]any{
				"job_id": job.ID, "username": job.ActorUsername,
				"provider": job.Provider, "method": job.Method,
				"provenance": job.Provenance, "estimated_bytes": job.EstimatedBytes,
				"plan_id": job.PlanID, "state": job.State,
			}})
	}, log)
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
	if s.diagnostics != nil {
		s.diagnostics.sweep(now)
	}
	if s.backups != nil {
		s.backups.sweep(now)
	}
	if s.restores != nil {
		s.restores.sweep(now)
	}
}

type protectedRoute struct {
	pattern string
	role    store.AccountRole
	handler http.HandlerFunc
}

// protectedRoutes is the complete authorization policy for the authenticated
// API. Dynamic event authorization is tightened further after the event scope
// or category is known.
func (s *Server) protectedRoutes() []protectedRoute {
	return []protectedRoute{
		{"POST /api/v1/logout", store.RoleViewer, s.handleLogout},
		{"GET /api/v1/session", store.RoleViewer, s.handleSession},
		{"POST /api/v1/session/password", store.RoleViewer, s.handleChangePassword},
		{"POST /api/v1/session/reauth", store.RoleViewer, s.handleReauth},
		{"GET /api/v1/account", store.RoleViewer, s.handleAccount},
		{"GET /api/v1/account/sessions", store.RoleViewer, s.handleAccountSessions},
		{"DELETE /api/v1/account/sessions/{session_id}", store.RoleViewer, s.handleRevokeOwnSession},
		{"GET /api/v1/accounts", store.RoleOwner, s.handleAccounts},
		{"GET /api/v1/accounts/{id}/sessions", store.RoleOwner, s.handleAdminSessions},

		{"GET /api/v1/devices", store.RoleViewer, s.handleDevices},
		{"GET /api/v1/devices/{id}", store.RoleViewer, s.handleDevice},
		{"GET /api/v1/devices/{id}/series", store.RoleViewer, s.handleDeviceSeries},
		{"GET /api/v1/devices/{id}/overhead", store.RoleViewer, s.handleOverhead},
		{"POST /api/v1/devices/{id}/focus", store.RoleViewer, s.handleFocus},
		{"POST /api/v1/devices/{id}/poll-interval", store.RoleAdmin, s.handlePollInterval},
		{"POST /api/v1/devices/{id}/name", store.RoleAdmin, s.handleRename},
		{"POST /api/v1/devices/adopt", store.RoleAdmin, s.handleAdopt},
		{"POST /api/v1/devices/inspect", store.RoleAdmin, s.handleInspect},
		{"POST /api/v1/devices/{id}/unadopt", store.RoleAdmin, s.handleUnadopt},
		{"POST /api/v1/devices/{id}/refresh-acl", store.RoleAdmin, s.handleRefreshACL},
		{"GET /api/v1/devices/{id}/capabilities/lldp", store.RoleAdmin, s.handleLLDPStatus},
		{"POST /api/v1/devices/{id}/capabilities/lldp", store.RoleAdmin, s.handleLLDPCapability},
		{"POST /api/v1/devices/{id}/reprobe", store.RoleAdmin, s.handleReprobe},
		{"POST /api/v1/devices/{id}/foreign/{section}/note", store.RoleAdmin, s.handleForeignNote},
		{"POST /api/v1/roaming/neighbours", store.RoleAdmin, s.handleNeighbours},
		{"GET /api/v1/roaming/neighbours", store.RoleViewer, s.handleLastNeighbours},
		{"GET /api/v1/site/mesh-health", store.RoleViewer, s.handleMeshHealth},
		{"POST /api/v1/site/verify-on-air", store.RoleOperator, s.handleOnAir},

		{"GET /api/v1/site", store.RoleViewer, s.handleSite},
		{"POST /api/v1/site/name", store.RoleAdmin, s.handleSiteName},
		{"GET /api/v1/site/wlans/{id}", store.RoleViewer, s.handleGetWLAN},
		{"POST /api/v1/site/wlans", store.RoleAdmin, s.handleSaveWLAN},
		{"POST /api/v1/site/wlans/{id}", store.RoleAdmin, s.handleSaveWLAN},
		{"DELETE /api/v1/site/wlans/{id}", store.RoleAdmin, s.handleDeleteWLAN},
		{"GET /api/v1/site/meshes/{id}", store.RoleViewer, s.handleGetMesh},
		{"POST /api/v1/site/meshes", store.RoleAdmin, s.handleSaveMesh},
		{"POST /api/v1/site/meshes/{id}", store.RoleAdmin, s.handleSaveMesh},
		{"DELETE /api/v1/site/meshes/{id}", store.RoleAdmin, s.handleDeleteMesh},
		{"POST /api/v1/site/uplinks", store.RoleAdmin, s.handleSaveUplink},
		{"POST /api/v1/site/uplinks/{id}", store.RoleAdmin, s.handleSaveUplink},
		{"DELETE /api/v1/site/uplinks/{id}", store.RoleAdmin, s.handleDeleteUplink},
		{"POST /api/v1/site/groups", store.RoleAdmin, s.handleSaveGroup},
		{"POST /api/v1/site/groups/{id}", store.RoleAdmin, s.handleSaveGroup},
		{"DELETE /api/v1/site/groups/{id}", store.RoleAdmin, s.handleDeleteGroup},
		{"POST /api/v1/site/networks", store.RoleAdmin, s.handleSaveNetwork},
		{"POST /api/v1/site/networks/{id}", store.RoleAdmin, s.handleSaveNetwork},
		{"DELETE /api/v1/site/networks/{id}", store.RoleAdmin, s.handleDeleteNetwork},
		{"POST /api/v1/site/zones/{name}", store.RoleAdmin, s.handleSaveZonePolicy},
		{"DELETE /api/v1/site/zones/{name}", store.RoleAdmin, s.handleDeleteZonePolicy},
		{"GET /api/v1/site/policies", store.RoleViewer, s.handlePolicies},
		{"POST /api/v1/site/policies", store.RoleAdmin, s.handleSavePolicy},
		{"POST /api/v1/site/policies/{id}", store.RoleAdmin, s.handleSavePolicy},
		{"DELETE /api/v1/site/policies/{id}", store.RoleAdmin, s.handleDeletePolicy},
		{"POST /api/v1/site/object-manager/compile", store.RoleAdmin, s.handleCompileObjects},
		{"POST /api/v1/clients/{mac}/policy", store.RoleAdmin, s.handleSaveClientPolicy},
		{"POST /api/v1/site/devices/{id}/override", store.RoleAdmin, s.handleSetOverride},
		{"GET /api/v1/site/preview", store.RoleAdmin, s.handlePreview},
		{"POST /api/v1/site/apply", store.RoleAdmin, s.handleApply},
		{"GET /api/v1/site/apply/{operation_id}", store.RoleAdmin, s.handleApplyOperationStatus},

		{"GET /api/v1/discovery", store.RoleAdmin, s.handleScanPlan},
		{"POST /api/v1/discovery/scan", store.RoleAdmin, s.handleScan},
		{"GET /api/v1/stats/{kind}", store.RoleViewer, s.handleStats},
		{"GET /api/v1/clients", store.RoleViewer, s.handleClients},
		{"GET /api/v1/clients/{mac}/observability", store.RoleViewer, s.handleClientObservability},
		{"GET /api/v1/events", store.RoleViewer, s.handleEvents},
		{"GET /api/v1/events/{id}", store.RoleViewer, s.handleEventDetail},
		{"GET /api/v1/topology", store.RoleViewer, s.handleTopology},
		{"GET /api/v1/topology/history", store.RoleViewer, s.handleTopologyHistory},
		{"GET /api/v1/radios", store.RoleViewer, s.handleRadios},
		{"POST /api/v1/devices/{id}/radios/{radio}/scan", store.RoleOperator, s.handleRadioScan},
		{"GET /api/v1/dashboard", store.RoleViewer, s.handleDashboard},
		{"GET /api/v1/speedtests", store.RoleViewer, s.handleSpeedTests},
		{"POST /api/v1/speedtests", store.RoleOperator, s.handleStartSpeedTest},
		{"GET /api/v1/speedtests/{id}", store.RoleViewer, s.handleSpeedTest},
		{"POST /api/v1/speedtests/{id}/cancel", store.RoleOperator, s.handleCancelSpeedTest},
		{"GET /api/v1/diagnostics", store.RoleAdmin, s.handleDiagnostics},
		{"POST /api/v1/diagnostics", store.RoleAdmin, s.handleStartDiagnostics},
		{"GET /api/v1/diagnostics/{id}", store.RoleAdmin, s.handleDiagnosticJob},
		{"POST /api/v1/diagnostics/{id}/cancel", store.RoleAdmin, s.handleCancelDiagnostics},
		{"GET /api/v1/diagnostics/{id}/download", store.RoleAdmin, s.handleDownloadDiagnostics},
		{"GET /api/v1/backups", store.RoleOwner, s.handleBackups},
		{"GET /api/v1/backups/{id}", store.RoleOwner, s.handleBackupJob},
		{"GET /api/v1/restores", store.RoleOwner, s.handleRestores},
		{"GET /api/v1/restores/previews/{id}", store.RoleOwner, s.handleRestorePreview},
		{"GET /api/v1/restores/suppression", store.RoleOwner, s.handleRestoreSuppression},
		{"GET /api/v1/live", store.RoleViewer, s.handleLive},
	}
}

// reauthenticatedRoutes is the complete step-up policy. Keeping it separate
// makes omission testable: every owner account mutation is registered here or
// it is not registered at all.
func (s *Server) reauthenticatedRoutes() []protectedRoute {
	return []protectedRoute{
		{"POST /api/v1/accounts", store.RoleOwner, s.handleCreateAccount},
		{"PATCH /api/v1/accounts/{id}/role", store.RoleOwner, s.handleSetAccountRole},
		{"PATCH /api/v1/accounts/{id}/enabled", store.RoleOwner, s.handleSetAccountEnabled},
		{"DELETE /api/v1/accounts/{id}", store.RoleOwner, s.handleDeleteAccount},
		{"POST /api/v1/accounts/{id}/password", store.RoleOwner, s.handleResetAccountPassword},
		{"DELETE /api/v1/accounts/{id}/sessions/{session_id}", store.RoleOwner, s.handleRevokeAdminSession},
		{"DELETE /api/v1/accounts/{id}/sessions", store.RoleOwner, s.handleRevokeAdminSessions},
		{"POST /api/v1/backups", store.RoleOwner, s.handleStartBackup},
		{"POST /api/v1/backups/{id}/cancel", store.RoleOwner, s.handleCancelBackup},
		{"GET /api/v1/backups/{id}/download", store.RoleOwner, s.handleDownloadBackup},
		{"POST /api/v1/restores/uploads", store.RoleOwner, s.handleRestoreUpload},
		{"POST /api/v1/restores/previews", store.RoleOwner, s.handleStartRestorePreview},
		{"POST /api/v1/restores/previews/{id}/cancel", store.RoleOwner, s.handleCancelRestorePreview},
		{"POST /api/v1/restores/previews/{id}/confirm", store.RoleOwner, s.handleConfirmRestore},
		{"POST /api/v1/restores/suppression/resume", store.RoleOwner, s.handleResumeRouterWrites},
	}
}

// Routes returns the API handler, to be mounted under /api/v1/.
//
// Only three routes are unauthenticated, and each is a deliberate exception:
// the setup-state probe (one bit, no data), first-run enrolment (which stops
// working the moment an account exists), and login itself.
func (s *Server) Routes() http.Handler {
	if s.operations != nil {
		s.operations.setSuppression(s.RouterWriteSuppression.Active)
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/setup", s.handleSetupState)
	// These two are unauthenticated, so the CSRF token cannot protect them —
	// there is no session to carry one yet. A same-origin gate stands in.
	// /setup especially: on a fresh controller it creates the administrator
	// account, and a cross-site POST could claim the install.
	mux.HandleFunc("POST /api/v1/setup", requireSameOrigin(s.handleSetup))
	mux.HandleFunc("POST /api/v1/login", requireSameOrigin(s.handleLogin))

	private := http.NewServeMux()
	for _, route := range s.protectedRoutes() {
		private.Handle(route.pattern, s.requireRole(route.role, route.handler))
	}
	for _, route := range s.reauthenticatedRoutes() {
		private.Handle(route.pattern, s.requireRole(route.role,
			s.requireRecentReauth(route.handler)))
	}

	mux.Handle("/api/v1/", s.requireAuth(private))
	return noStore(s.instanceID, s.admitRequests(mux))
}

// noStore keeps API responses out of caches. Everything here is either live
// fleet state or scoped to one signed-in operator, and neither should survive
// in a shared cache or a browser's back-forward store.
func noStore(instanceID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-OonfeeWRT-Instance", instanceID)
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

func writeCodedErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
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
			writeCodedErr(w, http.StatusUnsupportedMediaType, "invalid_request",
				"request body must be application/json")
			return false
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeCodedErr(w, http.StatusBadRequest, "invalid_request", "malformed request body")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeCodedErr(w, http.StatusBadRequest, "invalid_request", "malformed request body")
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
