package render

import (
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
)

func bridgeVLAN() Section {
	return Section{
		Config: "network", Type: "bridge-vlan", Name: "oowrt_bv20",
		Values: map[string]string{"device": "br-lan", "vlan": "20", OwnershipTag: "1"},
		Lists:  map[string][]string{"ports": {"lan1:t", "lan2:t"}},
	}
}

// The device holds our ports as an OPTION where UCI wants a LIST.
//
// Section.Lists records what that costs: uci.set accepts it, the device stores
// it, netifd ignores it, VLAN filtering comes on with no untagged membership,
// and the LAN goes down after the apply has already been confirmed healthy.
//
// It flattens to exactly the string our list joins to, so the text comparison
// passes and the plan reported "already matches" — meaning the controller
// could never repair a config a previous version of itself had written.
func TestMalformedListIsSeenAsAChangeToMake(t *testing.T) {
	current := map[string]string{
		"device": "br-lan", "vlan": "20", OwnershipTag: "1",
		"ports": "lan1:t lan2:t", // one option, not two list entries
		// flatten saw no lists in this section and said so.
		ListsKey: "",
	}
	if matches(bridgeVLAN(), current) {
		t.Fatal("a bridge-VLAN whose ports are stored as an option reported as " +
			"already matching: the malformed form can never be corrected")
	}

	existing := NewExisting(map[string]map[string]map[string]string{
		"network": {"oowrt_bv20": current},
	})
	doc := Doc{Sections: []Section{bridgeVLAN()}}
	ops := doc.Plan(existing).Ops
	// The option is deleted before the list is written. Whether rpcd's uci.set
	// converts a string option into a list on its own has not been measured
	// here, and the failure if it does not is the silent one this whole fix is
	// about.
	if len(ops) != 2 {
		t.Fatalf("want a delete of the option then a set, got %d: %+v", len(ops), ops)
	}
	if ops[0].Kind != applyengine.OpDelete || ops[0].Option != "ports" {
		t.Errorf("first op should drop the malformed option: %+v", ops[0])
	}
	if ops[1].Kind != applyengine.OpSet {
		t.Errorf("second op should rewrite the section: %+v", ops[1])
	}
	if got := ops[1].Lists["ports"]; len(got) != 2 {
		t.Errorf("the rewrite does not send a list: %v", got)
	}
}

// The delete fires ONLY for the malformed shape. A correctly-stored list that
// merely changed content is a plain set — deleting first on every list change
// would double the staged calls on the ordinary path.
func TestContentChangeToACorrectListIsJustASet(t *testing.T) {
	existing := NewExisting(map[string]map[string]map[string]string{
		"network": {"oowrt_bv20": {
			"device": "br-lan", "vlan": "20", OwnershipTag: "1",
			"ports": "lan1:t", ListsKey: "ports",
		}},
	})
	ops := Doc{Sections: []Section{bridgeVLAN()}}.Plan(existing).Ops
	if len(ops) != 1 || ops[0].Kind != applyengine.OpSet {
		t.Errorf("want a single set, got %+v", ops)
	}
}

// The correctly-stored form is left alone. Otherwise the fix is just "rewrite
// every list on every plan", and every preview reports changes that change
// nothing.
func TestCorrectlyStoredListIsAMatch(t *testing.T) {
	current := map[string]string{
		"device": "br-lan", "vlan": "20", OwnershipTag: "1",
		"ports":  "lan1:t lan2:t",
		ListsKey: "ports",
	}
	if !matches(bridgeVLAN(), current) {
		t.Error("a correctly-stored list reported as a difference; every " +
			"preview would show changes that change nothing")
	}
}

// And an Existing nobody recorded the shape for stays as it was: unknown is
// not "malformed". Guessing here would rewrite every list read by any future
// path that does not record the marker.
func TestUnrecordedShapeIsNotTreatedAsMalformed(t *testing.T) {
	current := map[string]string{
		"device": "br-lan", "vlan": "20", OwnershipTag: "1",
		"ports": "lan1:t lan2:t",
		// no ListsKey at all
	}
	if !matches(bridgeVLAN(), current) {
		t.Error("a section whose shape was never recorded was treated as " +
			"malformed; unknown and wrong are different states")
	}
}

// A genuine content change is still a change, marker or no marker.
func TestChangedListContentIsStillAChange(t *testing.T) {
	current := map[string]string{
		"device": "br-lan", "vlan": "20", OwnershipTag: "1",
		"ports": "lan1:t", ListsKey: "ports",
	}
	if matches(bridgeVLAN(), current) {
		t.Error("a bridge-VLAN that lost a port reported as matching")
	}
}
