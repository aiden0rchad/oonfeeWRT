package daemon

import (
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
)

// Parsing the mesh id back out of a section name is how an applied section is
// tied to the mesh in the site model. Reconstructing the name instead would
// mean asking the renderer what it WOULD do, when the question is what it did.
func TestMeshIDFromSection(t *testing.T) {
	cases := []struct {
		section string
		want    int
		ok      bool
	}{
		{"oowrt_mesh1_radio0", 1, true},
		{"oowrt_mesh42_radio1", 42, true},
		// A WLAN, not a mesh — the prefixes are one character apart and this is
		// the pair most likely to be confused.
		{"oowrt_wlan1_radio0", 0, false},
		{"default_radio0", 0, false},
		{"oowrt_mesh_radio0", 0, false},
		{"oowrt_mesh1", 0, false},
		{"oowrt_meshx_radio0", 0, false},
	}
	for _, c := range cases {
		got, ok := meshIDFromSection(c.section)
		if ok != c.ok || got != c.want {
			t.Errorf("%q: got (%d,%v), want (%d,%v)", c.section, got, ok, c.want, c.ok)
		}
	}
}

// The device's own section mapping wins. Where it is missing — which the
// captured hardware fixture shows really happens — a single mesh interface can
// be attributed, and two cannot: the site model permits one mesh id on two
// bands, and a wrong attribution is worse than none.
func TestIfaceForSectionRefusesToGuessBetweenTwo(t *testing.T) {
	sections := map[string]string{"phy0-mesh0": "oowrt_mesh1_radio0"}
	modes := map[string]string{"phy0-mesh0": "mesh", "phy0-ap0": "ap"}

	if got := ifaceForSection(sections, modes, "oowrt_mesh1_radio0"); got != "phy0-mesh0" {
		t.Errorf("the device's own mapping was not used: %q", got)
	}

	// No section names reported, one mesh interface: attributable.
	if got := ifaceForSection(nil, modes, "oowrt_mesh1_radio0"); got != "phy0-mesh0" {
		t.Errorf("a lone mesh interface should be attributable, got %q", got)
	}

	// No section names, two mesh interfaces: refuse.
	two := map[string]string{"phy0-mesh0": "mesh", "phy1-mesh0": "mesh"}
	if got := ifaceForSection(nil, two, "oowrt_mesh1_radio0"); got != "" {
		t.Errorf("guessed between two mesh interfaces: %q", got)
	}
}

// A poll that carries no interface list must not erase the one we have.
//
// The list rides a 15-minute cadence, so most polls carry none. Overwriting on
// every snapshot makes the readout flicker between a real state and "cannot
// tell" for reasons no operator could see — the same rule client scoping
// already had to learn.
func TestMeshFactsCarryTheInterfaceListForward(t *testing.T) {
	var m meshStore

	m.put(collector.Snapshot{
		DeviceID:     1,
		IfaceModes:   map[string]string{"phy0-mesh0": "mesh"},
		NetDevsFresh: true,
		Interfaces:   map[string]collector.Interface{"phy0-mesh0": {Up: true}},
	})
	if f := m.get(1); !f.ifacesFresh || f.modes["phy0-mesh0"] != "mesh" {
		t.Fatalf("first snapshot not retained: %+v", f)
	}

	// A baseline poll: no interface list, but liveness answered.
	m.put(collector.Snapshot{
		DeviceID:     1,
		NetDevsFresh: true,
		Interfaces:   map[string]collector.Interface{"phy0-mesh0": {Up: false}},
	})
	f := m.get(1)
	if !f.ifacesFresh || f.modes["phy0-mesh0"] != "mesh" {
		t.Error("a poll with no interface list erased the one we had")
	}
	if f.up["phy0-mesh0"] {
		t.Error("the fresh liveness reading was not applied")
	}
}

// And liveness that did NOT answer must not leave a stale "up" behind
// masquerading as current.
func TestMeshFactsDoNotClaimStaleLiveness(t *testing.T) {
	var m meshStore
	m.put(collector.Snapshot{
		DeviceID: 1, NetDevsFresh: true,
		Interfaces: map[string]collector.Interface{"phy0-mesh0": {Up: true}},
	})
	m.put(collector.Snapshot{DeviceID: 1}) // refused

	if f := m.get(1); f.netDevsFresh {
		t.Error("a poll whose liveness call did not answer still reports fresh, " +
			"so a stale 'up' would be rendered as current")
	}
}
