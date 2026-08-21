package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
)

// nftRulesetLimit bounds parser work. The ubus transport has its own response
// handling; this limit is specifically the point at which this policy proof
// refuses to reason about an unexpectedly large ruleset.
const nftRulesetLimit = 8 << 20

type ubusCaller interface {
	Call(context.Context, string, string, any, any) error
}

type nftExecResult struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
}

var (
	errNFTRulesetMalformed = errors.New("malformed nftables ruleset")
	errNFTRulesetTooLarge  = errors.New("nftables ruleset exceeds inspection limit")
)

// explicitFirewallPolicyConflict protects the stronger claim made by an
// explicit directional zone matrix. The inherited source -> wan behaviour is
// intentionally unchanged, as are APs and switches: none of those paths make
// an extra device call or turn an old configuration into a blocker.
func explicitFirewallPolicyConflict(ctx context.Context, c ubusCaller, site model.Site,
	dev model.Device, existing render.Existing) *render.Conflict {

	if !enforcesExplicitZonePolicy(site, dev) {
		return nil
	}
	if name, ok := activeFirewallInclude(existing); ok {
		return &render.Conflict{
			Config: "firewall", Section: name,
			Reason: fmt.Sprintf("active foreign firewall include %s can install policy outside the UCI sections oonfeeWRT can prove. Explicit directional zone policy is blocked because the matrix cannot be guaranteed; disable or remove the include, convert its policy to visible UCI sections, or reset the zone matrix to inherited defaults, then preview again", name),
		}
	}

	stdout, err := readNFTRuleset(ctx, c)
	if err != nil {
		return unobservableNFTConflict("the device did not return the ruleset")
	}

	artifact, err := foreignNFTRuntimePolicyScope(stdout, hasRouterInputPolicy(site) || len(site.ActiveZoneNames()) > 0)
	if err != nil {
		detail := "the returned ruleset was unreadable"
		if errors.Is(err, errNFTRulesetTooLarge) {
			detail = fmt.Sprintf("the returned ruleset exceeded the %d MiB inspection limit", nftRulesetLimit>>20)
		}
		return unobservableNFTConflict(detail)
	}
	if artifact == "" {
		return nil
	}
	return &render.Conflict{
		Config: "firewall", Section: "runtime-nftables",
		Reason: fmt.Sprintf("active nftables policy was found at %s outside the UCI policy model. Explicit directional zone policy is blocked because the matrix cannot be guaranteed; remove or narrow the custom nft hook or rule (including /etc/nftables.d snippets), or reset the zone matrix to inherited defaults, then preview again", artifact),
	}
}

func readNFTRuleset(ctx context.Context, c ubusCaller) (string, error) {
	if c == nil {
		return "", errors.New("nil nft runtime caller")
	}
	var out nftExecResult
	if err := c.Call(ctx, "file", "exec", map[string]any{
		"command": "/usr/sbin/nft",
		"params":  []string{"--terse", "--json", "list", "ruleset"},
	}, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("nft exited with status %d", out.Code)
	}
	return out.Stdout, nil
}

func unobservableNFTConflict(detail string) *render.Conflict {
	return &render.Conflict{
		Config: "firewall", Section: "runtime-nftables",
		Reason: fmt.Sprintf("%s, so active nftables policy cannot be verified with the controller's exact read-only nft grant. Explicit directional zone policy is blocked; restore device reachability or the ACL grant for `/usr/sbin/nft --terse --json list ruleset`, then preview again", detail),
	}
}

func enforcesExplicitZonePolicy(site model.Site, dev model.Device) bool {
	return dev.EffectiveFunctions().Routes() && site.HasExplicitFirewallIntent()
}

func hasRouterInputPolicy(site model.Site) bool {
	for _, policy := range site.Policies {
		if policy.Enabled && policy.Kind == model.PolicyFirewallRule && policy.Firewall != nil &&
			policy.Firewall.DestinationZone == "" {
			return true
		}
	}
	return false
}

// activeFirewallInclude mirrors the fw4 applicability boundary that can be
// established from UCI alone. In particular, the historical firewall.user
// section is inactive under fw4 unless it is explicitly marked compatible.
func activeFirewallInclude(existing render.Existing) (string, bool) {
	sections := existing.In("firewall")
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		values := sections[name]
		if values[".type"] != "include" || uciFalse(values["enabled"]) ||
			strings.TrimSpace(values["path"]) == "" {
			continue
		}

		includeType := strings.ToLower(strings.TrimSpace(values["type"]))
		if includeType == "" {
			includeType = "script"
		}
		if includeType == "script" {
			compatible, present := values["fw4_compatible"]
			if uciFalse(compatible) || (!present && values["path"] == "/etc/firewall.user") {
				continue
			}
		}
		return name, true
	}
	return "", false
}

