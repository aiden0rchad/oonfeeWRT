package reconcile

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// firewallPolicyExpectation is the UCI-to-runtime contract for controller
// traffic rules and DNAT redirects. A comment identifies candidate nft rules;
// it is never, by itself, evidence that their old match/action was replaced.
type firewallPolicyExpectation struct {
	comment, sectionType string
	values               map[string]string
}

func managedFirewallPolicyExpectation(sectionType string, values map[string]string) (firewallPolicyExpectation, bool) {
	comment, ok := managedFirewallPolicy(sectionType, values)
	if !ok {
		return firewallPolicyExpectation{}, false
	}
	copyValues := make(map[string]string, len(values))
	for key, value := range values {
		copyValues[key] = strings.TrimSpace(value)
	}
	return firewallPolicyExpectation{comment: comment, sectionType: sectionType, values: copyValues}, true
}

func sortedPolicyKeys(values map[string]firewallPolicyExpectation) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

type nftPolicyEvidence struct {
	nfproto, protocols        map[string]bool
	sourceCIDRs, destCIDRs    map[string]bool
	sourcePorts, destPorts    map[string]bool
	sourceMACs, outputDevices map[string]bool
	terminal, jumpChain       string
	dnatAddr                  string
	dnatPort                  string
	ipv4Match                 bool
}

func newNFTPolicyEvidence() nftPolicyEvidence {
	return nftPolicyEvidence{
		nfproto: map[string]bool{}, protocols: map[string]bool{},
		sourceCIDRs: map[string]bool{}, destCIDRs: map[string]bool{},
		sourcePorts: map[string]bool{}, destPorts: map[string]bool{},
		sourceMACs: map[string]bool{}, outputDevices: map[string]bool{},
	}
}

func (runtime *nftRuntime) hasExactPolicy(want firewallPolicyExpectation) bool {
	chain := ""
	switch want.sectionType {
	case "rule":
		if want.values["dest"] == "" {
			chain = "input_" + want.values["src"]
		} else {
			chain = "forward_" + want.values["src"]
		}
	case "redirect":
		chain = "dstnat_" + want.values["src"]
	default:
		return false
	}
	key := nftChainKey{Family: "inet", Table: "fw4", Name: chain}
	if _, ok := runtime.chains[key]; !ok {
		return false
	}
	for otherKey, rules := range runtime.rules {
		if otherKey == key {
			continue
		}
		for _, rule := range rules {
			if strings.TrimSpace(rule.Comment) == want.comment {
				return false
			}
		}
	}

	var found []nftPolicyEvidence
	var positions []int
	for i, rule := range runtime.rules[key] {
		if strings.TrimSpace(rule.Comment) != want.comment {
			continue
		}
		evidence, ok := parseNFTPolicyEvidence(rule.Expr)
		if !ok || !runtime.policyEvidenceMatches(want, evidence) {
			return false
		}
		found = append(found, evidence)
		positions = append(positions, i)
	}
	if len(found) == 0 {
		return false
	}

	wantProtocols := protocolSet(want.values["proto"])
	if len(wantProtocols) == 0 || wantProtocols["all"] || wantProtocols["any"] || wantProtocols["*"] {
		wantProtocols = map[string]bool{}
	}
	gotProtocols := map[string]bool{}
	for _, evidence := range found {
		for protocol := range evidence.protocols {
			gotProtocols[protocol] = true
		}
	}
	if !equalStringSets(wantProtocols, gotProtocols) {
		return false
	}
	if want.sectionType == "rule" {
		if !runtime.hasExactSourceDispatch(want) {
			return false
		}
		for _, position := range positions {
			if !runtime.policyRulePrecedence(key, want, position) {
				return false
			}
		}
		return true
	}
	return runtime.hasDNATForwardAcceptance(want)
}

