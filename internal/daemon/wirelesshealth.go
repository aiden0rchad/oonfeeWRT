package daemon

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

type wirelessRuntimeCaller interface {
	Call(context.Context, string, string, any, any) error
}

type wirelessRuntimeBSS struct {
	section  string
	ssid     string
	isolated bool
}

type wirelessRuntimePlan struct {
	desired []wirelessRuntimeBSS
	absent  []string
}

type wirelessRuntimeDevices map[string]struct {
	Interfaces []struct {
		IfName  string `json:"ifname"`
		Section string `json:"section"`
		Config  struct {
			SSID string `json:"ssid"`
		} `json:"config"`
	} `json:"interfaces"`
}

type wirelessRuntimeFailure struct {
	message  string
	terminal bool
}

func (e *wirelessRuntimeFailure) Error() string { return e.message }

func wirelessRuntimePlanFor(plan *reconcile.DevicePlan) *wirelessRuntimePlan {
	if plan == nil {
		return nil
	}
	want := map[string]wirelessRuntimeBSS{}
	for _, section := range plan.Doc.Sections {
		if section.Config != "wireless" || section.Values["ssid"] == "" ||
			section.Values["disabled"] == "1" {
			continue
		}
		want[section.Name] = wirelessRuntimeBSS{
			section:  section.Name,
			ssid:     section.Values["ssid"],
			isolated: section.Values["isolate"] == "1",
		}
	}

	out := &wirelessRuntimePlan{}
	for _, bss := range want {
		out.desired = append(out.desired, bss)
	}
	for _, op := range plan.Plan.Ops {
		if op.Config != "wireless" || op.Option != "" || op.Kind != applyengine.OpDelete {
			continue
		}
		if _, stillDesired := want[op.Section]; !stillDesired {
			out.absent = append(out.absent, op.Section)
		}
	}
	sort.Slice(out.desired, func(i, j int) bool {
		return out.desired[i].section < out.desired[j].section
	})
	sort.Strings(out.absent)
	return out
}

func (p *wirelessRuntimePlan) empty() bool {
	return p == nil || len(p.desired) == 0 && len(p.absent) == 0
}

// checkWirelessRuntimeOnce proves each desired UCI section separately. A set
// of SSID strings is insufficient: two radios can publish the same SSID while
// only one BSS actually came up. The sysfs check proves `bridge_isolate` across
// BSS bridge ports on this AP. It does not prove hostapd's same-BSS ap_isolate
// behavior; that remains a two-client acceptance check. Clients attached to
// another AP require additional L2 switch/bridge policy.
func checkWirelessRuntimeOnce(ctx context.Context, c wirelessRuntimeCaller,
	plan *wirelessRuntimePlan) error {
	if plan.empty() {
		return nil
	}

	var dump wirelessRuntimeDevices
	if err := c.Call(ctx, "luci-rpc", "getWirelessDevices", struct{}{}, &dump); err != nil {
		return wirelessInventoryFailure(err)
	}

	bySection := map[string][]struct {
		ifname string
		ssid   string
	}{}
	for _, radio := range dump {
		for _, iface := range radio.Interfaces {
			if iface.Section == "" {
				continue
			}
			bySection[iface.Section] = append(bySection[iface.Section], struct {
				ifname string
				ssid   string
			}{iface.IfName, iface.Config.SSID})
		}
	}

	for _, section := range plan.absent {
		if len(bySection[section]) != 0 {
			return &wirelessRuntimeFailure{message: fmt.Sprintf(
				"health: wireless.%s still maps to a runtime BSS after this change removed it",
				section)}
		}
	}
	for _, want := range plan.desired {
		matches := bySection[want.section]
		if len(matches) == 0 {
			return &wirelessRuntimeFailure{message: fmt.Sprintf(
				"health: wireless.%s was not mapped to a runtime interface by "+
					"luci-rpc.getWirelessDevices; this LuCI/OpenWrt version may not expose "+
					"managed section identities, so its SSID and bridge isolation cannot be proven",
				want.section)}
		}
		if len(matches) != 1 {
			return &wirelessRuntimeFailure{message: fmt.Sprintf(
				"health: wireless.%s maps to %d runtime interfaces; refusing to guess which BSS to verify",
				want.section, len(matches)), terminal: true}
		}
		got := matches[0]
		// netifd publishes the UCI section before it has assigned the runtime
		// interface name while wireless is reloading.  That is a normal settle
		// state, not malformed input; let the outer health loop retry it.  A
		// non-empty unsafe value is different: never turn that into a hostapd
		// object or sysfs path.
		if got.ifname == "" {
			return &wirelessRuntimeFailure{message: fmt.Sprintf(
				"health: wireless.%s is present but has no runtime interface name yet",
				want.section)}
		}
		if !safeWirelessIfname(got.ifname) {
			return &wirelessRuntimeFailure{message: fmt.Sprintf(
				"health: wireless.%s has unsafe runtime interface name %q",
				want.section, got.ifname), terminal: true}
		}
		if got.ssid != want.ssid {
			return &wirelessRuntimeFailure{message: fmt.Sprintf(
				"health: wireless.%s maps to %s with configured SSID %q, want %q",
				want.section, got.ifname, got.ssid, want.ssid)}
		}

		var status struct {
			SSID   string `json:"ssid"`
			Status string `json:"status"`
		}
		object := "hostapd." + got.ifname
		if err := c.Call(ctx, object, "get_status", struct{}{}, &status); err != nil {
			return wirelessStatusFailure(want.section, got.ifname, err)
		}
		if status.SSID != want.ssid {
			return &wirelessRuntimeFailure{message: fmt.Sprintf(
				"health: wireless.%s maps to %s, but hostapd is carrying SSID %q, want %q",
				want.section, got.ifname, status.SSID, want.ssid)}
		}
		if status.Status != "" && !strings.EqualFold(status.Status, "ENABLED") {
			return &wirelessRuntimeFailure{message: fmt.Sprintf(
				"health: wireless.%s maps to %s, but hostapd status is %q",
				want.section, got.ifname, status.Status)}
		}
		if !want.isolated {
			continue
		}

		isolatedPath := path.Join("/sys/class/net", got.ifname, "brport/isolated")
		var read struct {
			Data string `json:"data"`
		}
		if err := c.Call(ctx, "file", "read", map[string]string{"path": isolatedPath}, &read); err != nil {
			return bridgeIsolationFailure(want.section, got.ifname, isolatedPath, err)
		}
		if value := strings.TrimSpace(read.Data); value != "1" {
			return &wirelessRuntimeFailure{message: fmt.Sprintf(
				"health: wireless.%s (%s) reports brport isolated=%q after bridge_isolate=1; "+
					"this OpenWrt/netifd version may be ignoring bridge_isolate",
				want.section, got.ifname, value)}
		}
	}
	return nil
}

