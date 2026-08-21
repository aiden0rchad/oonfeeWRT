package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

type policyRequest struct {
	Order       *int                `json:"order"`
	Name        *string             `json:"name"`
	Kind        *model.PolicyKind   `json:"kind"`
	Origin      *model.PolicyOrigin `json:"origin"`
	Enabled     *bool               `json:"enabled"`
	Firewall    *model.FirewallRule `json:"firewall"`
	PortForward *model.PortForward  `json:"port_forward"`
	StaticRoute *model.StaticRoute  `json:"static_route"`
}

func (req policyRequest) model(id int) (model.Policy, error) {
	var missing []string
	if req.Name == nil {
		missing = append(missing, "name")
	}
	if req.Kind == nil {
		missing = append(missing, "kind")
	}
	if req.Enabled == nil {
		missing = append(missing, "enabled")
	}
	if len(missing) > 0 {
		return model.Policy{}, fmt.Errorf("policy must include %s", strings.Join(missing, ", "))
	}
	p := model.Policy{ID: id, Name: strings.TrimSpace(*req.Name), Kind: *req.Kind,
		Enabled: *req.Enabled, Firewall: req.Firewall, PortForward: req.PortForward,
		StaticRoute: req.StaticRoute, Origin: model.PolicyOriginManual}
	if req.Order != nil {
		p.Order = *req.Order
	}
	if req.Origin != nil {
		p.Origin = *req.Origin
	}
	return p, nil
}

type policyRowView struct {
	ID             string         `json:"id"`
	RecordID       int            `json:"record_id,omitempty"`
	Origin         string         `json:"origin"`
	Kind           string         `json:"kind"`
	Name           string         `json:"name"`
	Enabled        bool           `json:"enabled"`
	Order          int            `json:"order"`
	OrderScope     string         `json:"order_scope"`
	EffectiveScope map[string]any `json:"effective_scope"`
	Mutable        bool           `json:"mutable"`
	Renderable     bool           `json:"renderable"`
	GatedReason    string         `json:"gated_reason,omitempty"`
	Rule           any            `json:"rule,omitempty"`
}