func (runtime *nftRuntime) policyEvidenceMatches(want firewallPolicyExpectation, got nftPolicyEvidence) bool {
	values := want.values
	if !runtime.terminalMatches(want, got) {
		return false
	}

	family := strings.ToLower(values["family"])
	switch family {
	case "", "any":
		// The only family-agnostic policy currently emitted is a client MAC
		// block. Requiring no nfproto/IP expression proves it covers both v4/v6.
		if len(got.nfproto) != 0 || got.ipv4Match {
			return false
		}
	case "ipv4", "4":
		if !got.nfproto["ipv4"] && !got.nfproto["ip"] && !got.ipv4Match {
			return false
		}
		for value := range got.nfproto {
			if value != "ipv4" && value != "ip" {
				return false
			}
		}
	default:
		return false
	}

	if !equalStringSets(singleCIDR(values["src_ip"]), got.sourceCIDRs) ||
		!equalStringSets(singleCIDR(values["dest_ip"]), got.destCIDRs) ||
		!equalStringSets(singlePort(values[policySourcePort(want.sectionType)]), got.sourcePorts) ||
		!equalStringSets(singlePort(values[policyDestPort(want.sectionType)]), got.destPorts) ||
		!equalStringSets(canonicalMACSet(values["src_mac"]), got.sourceMACs) {
		return false
	}

	return len(got.outputDevices) == 0
}

func policySourcePort(sectionType string) string {
	if sectionType == "redirect" {
		return "src_port"
	}
	return "src_port"
}

func policyDestPort(sectionType string) string {
	if sectionType == "redirect" {
		return "src_dport"
	}
	return "dest_port"
}

func (runtime *nftRuntime) terminalMatches(want firewallPolicyExpectation, got nftPolicyEvidence) bool {
	switch want.sectionType {
	case "rule":
		target := strings.ToLower(want.values["target"])
		dest, src := want.values["dest"], want.values["src"]
		if dest != "" && dest != "*" {
			chain := target + "_to_" + dest
			return got.terminal == "" && got.jumpChain == chain &&
				runtime.hasExactActionDevices(nftChainKey{Family: "inet", Table: "fw4", Name: chain}, "oifname", target, false)
		}
		if got.jumpChain != "" {
			chain := target + "_from_" + src
			return (dest == "" || dest == "*") && got.jumpChain == chain &&
				runtime.hasExactActionDevices(nftChainKey{Family: "inet", Table: "fw4", Name: chain}, "iifname", target, strings.TrimSpace(want.values["family"]) == "")
		}
		return got.terminal == target
	case "redirect":
		return got.terminal == "dnat" && got.dnatAddr == want.values["dest_ip"] &&
			got.dnatPort == want.values["dest_port"]
	default:
		return false
	}
}

func (runtime *nftRuntime) hasExactActionDevices(chain nftChainKey, metaKey, wantAction string, requireAnyFamily bool) bool {
	if _, ok := runtime.chains[chain]; !ok {
		return false
	}
	devices := map[string]bool{}
	proved := false
	for _, rule := range runtime.rules[chain] {
		action, hasAction, valid := nftRuleTerminalAction(rule.Expr)
		if !valid {
			return false
		}
		if !hasAction || action != wantAction {
			continue
		}
		got, usable, anyFamily, exact := actionRuleDevices(rule.Expr, metaKey, wantAction)
		if !exact {
			return false
		}
		if !usable {
			continue
		}
		if requireAnyFamily && !anyFamily {
			continue
		}
		proved = true
		for device := range got {
			devices[device] = true
		}
	}
	return proved && len(devices) > 0
}

func nftRuleTerminalAction(exprs []json.RawMessage) (string, bool, bool) {
	action := ""
	for _, expr := range exprs {
		var statement map[string]json.RawMessage
		if json.Unmarshal(expr, &statement) != nil || len(statement) != 1 {
			return "", false, false
		}
		for kind := range statement {
			candidate := ""
			switch kind {
			case "accept", "drop", "reject":
				candidate = kind
			case "jump", "goto":
				targets, err := nftRuleTargets([]json.RawMessage{expr})
				if err != nil || len(targets) != 1 {
					return "", false, false
				}
				if targets[0] == "handle_reject" {
					candidate = "reject"
				}
			}
			if candidate != "" {
				if action != "" {
					return "", false, false
				}
				action = candidate
			}
		}
	}
	return action, action != "", true
}

