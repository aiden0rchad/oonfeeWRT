package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// Enroller brings devices under management and takes them back out.
//
// An interface rather than the daemon, for the same reason Fleet is: the
// orchestration needs a ubus client, the keyring, the store and the collector,
// and none of those belong in an HTTP handler. This package stays the wire
// format and the validation.
type Enroller interface {
	Inspect(ctx context.Context, req InspectRequest) (*InspectResult, error)
	Adopt(ctx context.Context, req AdoptRequest) (*AdoptResult, error)
	RefreshACL(ctx context.Context, req RefreshACLRequest) (*RefreshACLResult, error)
	LLDPCapability(ctx context.Context, req LLDPCapabilityRequest) (*LLDPCapabilityResult, error)
	Unadopt(ctx context.Context, req UnadoptRequest) (*UnadoptResult, error)
}

// InspectRequest authenticates to an OpenWrt device for a read-only capability
// probe. It deliberately has no SSH key: inspection uses ubus only and never
// bootstraps a login or writes the device.
type InspectRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	Scheme   string `json:"scheme,omitempty"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// InspectResult is the measured evidence used by the function picker. Unknown
// is explicit so a denied check cannot masquerade as unsupported hardware.
type InspectResult struct {
	MAC        string   `json:"mac"`
	Model      string   `json:"model"`
	Class      string   `json:"class"`
	Firmware   string   `json:"firmware"`
	RadioCount *int     `json:"radio_count"`
	LANPorts   []string `json:"lan_ports"`
	WANPort    string   `json:"wan_port,omitempty"`
	// SwitchMode is dsa-conditional, observe-only, unknown, or none. Even DSA
	// mode is conditional because the controller will not turn VLAN filtering
	// on by rewriting the device's existing LAN bridge.
	SwitchMode           string          `json:"switch_mode"`
	FunctionsSupported   []string        `json:"functions_supported"`
	FunctionsRecommended []string        `json:"functions_recommended"`
	FunctionsUnknown     []string        `json:"functions_unknown,omitempty"`
	GatewayEvidence      GatewayEvidence `json:"gateway_evidence"`
	Unobservable         []string        `json:"unobservable,omitempty"`
	Notes                []string        `json:"notes,omitempty"`
}

// GatewayEvidence separates a measured false from a refused read. Nil means
// the device did not answer that check.
type GatewayEvidence struct {
	ActiveWANDefaultRoute *bool `json:"active_wan_default_route"`
	LANDHCPEnabled        *bool `json:"lan_dhcp_enabled"`
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
	Host                     string   `json:"host"`
	Port                     int      `json:"port,omitempty"`
	Scheme                   string   `json:"scheme,omitempty"` // "http" (default) or "https"
	Name                     string   `json:"name,omitempty"`
	Role                     string   `json:"role,omitempty"`      // gateway|ap|switch
	Functions                []string `json:"functions,omitempty"` // independently selected responsibilities
	Username                 string   `json:"username"`
	Password                 string   `json:"password"`
	AcknowledgeRouterChanges bool     `json:"acknowledge_router_changes"`
	// PrivateKey is an optional PEM SSH key, used in preference to the
	// password. A device with key-only SSH — which is the sensible way to run
	// one — cannot be adopted without it.
	PrivateKey string `json:"private_key,omitempty"`
}

// adoptRequestWire preserves the difference between an omitted functions
// field and an explicit null. Omission is the legacy contract; null is a
// present but invalid selection and must not expand a bundled role.
type adoptRequestWire struct {
	Host                     string          `json:"host"`
	Port                     int             `json:"port,omitempty"`
	Scheme                   string          `json:"scheme,omitempty"`
	Name                     string          `json:"name,omitempty"`
	Role                     string          `json:"role,omitempty"`
	Functions                json.RawMessage `json:"functions,omitempty"`
	Username                 string          `json:"username"`
	Password                 string          `json:"password"`
	PrivateKey               string          `json:"private_key,omitempty"`
	AcknowledgeRouterChanges bool            `json:"acknowledge_router_changes"`
}

// AdoptResult reports what adoption produced. It deliberately carries no
// credential: the one it created is sealed in the keyring, and the one the
// operator supplied is gone.
type AdoptResult struct {
	DeviceID  int64    `json:"device_id"`
	MAC       string   `json:"mac"`
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Functions []string `json:"functions"`
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

// RefreshACLRequest upgrades the controller's existing read-only device ACL.
// The administrator credential is used only for this SSH transaction and is
// never stored. The controller login and all router configuration remain in
// place.
type RefreshACLRequest struct {
	DeviceID                 int64  `json:"-"`
	Username                 string `json:"username"`
	Password                 string `json:"password"`
	PrivateKey               string `json:"private_key,omitempty"`
	AcknowledgeRouterChanges bool   `json:"acknowledge_router_changes"`
}

type RefreshACLResult struct {
	DeviceID           int64    `json:"device_id"`
	Name               string   `json:"name"`
	ACLUpdated         bool     `json:"acl_updated"`
	ControllerVerified bool     `json:"controller_verified"`
	Features           []string `json:"features"`
	Unobservable       []string `json:"unobservable,omitempty"`
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	if s.Enroll == nil {
		writeErr(w, http.StatusServiceUnavailable, "device inspection is not available")
		return
	}
	var req InspectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validateInspectRequest(w, &req) {
		return
	}
	res, err := s.Enroll.Inspect(r.Context(), req)
	if err != nil {
		s.Log.Warn("device inspection failed", "host", req.Host, "err", err)
		s.logAuth(r.Context(), "device.inspect_failed", "warning", req.Username, clientAddr(r))
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func validateInspectRequest(w http.ResponseWriter, req *InspectRequest) bool {
	req.Host = strings.TrimSpace(req.Host)
	if req.Host == "" {
		writeErr(w, http.StatusBadRequest, "host is required")
		return false
	}
	if req.Username == "" {
		writeErr(w, http.StatusBadRequest, "the device's administrator username is required")
		return false
	}
	if req.Scheme != "" && req.Scheme != "http" && req.Scheme != "https" {
		writeErr(w, http.StatusBadRequest, "scheme must be http or https")
		return false
	}
	if req.Port < 0 || req.Port > 65535 {
		writeErr(w, http.StatusBadRequest, "port is out of range")
		return false
	}
	return true
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
	Removed              bool     `json:"removed_from_inventory"`
	RevertedSections     int      `json:"reverted_sections"`
	ConfigRevertComplete bool     `json:"config_revert_complete"`
	ConfigRemains        []string `json:"config_remains,omitempty"`
	LoginRemoved         bool     `json:"login_removed"`
	ACLRemoved           bool     `json:"acl_removed"`
	FootprintRemains     bool     `json:"footprint_remains"`
	Residue              []string `json:"residue,omitempty"`
	CleanupCommands      []string `json:"cleanup_commands,omitempty"`
	Errors               []string `json:"errors,omitempty"`
	// NeedsOperator marks the case where phase 1 succeeded and phase 2 could
	// not run for want of the device's admin credential.
	NeedsOperator bool `json:"needs_operator_credential"`
	// Error carries the call's overall failure when there IS one, so that a
	// non-2xx response can still be this whole report rather than a bare
	// string. Named to match what every other endpoint puts in an error body,
	// so a generic client reads it without knowing about un-adopt.
	Error string `json:"error,omitempty"`
}

// ErrOperatorRequired is returned by an Enroller when phase 2 needs the
// device's own administrator credential.
var ErrOperatorRequired = errors.New("api: the device's administrator credential is required")

func (s *Server) handleAdopt(w http.ResponseWriter, r *http.Request) {
	if s.Enroll == nil {
		writeErr(w, http.StatusServiceUnavailable, "adoption is not available")
		return
	}
	var wire adoptRequestWire
	if !decodeJSON(w, r, &wire) {
		return
	}
	req := AdoptRequest{
		Host: wire.Host, Port: wire.Port, Scheme: wire.Scheme, Name: wire.Name,
		Role: wire.Role, Username: wire.Username, Password: wire.Password,
		PrivateKey: wire.PrivateKey, AcknowledgeRouterChanges: wire.AcknowledgeRouterChanges,
	}
	if !req.AcknowledgeRouterChanges {
		writeErr(w, http.StatusBadRequest, "acknowledge_router_changes must be true to authorize adoption's documented router changes")
		return
	}
	if wire.Functions != nil {
		if strings.TrimSpace(string(wire.Functions)) == "null" {
			writeErr(w, http.StatusBadRequest, "functions must be an array with at least one of ap, gateway, switch; null is not a selection")
			return
		}
		if err := json.Unmarshal(wire.Functions, &req.Functions); err != nil {
			writeErr(w, http.StatusBadRequest, "functions must be an array of ap, gateway, switch")
			return
		}
	}
	base := InspectRequest{
		Host: req.Host, Port: req.Port, Scheme: req.Scheme,
		Username: req.Username, Password: req.Password,
	}
	if !validateInspectRequest(w, &base) {
		return
	}
	req.Host = base.Host
	// The role decides what the renderer will and will not send to this device,
	// so an unrecognised one is refused here — at the boundary, before anything
	// contacts the device. It used to be stored verbatim and compared later
	// with an exact string match, so "Gateway" adopted a router as an access
	// point: no addressing, no DHCP, no firewall zone, and nothing saying why.
	// Normalised on the way through so the stored value is canonical.
	role, err := model.ParseRole(req.Role)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	functions, err := model.ParseDeviceFunctions(req.Functions, role)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Functions = functions.Strings()
	req.Role = string(functions.PrimaryRole())

	// Adoption changes the fleet identity that a preview token binds. Keep it
	// out of the interval between an apply's fleet preflight and final write.
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
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

	// Un-adoption removes credentials, ownership and the inventory row, all of
	// which are part of the server-verified preview binding.
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	res, err := s.Enroll.Unadopt(r.Context(), req)
	if errors.Is(err, ErrOperatorRequired) {
		// Not a failure. Phase 1 ran; phase 2 needs a credential the controller
		// deliberately does not hold. 409 so a client can tell this from both
		// success and a real error.
		writeJSON(w, http.StatusConflict, res)
		return
	}
	if err != nil {
		// The report is still the answer when the call ALSO failed, and
		// discarding it to send a bare error string threw away the one thing
		// that cannot be recovered.
		//
		// Un-adopt can remove the inventory row and report that the device kept
		// a footprint, in the same call: a forced removal whose phase 2 got as
		// far as connecting and then failed to commit. Once that row is gone,
		// res.Residue is the ONLY remaining record of what is installed on that
		// device — and it went to a `{"error": "..."}` body that no client could
		// render. The same applied without Force, where the residue list is
		// exactly what the panel exists to show.
		if res != nil {
			res.Error = err.Error()
			writeJSON(w, http.StatusBadGateway, res)
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRefreshACL(w http.ResponseWriter, r *http.Request) {
	if s.Enroll == nil {
		writeErr(w, http.StatusServiceUnavailable, "device access refresh is not available")
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req RefreshACLRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.AcknowledgeRouterChanges {
		writeErr(w, http.StatusBadRequest, "acknowledge_router_changes must be true to authorize replacing the router ACL file")
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		writeErr(w, http.StatusBadRequest, "the device's administrator username is required")
		return
	}
	req.DeviceID = id
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	res, err := s.Enroll.RefreshACL(r.Context(), req)
	if err != nil {
		s.Log.Warn("device access refresh failed", "device", id, "err", err)
		s.logAuth(r.Context(), "device.acl_refresh_failed", "warning", req.Username, clientAddr(r))
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
