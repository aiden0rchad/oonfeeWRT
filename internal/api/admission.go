package api

import (
	"context"
	"net/http"
	"sync"
	"time"
)

const shutdownNoWrite = "server is shutting down; nothing was written"

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

// CloseAdmission prevents new API work and wakes site mutations queued behind
// an Apply. It is idempotent so error and normal shutdown paths can share it.
func (s *Server) CloseAdmission() {
	if s != nil && s.requests != nil {
		s.requests.close()
	}
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