type policyCapabilityView struct {
	Kind      string `json:"kind"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type policyMasterView struct {
	Rows         []policyRowView        `json:"rows"`
	Capabilities []policyCapabilityView `json:"capabilities"`
}

func (s *Server) policyMaster(ctx context.Context, site model.Site) (policyMasterView, error) {
	devices, err := s.Store.Devices(ctx)
	if err != nil {
		return policyMasterView{}, err
	}
	gatewayOK, gatewayReason := policyGatewayGate(devices, false)
	firewallOK, firewallReason := policyGatewayGate(devices, true)
	rows := []policyRowView{}

	for i, zone := range site.EffectiveZonePolicies() {
		origin := "legacy_default"
		if zone.Explicit {
			origin = "zone_matrix"
		}
		rows = append(rows, policyRowView{
			ID: "zone:" + zone.Name, Origin: origin, Kind: "zone_forward",
			Name: "Forward from " + zone.Name, Enabled: true, Order: i,
			OrderScope: "zone_forwarding", Mutable: zone.Explicit,
			EffectiveScope: map[string]any{"source_zone": zone.Name, "destination_zones": nonnilStrings(zone.ForwardTo),
				"connection_scope": "new", "existing_connections": "may_persist_until_conntrack_expiry"},
			Renderable: firewallOK, GatedReason: reasonUnless(firewallOK, firewallReason),
			Rule: map[string]any{"forward_to": nonnilStrings(zone.ForwardTo), "explicit": zone.Explicit},
		})
	}

	for _, policy := range site.Policies {
		ok, reason := gatewayOK, gatewayReason
		if policy.Kind == model.PolicyFirewallRule || policy.Kind == model.PolicyPortForward {
			ok, reason = firewallOK, firewallReason
		} else if policy.Kind == model.PolicyStaticRoute && policy.StaticRoute.NetworkID > 0 && ok {
			ok = false
			reason = "the selected managed network is proved per Gateway only by Preview; this aggregate table cannot observe current bridge/VLAN readiness"
		}
		rows = append(rows, policyRowView{
			ID: fmt.Sprintf("policy:%d", policy.ID), RecordID: policy.ID,
			Origin: string(policy.Origin), Kind: string(policy.Kind), Name: policy.Name,
			Enabled: policy.Enabled, Order: policy.Order, OrderScope: policyOrderScope(policy.Kind),
			EffectiveScope: policyScope(site, policy), Mutable: true,
			Renderable: ok, GatedReason: reasonUnless(ok, reason), Rule: policyRule(policy),
		})
	}

	for i, client := range site.PolicyClients {
		mac := strings.ToLower(client.MAC)
		if client.Blocked {
			ok, reason := firewallOK, firewallReason
			if ok && len(site.ActiveZoneNames()) == 0 {
				ok, reason = false, "no active managed source zone exists; the foreign lan zone is not rewritten"
			}
			rows = append(rows, policyRowView{
				ID: "client:block:" + mac, Origin: "client", Kind: "client_block",
				Name: "Block " + mac, Enabled: true, Order: 1_000_000 + i,
				OrderScope: "display_only", EffectiveScope: map[string]any{
					"client_mac": mac, "source_zones": site.ActiveZoneNames(),
					"traffic": "routed_forwarding", "destination_zones": "any",
					"address_families": []string{"ipv4", "ipv6"},
					"connection_scope": "new", "existing_connections": "may_persist_until_conntrack_expiry",
					"excludes": []string{"router_input", "same_l2"},
				}, Mutable: true, Renderable: ok, GatedReason: reasonUnless(ok, reason),
				Rule: map[string]any{"blocked": true},
			})
		}
		if client.FixedIP != "" {
			ok, reason := false, "the selected managed DHCP interface is proved per Gateway only by Preview; this aggregate table cannot observe current bridge/VLAN readiness"
			if !gatewayOK {
				reason = gatewayReason
			}
			rows = append(rows, policyRowView{
				ID: "client:fixed-ip:" + mac, Origin: "client", Kind: "fixed_ip",
				Name: "Fixed IP for " + mac, Enabled: true, Order: i,
				OrderScope: "display_only", EffectiveScope: map[string]any{
					"client_mac": mac, "fixed_ip": client.FixedIP,
				}, Mutable: true, Renderable: ok,
				GatedReason: reason,
				Rule:        map[string]any{"fixed_ip": client.FixedIP},
			})
		}
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].OrderScope != rows[j].OrderScope {
			return rows[i].OrderScope < rows[j].OrderScope
		}
		if rows[i].Order != rows[j].Order {
			return rows[i].Order < rows[j].Order
		}
		return rows[i].ID < rows[j].ID
	})
	return policyMasterView{Rows: rows, Capabilities: []policyCapabilityView{
		{Kind: "firewall", Available: firewallOK, Reason: reasonUnless(firewallOK, firewallReason)},
		{Kind: "nat", Available: firewallOK, Reason: reasonUnless(firewallOK, firewallReason)},
		{Kind: "route", Available: gatewayOK, Reason: reasonUnless(gatewayOK, gatewayReason)},
		{Kind: "fixed_ip", Available: gatewayOK, Reason: reasonUnless(gatewayOK, gatewayReason)},
		{Kind: "connection_state", Available: true, Reason: "firewall, NAT and client-block changes govern new flows; existing conntrack entries are not flushed and can persist until expiry"},
		{Kind: "priority", Available: false, Reason: "unavailable: order is display-only; this release rejects overlapping managed rules instead of pretending UCI section names enforce evaluation priority"},
		{Kind: "qos", Available: false, Reason: "unavailable: this build does not observe or own an SQM/tc backend"},
		{Kind: "rate_limit", Available: false, Reason: "unavailable: per-client rate limiting is QoS-backend gated"},
		{Kind: "application", Available: false, Reason: "unavailable: application identity requires a separately observed DPI capability; none is built"},
	}}, nil
}

func policyGatewayGate(devices []*store.Device, firewall4 bool) (bool, string) {
	var gateways []*store.Device
	for _, device := range devices {
		if device.Adopted() && model.DeviceFunctionsOf(device.Functions, device.Role).Routes() {
			gateways = append(gateways, device)
		}
	}
	if len(gateways) == 0 {
		return false, "no adopted device has the Gateway function"
	}
	if !firewall4 {
		return true, ""
	}
	for _, device := range gateways {
		var caps capability.Registry
		if json.Unmarshal([]byte(device.CapsJSON), &caps) != nil {
			return false, fmt.Sprintf("Gateway %s has an unreadable capability record", device.Name)
		}
		state := caps.State(capability.FeatFirewall4)
		if state != capability.Present {
			return false, fmt.Sprintf("Gateway %s reports firewall4 %s; re-probe after restoring the nft read grant", device.Name, state)
		}
	}
	return true, ""
}

func reasonUnless(ok bool, reason string) string {
	if ok {
		return ""
	}
	return reason
}

func nonnilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func policyOrderScope(kind model.PolicyKind) string {
	return "display_only"
}

func policyScope(site model.Site, p model.Policy) map[string]any {
	switch p.Kind {
	case model.PolicyFirewallRule:
		return map[string]any{"source_zone": p.Firewall.SourceZone,
			"destination_zone": p.Firewall.DestinationZone, "source_macs": nonnilStrings(p.Firewall.SourceMACs),
			"address_families": []string{"ipv4"}, "connection_scope": "new",
			"existing_connections": "may_persist_until_conntrack_expiry"}
	case model.PolicyPortForward:
		return map[string]any{"source_zone": p.PortForward.SourceZone,
			"destination_zone": p.PortForward.DestinationZone, "destination_ip": p.PortForward.DestinationIP,
			"address_families": []string{"ipv4"}, "connection_scope": "new",
			"existing_connections": "retain_their_original_conntrack_mapping_until_expiry"}
	case model.PolicyStaticRoute:
		via := "wan"
		if p.StaticRoute.NetworkID > 0 {
			if network, ok := site.NetworkByID(p.StaticRoute.NetworkID); ok {
				via = network.Name
			}
		}
		return map[string]any{"target": p.StaticRoute.Target, "via": via, "gateway": p.StaticRoute.Gateway,
			"address_families": []string{"ipv4"}}
	}
	return map[string]any{}
}

func policyRule(p model.Policy) any {
	switch p.Kind {
	case model.PolicyFirewallRule:
		return p.Firewall
	case model.PolicyPortForward:
		return p.PortForward
	case model.PolicyStaticRoute:
		return p.StaticRoute
	}
	return nil
}

func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	site, err := s.Store.Site(r.Context())
	if handleStoreErr(w, err, "site") {
		return
	}
	master, err := s.policyMaster(r.Context(), site)
	if handleStoreErr(w, err, "policy master table") {
		return
	}
	writeJSON(w, http.StatusOK, master)
}

func (s *Server) handleSavePolicy(w http.ResponseWriter, r *http.Request) {
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	id := 0
	if raw := r.PathValue("id"); raw != "" {
		var err error
		id, err = strconv.Atoi(raw)
		if err != nil || id <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid policy id")
			return
		}
	}
	var req policyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := req.model(id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if id > 0 && (req.Order == nil || req.Origin == nil) {
		site, err := s.Store.Site(r.Context())
		if handleStoreErr(w, err, "site") {
			return
		}
		var old *model.Policy
		for i := range site.Policies {
			if site.Policies[i].ID == id {
				old = &site.Policies[i]
				break
			}
		}
		if old == nil {
			writeErr(w, http.StatusNotFound, "no such policy")
			return
		}
		if req.Order == nil {
			p.Order = old.Order
		}
		if req.Origin == nil {
			p.Origin = old.Origin
		}
	}
	if err := s.Store.SavePolicy(r.Context(), &p); err != nil {
		if err == store.ErrNotFound {
			writeErr(w, http.StatusNotFound, "no such policy")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logSiteChange(r.Context(), "site.policy_saved", map[string]any{
		"policy": p.ID, "kind": p.Kind, "origin": p.Origin,
	})
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid policy id")
		return
	}
	if err := s.Store.DeletePolicy(r.Context(), id); err != nil {
		if err == store.ErrNotFound {
			writeErr(w, http.StatusNotFound, "no such policy")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logSiteChange(r.Context(), "site.policy_deleted", map[string]any{"policy": id})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id,
		"note": "desired state removed; devices are unchanged until Preview and Apply"})
}

func (s *Server) handleSaveClientPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	var req struct {
		Blocked *bool   `json:"blocked"`
		FixedIP *string `json:"fixed_ip"`
		Group   *string `json:"group"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	client, err := s.Store.SaveClientPolicy(r.Context(), r.PathValue("mac"), req.Blocked, req.FixedIP, req.Group)
	if err != nil {
		if err == store.ErrNotFound {
			writeErr(w, http.StatusNotFound, "no such client")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logSiteChange(r.Context(), "site.client_policy_saved", map[string]any{
		"client": client.MAC, "blocked": client.Blocked, "has_fixed_ip": client.FixedIP != "", "group": client.Group,
	})
	writeJSON(w, http.StatusOK, map[string]any{"client": client,
		"note": "desired state saved; no device changes until Preview and Apply"})
}

type objectManagerRequest struct {
	Objects  []objectTarget  `json:"objects"`
	Outcomes []objectOutcome `json:"outcomes"`
}

type objectTarget struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type objectOutcome struct {
	Kind            string `json:"kind"`
	DestinationZone string `json:"destination_zone,omitempty"`
	Target          string `json:"target,omitempty"`
	Gateway         string `json:"gateway,omitempty"`
	Metric          int    `json:"metric,omitempty"`
	RateKbps        int    `json:"rate_kbps,omitempty"`
}

type objectGate struct {
	Object  objectTarget `json:"object"`
	Outcome string       `json:"outcome"`
	Reason  string       `json:"reason"`
}

func (s *Server) handleCompileObjects(w http.ResponseWriter, r *http.Request) {
	var req objectManagerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Objects) == 0 || len(req.Outcomes) == 0 {
		writeErr(w, http.StatusBadRequest, "objects and outcomes must both be non-empty arrays")
		return
	}
	site, err := s.Store.Site(r.Context())
	if handleStoreErr(w, err, "site") {
		return
	}
	var drafts []model.Policy
	var gates []objectGate
	for _, object := range req.Objects {
		object.Kind = strings.TrimSpace(object.Kind)
		object.ID = strings.TrimSpace(object.ID)
		knownDevice := false
		if object.Kind == "device" {
			knownDevice, err = s.Store.ClientExists(r.Context(), object.ID)
			if handleStoreErr(w, err, "client inventory") {
				return
			}
		}
		if err := validateObject(site, object, knownDevice); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		for _, outcome := range req.Outcomes {
			compiled, reason, err := compileObjectOutcome(site, object, outcome)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if reason != "" {
				gates = append(gates, objectGate{Object: object, Outcome: outcome.Kind, Reason: reason})
				continue
			}
			drafts = append(drafts, compiled...)
		}
	}
	if drafts == nil {
		drafts = []model.Policy{}
	}
	if gates == nil {
		gates = []objectGate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"drafts": drafts, "gates": gates,
		"persisted": false, "applied": false,
		"note": "Object Manager only compiled inspectable drafts; Secure/firewall drafts are IPv4-only and govern new flows in this release. Existing conntrack sessions are not flushed. Save chosen drafts through /api/v1/site/policies, then Preview and Apply",
	})
}