func actionRuleDevices(exprs []json.RawMessage, metaKey, wantAction string) (map[string]bool, bool, bool, bool) {
	devices := map[string]bool{}
	terminal, familyOK, familyMatched, limited := false, true, false, false
	for _, expr := range exprs {
		var statement map[string]json.RawMessage
		if json.Unmarshal(expr, &statement) != nil || len(statement) != 1 {
			return nil, false, false, false
		}
		for kind, raw := range statement {
			switch kind {
			case "counter", "log":
			case "limit":
				limited = true
			case "accept", "drop", "reject":
				if kind != wantAction || terminal {
					return nil, false, false, false
				}
				terminal = true
			case "jump", "goto":
				targets, err := nftRuleTargets([]json.RawMessage{expr})
				if wantAction != "reject" || err != nil || len(targets) != 1 || targets[0] != "handle_reject" || terminal {
					return nil, false, false, false
				}
				terminal = true
			case "match":
				var match struct {
					Op    string          `json:"op"`
					Left  json.RawMessage `json:"left"`
					Right json.RawMessage `json:"right"`
				}
				if json.Unmarshal(raw, &match) != nil || (match.Op != "==" && match.Op != "in") {
					return nil, false, false, false
				}
				switch nftMetaKey(match.Left) {
				case metaKey:
					for _, device := range nftValues(match.Right) {
						if strings.TrimSpace(device) == "" {
							return nil, false, false, false
						}
						devices[device] = true
					}
				case "nfproto":
					familyMatched = true
					familyOK = false
					for _, family := range nftValues(match.Right) {
						familyOK = familyOK || family == "ipv4" || family == "ip"
					}
				default:
					return nil, false, false, false
				}
			default:
				return nil, false, false, false
			}
		}
	}
	return devices, terminal && familyOK && !limited && len(devices) > 0, !familyMatched, true
}

func (runtime *nftRuntime) policyRulePrecedence(chain nftChainKey, want firewallPolicyExpectation, position int) bool {
	wantAction := strings.ToLower(want.values["target"])
	for i, rule := range runtime.rules[chain] {
		if i >= position {
			break
		}
		if strings.TrimSpace(rule.Comment) == want.comment {
			continue
		}
		for _, action := range []string{"accept", "drop", "reject"} {
			if action == wantAction {
				continue
			}
			if unconditionalVerdict(rule.Expr, action) {
				return false
			}
		}
		if wantAction != "accept" && isCTDNATAccept(rule.Expr) {
			return false
		}
		target, ok := unconditionalJump(rule.Expr)
		if !ok {
			continue
		}
		targetChain := nftChainKey{Family: chain.Family, Table: chain.Table, Name: target}
		for _, action := range []string{"accept", "drop", "reject"} {
			if action != wantAction && runtime.chainReachesVerdict(targetChain, action, map[nftChainKey]bool{}) {
				return false
			}
		}
	}
	return true
}

func (runtime *nftRuntime) hasDNATForwardAcceptance(want firewallPolicyExpectation) bool {
	chain := nftChainKey{Family: "inet", Table: "fw4", Name: "forward_" + want.values["src"]}
	if _, ok := runtime.chains[chain]; !ok {
		return false
	}
	forwardDevices, forwardExact := runtime.dispatchDevices("forward", chain.Name)
	dstnatDevices, dstnatExact := runtime.dispatchDevices("dstnat", "dstnat_"+want.values["src"])
	if !forwardExact || !dstnatExact || !equalStringSets(forwardDevices, dstnatDevices) ||
		!runtime.baseDispatchPrecedesTerminal("forward", chain.Name) ||
		!runtime.baseDispatchPrecedesTerminal("dstnat", "dstnat_"+want.values["src"]) {
		return false
	}
	found := false
	for i, rule := range runtime.rules[chain] {
		if !isCTDNATAccept(rule.Expr) {
			continue
		}
		if found {
			return false
		}
		found = true
		for _, earlier := range runtime.rules[chain][:i] {
			if unconditionalVerdict(earlier.Expr, "drop") || unconditionalVerdict(earlier.Expr, "reject") {
				return false
			}
			if target, ok := unconditionalJump(earlier.Expr); ok {
				targetChain := nftChainKey{Family: chain.Family, Table: chain.Table, Name: target}
				if runtime.chainReachesVerdict(targetChain, "drop", map[nftChainKey]bool{}) ||
					runtime.chainReachesVerdict(targetChain, "reject", map[nftChainKey]bool{}) {
					return false
				}
			}
		}
	}
	return found
}

