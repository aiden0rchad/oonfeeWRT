package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/discovery"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// scanTimeout bounds a whole sweep. The worst legitimate case is MaxHosts
// addresses at the default concurrency and dial timeout — 4096 / 128 x 1.2 s =
// 39 s of dead addresses — plus the HTTP exchange with everything that answers.
// Generous, and bounded: an operator who navigates away must not leave a sweep
// running against their network indefinitely.
const scanTimeout = 120 * time.Second

// probeRequestBody is what one probe puts on the wire, for overhead accounting.
var probeRequestBody = discovery.RequestBody()

// Plan reports what a scan would cover.
func (d *Daemon) Plan(context.Context) (*api.ScanPlan, error) {
	nets, skipped, err := discovery.LocalNetworks()
	if err != nil {
		return nil, err
	}
	return &api.ScanPlan{
		Networks: discovery.NetworkStrings(nets),
		Hosts:    discovery.HostCount(nets),
		Skipped:  skipped,
	}, nil
}

// Scan sweeps for unmanaged devices and annotates what it finds with the
// inventory.
//
// Discovery is on demand only. There is no periodic rescan and no background
// timer, deliberately: a controller that sweeps the operator's subnet on a
// schedule is generating unsolicited traffic against hosts nobody asked it to
// touch, forever, and the value — noticing a device that appeared while nobody
// was looking — does not come close to justifying that. Adoption is a thing a
// person does; the scan can be a thing a person asks for.
func (d *Daemon) Scan(ctx context.Context, req api.ScanRequest) (*api.ScanResult, error) {
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()

	res, err := discovery.Sweep(ctx, discovery.Options{
		Networks: req.Networks,
		HTTPS:    req.HTTPS,
	})
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	// Index the inventory by address once rather than querying per candidate.
	known := map[string]*store.Device{}
	if devs, err := d.Store.Devices(ctx); err == nil {
		for _, dev := range devs {
			known[dev.Host] = dev
		}
	} else {
		// The sweep result is still worth returning; it just cannot say which
		// candidates are already managed. Saying nothing would be worse than
		// saying less, so log and carry on.
		d.Log.Warn("scan could not read the inventory, so already-adopted "+
			"devices will appear as new candidates", "err", err)
	}

	out := annotate(res, known)
	for _, f := range out.Found {
		if f.KnownDeviceID == 0 {
			continue
		}
		// The probe cost this managed device one request, made outside the poll
		// loop with a different HTTP client. Attribute it, so the Management
		// Overhead readout is not quietly understating what the controller does
		// to a device it manages.
		if col := d.collectorRef(); col != nil {
			col.NoteExternalRequest(f.KnownDeviceID, int64(len(probeRequestBody)))
		}
	}

	d.Log.Info("discovery scan", "swept", res.Swept, "answered", res.Answered,
		"found", len(res.Found), "network_failures", len(res.Failures), "ms", res.ElapsedMS)
	return out, nil
}

// annotate joins a sweep to the inventory.
//
// Separate from Scan so it can be tested without binding port 80: the sweep
// probes the standard port, and a test that cannot put a fake device there
// would otherwise be asserting the annotation against an empty result — which
// passes while proving nothing.
func annotate(res *discovery.Result, known map[string]*store.Device) *api.ScanResult {
	out := &api.ScanResult{
		Swept:     res.Swept,
		Answered:  res.Answered,
		Networks:  res.Networks,
		Skipped:   res.Skipped,
		Failures:  res.Failures,
		ElapsedMS: res.ElapsedMS,
		Found:     make([]api.DiscoveredDevice, 0, len(res.Found)),
	}
	for _, c := range res.Found {
		dd := api.DiscoveredDevice{Candidate: c}
		// Already-adopted devices are listed, not filtered out. Hiding them
		// turns "my router is missing from the scan" into a support question
		// with two indistinguishable causes — it was not found, or it was found
		// and suppressed.
		if dev, ok := known[c.Host]; ok && dev.Adopted() {
			dd.KnownDeviceID = dev.ID
			dd.KnownName = dev.Name
		}
		out.Found = append(out.Found, dd)
	}
	return out
}