func safeWirelessIfname(ifname string) bool {
	return ifname != "" && ifname != "." && ifname != ".." && len(ifname) <= 15 &&
		!strings.ContainsAny(ifname, "/\x00")
}

func wirelessInventoryFailure(err error) error {
	var denied *ubus.DeniedError
	if errors.As(err, &denied) {
		return &wirelessRuntimeFailure{message: fmt.Sprintf(
			"health: luci-rpc.getWirelessDevices was denied; re-adopt the device with the current "+
				"controller ACL before applying wireless changes: %v", err), terminal: true}
	}
	var status *ubus.StatusError
	if errors.As(err, &status) && (status.Status == ubus.StatusNotFound ||
		status.Status == ubus.StatusNotSupported || status.Status == ubus.StatusMethodNotFound) {
		return &wirelessRuntimeFailure{message: fmt.Sprintf(
			"health: luci-rpc.getWirelessDevices is unavailable on this LuCI/OpenWrt version, "+
				"so per-section wireless state cannot be proven: %v", err), terminal: true}
	}
	return &wirelessRuntimeFailure{message: fmt.Sprintf(
		"health: could not read per-section wireless runtime state: %v", err)}
}

func wirelessStatusFailure(section, ifname string, err error) error {
	var denied *ubus.DeniedError
	if errors.As(err, &denied) {
		return &wirelessRuntimeFailure{message: fmt.Sprintf(
			"health: hostapd.%s.get_status for wireless.%s was denied; re-adopt the device "+
				"with the current controller ACL: %v", ifname, section, err), terminal: true}
	}
	return &wirelessRuntimeFailure{message: fmt.Sprintf(
		"health: hostapd runtime for wireless.%s (%s) is unavailable: %v",
		section, ifname, err)}
}

func bridgeIsolationFailure(section, ifname, requestedPath string, err error) error {
	var denied *ubus.DeniedError
	if errors.As(err, &denied) {
		return &wirelessRuntimeFailure{message: fmt.Sprintf(
			"health: file.read of %s for wireless.%s was denied; re-adopt the device with "+
				"the current controller ACL, which grants both /sys/class/net/*/brport/isolated "+
				"and its canonical /sys/devices/*/brport/isolated target: %v",
			requestedPath, section, err), terminal: true}
	}
	var status *ubus.StatusError
	if errors.As(err, &status) && (status.Status == ubus.StatusPermissionDenied ||
		status.Status == ubus.StatusNotSupported) {
		return &wirelessRuntimeFailure{message: fmt.Sprintf(
			"health: bridge-port isolation for wireless.%s (%s) cannot be read on this "+
				"OpenWrt/rpcd version: %v", section, ifname, err), terminal: true}
	}
	return &wirelessRuntimeFailure{message: fmt.Sprintf(
		"health: bridge-port isolation state %s for wireless.%s is unavailable; "+
			"this OpenWrt/netifd version may not implement bridge_isolate for the BSS: %v",
		requestedPath, section, err)}
}

func terminalWirelessRuntimeFailure(err error) bool {
	var failure *wirelessRuntimeFailure
	return errors.As(err, &failure) && failure.terminal
}