func (runtime *nftRuntime) hasExactSourceDispatch(want firewallPolicyExpectation) bool {
	direction := "forward"
	if strings.TrimSpace(want.values["dest"]) == "" {
		direction = "input"
	}
	src := strings.TrimSpace(want.values["src"])
	target := direction + "_" + src
	otherDirection := "input"
	if direction == "input" {
		otherDirection = "forward"
	}
	devices, exact := runtime.dispatchDevices(direction, target)
	otherDevices, otherExact := runtime.dispatchDevices(otherDirection, otherDirection+"_"+src)
	if !exact || !otherExact || !equalStringSets(devices, otherDevices) {
		return false
	}
	return runtime.baseDispatchPrecedesTerminal(direction, target) &&
		runtime.baseDispatchPrecedesTerminal(otherDirection, otherDirection+"_"+src)
}

func (runtime *nftRuntime) baseDispatchPrecedesTerminal(base, target string) bool {
	key := nftChainKey{Family: "inet", Table: "fw4", Name: base}
	chain, ok := runtime.chains[key]
	if !ok {
		return false
	}
	wantHook := base
	if base == "dstnat" {
		wantHook = "prerouting"
	}
	if !strings.EqualFold(strings.TrimSpace(chain.Hook), wantHook) {
		return false
	}
	found := false
	for i, rule := range runtime.rules[key] {
		targets, err := nftRuleTargets(rule.Expr)
		if err != nil {
			return false
		}
		matched := false
		for _, got := range targets {
			matched = matched || got == target
		}
		if !matched {
			continue
		}
		if len(targets) != 1 {
			return false
		}
		if _, exact := dispatchRuleDevices(rule.Expr); !exact {
			return false
		}
		found = true
		for _, earlier := range runtime.rules[key][:i] {
			for _, action := range []string{"accept", "drop", "reject"} {
				if unconditionalVerdict(earlier.Expr, action) {
					return false
				}
			}
			if jump, ok := unconditionalJump(earlier.Expr); ok {
				jumpChain := nftChainKey{Family: key.Family, Table: key.Table, Name: jump}
				for _, action := range []string{"accept", "drop", "reject"} {
					if runtime.chainReachesVerdict(jumpChain, action, map[nftChainKey]bool{}) {
						return false
					}
				}
			}
		}
	}
	return found
}

func isCTDNATAccept(exprs []json.RawMessage) bool {
	matched, accepted := false, false
	for _, expr := range exprs {
		var statement map[string]json.RawMessage
		if json.Unmarshal(expr, &statement) != nil || len(statement) != 1 {
			return false
		}
		for kind, raw := range statement {
			switch kind {
			case "counter":
			case "accept":
				if accepted {
					return false
				}
				accepted = true
			case "match":
				var match struct {
					Op    string          `json:"op"`
					Left  json.RawMessage `json:"left"`
					Right json.RawMessage `json:"right"`
				}
				if json.Unmarshal(raw, &match) != nil || (match.Op != "==" && match.Op != "in") || nftCTKey(match.Left) != "status" {
					return false
				}
				values := nftValues(match.Right)
				if len(values) != 1 || values[0] != "dnat" || matched {
					return false
				}
				matched = true
			default:
				return false
			}
		}
	}
	return matched && accepted
}

