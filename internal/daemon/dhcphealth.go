package daemon

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

const dhcpSettleTimeout = 20 * time.Second

type dhcpRuntimeCaller interface {
	Call(context.Context, string, string, any, any) error
}

type dhcpRuntimePool struct {
	section, iface, address string
	prefix                  int
	rangeLine               string
}

type dhcpRuntimePlan struct {
	enabled []dhcpRuntimePool
	absent  []string
	err     error
}

type dhcpInterfaceDump struct {
	Interfaces []struct {
		Name      string `json:"interface"`
		Up        bool   `json:"up"`
		Addresses []struct {
			Address string `json:"address"`
			Mask    int    `json:"mask"`
		} `json:"ipv4-address"`
	} `json:"interface"`
}

type dnsmasqService struct {
	Instances map[string]struct {
		Running bool     `json:"running"`
		PID     int      `json:"pid"`
		Command []string `json:"command"`
	} `json:"instances"`
}

type dhcpFileRead struct {
	Data string `json:"data"`
}

// ownedInterfacesForPlan names L3/L2 interfaces whose runtime is part of this
// apply's claim. A BSS can be healthy in hostapd while its UCI network is
// absent or down, leaving associated clients isolated.
func ownedInterfacesForPlan(plan *reconcile.DevicePlan) []string {
	if plan == nil {
		return nil
	}
	touched := map[string]bool{}
	for _, op := range plan.Plan.Ops {
		if op.Config == "network" {
			touched[op.Section] = true
		}
	}
	required := map[string]bool{}
	for _, section := range plan.Doc.Sections {
		if section.Config == "network" && section.Type == "interface" &&
			section.Values[render.OwnershipTag] == "1" && touched[section.Name] {
			required[section.Name] = true
		}
		if section.Config != "wireless" || section.Values[render.OwnershipTag] != "1" {
			continue
		}
		for _, iface := range strings.Fields(section.Values["network"]) {
			if strings.HasPrefix(iface, render.NamePrefix+"_net_") {
				required[iface] = true
			}
		}
	}
	out := make([]string, 0, len(required))
	for iface := range required {
		out = append(out, iface)
	}
	sort.Strings(out)
	return out
}

