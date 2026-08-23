package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

const (
	shutdownNoWrite          = "server is shutting down; nothing was written"
	restoreInProgressNoWrite = "controller restore is in progress; nothing was written"
)

var (
	errOperationAdmissionClosed    = errors.New("api: operation admission closed")
	errOperationAdmissionExclusive = errors.New("api: restore-exclusive operation in progress")
	errOperationAdmissionBusy      = errors.New("api: operations are active")
	errOperationRouterSuppressed   = errors.New("api: router writes are suppressed")
	errOperationKindInvalid        = errors.New("api: invalid operation kind")
)

type operationKind uint8

const (
	operationApply operationKind = iota
	operationAdopt
	operationUnadopt
	operationRFScan
	operationSpeedTest
	operationDiagnostics
	operationBackup
	operationRestorePrepare
	operationCapability
	operationNeighbourReconcile
	operationKindCount
)

var operationKindNames = [...]string{
	operationApply:              "apply",
	operationAdopt:              "adopt",
	operationUnadopt:            "unadopt",
	operationRFScan:             "rf_scan",
	operationSpeedTest:          "speed_test",
	operationDiagnostics:        "diagnostics",
	operationBackup:             "backup",
	operationRestorePrepare:     "restore_prepare",
	operationCapability:         "capability",
	operationNeighbourReconcile: "neighbour_reconcile",
}

const restoreOperationName = "restore"

// operationGate keeps router/controller jobs out of a restore-exclusive
// interval. Admission and release are atomic; close only fences new work and
// never waits for holders, so shutdown cannot deadlock on an admitted handler.
type operationGate struct {
	mu         sync.Mutex
	closed     bool
	exclusive  bool
	suppressed bool
	active     [operationKindCount]uint64
	idle       chan struct{}
}

func (g *operationGate) begin(kind operationKind) (func(), error) {
	if kind >= operationKindCount {
		return nil, errOperationKindInvalid
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, errOperationAdmissionClosed
	}
	if g.exclusive {
		g.mu.Unlock()
		return nil, errOperationAdmissionExclusive
	}
	if g.suppressed && operationWritesRouter(kind) {
		g.mu.Unlock()
		return nil, errOperationRouterSuppressed
	}
	g.active[kind]++
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active[kind]--
			g.signalIdleLocked()
			g.mu.Unlock()
		})
	}, nil
}

func operationWritesRouter(kind operationKind) bool {
	switch kind {
	case operationApply, operationAdopt, operationUnadopt, operationRFScan,
		operationCapability, operationNeighbourReconcile:
		return true
	default:
		return false
	}
}

// upgrade atomically replaces one ordinary lease with the restore-exclusive
// lease. On failure the ordinary lease remains owned by the caller.
func (g *operationGate) upgrade(kind operationKind) (func(), []string, error) {
	if kind >= operationKindCount {
		return nil, nil, errOperationKindInvalid
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, nil, errOperationAdmissionClosed
	}
	if g.exclusive {
		g.mu.Unlock()
		return nil, []string{restoreOperationName}, errOperationAdmissionExclusive
	}
	if g.suppressed {
		g.mu.Unlock()
		return nil, nil, errOperationRouterSuppressed
	}
	if g.active[kind] == 0 {
		g.mu.Unlock()
		return nil, nil, errOperationKindInvalid
	}
	g.active[kind]--
	conflicts := g.conflictsLocked()
	if len(conflicts) != 0 {
		g.active[kind]++
		g.mu.Unlock()
		return nil, conflicts, errOperationAdmissionBusy
	}
	g.exclusive = true
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.exclusive = false
			g.signalIdleLocked()
			g.mu.Unlock()
		})
	}, nil, nil
}

func (g *operationGate) setSuppression(active bool) {
	g.mu.Lock()
	g.suppressed = active
	g.mu.Unlock()
}

