package deploy

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func TestMonitorACLMatchesObservationScopeAndHasNoWriteGrant(t *testing.T) {
	var managed, monitor map[string]map[string]json.RawMessage
	if err := json.Unmarshal(ACL, &managed); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(MonitorACL, &monitor); err != nil {
		t.Fatal(err)
	}
	if len(monitor) != 1 {
		t.Fatalf("monitor ACL groups=%d, want exactly one", len(monitor))
	}
	monitorGroup, ok := monitor["oonfeewrt-monitor"]
	if !ok {
		t.Fatal("monitor ACL does not define oonfeewrt-monitor")
	}
	if _, ok := monitorGroup["write"]; ok {
		t.Fatal("monitor ACL grants a write section")
	}
	var managedRead, monitorRead any
	if err := json.Unmarshal(managed["oonfeewrt"]["read"], &managedRead); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(monitorGroup["read"], &monitorRead); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(monitorRead, managedRead) {
		t.Fatal("monitor ACL observation scope differs from the managed ACL")
	}
}

func TestProductionACLHasOnlyUsedWriteScope(t *testing.T) {
	var acl map[string]struct {
		Write struct {
			UBus map[string][]string `json:"ubus"`
			UCI  []string            `json:"uci"`
			File map[string][]string `json:"file"`
		} `json:"write"`
	}
	if err := json.Unmarshal(ACL, &acl); err != nil {
		t.Fatal(err)
	}
	w := acl["oonfeewrt"].Write
	for _, method := range []string{"rename", "order", "rollback"} {
		if slices.Contains(w.UBus["uci"], method) {
			t.Errorf("unused uci.%s remains granted", method)
		}
	}
	if _, ok := w.UBus["network"]; ok {
		t.Error("unused network.reload scope remains granted")
	}
	if slices.Contains(w.UCI, "system") {
		t.Error("unused system UCI write scope remains granted")
	}
	if slices.Contains(w.UBus["hostapd.*"], "del_client") {
		t.Error("client disconnection must not be granted")
	}
	if !slices.Contains(w.UBus["hostapd.*"], "rrm_nr_set") {
		t.Error("managed 802.11k neighbour updates require rrm_nr_set")
	}
	if len(w.File) != 0 {
		t.Errorf("unused write file commands remain granted: %v", w.File)
	}
}

func TestProductionACLGrantsReadOnlyUTCClockSources(t *testing.T) {
	var acl map[string]struct {
		Read struct {
			UBus map[string][]string `json:"ubus"`
		} `json:"read"`
		Write struct {
			UBus map[string][]string `json:"ubus"`
		} `json:"write"`
	}
	if err := json.Unmarshal(ACL, &acl); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"getLocaltime", "getUnixtime"} {
		if !slices.Contains(acl["oonfeewrt"].Read.UBus["luci"], method) {
			t.Errorf("read-only clock source luci.%s is not granted", method)
		}
		if slices.Contains(acl["oonfeewrt"].Write.UBus["luci"], method) {
			t.Errorf("clock source luci.%s was granted write access", method)
		}
	}
}
