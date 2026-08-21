package deploy

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestDHCPRuntimeProofACLAuthorizesRequestedAndResolvedPathsReadOnly(t *testing.T) {
	type permissions struct {
		Ubus map[string][]string `json:"ubus"`
		File map[string][]string `json:"file"`
	}
	var groups map[string]struct {
		Read  permissions `json:"read"`
		Write permissions `json:"write"`
	}
	if err := json.Unmarshal(ACL, &groups); err != nil {
		t.Fatal(err)
	}
	controller := groups["oonfeewrt"]
	if !slices.Contains(controller.Read.Ubus["service"], "list") {
		t.Fatal("DHCP proof cannot inspect dnsmasq service state")
	}
	// /var is a symlink to /tmp on OpenWrt. Patched rpcd first authorizes the
	// caller's path and then re-authorizes the realpath target, so omitting
	// either entry makes file.read fail with UBUS_STATUS_PERMISSION_DENIED.
	for _, runtime := range []string{
		"/var/etc/dnsmasq.conf.*",
		"/tmp/etc/dnsmasq.conf.*",
	} {
		if got := controller.Read.File[runtime]; len(got) != 1 || got[0] != "read" {
			t.Errorf("runtime config grant %s = %v, want read only", runtime, got)
		}
		if _, ok := controller.Write.File[runtime]; ok {
			t.Errorf("dnsmasq runtime config path %s is writable", runtime)
		}
	}
	if slices.Contains(controller.Write.Ubus["service"], "list") {
		t.Fatal("service.list was unnecessarily added to write permissions")
	}
}

func TestBridgeIsolationProofACLAuthorizesRequestedAndResolvedPathsReadOnly(t *testing.T) {
	type permissions struct {
		File map[string][]string `json:"file"`
	}
	var groups map[string]struct {
		Read  permissions `json:"read"`
		Write permissions `json:"write"`
	}
	if err := json.Unmarshal(ACL, &groups); err != nil {
		t.Fatal(err)
	}
	controller := groups["oonfeewrt"]
	// rpcd checks the requested /sys/class symlink and its canonical
	// /sys/devices target. Omitting either makes the runtime proof fail closed.
	for _, runtime := range []string{
		"/sys/class/net/*/brport/isolated",
		"/sys/devices/*/brport/isolated",
	} {
		if got := controller.Read.File[runtime]; len(got) != 1 || got[0] != "read" {
			t.Errorf("bridge isolation grant %s = %v, want read only", runtime, got)
		}
		if _, ok := controller.Write.File[runtime]; ok {
			t.Errorf("bridge isolation path %s is writable", runtime)
		}
	}
}

func TestStaticRouteRuntimeACLIncludesExactBusyBoxFallbackReadOnly(t *testing.T) {
	type permissions struct {
		Ubus map[string][]string `json:"ubus"`
		File map[string][]string `json:"file"`
	}
	var groups map[string]struct {
		Read  permissions `json:"read"`
		Write permissions `json:"write"`
	}
	if err := json.Unmarshal(ACL, &groups); err != nil {
		t.Fatal(err)
	}
	controller := groups["oonfeewrt"]
	if !slices.Contains(controller.Read.Ubus["file"], "exec") {
		t.Fatal("static-route runtime proof cannot execute ip")
	}
	for _, command := range []string{
		"/sbin/ip -[46] -j route show table all",
		"/sbin/ip -[46] route show table all",
	} {
		if got := controller.Read.File[command]; len(got) != 1 || got[0] != "exec" {
			t.Errorf("route command grant %s = %v, want exec only", command, got)
		}
		if _, ok := controller.Write.File[command]; ok {
			t.Errorf("route command %s is writable", command)
		}
	}
	for _, broad := range []string{"/sbin/ip *", "/sbin/ip -[46] route *", "/sbin/ip -[46] route show *"} {
		if _, ok := controller.Read.File[broad]; ok {
			t.Errorf("route proof has an unnecessarily broad grant %s", broad)
		}
	}
}

func TestPhase4ObservationACLUsesExactStockReadOnlyFallbacks(t *testing.T) {
	type permissions struct {
		Ubus map[string][]string `json:"ubus"`
		File map[string][]string `json:"file"`
	}
	var groups map[string]struct {
		Read  permissions `json:"read"`
		Write permissions `json:"write"`
	}
	if err := json.Unmarshal(ACL, &groups); err != nil {
		t.Fatal(err)
	}
	controller := groups["oonfeewrt"]
	for _, command := range []string{
		"/sbin/ip -[46] neigh show",
		"/sbin/ip -[46] addr show",
		"/sbin/ip -[46] route show table all",
		"/usr/sbin/brctl showmacs *",
		"/usr/sbin/brctl showstp *",
		"/usr/sbin/lldpcli -f json show neighbors hidden",
		"/bin/ping -q -c 3 -W 1 1.1.1.1",
	} {
		if got := controller.Read.File[command]; len(got) != 1 || got[0] != "exec" {
			t.Errorf("stock observation grant %s = %v, want exec only", command, got)
		}
		if _, ok := controller.Write.File[command]; ok {
			t.Errorf("stock observation command %s is writable", command)
		}
	}
	if got := controller.Read.File["/proc/sys/kernel/random/boot_id"]; len(got) != 1 || got[0] != "read" {
		t.Errorf("boot identity grant = %v, want read only", got)
	}
	if _, ok := controller.Write.File["/proc/sys/kernel/random/boot_id"]; ok {
		t.Fatal("boot identity path is writable")
	}
	if !slices.Contains(controller.Read.Ubus["iwinfo"], "freqlist") {
		t.Fatal("radio channel inventory cannot call iwinfo.freqlist")
	}
	if slices.Contains(controller.Write.Ubus["iwinfo"], "freqlist") {
		t.Fatal("iwinfo.freqlist was unnecessarily granted as a write method")
	}
	for _, broad := range []string{
		"/sbin/ip *", "/usr/sbin/brctl *", "/bin/ping *",
		"/proc/sys/kernel/random/*",
	} {
		if _, ok := controller.Read.File[broad]; ok {
			t.Errorf("phase 4 observation has an unnecessarily broad grant %s", broad)
		}
	}
}
