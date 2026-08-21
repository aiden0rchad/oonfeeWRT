package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

const inspectTimeout = 60 * time.Second

// Inspect authenticates to a device and measures its hardware and current
// gateway use without opening SSH, installing the controller login, writing
// UCI, or adding an inventory row.
func (d *Daemon) Inspect(ctx context.Context, req api.InspectRequest) (*api.InspectResult, error) {
	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()

	https := req.Scheme == "https"
	endpoint, err := d.resolveWorkflowEndpoint(ctx, req.Host)
	if err != nil {
		return nil, err
	}
	host, err := endpoint.httpAuthority(req.Port, https)
	if err != nil {
		return nil, err
	}
	c := ubus.New(ubus.Options{Host: host, HTTPS: https, Timeout: 30 * time.Second})
	defer c.Close()
	if err := c.Login(ctx, req.Username, req.Password); err != nil {
		return nil, fmt.Errorf("could not sign in to %s: %w", req.Host, err)
	}
	mac, err := deviceMAC(ctx, c)
	if err != nil {
		return nil, err
	}
	caps, err := capability.Probe(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", req.Host, err)
	}
	evidence := inspectGatewayEvidence(ctx, c, caps.Ports.WAN)
	supported, recommended, unknown := assessFunctions(caps, evidence)

	out := &api.InspectResult{
		MAC: mac, Model: caps.Board.Model, Class: string(caps.Class),
		Firmware: caps.Board.Release, RadioCount: len(caps.Radios),
		LANPorts: append([]string{}, caps.Ports.LAN...), WANPort: caps.Ports.WAN,
		SwitchMode:           switchMode(caps),
		FunctionsSupported:   append([]string{}, supported...),
		FunctionsRecommended: append([]string{}, recommended...),
		FunctionsUnknown:     unknown, Notes: append([]string(nil), caps.Notes...),
		GatewayEvidence: api.GatewayEvidence{
			ActiveWANDefaultRoute: evidence.routePointer(),
			LANDHCPEnabled:        evidence.dhcpPointer(),
		},
	}
	switch out.SwitchMode {
	case "dsa-conditional":
		out.Notes = append(out.Notes, "wired switch ports are DSA-capable, but managed VLAN carriage still requires an existing VLAN-aware bridge; the controller will not enable it by rewriting the device's LAN")
	case "observe-only":
		out.Notes = append(out.Notes, "wired switch support is read-only port and forwarding-database visibility; this legacy swconfig layout is not managed for VLAN configuration")
	case "unknown":
		out.Notes = append(out.Notes, "wired switch management mode could not be determined; selecting switch does not invent per-port or VLAN control")
	}
	if evidence.activeDefault {
		out.Notes = append(out.Notes, "gateway recommendation: an active IPv4 default route was measured on the WAN interface")
	}
	if evidence.lanDHCPEnabled {
		out.Notes = append(out.Notes, "gateway recommendation: the LAN DHCP server is enabled")
	}
	for _, feature := range caps.Unobservable() {
		out.Unobservable = append(out.Unobservable, string(feature))
	}
	sort.Strings(out.Unobservable)
	return out, nil
}

func switchMode(caps *capability.Registry) string {
	if caps == nil {
		return "unknown"
	}
	dsa := caps.State(capability.FeatDSA)
	switchPorts := caps.State(capability.FeatSwitchPorts)
	switch {
	case dsa == capability.Present && len(caps.Ports.LAN) > 0:
		return "dsa-conditional"
	case dsa == capability.Absent && switchPorts == capability.Present:
		return "observe-only"
	case !dsa.Decided() || !switchPorts.Decided():
		return "unknown"
	default:
		return "none"
	}
}

type gatewayEvidence struct {
	routeKnown, activeDefault bool
	dhcpKnown, lanDHCPEnabled bool
}

type runtimeInterfaceDump struct {
	Interface []runtimeInterface `json:"interface"`
}

type runtimeInterface struct {
	Name     string         `json:"interface"`
	Device   string         `json:"device"`
	L3Device string         `json:"l3_device"`
	Up       bool           `json:"up"`
	Route    []runtimeRoute `json:"route"`
}

