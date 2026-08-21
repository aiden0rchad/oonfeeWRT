package reconcile

import (
	"context"
	"encoding/binary"
	"encoding/json"
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
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

type runtimeRouteExpectation struct {
	iface, target, gateway string
	metric                 int
	metricSet              bool
}

func (r runtimeRouteExpectation) key() string {
	metric := "*"
	if r.metricSet {
		metric = strconv.Itoa(r.metric)
	}
	return fmt.Sprintf("%s|%s|%s|%s", r.iface, r.target, r.gateway, metric)
}

func (r runtimeRouteExpectation) kernelKey(device string) string {
	return fmt.Sprintf("%s|%s|%d|%s", r.target, r.gateway, r.metric, device)
}

type runtimeLeaseExpectation struct {
	mac, ip string
	pool    runtimeDHCPPool
}

func (h runtimeLeaseExpectation) key() string { return h.mac + "|" + h.ip }

type runtimeDHCPPool struct {
	iface, address, rangeLine string
	prefix                    int
	subnet                    netip.Prefix
}

type policyRuntimeExpectation struct {
	routes, oldRoutes map[string]runtimeRouteExpectation
	leases, oldLeases map[string]runtimeLeaseExpectation
	settle            time.Duration
}

func composePolicyRuntimeHealth(plan *DevicePlan, next applyengine.HealthCheck) applyengine.HealthCheck {
	want, needed, buildErr := buildPolicyRuntimeExpectation(plan)
	if !needed {
		return next
	}
	return func(ctx context.Context, verify *ubus.Client) error {
		if buildErr != nil {
			return fmt.Errorf("managed route/fixed-IP runtime expectation: %w", buildErr)
		}
		if err := want.wait(ctx, verify); err != nil {
			return err
		}
		if next != nil {
			return next(ctx, verify)
		}
		return nil
	}
}

func composeManagedRuntimeHealth(plan *DevicePlan, next applyengine.HealthCheck) applyengine.HealthCheck {
	return composeFirewallRuntimeHealth(plan, composePolicyRuntimeHealth(plan, next))
}

func buildPolicyRuntimeExpectation(plan *DevicePlan) (*policyRuntimeExpectation, bool, error) {
	if plan == nil {
		return nil, false, nil
	}
	want := &policyRuntimeExpectation{
		routes: map[string]runtimeRouteExpectation{}, oldRoutes: map[string]runtimeRouteExpectation{},
		leases: map[string]runtimeLeaseExpectation{}, oldLeases: map[string]runtimeLeaseExpectation{},
	}
	networks := map[string]render.Section{}
	for _, section := range plan.Doc.Sections {
		if section.Config == "network" && section.Type == "interface" && section.Values[render.OwnershipTag] == "1" {
			networks[section.Name] = section
		}
	}
	pools := map[string]runtimeDHCPPool{}
	for _, section := range plan.Doc.Sections {
		if section.Config != "dhcp" || section.Type != "dhcp" || section.Values[render.OwnershipTag] != "1" {
			continue
		}
		network, ok := networks[section.Values["interface"]]
		if !ok {
			return want, true, fmt.Errorf("managed DHCP section %s has no rendered interface", section.Name)
		}
		pool, err := policyRuntimeDHCPPool(section, network)
		if err != nil {
			return want, true, err
		}
		pools[pool.iface] = pool
	}
	for _, section := range plan.Doc.Sections {
		if section.Values[render.OwnershipTag] != "1" {
			continue
		}
		switch {
		case section.Config == "network" && section.Type == "route":
			route, err := routeRuntimeExpectation(firewallSectionValues(section))
			if err != nil {
				return want, true, fmt.Errorf("%s: %w", section.Name, err)
			}
			want.routes[route.key()] = route
		case section.Config == "dhcp" && section.Type == "host":
			lease, err := leaseRuntimeExpectation(firewallSectionValues(section))
			if err != nil {
				return want, true, fmt.Errorf("%s: %w", section.Name, err)
			}
			addr, _ := netip.ParseAddr(lease.ip)
			for _, pool := range pools {
				if !pool.subnet.Contains(addr) {
					continue
				}
				if lease.pool.iface != "" {
					return want, true, fmt.Errorf("%s: managed fixed-IP address matches multiple DHCP interfaces", section.Name)
				}
				lease.pool = pool
			}
			if lease.pool.iface == "" {
				return want, true, fmt.Errorf("%s: managed fixed-IP address has no rendered DHCP pool", section.Name)
			}
			want.leases[lease.key()] = lease
		}
	}
	for _, values := range plan.Existing.In("network") {
		if values[render.OwnershipTag] != "1" || values[".type"] != "route" {
			continue
		}
		route, err := routeRuntimeExpectation(values)
		if err != nil {
			return want, true, err
		}
		want.oldRoutes[route.key()] = route
	}
	for _, values := range plan.Existing.In("dhcp") {
		if values[render.OwnershipTag] != "1" || values[".type"] != "host" {
			continue
		}
		lease, err := leaseRuntimeExpectation(values)
		if err != nil {
			return want, true, err
		}
		want.oldLeases[lease.key()] = lease
	}
	needed := len(want.routes)+len(want.oldRoutes)+len(want.leases)+len(want.oldLeases) > 0
	if !needed {
		return nil, false, nil
	}
	timeout := plan.Plan.Timeout
	if timeout <= 0 {
		timeout = applyengine.DefaultTimeout
	}
	want.settle = firewallSettleLimit
	if quarter := timeout / 4; quarter < want.settle {
		want.settle = quarter
	}
	if want.settle <= 0 {
		want.settle = firewallPollEvery
	}
	return want, true, nil
}

func routeRuntimeExpectation(values map[string]string) (runtimeRouteExpectation, error) {
	target, err := netip.ParsePrefix(strings.TrimSpace(values["target"]))
	if err != nil || !target.Addr().Is4() || target != target.Masked() {
		return runtimeRouteExpectation{}, errors.New("managed static route has no canonical IPv4 target")
	}
	gateway, err := netip.ParseAddr(strings.TrimSpace(values["gateway"]))
	if err != nil || !gateway.Is4() {
		return runtimeRouteExpectation{}, errors.New("managed static route has no IPv4 gateway")
	}
	iface := strings.TrimSpace(values["interface"])
	if iface == "" {
		return runtimeRouteExpectation{}, errors.New("managed static route has no interface")
	}
	metric, metricSet := 0, false
	if raw := strings.TrimSpace(values["metric"]); raw != "" {
		metricSet = true
		metric, err = strconv.Atoi(raw)
		if err != nil || metric < 0 {
			return runtimeRouteExpectation{}, errors.New("managed static route has an invalid metric")
		}
	}
	return runtimeRouteExpectation{iface: iface, target: target.String(), gateway: gateway.String(), metric: metric, metricSet: metricSet}, nil
}

func leaseRuntimeExpectation(values map[string]string) (runtimeLeaseExpectation, error) {
	mac := strings.ToLower(strings.TrimSpace(values["mac"]))
	if mac == "" || strings.ContainsAny(mac, " ,") {
		return runtimeLeaseExpectation{}, errors.New("managed fixed-IP host has no single MAC")
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(values["ip"]))
	if err != nil || !ip.Is4() {
		return runtimeLeaseExpectation{}, errors.New("managed fixed-IP host has no IPv4 address")
	}
	return runtimeLeaseExpectation{mac: mac, ip: ip.String()}, nil
}

func policyRuntimeDHCPPool(dhcp, network render.Section) (runtimeDHCPPool, error) {
	pool := runtimeDHCPPool{iface: dhcp.Values["interface"], address: network.Values["ipaddr"]}
	addr, err := netip.ParseAddr(pool.address)
	maskIP := net.ParseIP(network.Values["netmask"]).To4()
	if err != nil || !addr.Is4() || maskIP == nil {
		return pool, fmt.Errorf("managed DHCP section %s has an invalid rendered IPv4 interface", dhcp.Name)
	}
	ones, bits := net.IPMask(maskIP).Size()
	if bits != 32 || ones < 0 {
		return pool, fmt.Errorf("managed DHCP section %s has an invalid rendered netmask", dhcp.Name)
	}
	start, err := strconv.ParseUint(dhcp.Values["start"], 10, 32)
	if err != nil {
		return pool, fmt.Errorf("managed DHCP section %s has an invalid rendered pool start", dhcp.Name)
	}
	limit, err := strconv.ParseUint(dhcp.Values["limit"], 10, 32)
	if err != nil || limit == 0 {
		return pool, fmt.Errorf("managed DHCP section %s has an invalid rendered pool limit", dhcp.Name)
	}
	a := addr.As4()
	base := binary.BigEndian.Uint32(a[:]) & binary.BigEndian.Uint32(maskIP)
	last := start + limit - 1
	if start > uint64(^uint32(0))-uint64(base) || last > uint64(^uint32(0))-uint64(base) {
		return pool, fmt.Errorf("managed DHCP section %s has a rendered pool outside IPv4", dhcp.Name)
	}
	pool.prefix = ones
	pool.subnet = netip.PrefixFrom(addr, ones).Masked()
	pool.rangeLine = fmt.Sprintf("dhcp-range=set:%s,%s,%s,%s,%s", pool.iface,
		policyIPv4String(base+uint32(start)), policyIPv4String(base+uint32(last)),
		network.Values["netmask"], strings.TrimSpace(dhcp.Values["leasetime"]))
	return pool, nil
}

func policyIPv4String(v uint32) string {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], v)
	return netip.AddrFrom4(raw).String()
}

