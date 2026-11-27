package store

import (
	"context"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func TestPortRoleRoundTripAndNetworkDeleteBecomesTrunk(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	dev := &Device{MAC: "aa:aa:aa:aa:aa:10", Host: "192.0.2.10", Name: "gateway", Role: "gateway"}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	dom := &model.Network{Name: "Dom", VLAN: 50, CIDR: "192.168.50.1/24", Zone: "dom", Enabled: true}
	if err := db.SaveNetwork(ctx, dom); err != nil {
		t.Fatal(err)
	}
	role := model.PortRole{DeviceID: dev.ID, Port: "lan2", Mode: model.PortRoleAccess, NetworkID: dom.ID}
	if err := db.SavePortRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(site.PortRoles) != 1 || site.PortRoles[0].Mode != model.PortRoleAccess || site.PortRoles[0].Port != "lan2" {
		t.Fatalf("roles = %+v", site.PortRoles)
	}
	if err := db.SavePortRole(ctx, model.PortRole{
		DeviceID: dev.ID, Port: "lan2", Mode: model.PortRoleTrunk,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SavePortRole(ctx, model.PortRole{
		DeviceID: dev.ID, Port: "lan2", Mode: model.PortRoleAccess, NetworkID: dom.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteNetwork(ctx, dom.ID); err != nil {
		t.Fatal(err)
	}
	site, err = db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(site.PortRoles) != 1 || site.PortRoles[0].Mode != model.PortRoleTrunk || site.PortRoles[0].NetworkID != 0 {
		t.Fatalf("after network delete, roles = %+v, want an explicit trunk", site.PortRoles)
	}
}

func TestPortRoleRejectsManagementVLANAndUnknownPorts(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	dev := &Device{MAC: "aa:aa:aa:aa:aa:11", Host: "192.0.2.11", Name: "gateway", Role: "gateway"}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	lan := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.0.2.1/24", Zone: "lan", Enabled: true}
	if err := db.SaveNetwork(ctx, lan); err != nil {
		t.Fatal(err)
	}
	if err := db.SavePortRole(ctx, model.PortRole{
		DeviceID: dev.ID, Port: "lan2", Mode: model.PortRoleAccess, NetworkID: lan.ID,
	}); err == nil {
		t.Fatal("VLAN 1 was accepted as an access network")
	}
	if err := db.SavePortRole(ctx, model.PortRole{
		DeviceID: dev.ID, Port: "lan2:t", Mode: model.PortRoleTrunk,
	}); err == nil {
		t.Fatal("a tagged token was accepted as a port name")
	}
}
