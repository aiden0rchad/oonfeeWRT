package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Enroller brings devices under management and takes them back out.
//
// An interface rather than the daemon, for the same reason Fleet is: the
// orchestration needs a ubus client, the keyring, the store and the collector,
// and none of those belong in an HTTP handler. This package stays the wire
// format and the validation.
type Enroller interface {
	Adopt(ctx context.Context, req AdoptRequest) (*AdoptResult, error)
	Unadopt(ctx context.Context, req UnadoptRequest) (*UnadoptResult, error)
}

// AdoptRequest is what the operator supplies to bring a device under
// management.
//
// Username and Password are the DEVICE's existing administrator credential.
// They are used for exactly one transaction and never stored — the controller
// creates its own scoped login and keeps only that. Un-adoption asks for them
// again, because a controller that could remove its own ACL file could equally
// rewrite it and grant itself a shell (ARCHITECTURE §6).
type AdoptRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	Scheme   string `json:"scheme,omitempty"` // "http" (default) or "https"
	Name     string `json:"name,omitempty"`
	Role     string `json:"role,omitempty"` // gateway|ap|switch
	Username string `json:"username"`
	Password string `json:"password"`
	// PrivateKey is an optional PEM SSH key, used in preference to the
	// password. A device with key-only SSH — which is the sensible way to run
	// one — cannot be adopted without it.
	PrivateKey string `json:"private_key,omitempty"`
}

// AdoptResult reports what adoption produced. It deliberately carries no
// credential: the one it created is sealed in the keyring, and the one the
// operator supplied is gone.
type AdoptResult struct {
	DeviceID  int64    `json:"device_id"`
	MAC       string   `json:"mac"`
	Name      string   `json:"name"`
	Model     string   `json:"model"`
	Class     string   `json:"class"`
	Firmware  string   `json:"firmware"`
	CertFP    string   `json:"cert_fp,omitempty"`
	HostKeyFP string   `json:"host_key_fp,omitempty"`
	Features  []string `json:"features"`
	// Unobservable names the capabilities the probe could not determine — a
	// refused check, not a missing feature. Surfaced because a wider ACL is the
	// only thing that would change them, and the operator is the only one who
	// can decide that.
	Unobservable []string `json:"unobservable,omitempty"`
	Quirks       []string `json:"quirks,omitempty"`
	Notes        []string `json:"notes,omitempty"`
	// Warnings are things the operator should know about the DEVICE, observed
	// while adopting it. Not controller problems and not reasons to refuse —
	// facts a controller is well placed to notice and a person is not.
	Warnings []string `json:"warnings,omitempty"`
}

// UnadoptRequest removes the controller from a device.
//
// The operator credential is optional here, and its absence is a documented
// degradation rather than an error: phase 1 (giving the user's config back)
// runs under the controller's own login, while phase 2 (removing the login and
// the ACL file) cannot. A device whose admin password is lost keeps a visible,
// listed residue instead of a silently half-removed one.
type UnadoptRequest struct {
	DeviceID   int64  `json:"-"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	// Force removes the device from the inventory even if the device could not
	// be reached at all — for hardware that is gone for good.
	Force bool `json:"force,omitempty"`
}

// UnadoptResult says exactly what was and was not removed.
type UnadoptResult struct {
	Removed          bool     `json:"removed_from_inventory"`
	RevertedSections int      `json:"reverted_sections"`
	LoginRemoved     bool     `json:"login_removed"`
	ACLRemoved       bool     `json:"acl_removed"`
	FootprintRemains bool     `json:"footprint_remains"`
	Residue          []string `json:"residue,omitempty"`
	Errors           []string `json:"errors,omitempty"`
	// NeedsOperator marks the case where phase 1 succeeded and phase 2 could
	// not run for want of the device's admin credential.
	NeedsOperator bool `json:"needs_operator_credential"`
}

// ErrOperatorRequired is returned by an Enroller when phase 2 needs the
// device's own administrator credential.
var ErrOperatorRequired = errors.New("api: the device's administrator credential is required")

func (s *Server) handleAdopt(w http.ResponseWriter, r *http.Request) {
	if s.Enroll == nil {
		writeErr(w, http.StatusServiceUnavailable, "adoption is not available")
		return
	}
	var req AdoptRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Host = strings.TrimSpace(req.Host)
	if req.Host == "" {
		writeErr(w, http.StatusBadRequest, "host is required")
		return
	}
	if req.Username == "" {
		writeErr(w, http.StatusBadRequest, "the device's administrator username is required")
		return
	}
	if req.Scheme != "" && req.Scheme != "http" && req.Scheme != "https" {
		writeErr(w, http.StatusBadRequest, "scheme must be http or https")
		return
	}
	if req.Port < 0 || req.Port > 65535 {
		writeErr(w, http.StatusBadRequest, "port is out of range")
		return
	}

	res, err := s.Enroll.Adopt(r.Context(), req)
	if err != nil {
		// The message is shown to an operator who is mid-setup and needs to
		// know which step failed, so it is passed through rather than
		// flattened. It never contains the credential: the adoption package
		// does not put it in errors, and nothing here adds it.
		s.Log.Warn("adoption failed", "host", req.Host, "err", err)
		s.logAuth(r.Context(), "device.adopt_failed", "warning", req.Username, clientAddr(r))
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// The success event is written by the Enroller, which knows the device id,
	// MAC, model and class. Logging it here too would double every adoption in
	// the audit trail. The FAILURE event above is logged here on purpose: the
	// Enroller returns early and never gets to record one.
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleUnadopt(w http.ResponseWriter, r *http.Request) {
	if s.Enroll == nil {
		writeErr(w, http.StatusServiceUnavailable, "adoption is not available")
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req UnadoptRequest
	// An empty body is legitimate: it means "phase 1 only", which reports the
	// residue and asks for the credential.
	if r.ContentLength > 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	req.DeviceID = id

	res, err := s.Enroll.Unadopt(r.Context(), req)
	if errors.Is(err, ErrOperatorRequired) {
		// Not a failure. Phase 1 ran; phase 2 needs a credential the controller
		// deliberately does not hold. 409 so a client can tell this from both
		// success and a real error.
		writeJSON(w, http.StatusConflict, res)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