type policyObservationError struct{ err error }

func (e *policyObservationError) Error() string { return e.err.Error() }
func (e *policyObservationError) Unwrap() error { return e.err }

func (want *policyRuntimeExpectation) wait(ctx context.Context, caller ubusCaller) error {
	deadline := time.Now().Add(want.settle)
	var last error
	for {
		last = want.verifyOnce(ctx, caller)
		if last == nil {
			return nil
		}
		var observation *policyObservationError
		if errors.As(last, &observation) {
			return fmt.Errorf("managed route/fixed-IP runtime unavailable after apply: %w", last)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("managed route/fixed-IP runtime did not settle after apply: %w", last)
		}
		pause := firewallPollEvery
		if remaining < pause {
			pause = remaining
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type policyInterfaceDump struct {
	Interfaces []struct {
		Name      string `json:"interface"`
		L3Device  string `json:"l3_device"`
		Up        bool   `json:"up"`
		Metric    int    `json:"metric"`
		IP4Table  int    `json:"ip4table"`
		Addresses []struct {
			Address string `json:"address"`
			Mask    int    `json:"mask"`
		} `json:"ipv4-address"`
		Route []struct {
			Target  string `json:"target"`
			Mask    int    `json:"mask"`
			Nexthop string `json:"nexthop"`
			Metric  *int   `json:"metric"`
			Failed  bool   `json:"failed"`
			Source  string `json:"source"`
			Table   *int   `json:"table"`
		} `json:"route"`
	} `json:"interface"`
}

type policyInterfaceObservation struct {
	device    string
	up        bool
	metric    int
	addresses map[string]bool
}

func (want *policyRuntimeExpectation) verifyOnce(ctx context.Context, caller ubusCaller) error {
	var dump policyInterfaceDump
	interfaces := map[string]policyInterfaceObservation{}
	if len(want.routes)+len(want.oldRoutes)+len(want.leases) > 0 {
		if err := caller.Call(ctx, "network.interface", "dump", struct{}{}, &dump); err != nil {
			return &policyObservationError{fmt.Errorf("network.interface dump: %w", err)}
		}
		for _, iface := range dump.Interfaces {
			observation := policyInterfaceObservation{device: strings.TrimSpace(iface.L3Device), up: iface.Up,
				metric: iface.Metric, addresses: map[string]bool{}}
			for _, address := range iface.Addresses {
				observation.addresses[fmt.Sprintf("%s/%d", strings.TrimSpace(address.Address), address.Mask)] = true
			}
			interfaces[iface.Name] = observation
		}
	}
	if len(want.routes)+len(want.oldRoutes) > 0 {
		var netifd []runtimeRouteExpectation
		for _, iface := range dump.Interfaces {
			for _, route := range iface.Route {
				table := iface.IP4Table
				if route.Table != nil {
					table = *route.Table
				}
				if route.Failed || !unconstrainedRouteSource(route.Source) || (table != 0 && table != 254) {
					continue
				}
				target, ok := runtimeRoutePrefix(route.Target, route.Mask)
				gateway, err := netip.ParseAddr(strings.TrimSpace(route.Nexthop))
				if !ok || err != nil || !gateway.Is4() {
					continue
				}
				metric := iface.Metric
				if route.Metric != nil {
					metric = *route.Metric
				}
				netifd = append(netifd, runtimeRouteExpectation{iface: iface.Name, target: target.String(), gateway: gateway.String(), metric: metric, metricSet: true})
			}
		}
		kernel, err := readKernelIPv4Routes(ctx, caller)
		if err != nil {
			return &policyObservationError{err}
		}
		for key := range want.routes {
			route, ok := effectiveRuntimeRoute(want.routes[key], interfaces)
			if !ok || !containsRuntimeRoute(netifd, route) || !kernel.exact[route.kernelKey(interfaces[route.iface].device)] {
				return fmt.Errorf("managed static route %s is absent or differs", key)
			}
		}
		for key := range want.oldRoutes {
			route, resolved := effectiveRuntimeRoute(want.oldRoutes[key], interfaces)
			if !resolved {
				route = want.oldRoutes[key]
			}
			desired := false
			for _, candidate := range want.routes {
				if effective, ok := effectiveRuntimeRoute(candidate, interfaces); ok && resolved && sameRuntimeRoute(effective, route) {
					desired = true
					break
				}
			}
			if (!desired && containsRuntimeRoute(netifd, route)) || kernelRouteStillPresent(kernel, route, want.routes, interfaces) {
				return fmt.Errorf("stale managed static route %s is still active", key)
			}
		}
	}
	if len(want.leases)+len(want.oldLeases) == 0 {
		return nil
	}
	instances, err := readDNSMasqRuntime(ctx, caller)
	if err != nil {
		return &policyObservationError{err}
	}
	if len(instances) == 0 && len(want.leases) > 0 {
		return errors.New("dnsmasq has no running instance for the managed fixed-IP host")
	}
	for key, lease := range want.leases {
		iface, ok := interfaces[lease.pool.iface]
		if !ok || !iface.up || !iface.addresses[fmt.Sprintf("%s/%d", lease.pool.address, lease.pool.prefix)] {
			return fmt.Errorf("managed fixed-IP host %s has no up managed interface %s with address %s/%d", key,
				lease.pool.iface, lease.pool.address, lease.pool.prefix)
		}
		addr, _ := netip.ParseAddr(lease.ip)
		applicable, exactRange := 0, false
		for _, instance := range instances {
			exactRange = exactRange || instance.exactRanges[lease.pool.rangeLine]
			if !instance.covers(addr) {
				continue
			}
			applicable++
			if !instance.hosts[key] {
				return fmt.Errorf("managed fixed-IP host %s is absent or conditional in applicable dnsmasq instance %s", key, instance.path)
			}
		}
		if applicable == 0 {
			return fmt.Errorf("managed fixed-IP host %s has no running dnsmasq DHCP range covering its address", key)
		}
		if !exactRange {
			return fmt.Errorf("managed fixed-IP host %s has no exact managed dnsmasq range %s", key, lease.pool.rangeLine)
		}
	}
	for key := range want.oldLeases {
		if _, desired := want.leases[key]; desired {
			continue
		}
		for _, instance := range instances {
			if instance.allHosts[key] {
				return fmt.Errorf("stale managed fixed-IP host %s is still active in dnsmasq instance %s", key, instance.path)
			}
		}
	}
	return nil
}

func unconstrainedRouteSource(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "all") {
		return true
	}
	prefix, err := netip.ParsePrefix(raw)
	return err == nil && prefix.Addr().Is4() && prefix.Masked() == netip.MustParsePrefix("0.0.0.0/0")
}

func effectiveRuntimeRoute(route runtimeRouteExpectation, interfaces map[string]policyInterfaceObservation) (runtimeRouteExpectation, bool) {
	iface, ok := interfaces[route.iface]
	if !ok || iface.device == "" {
		return route, false
	}
	if !route.metricSet {
		route.metric, route.metricSet = iface.metric, true
	}
	return route, true
}

func sameRuntimeRoute(a, b runtimeRouteExpectation) bool {
	return a.iface == b.iface && a.target == b.target && a.gateway == b.gateway &&
		a.metricSet && b.metricSet && a.metric == b.metric
}

func containsRuntimeRoute(routes []runtimeRouteExpectation, want runtimeRouteExpectation) bool {
	for _, route := range routes {
		if route.iface == want.iface && route.target == want.target && route.gateway == want.gateway &&
			(!want.metricSet || route.metric == want.metric) {
			return true
		}
	}
	return false
}

func runtimeRoutePrefix(target string, mask int) (netip.Prefix, bool) {
	if prefix, err := netip.ParsePrefix(strings.TrimSpace(target)); err == nil && prefix.Addr().Is4() {
		return prefix.Masked(), prefix.Bits() == mask || mask == 0
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(target))
	if err != nil || !addr.Is4() || mask < 0 || mask > 32 {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(addr, mask).Masked(), true
}

type kernelRouteObservation struct {
	target, gateway, device string
	metric                  int
	main, unconstrained     bool
	unicast                 bool
}

type kernelRouteSnapshot struct {
	exact map[string]bool
	all   []kernelRouteObservation
}

func readKernelIPv4Routes(ctx context.Context, caller ubusCaller) (kernelRouteSnapshot, error) {
	var result nftExecResult
	jsonErr := caller.Call(ctx, "file", "exec", map[string]any{
		"command": "/sbin/ip", "params": []string{"-4", "-j", "route", "show", "table", "all"},
	}, &result)
	if jsonErr == nil {
		if snapshot, err := parseKernelIPv4RoutesJSON(result); err == nil {
			return snapshot, nil
		} else {
			jsonErr = err
		}
	}

	// BusyBox ip (including OpenWrt 25.12's v1.37 build) has no -j. Keep the
	// structured path for ip-full, then fall back to the stock, package-free
	// text form. The argv is deliberately fixed so the rpcd ACL need not grant
	// arbitrary route lookups.
	result = nftExecResult{}
	if err := caller.Call(ctx, "file", "exec", map[string]any{
		"command": "/sbin/ip", "params": []string{"-4", "route", "show", "table", "all"},
	}, &result); err != nil {
		return kernelRouteSnapshot{}, fmt.Errorf("kernel IPv4 route table: JSON form unavailable (%v); BusyBox form: %w", jsonErr, err)
	}
	snapshot, err := parseKernelIPv4RoutesText(result)
	if err != nil {
		return kernelRouteSnapshot{}, fmt.Errorf("kernel IPv4 route table: JSON form unavailable (%v); BusyBox form: %w", jsonErr, err)
	}
	return snapshot, nil
}

func parseKernelIPv4RoutesJSON(result nftExecResult) (kernelRouteSnapshot, error) {
	if result.Code != 0 || len(result.Stdout) == 0 || len(result.Stdout) > nftRulesetLimit {
		return kernelRouteSnapshot{}, errors.New("kernel IPv4 route table returned no bounded successful JSON result")
	}
	var rows []struct {
		Destination string          `json:"dst"`
		Gateway     string          `json:"gateway"`
		Device      string          `json:"dev"`
		Metric      int             `json:"metric"`
		Table       json.RawMessage `json:"table"`
		From        string          `json:"from"`
		Type        string          `json:"type"`
	}
	if json.Unmarshal([]byte(result.Stdout), &rows) != nil {
		return kernelRouteSnapshot{}, errors.New("kernel IPv4 route table returned unreadable JSON")
	}
	out := newKernelRouteSnapshot()
	for _, row := range rows {
		target, ok := kernelRoutePrefix(row.Destination)
		if !ok {
			continue
		}
		gateway := ""
		if parsed, err := netip.ParseAddr(strings.TrimSpace(row.Gateway)); err == nil && parsed.Is4() {
			gateway = parsed.String()
		}
		kind := strings.TrimSpace(row.Type)
		observation := kernelRouteObservation{target: target.String(), gateway: gateway, device: strings.TrimSpace(row.Device),
			metric: row.Metric, main: kernelMainTable(row.Table), unconstrained: unconstrainedRouteSource(row.From),
			unicast: kind == "" || strings.EqualFold(kind, "unicast")}
		out.add(observation)
	}
	return out, nil
}

func parseKernelIPv4RoutesText(result nftExecResult) (kernelRouteSnapshot, error) {
	if result.Code != 0 || len(result.Stdout) == 0 || len(result.Stdout) > nftRulesetLimit {
		return kernelRouteSnapshot{}, errors.New("kernel IPv4 route table returned no bounded successful text result")
	}
	out := newKernelRouteSnapshot()
	rows := 0
	for lineNumber, raw := range strings.Split(result.Stdout, "\n") {
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			continue
		}
		observation, err := parseKernelIPv4RouteText(fields)
		if err != nil {
			return kernelRouteSnapshot{}, fmt.Errorf("unreadable text row %d: %w", lineNumber+1, err)
		}
		out.add(observation)
		rows++
	}
	if rows == 0 {
		return kernelRouteSnapshot{}, errors.New("kernel IPv4 route table returned no text rows")
	}
	return out, nil
}

func newKernelRouteSnapshot() kernelRouteSnapshot {
	return kernelRouteSnapshot{exact: map[string]bool{}}
}

func (snapshot *kernelRouteSnapshot) add(observation kernelRouteObservation) {
	snapshot.all = append(snapshot.all, observation)
	if observation.gateway == "" || observation.device == "" || !observation.main ||
		!observation.unconstrained || !observation.unicast {
		return
	}
	route := runtimeRouteExpectation{target: observation.target, gateway: observation.gateway, metric: observation.metric}
	snapshot.exact[route.kernelKey(observation.device)] = true
}

func parseKernelIPv4RouteText(fields []string) (kernelRouteObservation, error) {
	observation := kernelRouteObservation{main: true, unconstrained: true, unicast: true}
	index := 0
	typeSet := false
	if kernelRouteType(fields[0]) {
		observation.unicast = strings.EqualFold(fields[0], "unicast")
		typeSet = true
		index++
	}
	if index >= len(fields) {
		return observation, errors.New("route type has no destination")
	}
	target := fields[index]
	index++
	if target == "default" {
		target = "0.0.0.0/0"
	}
	prefix, ok := kernelRoutePrefix(target)
	if !ok {
		return observation, fmt.Errorf("invalid IPv4 destination %q", target)
	}
	observation.target = prefix.String()
	gatewaySet, deviceSet, metricSet, tableSet, sourceSet := false, false, false, false, false

	for index < len(fields) {
		keyword := fields[index]
		index++
		switch keyword {
		case "via":
			if gatewaySet {
				return observation, errors.New("route has multiple gateways")
			}
			gatewaySet = true
			value, next, ok := kernelRouteTextValue(fields, index)
			if !ok {
				return observation, errors.New("via has no gateway")
			}
			index = next
			if value == "inet" {
				value, index, ok = kernelRouteTextValue(fields, index)
				if !ok {
					return observation, errors.New("via inet has no gateway")
				}
			}
			gateway, err := netip.ParseAddr(value)
			if err != nil || !gateway.Is4() {
				return observation, fmt.Errorf("invalid IPv4 gateway %q", value)
			}
			observation.gateway = gateway.String()
		case "dev":
			if deviceSet {
				return observation, errors.New("route has multiple interfaces")
			}
			deviceSet = true
			value, next, ok := kernelRouteTextValue(fields, index)
			if !ok {
				return observation, errors.New("dev has no interface")
			}
			observation.device, index = value, next
		case "metric":
			if metricSet {
				return observation, errors.New("route has multiple metrics")
			}
			metricSet = true
			value, next, ok := kernelRouteTextValue(fields, index)
			if !ok {
				return observation, errors.New("metric has no value")
			}
			metric, err := strconv.Atoi(value)
			if err != nil || metric < 0 {
				return observation, fmt.Errorf("invalid metric %q", value)
			}
			observation.metric, index = metric, next
		case "table":
			if tableSet {
				return observation, errors.New("route has multiple tables")
			}
			tableSet = true
			value, next, ok := kernelRouteTextValue(fields, index)
			if !ok {
				return observation, errors.New("table has no value")
			}
			observation.main, index = value == "main" || value == "254", next
		case "from":
			if sourceSet {
				return observation, errors.New("route has multiple sources")
			}
			sourceSet = true
			value, next, ok := kernelRouteTextValue(fields, index)
			if !ok {
				return observation, errors.New("from has no source")
			}
			unconstrained, valid := kernelRouteTextSource(value)
			if !valid {
				return observation, fmt.Errorf("invalid IPv4 source %q", value)
			}
			observation.unconstrained, index = unconstrained, next
		case "type":
			if typeSet {
				return observation, errors.New("route has multiple types")
			}
			typeSet = true
			value, next, ok := kernelRouteTextValue(fields, index)
			if !ok || !kernelRouteType(value) {
				return observation, fmt.Errorf("invalid route type %q", value)
			}
			observation.unicast, index = strings.EqualFold(value, "unicast"), next
		case "proto", "scope", "src", "pref", "expires", "mtu", "advmss", "hoplimit", "realm",
			"rtt", "rttvar", "rto_min", "ssthresh", "cwnd", "initcwnd", "initrwnd", "features",
			"quickack", "congctl", "fastopen_no_cookie", "uid", "nhid":
			_, next, ok := kernelRouteTextValue(fields, index)
			if !ok {
				return observation, fmt.Errorf("%s has no value", keyword)
			}
			index = next
		case "onlink", "linkdown", "pervasive", "dead", "offload", "trap", "notify", "cache":
			// Flags do not change the route identity this proof needs.
		default:
			return observation, fmt.Errorf("unsupported route attribute %q", keyword)
		}
	}
	return observation, nil
}

func kernelRouteTextValue(fields []string, index int) (string, int, bool) {
	if index >= len(fields) || fields[index] == "" {
		return "", index, false
	}
	return fields[index], index + 1, true
}

func kernelRouteTextSource(raw string) (bool, bool) {
	if strings.EqualFold(raw, "all") {
		return true, true
	}
	prefix, err := netip.ParsePrefix(raw)
	if err == nil && prefix.Addr().Is4() {
		return prefix.Masked() == netip.MustParsePrefix("0.0.0.0/0"), true
	}
	addr, err := netip.ParseAddr(raw)
	return false, err == nil && addr.Is4()
}

func kernelRouteType(raw string) bool {
	switch strings.ToLower(raw) {
	case "unicast", "local", "broadcast", "multicast", "throw", "unreachable", "prohibit", "blackhole", "nat", "anycast":
		return true
	default:
		return false
	}
}

func kernelMainTable(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		return name == "main" || name == "254"
	}
	var number int
	return json.Unmarshal(raw, &number) == nil && number == 254
}

func kernelRoutePrefix(raw string) (netip.Prefix, bool) {
	if prefix, err := netip.ParsePrefix(strings.TrimSpace(raw)); err == nil && prefix.Addr().Is4() {
		return prefix.Masked(), true
	}
	if addr, err := netip.ParseAddr(strings.TrimSpace(raw)); err == nil && addr.Is4() {
		return netip.PrefixFrom(addr, 32), true
	}
	return netip.Prefix{}, false
}

func kernelRouteStillPresent(kernel kernelRouteSnapshot, route runtimeRouteExpectation,
	desired map[string]runtimeRouteExpectation, interfaces map[string]policyInterfaceObservation) bool {
	for _, observation := range kernel.all {
		if observation.target != route.target || (observation.gateway != "" && observation.gateway != route.gateway) ||
			(route.metricSet && observation.metric != route.metric) {
			continue
		}
		expected := false
		for _, candidate := range desired {
			effective, ok := effectiveRuntimeRoute(candidate, interfaces)
			if ok && observation.target == effective.target && observation.gateway == effective.gateway &&
				observation.device == interfaces[effective.iface].device && observation.metric == effective.metric &&
				observation.main && observation.unconstrained && observation.unicast {
				expected = true
				break
			}
		}
		if !expected {
			return true
		}
	}
	return false
}

type policyDNSMasqService struct {
	Instances map[string]struct {
		Running bool     `json:"running"`
		PID     int      `json:"pid"`
		Command []string `json:"command"`
	} `json:"instances"`
}

type policyDNSMasqRuntime struct {
	path        string
	hosts       map[string]bool
	allHosts    map[string]bool
	exactRanges map[string]bool
	ranges      []netip.Prefix
}

func (runtime policyDNSMasqRuntime) covers(addr netip.Addr) bool {
	for _, prefix := range runtime.ranges {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func readDNSMasqRuntime(ctx context.Context, caller ubusCaller) ([]policyDNSMasqRuntime, error) {
	services := map[string]policyDNSMasqService{}
	if err := caller.Call(ctx, "service", "list", map[string]any{"name": "dnsmasq", "verbose": true}, &services); err != nil {
		return nil, fmt.Errorf("service.list dnsmasq: %w", err)
	}
	var paths []string
	for _, instance := range services["dnsmasq"].Instances {
		if !instance.Running {
			continue
		}
		if instance.PID <= 0 {
			return nil, errors.New("dnsmasq reports a running instance without a positive pid")
		}
		config, ok := policyDNSMasqConfigPath(instance.Command)
		if !ok {
			return nil, errors.New("running dnsmasq does not expose a safe runtime config path")
		}
		paths = append(paths, config)
	}
	if len(paths) == 0 {
		return []policyDNSMasqRuntime{}, nil
	}
	sort.Strings(paths)
	paths = compactPolicyStrings(paths)
	instances := make([]policyDNSMasqRuntime, 0, len(paths))
	for _, config := range paths {
		var file struct {
			Data string `json:"data"`
		}
		if err := caller.Call(ctx, "file", "read", map[string]string{"path": config}, &file); err != nil {
			return nil, fmt.Errorf("read dnsmasq runtime config: %w", err)
		}
		runtime := policyDNSMasqRuntime{path: config, hosts: map[string]bool{}, allHosts: map[string]bool{}, exactRanges: map[string]bool{}}
		for _, raw := range strings.Split(file.Data, "\n") {
			line := strings.TrimSpace(raw)
			switch {
			case strings.HasPrefix(line, "dhcp-host="):
				fields := strings.Split(strings.TrimPrefix(line, "dhcp-host="), ",")
				key := dnsmasqHostKey(fields)
				if key != "" {
					runtime.allHosts[key] = true
					if len(fields) == 2 {
						runtime.hosts[key] = true
					}
				}
			case strings.HasPrefix(line, "dhcp-range="):
				rawRange := strings.TrimPrefix(line, "dhcp-range=")
				prefix, ipv4, err := dnsmasqIPv4Range(rawRange)
				if err != nil {
					return nil, fmt.Errorf("read dnsmasq runtime config %s: %w", config, err)
				}
				if ipv4 {
					runtime.ranges = append(runtime.ranges, prefix)
					runtime.exactRanges["dhcp-range="+rawRange] = true
				}
			}
		}
		instances = append(instances, runtime)
	}
	return instances, nil
}

func dnsmasqHostKey(fields []string) string {
	mac, ip := "", ""
	for _, field := range fields {
		value := strings.TrimSpace(field)
		if mac == "" {
			if parsed, err := net.ParseMAC(value); err == nil && len(parsed) == 6 {
				mac = strings.ToLower(parsed.String())
			}
		}
		if ip == "" {
			if parsed, err := netip.ParseAddr(value); err == nil && parsed.Is4() {
				ip = parsed.String()
			}
		}
	}
	if mac == "" || ip == "" {
		return ""
	}
	return (runtimeLeaseExpectation{mac: mac, ip: ip}).key()
}

func compactPolicyStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func dnsmasqIPv4Range(raw string) (netip.Prefix, bool, error) {
	fields := strings.Split(raw, ",")
	startIndex := -1
	var start netip.Addr
	for i, field := range fields {
		addr, err := netip.ParseAddr(strings.TrimSpace(field))
		if err == nil && addr.Is4() {
			startIndex, start = i, addr
			break
		}
	}
	if startIndex < 0 {
		if strings.Contains(raw, ":") || strings.Contains(raw, "constructor:") {
			return netip.Prefix{}, false, nil
		}
		return netip.Prefix{}, false, errors.New("unreadable IPv4 dhcp-range")
	}
	for _, field := range fields[startIndex+1:] {
		mask := net.ParseIP(strings.TrimSpace(field)).To4()
		if mask == nil {
			continue
		}
		ones, bits := net.IPMask(mask).Size()
		if bits == 32 && ones >= 0 {
			return netip.PrefixFrom(start, ones).Masked(), true, nil
		}
	}
	return netip.Prefix{}, false, errors.New("IPv4 dhcp-range has no explicit contiguous netmask")
}

func policyDNSMasqConfigPath(command []string) (string, bool) {
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