type runtimeRoute struct {
	Target string `json:"target"`
	Mask   int    `json:"mask"`
}

func (e gatewayEvidence) routePointer() *bool {
	if !e.routeKnown {
		return nil
	}
	v := e.activeDefault
	return &v
}

func (e gatewayEvidence) dhcpPointer() *bool {
	if !e.dhcpKnown {
		return nil
	}
	v := e.lanDHCPEnabled
	return &v
}

// inspectGatewayEvidence asks what the device is doing now. A WAN-labelled
// port alone is not evidence of gateway use: the reference AP has one too.
func inspectGatewayEvidence(ctx context.Context, c *ubus.Client, wanDevice string) gatewayEvidence {
	var evidence gatewayEvidence
	var dump runtimeInterfaceDump
	if err := c.Call(ctx, "network.interface", "dump", nil, &dump); err == nil {
		evidence.routeKnown = true
		evidence.activeDefault = activeWANDefault(dump, wanDevice)
	}

	var dhcp struct {
		Values map[string]map[string]any `json:"values"`
	}
	err := c.Call(ctx, "uci", "get", map[string]any{"config": "dhcp"}, &dhcp)
	if err == nil {
		evidence.dhcpKnown = true
		for _, section := range dhcp.Values {
			if textValue(section[".type"]) == "dhcp" &&
				textValue(section["interface"]) == "lan" &&
				textValue(section["ignore"]) != "1" {
				evidence.lanDHCPEnabled = true
			}
		}
	} else {
		var status *ubus.StatusError
		if errors.As(err, &status) &&
			(status.Status == ubus.StatusNotFound || status.Status == ubus.StatusNoData) {
			evidence.dhcpKnown = true
		}
	}
	return evidence
}

func activeWANDefault(dump runtimeInterfaceDump, wanDevice string) bool {
	for _, iface := range dump.Interface {
		// An AP normally has a management default route on LAN. Only the
		// conventional routed uplink is gateway evidence; treating every
		// default route as one recreated the unauthenticated discovery false
		// positive this inspection exists to correct.
		isWAN := iface.Name == "wan" || (wanDevice != "" &&
			(iface.Device == wanDevice || iface.L3Device == wanDevice))
		if !iface.Up || !isWAN {
			continue
		}
		for _, route := range iface.Route {
			if route.Target == "0.0.0.0" && route.Mask == 0 {
				return true
			}
		}
	}
	return false
}

func textValue(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func assessFunctions(caps *capability.Registry, evidence gatewayEvidence) (
	supported, recommended, unknown []string) {
	if caps == nil {
		return nil, nil, []string{"gateway", "ap", "switch"}
	}
	add := func(dst *[]string, fn model.DeviceFunction) {
		*dst = append(*dst, string(fn))
	}

	if caps.Ports.WAN != "" || evidence.activeDefault || evidence.lanDHCPEnabled {
		add(&supported, model.FunctionGateway)
	}
	if evidence.activeDefault || evidence.lanDHCPEnabled {
		add(&recommended, model.FunctionGateway)
	} else if !evidence.routeKnown || !evidence.dhcpKnown {
		add(&unknown, model.FunctionGateway)
	}

	if len(caps.Radios) > 0 {
		add(&supported, model.FunctionAP)
		add(&recommended, model.FunctionAP)
	} else if !caps.State(capability.FeatSurvey).Decided() {
		add(&unknown, model.FunctionAP)
	}

	switchState := caps.State(capability.FeatSwitchPorts)
	if len(caps.Ports.LAN) > 0 || switchState == capability.Present {
		add(&supported, model.FunctionSwitch)
	}
	if len(caps.Ports.LAN) > 1 || switchState == capability.Present {
		add(&recommended, model.FunctionSwitch)
	} else if !switchState.Decided() && len(caps.Ports.LAN) == 0 {
		add(&unknown, model.FunctionSwitch)
	}
	return supported, recommended, unknown
}