func nftCTKey(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		value = strings.ToLower(strings.TrimSpace(value))
		if strings.HasPrefix(value, "ct ") {
			return strings.TrimSpace(strings.TrimPrefix(value, "ct "))
		}
	}
	var left struct {
		CT *struct {
			Key string `json:"key"`
		} `json:"ct"`
	}
	if json.Unmarshal(raw, &left) == nil && left.CT != nil {
		return strings.ToLower(strings.TrimSpace(left.CT.Key))
	}
	return ""
}

func parseNFTPolicyEvidence(exprs []json.RawMessage) (nftPolicyEvidence, bool) {
	out := newNFTPolicyEvidence()
	for _, expr := range exprs {
		var statement map[string]json.RawMessage
		if json.Unmarshal(expr, &statement) != nil || len(statement) != 1 {
			return out, false
		}
		for kind, raw := range statement {
			switch kind {
			case "counter":
			case "accept", "drop", "reject":
				if out.terminal != "" {
					return out, false
				}
				out.terminal = kind
			case "jump", "goto":
				targets, err := nftRuleTargets([]json.RawMessage{expr})
				if err != nil || len(targets) != 1 || out.terminal != "" || out.jumpChain != "" {
					return out, false
				}
				if targets[0] == "handle_reject" {
					out.terminal = "reject"
				} else {
					out.jumpChain = targets[0]
				}
			case "dnat":
				if out.terminal != "" {
					return out, false
				}
				addr, port, ok := nftNATTarget(raw)
				if !ok {
					return out, false
				}
				out.terminal, out.dnatAddr, out.dnatPort = "dnat", addr, port
			case "match":
				if !addNFTPolicyMatch(&out, raw) {
					return out, false
				}
			default:
				return out, false
			}
		}
	}
	return out, out.terminal != "" || out.jumpChain != ""
}

func addNFTPolicyMatch(out *nftPolicyEvidence, raw json.RawMessage) bool {
	var match struct {
		Op    string          `json:"op"`
		Left  json.RawMessage `json:"left"`
		Right json.RawMessage `json:"right"`
	}
	if json.Unmarshal(raw, &match) != nil || (match.Op != "==" && match.Op != "in") {
		return false
	}
	var left struct {
		Meta *struct {
			Key string `json:"key"`
		} `json:"meta"`
		Payload *struct {
			Protocol string `json:"protocol"`
			Field    string `json:"field"`
		} `json:"payload"`
	}
	if json.Unmarshal(match.Left, &left) != nil {
		return false
	}
	if left.Meta != nil {
		values := nftValues(match.Right)
		switch left.Meta.Key {
		case "nfproto":
			addStrings(out.nfproto, values)
		case "l4proto":
			addStrings(out.protocols, values)
		case "oifname":
			addStrings(out.outputDevices, values)
		default:
			return false
		}
		return true
	}
	if left.Payload == nil {
		return false
	}
	protocol, field := strings.ToLower(left.Payload.Protocol), strings.ToLower(left.Payload.Field)
	switch {
	case protocol == "ip" && (field == "saddr" || field == "daddr"):
		prefixes, ok := nftCIDRValues(match.Right)
		if !ok {
			return false
		}
		out.ipv4Match = true
		if field == "saddr" {
			addStrings(out.sourceCIDRs, prefixes)
		} else {
			addStrings(out.destCIDRs, prefixes)
		}
		return true
	case protocol == "ether" && field == "saddr":
		macs := nftValues(match.Right)
		for _, mac := range macs {
			parsed, err := netip.ParseAddr(mac)
			if err == nil && parsed.IsValid() {
				return false
			}
			out.sourceMACs[strings.ToLower(mac)] = true
		}
		return len(macs) > 0
	case (protocol == "tcp" || protocol == "udp" || protocol == "th") &&
		(field == "sport" || field == "dport"):
		ports, ok := nftPortValues(match.Right)
		if !ok {
			return false
		}
		if protocol != "th" {
			out.protocols[protocol] = true
		}
		if field == "sport" {
			addStrings(out.sourcePorts, ports)
		} else {
			addStrings(out.destPorts, ports)
		}
		return true
	default:
		return false
	}
}