func uciFalse(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "no", "off":
		return true
	default:
		return false
	}
}

type nftChainKey struct {
	Family string
	Table  string
	Name   string
}

func (k nftChainKey) String() string { return k.Family + "/" + k.Table + "/" + k.Name }

type nftChain struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Name   string `json:"name"`
	Hook   string `json:"hook"`
	Policy string `json:"policy"`
}

type nftRule struct {
	Family  string            `json:"family"`
	Table   string            `json:"table"`
	Chain   string            `json:"chain"`
	Expr    []json.RawMessage `json:"expr"`
	Comment string            `json:"comment"`
}

type nftPolicyFinding struct {
	artifact string
	kind     string
}

type nftRuntime struct {
	chains map[nftChainKey]nftChain
	rules  map[nftChainKey][]nftRule
}

// foreignNFTRuntimePolicy returns the first deterministic active transit
// artifact not attributable to fw4's generated rules. It does not infer file
// provenance: nft JSON does not retain whether a rule came from UCI, a package,
// /etc/nftables.d, or a direct command.
func foreignNFTRuntimePolicy(stdout string) (string, error) {
	return foreignNFTRuntimePolicyScope(stdout, false)
}

func foreignNFTRuntimePolicyScope(stdout string, includeInput bool) (string, error) {
	runtime, err := parseNFTRuntime(stdout)
	if err != nil {
		return "", err
	}
	return runtime.foreignPolicy(includeInput)
}

func parseNFTRuntime(stdout string) (*nftRuntime, error) {
	if len(stdout) == 0 || len(stdout) > nftRulesetLimit {
		if len(stdout) > nftRulesetLimit {
			return nil, errNFTRulesetTooLarge
		}
		return nil, errNFTRulesetMalformed
	}
	var envelope struct {
		NFTables json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || len(envelope.NFTables) == 0 {
		return nil, errNFTRulesetMalformed
	}
	var records []json.RawMessage
	if err := json.Unmarshal(envelope.NFTables, &records); err != nil {
		return nil, errNFTRulesetMalformed
	}

	chains := map[nftChainKey]nftChain{}
	rules := map[nftChainKey][]nftRule{}
	for _, record := range records {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(record, &object); err != nil || len(object) == 0 {
			return nil, errNFTRulesetMalformed
		}
		if raw, ok := object["chain"]; ok {
			var chain nftChain
			if err := json.Unmarshal(raw, &chain); err != nil || chain.Family == "" || chain.Table == "" || chain.Name == "" {
				return nil, errNFTRulesetMalformed
			}
			key := nftChainKey{Family: chain.Family, Table: chain.Table, Name: chain.Name}
			if _, duplicate := chains[key]; duplicate {
				return nil, errNFTRulesetMalformed
			}
			chains[key] = chain
		}
		if raw, ok := object["rule"]; ok {
			var rule nftRule
			if err := json.Unmarshal(raw, &rule); err != nil || rule.Family == "" || rule.Table == "" || rule.Chain == "" || rule.Expr == nil {
				return nil, errNFTRulesetMalformed
			}
			key := nftChainKey{Family: rule.Family, Table: rule.Table, Name: rule.Chain}
			rules[key] = append(rules[key], rule)
		}
	}
	for key := range rules {
		if _, ok := chains[key]; !ok {
			return nil, errNFTRulesetMalformed
		}
	}
	return &nftRuntime{chains: chains, rules: rules}, nil
}

func (runtime *nftRuntime) foreignPolicy(includeInput bool) (string, error) {
	chains, rules := runtime.chains, runtime.rules

	var starts []nftChainKey
	var findings []nftPolicyFinding
	for key, chain := range chains {
		if !transitHook(chain.Hook) && !(includeInput && strings.EqualFold(strings.TrimSpace(chain.Hook), "input")) {
			continue
		}
		starts = append(starts, key)
		if chain.Policy != "" && strings.ToLower(chain.Policy) != "accept" && !stockFW4BaseChain(key, chain.Hook) {
			findings = append(findings, nftPolicyFinding{artifact: key.String(), kind: "base-chain policy"})
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].String() < starts[j].String() })

	reachable := map[nftChainKey]bool{}
	queue := append([]nftChainKey(nil), starts...)
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if reachable[key] {
			continue
		}
		reachable[key] = true
		for _, rule := range rules[key] {
			targets, err := nftRuleTargets(rule.Expr)
			if err != nil {
				return "", errNFTRulesetMalformed
			}
			for _, target := range targets {
				next := nftChainKey{Family: key.Family, Table: key.Table, Name: target}
				if _, exists := chains[next]; !exists {
					return "", errNFTRulesetMalformed
				}
				if !reachable[next] {
					queue = append(queue, next)
				}
			}
		}
	}

	keys := make([]nftChainKey, 0, len(reachable))
	for key := range reachable {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, key := range keys {
		for _, rule := range rules[key] {
			if generatedFW4Rule(key, rule.Comment) {
				continue
			}
			effects, err := nftRuleEffects(rule.Expr)
			if err != nil {
				return "", errNFTRulesetMalformed
			}
			if len(effects) == 0 || stockUncommentedFW4Rule(key, rule.Expr, effects) {
				continue
			}
			findings = append(findings, nftPolicyFinding{artifact: key.String(), kind: "rule"})
		}
	}
	if len(findings) == 0 {
		return "", nil
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].artifact != findings[j].artifact {
			return findings[i].artifact < findings[j].artifact
		}
		return findings[i].kind < findings[j].kind
	})
	return findings[0].artifact, nil
}

