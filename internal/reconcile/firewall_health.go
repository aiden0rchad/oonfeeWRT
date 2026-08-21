package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

const (
	firewallSettleLimit = 15 * time.Second
	firewallPollEvery   = 500 * time.Millisecond
)

type firewallService struct {
	comment string
	kind    string
	chain   string
}

type firewallRuntimeExpectation struct {
	touchedSources map[string]bool
	desiredZones   map[string]bool
	desiredDevices map[string]map[string]bool
	desiredEdges   map[string]map[string]bool
	destinationSet map[string]bool
	desiredService map[string]firewallService
	oldService     map[string]bool
	desiredPolicy  map[string]firewallPolicyExpectation
	oldPolicy      map[string]bool
	observeInput   bool
	settle         time.Duration
}

// composeFirewallRuntimeHealth makes a committed UCI firewall change prove
// that fw4 loaded the corresponding runtime policy before it can be confirmed.
// Plans without controller-owned firewall operations retain their old health
// dependencies (important for APs and legacy installs without managed zones).
func composeFirewallRuntimeHealth(plan *DevicePlan, next applyengine.HealthCheck) applyengine.HealthCheck {
	want, needed, buildErr := buildFirewallRuntimeExpectation(plan)
	if !needed {
		return next
	}
	return func(ctx context.Context, verify *ubus.Client) error {
		if buildErr != nil {
			return fmt.Errorf("managed firewall runtime expectation: %w", buildErr)
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

func buildFirewallRuntimeExpectation(plan *DevicePlan) (*firewallRuntimeExpectation, bool, error) {
	if plan == nil {
		return nil, false, nil
	}
	desiredByName := map[string]render.Section{}
	desiredNetworkByName := map[string]render.Section{}
	desiredWirelessByName := map[string]render.Section{}
	referencedNetworks := map[string]bool{}
	for _, section := range plan.Doc.Sections {
		if section.Config == "firewall" {
			desiredByName[section.Name] = section
			if section.Type == "zone" && section.Values[render.OwnershipTag] == "1" {
				for _, network := range section.Lists["network"] {
					referencedNetworks[network] = true
				}
			}
		}
		if section.Config == "network" && section.Type == "interface" {
			desiredNetworkByName[section.Name] = section
		}
		if section.Config == "wireless" && section.Type == "wifi-iface" {
			desiredWirelessByName[section.Name] = section
		}
	}
	for _, section := range plan.Existing.In("firewall") {
		if section[".type"] == "zone" && section[render.OwnershipTag] == "1" {
			for _, network := range strings.Fields(section["network"]) {
				referencedNetworks[network] = true
			}
		}
	}

	want := &firewallRuntimeExpectation{
		touchedSources: map[string]bool{},
		desiredZones:   map[string]bool{},
		desiredDevices: map[string]map[string]bool{},
		desiredEdges:   map[string]map[string]bool{},
		// lan is device-owned and can never be selected as a managed
		// destination, but it must remain in the runtime observation scope. A
		// stale source -> lan forwarding is exactly the no-management-LAN policy
		// this verifier must reject rather than overlook.
		destinationSet: map[string]bool{"lan": true, "wan": true},
		desiredService: map[string]firewallService{},
		oldService:     map[string]bool{},
		desiredPolicy:  map[string]firewallPolicyExpectation{},
		oldPolicy:      map[string]bool{},
	}
	needed := false
	for _, op := range plan.Plan.Ops {
		if op.Config == "wireless" {
			if desired, ok := desiredWirelessByName[op.Section]; ok &&
				desired.Values[render.OwnershipTag] == "1" {
				for _, network := range strings.Fields(desired.Values["network"]) {
					if referencedNetworks[network] {
						needed = true
					}
				}
			}
			continue
		}
		if op.Config == "network" && referencedNetworks[op.Section] {
			desired, hasDesired := desiredNetworkByName[op.Section]
			old := plan.Existing.In("network")[op.Section]
			if hasDesired && desired.Values[render.OwnershipTag] == "1" ||
				old[render.OwnershipTag] == "1" {
				needed = true
			}
			continue
		}
		if op.Config != "firewall" {
			continue
		}
		desired, hasDesired := desiredByName[op.Section]
		old := plan.Existing.In("firewall")[op.Section]
		desiredOwned := hasDesired && desired.Values[render.OwnershipTag] == "1"
		oldOwned := old[render.OwnershipTag] == "1"
		if !desiredOwned && !oldOwned {
			continue
		}
		needed = true
		if desiredOwned {
			if err := want.touch(desired.Type, firewallSectionValues(desired)); err != nil {
				return want, true, fmt.Errorf("%s: %w", op.Section, err)
			}
		}
		if oldOwned {
			if err := want.touch(old[".type"], old); err != nil {
				return want, true, fmt.Errorf("%s: %w", op.Section, err)
			}
			if service, ok := managedFirewallService(old); ok {
				want.oldService[service.comment] = true
			} else if comment, ok := managedFirewallPolicy(old[".type"], old); ok {
				want.oldPolicy[comment] = true
			}
		}
	}
	// An empty plan can still be unhealthy: UCI may already match while fw4
	// never loaded it. Validate every desired managed firewall source before
	// reporting the no-write path as Applied or recording ownership.
	if !needed && len(plan.Plan.Ops) == 0 {
		sections := append([]render.Section(nil), plan.Doc.Sections...)
		sort.Slice(sections, func(i, j int) bool {
			if sections[i].Config != sections[j].Config {
				return sections[i].Config < sections[j].Config
			}
			return sections[i].Name < sections[j].Name
		})
		for _, section := range sections {
			if section.Config != "firewall" || section.Values[render.OwnershipTag] != "1" {
				continue
			}
			needed = true
			if err := want.touch(section.Type, firewallSectionValues(section)); err != nil {
				return want, true, fmt.Errorf("%s: %w", section.Name, err)
			}
		}
	}
	if !needed {
		return nil, false, nil
	}
	// One runtime read can prove the whole managed firewall. Once any managed
	// firewall section changes (or a no-op claims the model already matches),
	// include every desired source/edge/service, plus every previously owned
	// source/service whose absence may be the desired state.
	desiredSections := append([]render.Section(nil), plan.Doc.Sections...)
	sort.Slice(desiredSections, func(i, j int) bool {
		if desiredSections[i].Config != desiredSections[j].Config {
			return desiredSections[i].Config < desiredSections[j].Config
		}
		return desiredSections[i].Name < desiredSections[j].Name
	})
	for _, section := range desiredSections {
		if section.Config != "firewall" || section.Values[render.OwnershipTag] != "1" {
			continue
		}
		if err := want.touch(section.Type, firewallSectionValues(section)); err != nil {
			return want, true, fmt.Errorf("%s: %w", section.Name, err)
		}
		if _, service := managedFirewallService(firewallSectionValues(section)); !service {
			if policy, ok := managedFirewallPolicyExpectation(section.Type, firewallSectionValues(section)); ok {
				want.desiredPolicy[policy.comment] = policy
			}
		}
	}
	oldNames := make([]string, 0, len(plan.Existing.In("firewall")))
	for name := range plan.Existing.In("firewall") {
		oldNames = append(oldNames, name)
	}
	sort.Strings(oldNames)
	for _, name := range oldNames {
		old := plan.Existing.In("firewall")[name]
		if old[render.OwnershipTag] != "1" {
			continue
		}
		if err := want.touch(old[".type"], old); err != nil {
			return want, true, fmt.Errorf("%s: %w", name, err)
		}
		if service, ok := managedFirewallService(old); ok {
			want.oldService[service.comment] = true
		} else if comment, ok := managedFirewallPolicy(old[".type"], old); ok {
			want.oldPolicy[comment] = true
		}
	}
	networkDevices := map[string]string{}
	for _, section := range plan.Doc.Sections {
		if section.Config == "network" && section.Type == "interface" &&
			section.Values[render.OwnershipTag] == "1" {
			networkDevices[section.Name] = strings.TrimSpace(section.Values["device"])
		}
	}

	// Runtime intent is the complete desired model for every source affected by
	// this transaction, not just the sections represented by individual ops.
	// This is what detects both a missing new edge and a stale deleted edge.
	for _, section := range plan.Doc.Sections {
		if section.Config != "firewall" || section.Values[render.OwnershipTag] != "1" {
			continue
		}
		switch section.Type {
		case "zone":
			zone := strings.TrimSpace(section.Values["name"])
			if zone != "" {
				want.observeInput = true
				want.destinationSet[zone] = true
				if want.touchedSources[zone] {
					want.desiredZones[zone] = true
					want.desiredDevices[zone] = map[string]bool{}
					for _, network := range section.Lists["network"] {
						device, ok := networkDevices[network]
						if !ok || device == "" {
							return want, true, fmt.Errorf("managed zone %q network %q has no desired runtime device", zone, network)
						}
						want.desiredDevices[zone][device] = true
					}
					if len(want.desiredDevices[zone]) == 0 {
						return want, true, fmt.Errorf("managed zone %q has no desired runtime devices", zone)
					}
				}
			}
		case "forwarding":
			src, dest := strings.TrimSpace(section.Values["src"]), strings.TrimSpace(section.Values["dest"])
			want.destinationSet[dest] = dest != ""
			if want.touchedSources[src] && src != "" && dest != "" {
				if want.desiredEdges[src] == nil {
					want.desiredEdges[src] = map[string]bool{}
				}
				want.desiredEdges[src][dest] = true
			}
		case "rule":
			service, ok := managedFirewallService(firewallSectionValues(section))
			if ok && want.touchedSources[service.chain] {
				want.desiredService[service.comment] = service
				want.observeInput = true
			}
			if !ok {
				if policy, supported := managedFirewallPolicyExpectation(section.Type, firewallSectionValues(section)); supported {
					want.desiredPolicy[policy.comment] = policy
					want.observeInput = want.observeInput || strings.TrimSpace(policy.values["dest"]) == ""
				}
			}
		case "redirect":
			if policy, ok := managedFirewallPolicyExpectation(section.Type, firewallSectionValues(section)); ok {
				want.desiredPolicy[policy.comment] = policy
			}
		}
	}
	for _, old := range plan.Existing.In("firewall") {
		if old[render.OwnershipTag] != "1" {
			continue
		}
		switch old[".type"] {
		case "zone":
			want.destinationSet[strings.TrimSpace(old["name"])] = strings.TrimSpace(old["name"]) != ""
		case "forwarding":
			want.destinationSet[strings.TrimSpace(old["dest"])] = strings.TrimSpace(old["dest"]) != ""
		}
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

func firewallSectionValues(section render.Section) map[string]string {
	values := make(map[string]string, len(section.Values)+len(section.Lists))
	for name, value := range section.Values {
		values[name] = value
	}
	for name, value := range section.Lists {
		values[name] = strings.Join(value, " ")
	}
	return values
}

func (want *firewallRuntimeExpectation) touch(sectionType string, values map[string]string) error {
	switch sectionType {
	case "zone":
		zone := strings.TrimSpace(values["name"])
		if zone == "" {
			return errors.New("managed zone has no runtime name")
		}
		want.touchedSources[zone] = true
		want.destinationSet[zone] = true
	case "forwarding":
		src, dest := strings.TrimSpace(values["src"]), strings.TrimSpace(values["dest"])
		if src == "" || dest == "" {
			return errors.New("managed forwarding has no source or destination")
		}
		want.touchedSources[src] = true
		want.destinationSet[dest] = true
	case "rule":
		service, ok := managedFirewallService(values)
		if ok {
			want.touchedSources[service.chain] = true
			return nil
		}
		if _, ok := managedFirewallPolicy(sectionType, values); !ok {
			return errors.New("managed firewall rule is not a supported policy rule")
		}
	case "redirect":
		if _, ok := managedFirewallPolicy(sectionType, values); !ok {
			return errors.New("managed firewall redirect is not a supported DNAT policy")
		}
	default:
		return fmt.Errorf("unsupported managed firewall section type %q", sectionType)
	}
	return nil
}

func managedFirewallPolicy(sectionType string, values map[string]string) (string, bool) {
	name := strings.TrimSpace(values["name"])
	if name == "" {
		return "", false
	}
	switch sectionType {
	case "rule":
		switch strings.ToUpper(strings.TrimSpace(values["target"])) {
		case "ACCEPT", "DROP", "REJECT":
			return "!fw4: " + name, true
		}
	case "redirect":
		if strings.EqualFold(strings.TrimSpace(values["target"]), "DNAT") &&
			strings.TrimSpace(values["dest_ip"]) != "" {
			return "!fw4: " + name, true
		}
	}
	return "", false
}

func managedFirewallService(values map[string]string) (firewallService, bool) {
	if !strings.EqualFold(values["target"], "ACCEPT") ||
		!strings.EqualFold(values["family"], "ipv4") {
		return firewallService{}, false
	}
	src := strings.TrimSpace(values["src"])
	name := strings.TrimSpace(values["name"])
	if src == "" || name == "" {
		return firewallService{}, false
	}
	service := firewallService{comment: "!fw4: " + name, chain: src}
	proto := protocolSet(values["proto"])
	switch {
	case values["src_port"] == "68" && values["dest_port"] == "67" && proto["udp"]:
		service.kind = "dhcp"
	case values["src_port"] == "" && values["dest_port"] == "53" && proto["tcp"] && proto["udp"]:
		service.kind = "dns"
	default:
		return firewallService{}, false
	}
	return service, true
}

func protocolSet(raw string) map[string]bool {
	out := map[string]bool{}
	for _, protocol := range strings.Fields(strings.NewReplacer(",", " ", "{", " ", "}", " ").Replace(strings.ToLower(raw))) {
		out[strings.Trim(protocol, `"'`)] = true
	}
	return out
}

func (want *firewallRuntimeExpectation) wait(ctx context.Context, caller ubusCaller) error {
	deadline := time.Now().Add(want.settle)
	var last error
	for {
		stdout, err := readNFTRuleset(ctx, caller)
		if err != nil {
			return fmt.Errorf("managed firewall runtime unavailable after apply: %w", err)
		}
		runtime, err := parseNFTRuntime(stdout)
		if err != nil {
			return fmt.Errorf("managed firewall runtime unreadable after apply: %w", err)
		}
		if artifact, err := runtime.foreignPolicy(want.observeInput); err != nil {
			return fmt.Errorf("managed firewall runtime unreadable after apply: %w", err)
		} else if artifact != "" {
			last = fmt.Errorf("managed firewall runtime contains active policy outside the observable fw4 model at %s", artifact)
		} else {
			last = want.verify(runtime)
		}
		if last == nil {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("managed firewall runtime did not settle after apply: %w", last)
		}
		pause := firewallPollEvery
		if remaining < pause {
			pause = remaining
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("managed firewall runtime check: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (want *firewallRuntimeExpectation) verify(runtime *nftRuntime) error {
	sources := sortedTrueKeys(want.touchedSources)
	if len(want.desiredZones) > 0 {
		if !runtime.hasClosedBasePolicy("input", "input") {
			return errors.New("fw4 input base chain has no non-ACCEPT fallthrough policy")
		}
		if !runtime.hasClosedBasePolicy("forward", "forward") {
			return errors.New("fw4 forward base chain has no non-ACCEPT fallthrough policy")
		}
	}
	for _, source := range sources {
		input := nftChainKey{Family: "inet", Table: "fw4", Name: "input_" + source}
		forward := nftChainKey{Family: "inet", Table: "fw4", Name: "forward_" + source}
		if !want.desiredZones[source] {
			if runtime.hasBaseTarget("input", input.Name) || runtime.hasBaseTarget("forward", forward.Name) {
				return fmt.Errorf("deleted managed zone %q is still reachable", source)
			}
			continue
		}
		inputDevices, inputDispatch := runtime.dispatchDevices("input", input.Name)
		if _, ok := runtime.chains[input]; !ok || !inputDispatch {
			return fmt.Errorf("managed zone %q input chain was not loaded", source)
		}
		if diff := firstSetDifference(want.desiredDevices[source], inputDevices); diff != "" {
			return fmt.Errorf("managed zone %q input dispatch is missing device %q", source, diff)
		}
		if diff := firstSetDifference(inputDevices, want.desiredDevices[source]); diff != "" {
			return fmt.Errorf("managed zone %q input dispatch still includes device %q", source, diff)
		}
		forwardDevices, forwardDispatch := runtime.dispatchDevices("forward", forward.Name)
		if _, ok := runtime.chains[forward]; !ok || !forwardDispatch {
			return fmt.Errorf("managed zone %q forward chain was not loaded", source)
		}
		if diff := firstSetDifference(want.desiredDevices[source], forwardDevices); diff != "" {
			return fmt.Errorf("managed zone %q forward dispatch is missing device %q", source, diff)
		}
		if diff := firstSetDifference(forwardDevices, want.desiredDevices[source]); diff != "" {
			return fmt.Errorf("managed zone %q forward dispatch still includes device %q", source, diff)
		}
		if !runtime.hasClosedTail(input, "reject_from_"+source, "drop_from_"+source) {
			return fmt.Errorf("managed zone %q has no default reject/drop input path", source)
		}
		if !runtime.hasClosedTail(forward, "reject_to_"+source, "drop_to_"+source) {
			return fmt.Errorf("managed zone %q has no default reject/drop forwarding path", source)
		}
		actual := runtime.managedDestinations(forward, want.destinationSet)
		desired := want.desiredEdges[source]
		if diff := firstSetDifference(desired, actual); diff != "" {
			return fmt.Errorf("managed forwarding %s -> %s was not loaded", source, diff)
		}
		if diff := firstSetDifference(actual, desired); diff != "" {
			return fmt.Errorf("stale managed forwarding %s -> %s is still loaded", source, diff)
		}
		for dest := range desired {
			acceptChain := nftChainKey{Family: "inet", Table: "fw4", Name: "accept_to_" + dest}
			if !runtime.chainReachesVerdict(acceptChain, "accept", map[nftChainKey]bool{}) {
				return fmt.Errorf("managed forwarding %s -> %s has no accepting destination path", source, dest)
			}
			if devices, managedDestination := want.desiredDevices[dest]; managedDestination {
				actualDevices, exact := runtime.verdictDevices(acceptChain, "oifname", "accept")
				if !exact {
					return fmt.Errorf("managed destination zone %q has no exact accepting device dispatch", dest)
				}
				if diff := firstSetDifference(devices, actualDevices); diff != "" {
					return fmt.Errorf("managed destination zone %q accept dispatch is missing device %q", dest, diff)
				}
				if diff := firstSetDifference(actualDevices, devices); diff != "" {
					return fmt.Errorf("managed destination zone %q accept dispatch still includes device %q", dest, diff)
				}
			}
		}
	}

	comments := sortedServiceKeys(want.desiredService)
	for _, comment := range comments {
		service := want.desiredService[comment]
		chain := nftChainKey{Family: "inet", Table: "fw4", Name: "input_" + service.chain}
		if !runtime.hasServiceRule(chain, service) {
			return fmt.Errorf("managed %s service rule for zone %q was not loaded", service.kind, service.chain)
		}
	}
	for _, comment := range sortedTrueKeys(want.oldService) {
		if _, desired := want.desiredService[comment]; !desired && runtime.hasComment(comment) {
			return fmt.Errorf("stale managed service rule %q is still loaded", comment)
		}
	}
	for _, comment := range sortedPolicyKeys(want.desiredPolicy) {
		if !runtime.hasExactPolicy(want.desiredPolicy[comment]) {
			return fmt.Errorf("managed firewall policy %q was not loaded with its exact action, scope and match", comment)
		}
	}
	for _, comment := range sortedTrueKeys(want.oldPolicy) {
		if _, desired := want.desiredPolicy[comment]; !desired && runtime.hasComment(comment) {
			return fmt.Errorf("stale managed firewall policy %q is still loaded", comment)
		}
	}
	return nil
}

func (runtime *nftRuntime) hasClosedBasePolicy(name, hook string) bool {
	chain, ok := runtime.chains[nftChainKey{Family: "inet", Table: "fw4", Name: name}]
	if !ok || !strings.EqualFold(strings.TrimSpace(chain.Hook), hook) {
		return false
	}
	policy := strings.ToLower(strings.TrimSpace(chain.Policy))
	return policy != "" && policy != "accept"
}

func (runtime *nftRuntime) hasBaseTarget(chain, target string) bool {
	key := nftChainKey{Family: "inet", Table: "fw4", Name: chain}
	for _, rule := range runtime.rules[key] {
		targets, err := nftRuleTargets(rule.Expr)
		if err == nil {
			for _, got := range targets {
				if got == target {
					return true
				}
			}
		}
	}
	return false
}

func (runtime *nftRuntime) dispatchDevices(chain, target string) (map[string]bool, bool) {
	key := nftChainKey{Family: "inet", Table: "fw4", Name: chain}
	devices := map[string]bool{}
	found := false
	for _, rule := range runtime.rules[key] {
		targets, err := nftRuleTargets(rule.Expr)
		if err != nil {
			return nil, false
		}
		hasTarget := false
		for _, candidate := range targets {
			hasTarget = hasTarget || candidate == target
		}
		if !hasTarget {
			continue
		}
		if len(targets) != 1 {
			return nil, false
		}
		found = true
		names, exact := dispatchRuleDevices(rule.Expr)
		if !exact {
			return nil, false
		}
		for name := range names {
			devices[name] = true
		}
	}
	return devices, found && len(devices) > 0
}

func dispatchRuleDevices(exprs []json.RawMessage) (map[string]bool, bool) {
	devices := map[string]bool{}
	matched := false
	for _, expr := range exprs {
		var statement map[string]json.RawMessage
		if json.Unmarshal(expr, &statement) != nil || len(statement) != 1 {
			return nil, false
		}
		for kind, raw := range statement {
			switch kind {
			case "counter", "jump", "goto":
			case "match":
				if matched {
					return nil, false
				}
				var match struct {
					Op    string          `json:"op"`
					Left  json.RawMessage `json:"left"`
					Right json.RawMessage `json:"right"`
				}
				if json.Unmarshal(raw, &match) != nil || (match.Op != "==" && match.Op != "in") || nftMetaKey(match.Left) != "iifname" {
					return nil, false
				}
				for _, name := range nftValues(match.Right) {
					if strings.TrimSpace(name) == "" {
						return nil, false
					}
					devices[name] = true
				}
				matched = true
			default:
				return nil, false
			}
		}
	}
	return devices, matched && len(devices) > 0
}

func (runtime *nftRuntime) verdictDevices(chain nftChainKey, metaKey, verdict string) (map[string]bool, bool) {
	devices := map[string]bool{}
	found := false
	for _, rule := range runtime.rules[chain] {
		effects, err := nftRuleEffects(rule.Expr)
		if err != nil {
			return nil, false
		}
		if !hasEffect(effects, verdict) {
			continue
		}
		names, exact := verdictRuleDevices(rule.Expr, metaKey, verdict)
		if !exact {
			return nil, false
		}
		found = true
		for name := range names {
			devices[name] = true
		}
	}
	return devices, found && len(devices) > 0
}

func verdictRuleDevices(exprs []json.RawMessage, metaKey, verdict string) (map[string]bool, bool) {
	devices := map[string]bool{}
	matched, terminal := false, false
	for _, expr := range exprs {
		var statement map[string]json.RawMessage
		if json.Unmarshal(expr, &statement) != nil || len(statement) != 1 {
			return nil, false
		}
		for kind, raw := range statement {
			switch kind {
			case "counter":
			case verdict:
				if terminal {
					return nil, false
				}
				terminal = true
			case "match":
				if matched {
					return nil, false
				}
				var match struct {
					Op    string          `json:"op"`
					Left  json.RawMessage `json:"left"`
					Right json.RawMessage `json:"right"`
				}
				if json.Unmarshal(raw, &match) != nil || (match.Op != "==" && match.Op != "in") || nftMetaKey(match.Left) != metaKey {
					return nil, false
				}
				for _, name := range nftValues(match.Right) {
					if strings.TrimSpace(name) == "" {
						return nil, false
					}
					devices[name] = true
				}
				matched = true
			default:
				return nil, false
			}
		}
	}
	return devices, matched && terminal && len(devices) > 0
}

func (runtime *nftRuntime) managedDestinations(chain nftChainKey, scope map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, rule := range runtime.rules[chain] {
		target, ok := unconditionalJump(rule.Expr)
		if !ok || !strings.HasPrefix(target, "accept_to_") {
			continue
		}
		dest := strings.TrimPrefix(target, "accept_to_")
		if scope[dest] {
			out[dest] = true
		}
	}
	return out
}

func unconditionalJump(exprs []json.RawMessage) (string, bool) {
	var target string
	for _, expr := range exprs {
		var statement map[string]json.RawMessage
		if json.Unmarshal(expr, &statement) != nil || len(statement) != 1 {
			return "", false
		}
		for kind := range statement {
			switch kind {
			case "counter":
			case "jump", "goto":
				targets, err := nftRuleTargets([]json.RawMessage{expr})
				if err != nil || len(targets) != 1 || target != "" {
					return "", false
				}
				target = targets[0]
			default:
				return "", false
			}
		}
	}
	return target, target != ""
}

func (runtime *nftRuntime) hasClosedTail(chain nftChainKey, terminalTargets ...string) bool {
	rules := runtime.rules[chain]
	for i := len(rules) - 1; i >= 0; i-- {
		effects, err := nftRuleEffects(rules[i].Expr)
		if err != nil || len(effects) == 0 {
			continue
		}
		if unconditionalVerdict(rules[i].Expr, "reject") || unconditionalVerdict(rules[i].Expr, "drop") {
			return true
		}
		target, ok := unconditionalJump(rules[i].Expr)
		if !ok || !stringIn(target, terminalTargets) {
			return false
		}
		return runtime.chainReachesAnyVerdict(nftChainKey{Family: chain.Family, Table: chain.Table, Name: target}, map[nftChainKey]bool{})
	}
	return false
}

func stringIn(value string, values []string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func unconditionalVerdict(exprs []json.RawMessage, verdict string) bool {
	found := false
	for _, expr := range exprs {
		var statement map[string]json.RawMessage
		if json.Unmarshal(expr, &statement) != nil || len(statement) != 1 {
			return false
		}
		for kind := range statement {
			switch kind {
			case "counter":
			case verdict:
				if found {
					return false
				}
				found = true
			default:
				return false
			}
		}
	}
	return found
}

func (runtime *nftRuntime) chainReachesAnyVerdict(chain nftChainKey, seen map[nftChainKey]bool) bool {
	return runtime.chainReachesVerdict(chain, "reject", seen) ||
		runtime.chainReachesVerdict(chain, "drop", map[nftChainKey]bool{})
}

func (runtime *nftRuntime) chainReachesVerdict(chain nftChainKey, verdict string, seen map[nftChainKey]bool) bool {
	if seen[chain] {
		return false
	}
	if _, ok := runtime.chains[chain]; !ok {
		return false
	}
	seen[chain] = true
	for _, rule := range runtime.rules[chain] {
		effects, err := nftRuleEffects(rule.Expr)
		if err != nil {
			continue
		}
		if hasEffect(effects, verdict) {
			return true
		}
		targets, err := nftRuleTargets(rule.Expr)
		if err != nil {
			continue
		}
		for _, target := range targets {
			if runtime.chainReachesVerdict(nftChainKey{Family: chain.Family, Table: chain.Table, Name: target}, verdict, seen) {
				return true
			}
		}
	}
	return false
}

func hasEffect(effects []string, want string) bool {
	for _, effect := range effects {
		if effect == want {
			return true
		}
	}
	return false
}

func (runtime *nftRuntime) hasComment(comment string) bool {
	for _, rules := range runtime.rules {
		for _, rule := range rules {
			if strings.TrimSpace(rule.Comment) == comment {
				return true
			}
		}
	}
	return false
}

func (runtime *nftRuntime) hasServiceRule(chain nftChainKey, service firewallService) bool {
	protocols := map[string]bool{}
	found := false
	for _, rule := range runtime.rules[chain] {
		if strings.TrimSpace(rule.Comment) != service.comment {
			continue
		}
		found = true
		evidence, ok := serviceRuleEvidence(rule.Expr)
		if !ok {
			return false
		}
		switch service.kind {
		case "dhcp":
			if evidence.sport != "68" || evidence.dport != "67" ||
				len(evidence.protocols) != 1 || !evidence.protocols["udp"] {
				return false
			}
		case "dns":
			if evidence.sport != "" || evidence.dport != "53" {
				return false
			}
			for protocol := range evidence.protocols {
				if protocol != "tcp" && protocol != "udp" {
					return false
				}
				protocols[protocol] = true
			}
		default:
			return false
		}
	}
	if !found {
		return false
	}
	if service.kind == "dns" {
		return protocols["tcp"] && protocols["udp"]
	}
	return true
}

type nftServiceRuleEvidence struct {
	protocols    map[string]bool
	sport, dport string
}

func serviceRuleEvidence(exprs []json.RawMessage) (nftServiceRuleEvidence, bool) {
	metaProtocols := map[string]bool{}
	payloadProtocols := map[string]bool{}
	ports := map[string]string{}
	ipv4, accept := false, false
	for _, expr := range exprs {
		var statement map[string]json.RawMessage
		if json.Unmarshal(expr, &statement) != nil || len(statement) != 1 {
			return nftServiceRuleEvidence{}, false
		}
		for statementKind, raw := range statement {
			switch statementKind {
			case "counter":
			case "accept":
				if accept {
					return nftServiceRuleEvidence{}, false
				}
				accept = true
			case "match":
				var match struct {
					Op    string          `json:"op"`
					Left  json.RawMessage `json:"left"`
					Right json.RawMessage `json:"right"`
				}
				if json.Unmarshal(raw, &match) != nil {
					return nftServiceRuleEvidence{}, false
				}
				path, protocol, field := nftMatchLeft(match.Left)
				switch path {
				case "nfproto":
					if match.Op != "==" || nftScalar(match.Right) != "ipv4" {
						return nftServiceRuleEvidence{}, false
					}
					ipv4 = true
				case "l4proto":
					if match.Op != "==" && match.Op != "in" {
						return nftServiceRuleEvidence{}, false
					}
					for _, value := range nftValues(match.Right) {
						metaProtocols[value] = true
					}
				case "port":
					if match.Op != "==" {
						return nftServiceRuleEvidence{}, false
					}
					if protocol == "tcp" || protocol == "udp" {
						payloadProtocols[protocol] = true
					} else if protocol != "th" {
						return nftServiceRuleEvidence{}, false
					}
					value := nftScalar(match.Right)
					if previous, exists := ports[field]; exists && previous != value {
						return nftServiceRuleEvidence{}, false
					}
					ports[field] = value
				default:
					return nftServiceRuleEvidence{}, false
				}
			default:
				return nftServiceRuleEvidence{}, false
			}
		}
	}
	if !accept || !ipv4 {
		return nftServiceRuleEvidence{}, false
	}
	protocols := map[string]bool{}
	switch {
	case len(metaProtocols) > 0 && len(payloadProtocols) > 0:
		for protocol := range metaProtocols {
			if payloadProtocols[protocol] {
				protocols[protocol] = true
			}
		}
	case len(metaProtocols) > 0:
		protocols = metaProtocols
	default:
		protocols = payloadProtocols
	}
	if len(protocols) == 0 {
		return nftServiceRuleEvidence{}, false
	}
	return nftServiceRuleEvidence{protocols: protocols, sport: ports["sport"], dport: ports["dport"]}, true
}

func nftMatchLeft(raw json.RawMessage) (path, protocol, field string) {
	var left struct {
		Meta *struct {
			Key string `json:"key"`
		} `json:"meta"`
		Payload *struct {
			Protocol string `json:"protocol"`
			Field    string `json:"field"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &left) != nil {
		return "", "", ""
	}
	if left.Meta != nil {
		switch left.Meta.Key {
		case "nfproto", "l4proto":
			return left.Meta.Key, "", ""
		}
	}
	if left.Payload != nil && (left.Payload.Field == "sport" || left.Payload.Field == "dport") {
		return "port", left.Payload.Protocol, left.Payload.Field
	}
	return "", "", ""
}

func nftMetaKey(raw json.RawMessage) string {
	var left struct {
		Meta *struct {
			Key string `json:"key"`
		} `json:"meta"`
	}
	if json.Unmarshal(raw, &left) != nil || left.Meta == nil {
		return ""
	}
	return left.Meta.Key
}

func nftValues(raw json.RawMessage) []string {
	var array []any
	if json.Unmarshal(raw, &array) == nil {
		out := make([]string, 0, len(array))
		for _, value := range array {
			out = append(out, strings.ToLower(fmt.Sprint(value)))
		}
		return out
	}
	var set struct {
		Set []any `json:"set"`
	}
	if json.Unmarshal(raw, &set) == nil && set.Set != nil {
		out := make([]string, 0, len(set.Set))
		for _, value := range set.Set {
			out = append(out, strings.ToLower(fmt.Sprint(value)))
		}
		return out
	}
	return []string{nftScalar(raw)}
}

func nftScalar(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	var result string
	switch typed := value.(type) {
	case float64:
		result = strconv.FormatFloat(typed, 'f', -1, 64)
	case string:
		result = strings.ToLower(typed)
	default:
		result = strings.ToLower(fmt.Sprint(typed))
	}
	switch result {
	case "bootpc":
		return "68"
	case "bootps":
		return "67"
	case "domain":
		return "53"
	default:
		return result
	}
}

func sortedTrueKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value, present := range values {
		if present && value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func sortedServiceKeys(values map[string]firewallService) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func firstSetDifference(left, right map[string]bool) string {
	for _, value := range sortedTrueKeys(left) {
		if !right[value] {
			return value
		}
	}
	return ""
}
