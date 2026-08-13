package daemon

import (
	"net/http"
)

// routes builds the HTTP surface.
//
// Phase 0 serves exactly one endpoint. The API and the UI arrive in Phase 1;
// what has to exist now is the thing the container orchestrator polls, because a
// daemon with no health endpoint is one a supervisor cannot tell from a hung one.
func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", d.healthz)
	return mux
}

// healthz reports liveness: unauthenticated, and it says nothing about the
// fleet.
//
// Both properties are deliberate. It is unauthenticated because a health check
// that needs a credential is one more thing that can fail for reasons unrelated
// to health, and it reveals nothing — which is why it must stay that way. It
// reports only this process, not device reachability: a controller that marks
// itself unhealthy because a router is offline gets restarted by its supervisor,
// which fixes nothing and loses the poll history that would have explained it.
func (d *Daemon) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
