package daemon

import (
	"context"
	"net/http"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/collector"
)

// routes builds the HTTP surface: the health endpoint the orchestrator polls,
// and the Phase 1 API under /api/v1.
func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", d.healthz)

	d.api = api.New(d.Store, fleetAdapter{d}, d, d.Log)
	// Set rather than passed to New: discovery is optional to the API — it
	// serves the fleet perfectly well without one — and growing the constructor
	// for every optional collaborator is how a constructor becomes a config
	// struct nobody reads.
	d.api.Scan = d
	d.api.Provision = d
	d.api.Reprobe = d
	// Lets a poll-interval change take effect immediately: the collector holds
	// the interval in its target, so the row alone would not move until restart.
	d.api.Retrack = func(id int64) {
		dev, err := d.Store.DeviceByID(context.Background(), id)
		if err != nil {
			d.Log.Warn("could not re-register a device after a settings change",
				"device", id, "err", err)
			return
		}
		d.Track(dev)
	}
	mux.Handle("/api/v1/", d.api.Routes())
	d.mountUI(mux)
	return mux
}

// fleetAdapter exposes the collector to the API without handing it the whole
// daemon. The API can then be tested against a stub rather than against a
// keyring, a listener and a router.
type fleetAdapter struct{ d *Daemon }

func (f fleetAdapter) Focus(deviceID int64) func() { return f.d.Focus(deviceID) }

func (f fleetAdapter) Tier(deviceID int64) (collector.Tier, bool) {
	c := f.d.collectorRef()
	if c == nil {
		return "", false
	}
	return c.Tier(deviceID)
}

func (f fleetAdapter) Quiesced(deviceID int64) bool {
	c := f.d.collectorRef()
	return c != nil && c.Quiesced(deviceID)
}

func (f fleetAdapter) LiveClients(deviceID int64) (int, bool) {
	return f.d.liveClients(deviceID)
}

func (f fleetAdapter) Degraded(deviceID int64) ([]collector.Degradation, bool) {
	c := f.d.collectorRef()
	if c == nil {
		return nil, false
	}
	return c.Degraded(deviceID)
}

func (f fleetAdapter) Overhead(deviceID int64) (collector.Overhead, bool) {
	c := f.d.collectorRef()
	if c == nil {
		return collector.Overhead{}, false
	}
	return c.Overhead(deviceID)
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
