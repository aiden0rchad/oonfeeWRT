package api

import (
	"context"
	"net/http"

	"github.com/aiden0rchad/oonfeewrt/internal/discovery"
)

// Scanner looks for devices that are not yet managed.
//
// Separate from Enroller because the two have opposite trust properties.
// Adoption acts on one address an operator named and holds their credential
// while it does; a scan touches every address in a subnet and holds nothing.
// Keeping them apart makes it hard to accidentally give the sweep a credential.
type Scanner interface {
	// Plan reports what a scan would cover without performing one.
	Plan(ctx context.Context) (*ScanPlan, error)
	// Scan sweeps and returns candidates, annotated with what the controller
	// already knows about them.
	Scan(ctx context.Context, req ScanRequest) (*ScanResult, error)
}

// ScanPlan is what a scan would do, so the UI can say so before doing it.
type ScanPlan struct {
	Networks []string `json:"networks"`
	// Hosts is how many addresses would be probed. Shown because a scan is an
	// unsolicited burst of traffic on the operator's own network and they are
	// entitled to know its size in advance.
	Hosts int `json:"hosts"`
	// Skipped is why a network is not in the list. Load-bearing: without it, a
	// controller that declined to look at the operator's subnet reports "no
	// devices found", which reads as a fact about their network rather than
	// about the controller.
	Skipped []string `json:"skipped,omitempty"`
}

// ScanRequest optionally narrows or widens what a scan covers.
type ScanRequest struct {
	// Networks in CIDR form. Empty means the host's own attached networks.
	Networks []string `json:"networks,omitempty"`
	// HTTPS additionally probes 443. Off by default because it doubles the
	// sweep for a case that is rare on stock firmware.
	HTTPS bool `json:"https,omitempty"`
}

// DiscoveredDevice is a candidate, plus whatever the inventory already says
// about that address.
type DiscoveredDevice struct {
	discovery.Candidate
	// KnownDeviceID and KnownName are set when an adopted device currently has
	// this address.
	//
	// Matched on address, which is a hint and not an identity — a device is
	// identified by MAC, and the MAC cannot be read before authenticating
	// (stock rpcd refuses system.board to an unauthenticated caller; measured).
	// So a device that changed address appears here as a new candidate, and
	// adopting it will be refused with "already adopted" once its MAC is read.
	// That is the right order: the check that matters happens where the truth
	// is available.
	KnownDeviceID int64  `json:"known_device_id,omitempty"`
	KnownName     string `json:"known_name,omitempty"`
}

// ScanResult is one sweep.
type ScanResult struct {
	Found []DiscoveredDevice `json:"found"`
	// Swept and Answered make an empty Found legible: "probed 508 addresses, 12
	// answered, none of them published a ubus endpoint" is an answer. An empty
	// list on its own is indistinguishable from a scanner that did nothing.
	Swept    int      `json:"swept"`
	Answered int      `json:"answered"`
	Networks []string `json:"networks"`
	Skipped  []string `json:"skipped,omitempty"`
	// Failures are networks that were attempted but could not be tested. This
	// is additive so existing clients keep their summary fields; clients that
	// understand it must not describe an empty Found as proof the network is
	// empty when a route failure is present.
	Failures  []discovery.NetworkFailure `json:"failures,omitempty"`
	ElapsedMS int64                      `json:"elapsed_ms"`
}

// maxScanNetworks bounds an operator-supplied list. Each network is separately
// size-checked; this stops a thousand tiny ones adding up to the same thing.
const maxScanNetworks = 8

func (s *Server) handleScanPlan(w http.ResponseWriter, r *http.Request) {
	if s.Scan == nil {
		writeErr(w, http.StatusServiceUnavailable, "discovery is not available")
		return
	}
	plan, err := s.Scan.Plan(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if s.Scan == nil {
		writeErr(w, http.StatusServiceUnavailable, "discovery is not available")
		return
	}
	var req ScanRequest
	if r.ContentLength > 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	if len(req.Networks) > maxScanNetworks {
		writeErr(w, http.StatusBadRequest,
			"at most "+itoa(maxScanNetworks)+" networks can be scanned at once")
		return
	}
	// Validate before sweeping so a typo comes back immediately rather than
	// after the scan has probed everything else in the list.
	if _, _, err := discovery.ParseNetworks(req.Networks); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.Scan.Scan(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