func (g *operationGate) beginExclusive() (func(), []string, error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, nil, errOperationAdmissionClosed
	}
	if g.exclusive {
		g.mu.Unlock()
		return nil, []string{restoreOperationName}, errOperationAdmissionExclusive
	}
	conflicts := g.conflictsLocked()
	if len(conflicts) != 0 {
		g.mu.Unlock()
		return nil, conflicts, errOperationAdmissionBusy
	}
	g.exclusive = true
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.exclusive = false
			g.signalIdleLocked()
			g.mu.Unlock()
		})
	}, nil, nil
}

func (g *operationGate) conflictsLocked() []string {
	conflicts := make([]string, 0, operationKindCount)
	for kind, count := range g.active {
		if count != 0 {
			conflicts = append(conflicts, operationKindNames[kind])
		}
	}
	return conflicts
}

func (g *operationGate) conflicts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.exclusive {
		return []string{restoreOperationName}
	}
	return g.conflictsLocked()
}

func (g *operationGate) signalIdleLocked() {
	if !g.exclusive && !g.hasActiveLocked() && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

func (g *operationGate) hasActiveLocked() bool {
	for _, count := range g.active {
		if count != 0 {
			return true
		}
	}
	return false
}

func (g *operationGate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// wait is a drain primitive: callers fence new work with close before waiting.
func (g *operationGate) wait(d time.Duration) bool {
	g.mu.Lock()
	if !g.exclusive && !g.hasActiveLocked() {
		g.mu.Unlock()
		return true
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	idle := g.idle
	g.mu.Unlock()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

// requestGate keeps accepted API work alive until its handler, including any
// detached durable receipt write, has returned. Closing it is the first daemon
// shutdown step, so no handler can appear after the database drain observes
// zero.
type requestGate struct {
	mu       sync.Mutex
	closed   bool
	active   int
	idle     chan struct{}
	stopping chan struct{}
}

func newRequestGate() *requestGate {
	return &requestGate{stopping: make(chan struct{})}
}

func (g *requestGate) begin() (func(), bool) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, false
	}
	g.active++
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			if g.active == 0 && g.idle != nil {
				close(g.idle)
				g.idle = nil
			}
			g.mu.Unlock()
		})
	}, true
}

func (g *requestGate) close() {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		close(g.stopping)
	}
	g.mu.Unlock()
}

func (g *requestGate) wait(d time.Duration) bool {
	g.mu.Lock()
	if g.active == 0 {
		g.mu.Unlock()
		return true
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	idle := g.idle
	g.mu.Unlock()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

func (g *requestGate) inFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

// siteMutex is a context-aware mutex. A closed request gate wakes queued site
// mutations so they can return a no-write result instead of starting during
// shutdown. The post-acquire check closes the select race where both the lock
// and shutdown become ready together.
type siteMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *siteMutex) init() {
	m.once.Do(func() {
		m.token = make(chan struct{}, 1)
		m.token <- struct{}{}
	})
}

func (m *siteMutex) Lock() {
	m.init()
	<-m.token
}

func (m *siteMutex) LockContext(ctx context.Context, stopping <-chan struct{}) bool {
	m.init()
	select {
	case <-ctx.Done():
		return false
	case <-stopping:
		return false
	case <-m.token:
	}
	select {
	case <-stopping:
		m.Unlock()
		return false
	default:
		return true
	}
}

func (m *siteMutex) TryLock() bool {
	m.init()
	select {
	case <-m.token:
		return true
	default:
		return false
	}
}

func (m *siteMutex) Unlock() {
	m.init()
	select {
	case m.token <- struct{}{}:
	default:
		panic("api: unlock of unlocked site mutex")
	}
}

func (s *Server) admitRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done, ok := s.requests.begin()
		if !ok {
			writeErr(w, http.StatusServiceUnavailable, shutdownNoWrite)
			return
		}
		defer done()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) lockSiteMutation(w http.ResponseWriter, r *http.Request) bool {
	if s.siteMu.LockContext(r.Context(), s.requests.stopping) {
		return true
	}
	writeErr(w, http.StatusServiceUnavailable, shutdownNoWrite)
	return false
}

