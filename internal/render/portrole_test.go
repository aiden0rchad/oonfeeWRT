package render

import (
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func vlan1WithPorts(ports string) Existing {
	existing := vlanAware()
	existing.Configs["network"]["their_bv1"]["ports"] = ports
	existing.Configs["network"]["their_bv1"][ListsKey] = "ports"
	return existing
}

func accessSite(roles ...model.PortRole) model.Site {
	return model.Site{
		UUID: "abc",
		Networks: []model.Network{
			{ID: 1, Name: "Dom", VLAN: 50, CIDR: "192.168.50.1/24", Zone: "dom", Enabled: true},
			{ID: 2, Name: "Internal", VLAN: 60, CIDR: "192.168.60.1/24", Zone: "internal", Enabled: true},
		},
		PortRoles: roles,
	}
}

func gateway() model.Device { return model.Device{ID: 7, Role: model.RoleGateway} }

func TestDefaultTrunkDoesNotRewriteVLAN1(t *testing.T) {
	existing := vlan1WithPorts("lan1:u* lan2 lan3")
	doc, rep, err := Render(accessSite(), gateway(), netCaps(), existing)
	if err != nil || rep.HasConflicts() {
		t.Fatalf("render: %v conflicts=%+v", err, rep.Conflicts)
	}
	if got := strings.Join(sectionsIn(doc, "network")["oowrt_bv50"].Lists["ports"], " "); got != "lan1:t lan2:t lan3:t lan4:t" {
		t.Fatalf("Dom ports = %q, want every LAN port tagged", got)
	}
	if len(doc.Patches) != 0 {
		t.Fatalf("default trunk edited VLAN 1: %+v", doc.Patches)
	}
	for _, op := range doc.Plan(existing).Ops {
		if op.Section == "their_bv1" || op.Section == "lan" || op.Values["vlan_filtering"] != "" {
			t.Fatalf("default trunk produced %s %s %+v", op.Kind, op.Section, op.Values)
		}
	}
}

func TestAccessPortIsUntaggedOnItsVLANOnly(t *testing.T) {
	existing := vlan1WithPorts("lan1:u* lan2 lan3")
	doc, rep, err := Render(accessSite(model.PortRole{
		DeviceID: 7, Port: "lan2", Mode: model.PortRoleAccess, NetworkID: 1,
	}), gateway(), netCaps(), existing)
	if err != nil || rep.HasConflicts() || len(rep.Omissions) != 0 {
		t.Fatalf("render: %v conflicts=%+v omissions=%+v", err, rep.Conflicts, rep.Omissions)
	}
	if got := strings.Join(sectionsIn(doc, "network")["oowrt_bv50"].Lists["ports"], " "); got != "lan1:t lan2:u* lan3:t lan4:t" {
		t.Fatalf("Dom ports = %q, want lan2 untagged and primary", got)
	}
	if got := strings.Join(sectionsIn(doc, "network")["oowrt_bv60"].Lists["ports"], " "); got != "lan1:t lan3:t lan4:t" {
		t.Fatalf("Internal ports = %q, want lan2 absent", got)
	}
	if doc.Patches == nil || len(doc.Patches) != 1 {
		t.Fatalf("patches = %+v", doc.Patches)
	}
	patch := doc.Patches[0]
	if patch.Config != "network" || patch.Section != "their_bv1" || len(patch.Values) != 0 {
		t.Fatalf("VLAN 1 patch rewrote more than membership: %+v", patch)
	}
	if got := strings.Join(patch.Lists["ports"], " "); got != "lan1:u* lan3" {
		t.Fatalf("VLAN 1 ports = %q, want lan2 removed and the other tokens kept", got)
	}
	var staged bool
	for _, op := range doc.Plan(existing).Ops {
		if op.Config == "network" && op.Section == "lan" {
			t.Fatal("access role moved or edited interface lan")
		}
		// Owned bridge-vlan sections set device and local. The foreign patch
		// must not: those options identify VLAN 1 and the CPU port.
		if op.Patch && (op.Values["vlan_filtering"] != "" || op.Values["device"] != "" || op.Values["local"] != "") {
			t.Fatalf("access role wrote %s %s %+v", op.Section, op.Option, op.Values)
		}
		if op.Section == "their_bv1" && op.Kind == applyengine.OpSet && op.Patch {
			if got := strings.Join(op.Lists["ports"], " "); got != "lan1:u* lan3" {
				t.Fatalf("staged VLAN 1 ports = %q", got)
			}
			if op.Values[OwnershipTag] != "" || len(op.Values) != 0 {
				t.Fatalf("VLAN 1 patch claimed the section: %+v", op.Values)
			}
			staged = true
		}
	}
	if !staged {
		t.Fatal("VLAN 1 membership change was not staged")
	}
}

func TestAccessOnAnotherDeviceDoesNotChangeThisOne(t *testing.T) {
	existing := vlan1WithPorts("lan1:u* lan2 lan3")
	doc, rep, err := Render(accessSite(model.PortRole{
		DeviceID: 8, Port: "lan2", Mode: model.PortRoleAccess, NetworkID: 1,
	}), gateway(), netCaps(), existing)
	if err != nil || len(rep.Omissions) != 0 || len(doc.Patches) != 0 {
		t.Fatalf("err=%v omissions=%+v patches=%+v", err, rep.Omissions, doc.Patches)
	}
	if got := strings.Join(sectionsIn(doc, "network")["oowrt_bv50"].Lists["ports"], " "); got != "lan1:t lan2:t lan3:t lan4:t" {
		t.Fatalf("ports = %q", got)
	}
}

func TestAccessRefusesToRemoveLastUntaggedVLAN1Port(t *testing.T) {
	existing := vlan1WithPorts("lan2")
	doc, rep, err := Render(accessSite(model.PortRole{
		DeviceID: 7, Port: "lan2", Mode: model.PortRoleAccess, NetworkID: 1,
	}), gateway(), netCaps(), existing)
	if err != nil || rep.HasConflicts() {
		t.Fatalf("render: %v %+v", err, rep.Conflicts)
	}
	if len(doc.Patches) != 0 {
		t.Fatalf("refused role still edited VLAN 1: %+v", doc.Patches)
	}
	if got := strings.Join(sectionsIn(doc, "network")["oowrt_bv50"].Lists["ports"], " "); got != "lan1:t lan2:t lan3:t lan4:t" {
		t.Fatalf("refused access still untagged the port: %q", got)
	}
	if len(rep.Omissions) != 1 || !strings.Contains(rep.Omissions[0].Reason, "no untagged member") {
		t.Fatalf("omissions = %+v", rep.Omissions)
	}
}

func TestAccessKeepsEarlierPortsWhenALaterOneWouldEmptyVLAN1(t *testing.T) {
	existing := vlan1WithPorts("lan1:u* lan2")
	doc, rep, err := Render(accessSite(
		model.PortRole{DeviceID: 7, Port: "lan1", Mode: model.PortRoleAccess, NetworkID: 1},
		model.PortRole{DeviceID: 7, Port: "lan2", Mode: model.PortRoleAccess, NetworkID: 2},
	), gateway(), netCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(doc.Patches[0].Lists["ports"], " "); got != "lan2" {
		t.Fatalf("VLAN 1 ports = %q, want lan1 removed and lan2 kept", got)
	}
	if got := strings.Join(sectionsIn(doc, "network")["oowrt_bv50"].Lists["ports"], " "); !strings.Contains(got, "lan1:u*") || strings.Contains(got, "lan2:u*") {
		t.Fatalf("Dom ports = %q", got)
	}
	if got := strings.Join(sectionsIn(doc, "network")["oowrt_bv60"].Lists["ports"], " "); strings.Contains(got, "lan2:u*") || !strings.Contains(got, "lan2:t") {
		t.Fatalf("Internal ports = %q, want lan2 still tagged", got)
	}
	if len(rep.Omissions) != 1 || !strings.Contains(rep.Omissions[0].Reason, "lan2") {
		t.Fatalf("omissions = %+v", rep.Omissions)
	}
}

func TestExplicitTrunkRestoresAMissingVLAN1Member(t *testing.T) {
	existing := vlan1WithPorts("lan1:u*")
	doc, rep, err := Render(accessSite(model.PortRole{
		DeviceID: 7, Port: "lan2", Mode: model.PortRoleTrunk,
	}), gateway(), netCaps(), existing)
	if err != nil || len(rep.Omissions) != 0 {
		t.Fatalf("err=%v omissions=%+v", err, rep.Omissions)
	}
	if got := strings.Join(doc.Patches[0].Lists["ports"], " "); got != "lan1:u* lan2:u*" {
		t.Fatalf("VLAN 1 ports = %q", got)
	}
	if got := strings.Join(sectionsIn(doc, "network")["oowrt_bv50"].Lists["ports"], " "); got != "lan1:t lan2:t lan3:t lan4:t" {
		t.Fatalf("trunk port was not tagged on the panel VLAN: %q", got)
	}
}

func TestExplicitTrunkDoesNotRewriteAnExistingUntaggedToken(t *testing.T) {
	existing := vlan1WithPorts("lan1:u* lan2 lan3")
	doc, _, err := Render(accessSite(model.PortRole{
		DeviceID: 7, Port: "lan2", Mode: model.PortRoleTrunk,
	}), gateway(), netCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Patches) != 0 {
		t.Fatalf("explicit trunk rewrote VLAN 1: %+v", doc.Patches)
	}
}

func TestAccessIsOmittedWhenVLAN1PortsAreNotAList(t *testing.T) {
	doc, rep, err := Render(accessSite(model.PortRole{
		DeviceID: 7, Port: "lan2", Mode: model.PortRoleAccess, NetworkID: 1,
	}), gateway(), netCaps(), vlanAware())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Patches) != 0 {
		t.Fatalf("unknown ports shape was still patched: %+v", doc.Patches)
	}
	if got := strings.Join(sectionsIn(doc, "network")["oowrt_bv50"].Lists["ports"], " "); got != "lan1:t lan2:t lan3:t lan4:t" {
		t.Fatalf("ports = %q", got)
	}
	if len(rep.Omissions) != 1 || !strings.Contains(rep.Omissions[0].Reason, "not a UCI list") {
		t.Fatalf("omissions = %+v", rep.Omissions)
	}
}

func TestWANPortRoleIsNeverApplied(t *testing.T) {
	existing := vlan1WithPorts("lan1:u* lan2 lan3")
	doc, rep, err := Render(accessSite(model.PortRole{
		DeviceID: 7, Port: "wan", Mode: model.PortRoleAccess, NetworkID: 1,
	}), gateway(), netCaps(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Patches) != 0 {
		t.Fatalf("WAN role edited VLAN 1: %+v", doc.Patches)
	}
	if len(rep.Omissions) != 1 || !strings.Contains(rep.Omissions[0].Reason, "WAN") {
		t.Fatalf("omissions = %+v", rep.Omissions)
	}
}

func TestTaggedVLAN1MemberCanLeaveWithoutCountingAsManagement(t *testing.T) {
	existing := vlan1WithPorts("lan1:u* lan2:t")
	doc, rep, err := Render(accessSite(model.PortRole{
		DeviceID: 7, Port: "lan2", Mode: model.PortRoleAccess, NetworkID: 1,
	}), gateway(), netCaps(), existing)
	if err != nil || len(rep.Omissions) != 0 {
		t.Fatalf("err=%v omissions=%+v", err, rep.Omissions)
	}
	if got := strings.Join(doc.Patches[0].Lists["ports"], " "); got != "lan1:u*" {
		t.Fatalf("VLAN 1 ports = %q", got)
	}
}
