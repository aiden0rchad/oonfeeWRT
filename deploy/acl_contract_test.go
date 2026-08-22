package deploy

import (
	"encoding/json"
	"slices"
	"testing"
)

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
