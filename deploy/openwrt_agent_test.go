package deploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestOptionalAgentOnlyExposesReadMethods(t *testing.T) {
	path := "openwrt-agent/files/oonfeewrt-agent"
	output, err := exec.Command("sh", path, "list").Output()
	if err != nil {
		t.Fatal(err)
	}
	var methods map[string]map[string]any
	if err := json.Unmarshal(output, &methods); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 4 {
		t.Fatalf("unexpected methods: %s", output)
	}
	for _, name := range []string{"status", "board", "info", "wireguard"} {
		args, ok := methods[name]
		if !ok || len(args) != 0 {
			t.Fatalf("unexpected signature for %s", name)
		}
	}
	output, err = exec.Command("sh", path, "call", "status").Output()
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		ReadOnly                 bool `json:"read_only"`
		ProtocolVersion          int  `json:"protocol_version"`
		FirmwareInstallSupported bool `json:"firmware_install_supported"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		t.Fatal(err)
	}
	if !status.ReadOnly || status.ProtocolVersion != 1 || status.FirmwareInstallSupported {
		t.Fatalf("unsafe advertised capabilities: %s", output)
	}
	for _, method := range []string{"install", "upgrade", "exec", "status; touch /tmp/oonfeewrt-agent-must-not-exist"} {
		if err := exec.Command("sh", path, "call", method).Run(); err == nil {
			t.Fatalf("accepted unadvertised method %q", method)
		}
	}
}

func TestOptionalAgentACLDoesNotExpandDefaultAdoption(t *testing.T) {
	body, err := os.ReadFile("openwrt-agent/files/oonfeewrt-agent.json")
	if err != nil {
		t.Fatal(err)
	}
	var groups map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body, &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups["oonfeewrt-agent-read"] == nil {
		t.Fatalf("unexpected groups: %s", body)
	}
	group := groups["oonfeewrt-agent-read"]
	if _, ok := group["write"]; ok {
		t.Fatal("helper grants write permission")
	}
	var reads map[string]map[string][]string
	if err := json.Unmarshal(group["read"], &reads); err != nil {
		t.Fatal(err)
	}
	if len(reads) != 1 || len(reads["ubus"]) != 1 || len(reads["ubus"]["oonfeewrt-agent"]) != 4 {
		t.Fatalf("unsafe read grants: %s", group["read"])
	}
	for _, name := range []string{"acl/oonfeewrt.json", "acl/oonfeewrt-monitor.json"} {
		base, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(base), "oonfeewrt-agent") {
			t.Fatalf("optional helper leaked into %s", name)
		}
	}
}

func TestOptionalAgentSelectsOnlyPublicWireGuardFields(t *testing.T) {
	body, err := os.ReadFile("openwrt-agent/files/oonfeewrt-agent")
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	for _, fixed := range []string{"$(wg show interfaces)", "$(wg show all latest-handshakes)", "$(wg show all transfer)"} {
		if !strings.Contains(source, fixed) {
			t.Fatalf("missing fixed public selector %s", fixed)
		}
	}
	for _, forbidden := range []string{"wg show all dump", "wg showconf", "wg set", "wg-quick", "wg show all private-key", "wg show all preshared-keys"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("unsafe helper operation %s", forbidden)
		}
	}
	if !strings.Contains(source, `json_add_string handshakes "$handshakes"`) || !strings.Contains(source, `json_add_string transfers "$transfers"`) {
		t.Fatal("public-field output is not JSON escaped")
	}
}