func nftNATTarget(raw json.RawMessage) (string, string, bool) {
	var target struct {
		Addr json.RawMessage `json:"addr"`
		Port json.RawMessage `json:"port"`
	}
	if json.Unmarshal(raw, &target) != nil || len(target.Addr) == 0 || len(target.Port) == 0 {
		return "", "", false
	}
	addr := nftScalar(target.Addr)
	if parsed, err := netip.ParseAddr(addr); err != nil || !parsed.Is4() {
		return "", "", false
	}
	ports, ok := nftPortValues(target.Port)
	if !ok || len(ports) != 1 {
		return "", "", false
	}
	return addr, ports[0], true
}

func nftCIDRValues(raw json.RawMessage) ([]string, bool) {
	items := nftCollection(raw)
	var out []string
	for _, item := range items {
		var prefix struct {
			Prefix *struct {
				Addr string `json:"addr"`
				Len  int    `json:"len"`
			} `json:"prefix"`
		}
		if json.Unmarshal(item, &prefix) == nil && prefix.Prefix != nil {
			p, err := netip.ParsePrefix(fmt.Sprintf("%s/%d", prefix.Prefix.Addr, prefix.Prefix.Len))
			if err != nil || !p.Addr().Is4() {
				return nil, false
			}
			out = append(out, p.Masked().String())
			continue
		}
		addr, err := netip.ParseAddr(nftScalar(item))
		if err != nil || !addr.Is4() {
			return nil, false
		}
		out = append(out, netip.PrefixFrom(addr, 32).String())
	}
	return out, len(out) > 0
}

func nftPortValues(raw json.RawMessage) ([]string, bool) {
	items := nftCollection(raw)
	var out []string
	for _, item := range items {
		var interval struct {
			Range []json.RawMessage `json:"range"`
		}
		if json.Unmarshal(item, &interval) == nil && interval.Range != nil {
			if len(interval.Range) != 2 {
				return nil, false
			}
			lo, err1 := strconv.Atoi(nftScalar(interval.Range[0]))
			hi, err2 := strconv.Atoi(nftScalar(interval.Range[1]))
			if err1 != nil || err2 != nil || lo < 1 || hi < lo || hi > 65535 {
				return nil, false
			}
			out = append(out, strconv.Itoa(lo)+"-"+strconv.Itoa(hi))
			continue
		}
		port, err := strconv.Atoi(nftScalar(item))
		if err != nil || port < 1 || port > 65535 {
			return nil, false
		}
		out = append(out, strconv.Itoa(port))
	}
	return out, len(out) > 0
}

func nftCollection(raw json.RawMessage) []json.RawMessage {
	var array []json.RawMessage
	if json.Unmarshal(raw, &array) == nil {
		return array
	}
	var set struct {
		Set []json.RawMessage `json:"set"`
	}
	if json.Unmarshal(raw, &set) == nil && set.Set != nil {
		return set.Set
	}
	return []json.RawMessage{raw}
}

func singleCIDR(raw string) map[string]bool {
	out := map[string]bool{}
	if raw == "" {
		return out
	}
	prefix, err := netip.ParsePrefix(raw)
	if err == nil {
		out[prefix.Masked().String()] = true
	}
	return out
}

func singlePort(raw string) map[string]bool {
	out := map[string]bool{}
	if raw != "" {
		out[raw] = true
	}
	return out
}

func canonicalMACSet(raw string) map[string]bool {
	out := map[string]bool{}
	for _, mac := range strings.Fields(raw) {
		out[strings.ToLower(mac)] = true
	}
	return out
}

func addStrings(dst map[string]bool, values []string) {
	for _, value := range values {
		dst[strings.ToLower(value)] = true
	}
}

func equalStringSets(a, b map[string]bool) bool {
	return firstSetDifference(a, b) == "" && firstSetDifference(b, a) == ""
}