func waitForOwnedInterfaces(ctx context.Context, c dhcpRuntimeCaller, required []string) error {
	if len(required) == 0 {
		return nil
	}
	deadline := time.Now().Add(dhcpSettleTimeout)
	var last error
	for {
		last = checkOwnedInterfacesOnce(ctx, c, required)
		if last == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w; managed interfaces did not settle within %s, so the device will revert",
				last, dhcpSettleTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func checkOwnedInterfacesOnce(ctx context.Context, c dhcpRuntimeCaller, required []string) error {
	var dump dhcpInterfaceDump
	if err := c.Call(ctx, "network.interface", "dump", struct{}{}, &dump); err != nil {
		return fmt.Errorf("health: could not read managed interface runtime: %w", err)
	}
	state := map[string]bool{}
	for _, iface := range dump.Interfaces {
		state[iface.Name] = iface.Up
	}
	var unavailable []string
	for _, iface := range required {
		up, present := state[iface]
		switch {
		case !present:
			unavailable = append(unavailable, iface+" (absent)")
		case !up:
			unavailable = append(unavailable, iface+" (down)")
		}
	}
	if len(unavailable) > 0 {
		return fmt.Errorf("health: managed interface required by this plan is not up: %v", unavailable)
	}
	return nil
}

// dhcpRuntimePlanFor narrows the runtime gate to this apply's DHCP claims.
// A touched WLAN on an owned routed network depends on that network's pool too:
// confirming a broadcasting guest BSS while its unchanged DHCP range is absent
// would record a network clients cannot use. AP-only attachments render no pool
// and retain no DHCP runtime dependency.
func dhcpRuntimePlanFor(plan *reconcile.DevicePlan) *dhcpRuntimePlan {
	if plan == nil {
		return nil
	}
	touched := map[render.SectionRef]bool{}
	for _, op := range plan.Plan.Ops {
		touched[render.SectionRef{Config: op.Config, Name: op.Section}] = true
	}

	networks := map[string]render.Section{}
	wlanNetworks := map[string]bool{}
	for _, section := range plan.Doc.Sections {
		if section.Config == "network" && section.Type == "interface" &&
			section.Values[render.OwnershipTag] == "1" {
			networks[section.Name] = section
		}
		if section.Config == "wireless" && section.Values[render.OwnershipTag] == "1" &&
			touched[render.SectionRef{Config: "wireless", Name: section.Name}] {
			for _, iface := range strings.Fields(section.Values["network"]) {
				wlanNetworks[iface] = true
			}
		}
	}

	check := &dhcpRuntimePlan{}
	absent := map[string]bool{}
	enabled := map[string]bool{}
	for _, section := range plan.Doc.Sections {
		if section.Config != "dhcp" || section.Type != "dhcp" ||
			section.Values[render.OwnershipTag] != "1" {
			continue
		}
		iface := section.Values["interface"]
		if !touched[render.SectionRef{Config: "dhcp", Name: section.Name}] &&
			!touched[render.SectionRef{Config: "network", Name: iface}] &&
			!wlanNetworks[iface] {
			continue
		}
		network, ok := networks[iface]
		if !ok {
			check.err = fmt.Errorf("health: rendered DHCP section %s has no owned runtime interface %s",
				section.Name, iface)
			return check
		}
		pool, err := buildDHCPRuntimePool(section, network)
		if err != nil {
			check.err = err
			return check
		}
		check.enabled = append(check.enabled, pool)
		enabled[iface] = true

		if previous := plan.Existing.In("dhcp")[section.Name]; previous[render.OwnershipTag] == "1" && previous["interface"] != "" &&
			previous["interface"] != iface {
			absent[previous["interface"]] = true
		}
	}

	// A disabled or deleted controller pool renders no desired DHCP section.
	// Use the previous owned section to name only the interface-tagged range we
	// must stop; this makes no claim that foreign DHCP is absent.
	for _, op := range plan.Plan.Ops {
		if op.Config != "dhcp" || op.Kind != applyengine.OpDelete || op.Option != "" {
			continue
		}
		previous := plan.Existing.In("dhcp")[op.Section]
		if previous[render.OwnershipTag] == "1" && previous["interface"] != "" {
			absent[previous["interface"]] = true
		}
	}
	for iface := range enabled {
		delete(absent, iface)
	}
	for iface := range absent {
		check.absent = append(check.absent, iface)
	}
	sort.Strings(check.absent)
	if len(check.enabled) == 0 && len(check.absent) == 0 && check.err == nil {
		return nil
	}
	return check
}

func buildDHCPRuntimePool(dhcp, network render.Section) (dhcpRuntimePool, error) {
	pool := dhcpRuntimePool{section: dhcp.Name, iface: dhcp.Values["interface"],
		address: network.Values["ipaddr"]}
	addr, err := netip.ParseAddr(pool.address)
	if err != nil || !addr.Is4() {
		return pool, fmt.Errorf("health: DHCP section %s has an invalid rendered IPv4 address", dhcp.Name)
	}
	maskIP := net.ParseIP(network.Values["netmask"]).To4()
	if maskIP == nil {
		return pool, fmt.Errorf("health: DHCP section %s has an invalid rendered netmask", dhcp.Name)
	}
	mask := net.IPMask(maskIP)
	ones, bits := mask.Size()
	if bits != 32 || ones < 0 {
		return pool, fmt.Errorf("health: DHCP section %s has a non-contiguous rendered netmask", dhcp.Name)
	}
	start, err := strconv.ParseUint(dhcp.Values["start"], 10, 32)
	if err != nil {
		return pool, fmt.Errorf("health: DHCP section %s has an invalid rendered pool start", dhcp.Name)
	}
	limit, err := strconv.ParseUint(dhcp.Values["limit"], 10, 32)
	if err != nil || limit == 0 {
		return pool, fmt.Errorf("health: DHCP section %s has an invalid rendered pool limit", dhcp.Name)
	}
	a := addr.As4()
	base := binary.BigEndian.Uint32(a[:]) & binary.BigEndian.Uint32(maskIP)
	last := start + limit - 1
	if start > uint64(^uint32(0))-uint64(base) || last > uint64(^uint32(0))-uint64(base) {
		return pool, fmt.Errorf("health: DHCP section %s has a rendered pool outside IPv4", dhcp.Name)
	}
	pool.prefix = ones
	pool.rangeLine = fmt.Sprintf("dhcp-range=set:%s,%s,%s,%s,%s", pool.iface,
		ipv4String(base+uint32(start)), ipv4String(base+uint32(last)),
		network.Values["netmask"], strings.TrimSpace(dhcp.Values["leasetime"]))
	return pool, nil
}

func ipv4String(v uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b).String()
}

func waitForDHCPRuntime(ctx context.Context, c dhcpRuntimeCaller, check *dhcpRuntimePlan) error {
	if check == nil {
		return nil
	}
	if check.err != nil {
		return check.err
	}
	deadline := time.Now().Add(dhcpSettleTimeout)
	var last error
	for {
		last = checkDHCPRuntimeOnce(ctx, c, check)
		if last == nil || dhcpObservationPermanent(last) {
			return last
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w; DHCP runtime did not settle within %s, so the device will revert",
				last, dhcpSettleTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func checkDHCPRuntimeOnce(ctx context.Context, c dhcpRuntimeCaller, check *dhcpRuntimePlan) error {
	if check.err != nil {
		return check.err
	}
	if len(check.enabled) > 0 {
		var dump dhcpInterfaceDump
		if err := c.Call(ctx, "network.interface", "dump", struct{}{}, &dump); err != nil {
			return dhcpObservationError("managed interface runtime", err)
		}
		for _, pool := range check.enabled {
			if err := proveDHCPInterface(pool, dump); err != nil {
				return err
			}
		}
	}

	services := map[string]dnsmasqService{}
	if err := c.Call(ctx, "service", "list",
		map[string]any{"name": "dnsmasq", "verbose": true}, &services); err != nil {
		return dhcpObservationError("service.list for dnsmasq", err)
	}
	paths := []string{}
	for _, instance := range services["dnsmasq"].Instances {
		if !instance.Running {
			continue
		}
		if instance.PID <= 0 {
			return errors.New("health: dnsmasq reports a running instance without a positive pid")
		}
		path, ok := dnsmasqConfigPath(instance.Command)
		if !ok {
			return errors.New("health: a running dnsmasq instance does not expose a safe -C runtime config path")
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		if len(check.enabled) == 0 {
			return nil // no process can serve the controller-owned pool we removed
		}
		return errors.New("health: dnsmasq has no running instance for the enabled controller-owned DHCP pool")
	}
	sort.Strings(paths)
	paths = compactStrings(paths)

	exact := map[string]bool{}
	activeIfaces := map[string]bool{}
	for _, path := range paths {
		var file dhcpFileRead
		if err := c.Call(ctx, "file", "read", map[string]string{"path": path}, &file); err != nil {
			return dhcpObservationError("dnsmasq runtime config", err)
		}
		for _, raw := range strings.Split(file.Data, "\n") {
			line := strings.TrimSpace(raw)
			if strings.HasPrefix(line, "dhcp-range=") {
				exact[line] = true
				for _, field := range strings.Split(strings.TrimPrefix(line, "dhcp-range="), ",") {
					if strings.HasPrefix(field, "set:") {
						activeIfaces[strings.TrimPrefix(field, "set:")] = true
					}
				}
			}
		}
	}
	for _, pool := range check.enabled {
		if !exact[pool.rangeLine] {
			return fmt.Errorf("health: running dnsmasq does not contain the exact controller-owned range for %s (%s)",
				pool.iface, pool.section)
		}
	}
	for _, iface := range check.absent {
		if activeIfaces[iface] {
			return fmt.Errorf("health: a dnsmasq range still targets %s, so the removed controller-owned pool cannot be proved inactive; this does not assert whether other DHCP servers are present", iface)
		}
	}
	return nil
}

func proveDHCPInterface(pool dhcpRuntimePool, dump dhcpInterfaceDump) error {
	for _, iface := range dump.Interfaces {
		if iface.Name != pool.iface {
			continue
		}
		if !iface.Up {
			return fmt.Errorf("health: managed DHCP interface %s is down", pool.iface)
		}
		for _, address := range iface.Addresses {
			if address.Address == pool.address && address.Mask == pool.prefix {
				return nil
			}
		}
		return fmt.Errorf("health: managed DHCP interface %s is up without expected address %s/%d",
			pool.iface, pool.address, pool.prefix)
	}
	return fmt.Errorf("health: managed DHCP interface %s is absent", pool.iface)
}

func dnsmasqConfigPath(command []string) (string, bool) {
	var candidate string
	for i, arg := range command {
		switch {
		case arg == "-C" && i+1 < len(command):
			candidate = command[i+1]
		case strings.HasPrefix(arg, "-C") && len(arg) > 2:
			candidate = strings.TrimPrefix(arg, "-C")
		case strings.HasPrefix(arg, "--conf-file="):
			candidate = strings.TrimPrefix(arg, "--conf-file=")
		}
		if candidate != "" {
			break
		}
	}
	const prefix = "/var/etc/dnsmasq.conf."
	if path.Clean(candidate) != candidate || !strings.HasPrefix(candidate, prefix) {
		return "", false
	}
	suffix := strings.TrimPrefix(candidate, prefix)
	if suffix == "" || strings.Contains(suffix, "/") {
		return "", false
	}
	for _, r := range suffix {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.') {
			return "", false
		}
	}
	return candidate, true
}

func compactStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, value := range in[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

type dhcpRuntimeObservation struct {
	target string
	err    error
}

func dhcpObservationError(target string, err error) error {
	return &dhcpRuntimeObservation{target: target, err: err}
}

func (e *dhcpRuntimeObservation) Error() string {
	return fmt.Sprintf("health: controller cannot observe %s: %v. The current UI repair is to un-adopt this device (which first removes controller-owned device configuration and its controller login/ACL), then adopt it again so the current access-control file is installed; preview before restoring site configuration. The new ACL grants only service.list plus read-only access to the service path /var/etc/dnsmasq.conf.* and its canonical /tmp/etc/dnsmasq.conf.* target for this proof",
		e.target, e.err)
}

func (e *dhcpRuntimeObservation) Unwrap() error { return e.err }

func dhcpObservationPermanent(err error) bool {
	var observation *dhcpRuntimeObservation
	if !errors.As(err, &observation) {
		return false
	}
	var denied *ubus.DeniedError
	if errors.As(observation.err, &denied) {
		// Health runs inside the applying session's confirm window. That session
		// just staged and applied successfully, so a denial of this new method is
		// an ACL gap; re-login is deliberately suppressed until confirm.
		return true
	}
	var status *ubus.StatusError
	if errors.As(observation.err, &status) {
		switch status.Status {
		case ubus.StatusPermissionDenied, ubus.StatusMethodNotFound,
			ubus.StatusNotSupported, ubus.StatusInvalidCommand:
			return true
		}
	}
	var protocol *ubus.ProtocolError
	return errors.As(observation.err, &protocol)
}