func (s *Server) beginOperation(w http.ResponseWriter, kind operationKind) (func(), bool) {
	if s == nil || s.operations == nil {
		writeErr(w, http.StatusServiceUnavailable, shutdownNoWrite)
		return nil, false
	}
	release, err := s.operations.begin(kind)
	switch {
	case err == nil:
		return release, true
	case errors.Is(err, errOperationAdmissionExclusive):
		writeCodedErr(w, http.StatusServiceUnavailable, "restore_in_progress",
			restoreInProgressNoWrite)
	case errors.Is(err, errOperationAdmissionClosed):
		writeErr(w, http.StatusServiceUnavailable, shutdownNoWrite)
	case errors.Is(err, errOperationRouterSuppressed):
		writeCodedErr(w, http.StatusLocked, "router_writes_suppressed",
			"router writes are suppressed pending owner review; nothing was written")
	default:
		writeErr(w, http.StatusInternalServerError, "operation admission failed; nothing was written")
	}
	return nil, false
}

func (s *Server) upgradeRestorePrepareToExclusive() (func(), []string, error) {
	if s == nil || s.operations == nil {
		return nil, nil, errOperationAdmissionClosed
	}
	return s.operations.upgrade(operationRestorePrepare)
}

// BeginCapabilityOperation admits daemon-owned automatic reprobes into the
// same capability lease as their HTTP-triggered counterparts.
func (s *Server) BeginCapabilityOperation() (func(), bool) {
	if s == nil || s.operations == nil {
		return nil, false
	}
	release, err := s.operations.begin(operationCapability)
	return release, err == nil
}

// BeginNeighbourReconcileOperation admits daemon-owned 802.11k reconciles.
// The lease covers the complete stored-state read and router write cycle.
func (s *Server) BeginNeighbourReconcileOperation() (func(), bool) {
	if s == nil || s.operations == nil {
		return nil, false
	}
	release, err := s.operations.begin(operationNeighbourReconcile)
	return release, err == nil
}

// BeginRestoreExclusiveOperation reserves the operation gate for a restore.
// It does not mutate controller state; the future restore workflow owns the
// returned lease until its exclusive work has finished. This gate covers
// router writes and long-lived jobs, not every ordinary database mutation.
// Restore confirmation must therefore only validate/stage its artifact, fence
// request admission, write a durable restart marker, and return 202. Shutdown
// drains already-admitted requests; only startup, with the database closed,
// may take the safety backup and replace it.
func (s *Server) BeginRestoreExclusiveOperation() (func(), []string, error) {
	if s == nil || s.operations == nil {
		return nil, nil, errOperationAdmissionClosed
	}
	return s.operations.beginExclusive()
}

// CloseAdmission prevents new API work and wakes site mutations queued behind
// an Apply. It is idempotent so error and normal shutdown paths can share it.
func (s *Server) CloseAdmission() {
	if s == nil {
		return
	}
	if s.operations != nil {
		s.operations.close()
	}
	if s.requests != nil {
		s.requests.close()
	}
}

// WaitForOperations drains leases after CloseAdmission has fenced new work.
func (s *Server) WaitForOperations(timeout time.Duration) bool {
	return s == nil || s.operations == nil || s.operations.wait(timeout)
}

// ActiveOperations reports unique, fixed operation names for shutdown errors.
func (s *Server) ActiveOperations() []string {
	if s == nil || s.operations == nil {
		return nil
	}
	return s.operations.conflicts()
}

// WaitForDrain waits for every request accepted before CloseAdmission to leave
// its handler. Apply handlers remain counted through terminal receipt storage.
func (s *Server) WaitForDrain(timeout time.Duration) bool {
	return s == nil || s.requests == nil || s.requests.wait(timeout)
}

// ActiveRequests reports accepted API handlers that have not returned.
func (s *Server) ActiveRequests() int {
	if s == nil || s.requests == nil {
		return 0
	}
	return s.requests.inFlight()
}