func validateObject(site model.Site, object objectTarget, knownDevice bool) error {
	switch object.Kind {
	case "device":
		if _, err := model.CanonicalMACs([]string{object.ID}); err != nil {
			return fmt.Errorf("Object Manager device %q is invalid: %w", object.ID, err)
		}
		if knownDevice {
			return nil
		}
		return fmt.Errorf("Object Manager device %q is not in the observed client inventory", object.ID)
	case "group":
		for _, client := range site.PolicyClients {
			if client.Group == object.ID && object.ID != "" {
				return nil
			}
		}
		return fmt.Errorf("Object Manager group %q has no members", object.ID)
	case "network":
		if object.ID == "wan" {
			return nil
		}
		id, err := strconv.Atoi(object.ID)
		if err != nil {
			return fmt.Errorf("Object Manager network id %q is invalid", object.ID)
		}
		network, ok := site.NetworkByID(id)
		if !ok || !network.Enabled || network.VLAN <= 1 {
			return fmt.Errorf("Object Manager network %d is not active and managed", id)
		}
		return nil
	default:
		return fmt.Errorf("Object Manager object kind %q must be device, group or network", object.Kind)
	}
}

func compileObjectOutcome(site model.Site, object objectTarget, outcome objectOutcome) ([]model.Policy, string, error) {
	switch outcome.Kind {
	case "secure":
		if object.Kind == "network" && object.ID == "wan" {
			return nil, "WAN is destination-only in the managed-zone model; inbound access needs an explicit firewall rule or port forward", nil
		}
		dest := outcome.DestinationZone
		if dest == "" {
			dest = "wan"
		}
		sourceZones, macs, err := objectFirewallScope(site, object)
		if err != nil {
			return nil, "", err
		}
		compiled := make([]model.Policy, 0, len(sourceZones))
		for _, sourceZone := range sourceZones {
			name := "Secure " + object.Kind + " " + object.ID
			if len(sourceZones) > 1 {
				name += " from " + sourceZone
			}
			compiled = append(compiled, model.Policy{Name: name,
				Kind: model.PolicyFirewallRule, Origin: model.PolicyOriginObjectManager, Enabled: true,
				Firewall: &model.FirewallRule{Action: model.FirewallReject, SourceZone: sourceZone,
					DestinationZone: dest, Protocols: []string{"all"}, SourceMACs: macs}})
		}
		check := site
		check.Policies = append(append([]model.Policy(nil), site.Policies...), compiled...)
		if errs := check.ValidatePolicies(); len(errs) > 0 {
			return nil, "", errs[0]
		}
		return compiled, "", nil

	case "route":
		if object.Kind != "network" {
			return nil, "per-device and group Route outcomes require an observable ip-rule/table backend; this build only compiles static network routes", nil
		}
		networkID := 0
		if object.ID != "wan" {
			networkID, _ = strconv.Atoi(object.ID)
		}
		p := model.Policy{Name: "Route " + outcome.Target + " via " + object.ID,
			Kind: model.PolicyStaticRoute, Origin: model.PolicyOriginObjectManager, Enabled: true,
			StaticRoute: &model.StaticRoute{NetworkID: networkID, Target: outcome.Target,
				Gateway: outcome.Gateway, Metric: outcome.Metric}}
		check := site
		check.Policies = append(append([]model.Policy(nil), site.Policies...), p)
		if errs := check.ValidatePolicies(); len(errs) > 0 {
			return nil, "", errs[0]
		}
		return []model.Policy{p}, "", nil

	case "qos":
		return nil, "QoS is unavailable: this build does not observe or own an SQM/tc backend; no rate-limit rule was invented", nil
	case "application":
		return nil, "application outcomes are unavailable until a DPI capability is separately observed and built", nil
	default:
		return nil, "", fmt.Errorf("Object Manager outcome %q must be secure, route, qos or application", outcome.Kind)
	}
}

func objectFirewallScope(site model.Site, object objectTarget) ([]string, []string, error) {
	if object.Kind == "network" {
		id, _ := strconv.Atoi(object.ID)
		network, _ := site.NetworkByID(id)
		zone := network.Zone
		if zone == "" {
			zone = network.Name
		}
		return []string{zone}, nil, nil
	}
	// Client/group membership does not prove a network attachment. A secure
	// draft is emitted once per active source zone, preserving exact scope.
	var macs []string
	if object.Kind == "device" {
		macs = append(macs, object.ID)
	}
	for _, client := range site.PolicyClients {
		if object.Kind == "group" && client.Group == object.ID {
			macs = append(macs, client.MAC)
		}
	}
	macs, err := model.CanonicalMACs(macs)
	if err != nil {
		return nil, nil, err
	}
	zones := site.ActiveZoneNames()
	if len(zones) == 0 {
		return nil, nil, fmt.Errorf("Object Manager cannot secure %s %q because no active managed source zone exists", object.Kind, object.ID)
	}
	return zones, macs, nil
}