func transitHook(hook string) bool {
	switch strings.ToLower(strings.TrimSpace(hook)) {
	case "ingress", "prerouting", "forward", "postrouting", "egress":
		return true
	default:
		return false
	}
}

func stockFW4BaseChain(key nftChainKey, hook string) bool {
	if key.Family != "inet" || key.Table != "fw4" {
		return false
	}
	want := map[string]string{
		"input":              "input",
		"mangle_input":       "input",
		"forward":            "forward",
		"prerouting":         "prerouting",
		"dstnat":             "prerouting",
		"raw_prerouting":     "prerouting",
		"mangle_prerouting":  "prerouting",
		"mangle_forward":     "forward",
		"srcnat":             "postrouting",
		"mangle_postrouting": "postrouting",
	}
	return want[key.Name] == strings.ToLower(hook)
}

func generatedFW4Rule(key nftChainKey, comment string) bool {
	return key.Family == "inet" && key.Table == "fw4" &&
		strings.HasPrefix(strings.TrimSpace(comment), "!fw4:")
}

func nftRuleEffects(exprs []json.RawMessage) ([]string, error) {
	var effects []string
	for _, expr := range exprs {
		var statement map[string]json.RawMessage
		if err := json.Unmarshal(expr, &statement); err != nil || len(statement) != 1 {
			return nil, errNFTRulesetMalformed
		}
		for kind := range statement {
			switch kind {
			case "match", "counter", "limit", "log", "quota":
				// These can observe or select a packet, but without another
				// statement they do not change its forwarding verdict.
			default:
				effects = append(effects, kind)
			}
		}
	}
	sort.Strings(effects)
	return effects, nil
}

func nftRuleTargets(exprs []json.RawMessage) ([]string, error) {
	var targets []string
	for _, expr := range exprs {
		var statement map[string]json.RawMessage
		if err := json.Unmarshal(expr, &statement); err != nil || len(statement) != 1 {
			return nil, errNFTRulesetMalformed
		}
		for kind, raw := range statement {
			if kind != "jump" && kind != "goto" {
				continue
			}
			var target string
			if err := json.Unmarshal(raw, &target); err != nil {
				var value struct {
					Target string `json:"target"`
				}
				if err := json.Unmarshal(raw, &value); err != nil {
					return nil, errNFTRulesetMalformed
				}
				target = value.Target
			}
			if strings.TrimSpace(target) == "" {
				return nil, errNFTRulesetMalformed
			}
			targets = append(targets, target)
		}
	}
	return targets, nil
}

func stockUncommentedFW4Rule(key nftChainKey, exprs []json.RawMessage, effects []string) bool {
	if key.Family != "inet" || key.Table != "fw4" || len(effects) != 1 {
		return false
	}
	if key.Name == "forward" && effects[0] == "flow" {
		return true
	}
	if effects[0] != "jump" {
		return false
	}
	targets, err := nftRuleTargets(exprs)
	if err != nil || len(targets) != 1 {
		return false
	}
	if (key.Name == "forward" || key.Name == "input") && targets[0] == "handle_reject" {
		return true
	}
	if strings.HasPrefix(key.Name, "input_") {
		zone := strings.TrimPrefix(key.Name, "input_")
		for _, verdict := range []string{"accept", "reject", "drop"} {
			if targets[0] == verdict+"_from_"+zone {
				return true
			}
		}
		return false
	}
	if !strings.HasPrefix(key.Name, "forward_") {
		return false
	}
	zone := strings.TrimPrefix(key.Name, "forward_")
	for _, verdict := range []string{"accept", "reject", "drop"} {
		if targets[0] == verdict+"_to_"+zone {
			return true
		}
	}
	return false
}
