package adoption

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// faultConfigCaller models the only property phase 1 depends on: deletes are
// session-local until commit, and revert discards a partial staged group.
type faultConfigCaller struct {
	sections  map[string]map[string]bool
	staged    map[string]map[string]bool
	deleteErr map[string]error
	commitErr map[string]error
}

func newFaultConfigCaller(sections ...Section) *faultConfigCaller {
	c := &faultConfigCaller{
		sections: map[string]map[string]bool{}, staged: map[string]map[string]bool{},
		deleteErr: map[string]error{}, commitErr: map[string]error{},
	}
	for _, s := range sections {
		if c.sections[s.Config] == nil {
			c.sections[s.Config] = map[string]bool{}
		}
		c.sections[s.Config][s.Section] = true
	}
	return c
}

func (c *faultConfigCaller) Call(_ context.Context, object, method string, args, _ any) error {
	if object != "uci" {
		return fmt.Errorf("unexpected object %s", object)
	}
	m, _ := args.(map[string]any)
	config, _ := m["config"].(string)
	switch method {
	case "delete":
		section, _ := m["section"].(string)
		ref := config + "." + section
		if err := c.deleteErr[ref]; err != nil {
			return err
		}
		if c.staged[config] == nil {
			c.staged[config] = map[string]bool{}
		}
		c.staged[config][section] = true
		return nil
	case "commit":
		if err := c.commitErr[config]; err != nil {
			return err
		}
		for section := range c.staged[config] {
			delete(c.sections[config], section)
		}
		delete(c.staged, config)
		return nil
	case "revert":
		delete(c.staged, config)
		return nil
	default:
		return fmt.Errorf("unexpected method %s", method)
	}
}

func armedBoot() *fakeBoot {
	return &fakeBoot{
		acl: map[string][]byte{DefaultACLPath: []byte("acl")}, login: DefaultUser,
	}
}

func TestUnadoptPreservesEverythingWithoutControllerProof(t *testing.T) {
	owned := []Section{{Config: "wireless", Section: "oowrt_wlan1_radio0"}}
	boot := armedBoot()
	rep, err := testAdopter().Unadopt(context.Background(), nil, boot, owned)
	if !errors.Is(err, ErrControllerRequired) {
		t.Fatalf("error = %v, want ErrControllerRequired", err)
	}
	if rep.ConfigRevertComplete || len(rep.ConfigRemains) != 1 || len(rep.Reverted) != 0 {
		t.Fatalf("an unproved phase 1 was reported as reverted: %+v", rep)
	}
	if boot.login != DefaultUser || len(boot.acl[DefaultACLPath]) == 0 {
		t.Fatal("phase 2 removed the login or ACL without a proved phase 1")
	}
	if !rep.FootprintRemains {
		t.Fatal("an untouched controller footprint was reported absent")
	}
}

func TestUnadoptNeedsNoControllerSessionForKnownEmptyLedger(t *testing.T) {
	boot := armedBoot()
	rep, err := testAdopter().Unadopt(context.Background(), nil, boot, nil)
	if err != nil {
		t.Fatalf("empty ownership ledger should make phase 1 complete: %v", err)
	}
	if !rep.ConfigRevertComplete || len(rep.ConfigRemains) != 0 {
		t.Fatalf("empty phase 1 was not reported complete: %+v", rep)
	}
	if rep.FootprintRemains || !rep.LoginRemoved || !rep.ACLRemoved {
		t.Fatalf("SSH footprint was not removed after empty phase 1: %+v", rep)
	}
	if boot.login != "" || len(boot.acl) != 0 {
		t.Fatal("fake device still contains the login or ACL")
	}
}

func TestUnadoptDeleteFailureDiscardsPartialGroupAndSkipsPhase2(t *testing.T) {
	owned := []Section{
		{Config: "wireless", Section: "oowrt_wlan1_radio0"},
		{Config: "wireless", Section: "oowrt_wlan1_radio1"},
	}
	controller := newFaultConfigCaller(owned...)
	controller.deleteErr["wireless.oowrt_wlan1_radio1"] = errors.New("permission denied")
	boot := armedBoot()
	rep, err := testAdopter().Unadopt(context.Background(), controller, boot, owned)
	if err == nil {
		t.Fatal("a failed owned-section delete reported success")
	}
	for _, s := range owned {
		if !controller.sections[s.Config][s.Section] {
			t.Fatalf("partial phase 1 committed deletion of %s.%s", s.Config, s.Section)
		}
	}
	if rep.ConfigRevertComplete || len(rep.ConfigRemains) != len(owned) || len(rep.Reverted) != 0 {
		t.Fatalf("partial phase 1 report = %+v", rep)
	}
	if boot.login != DefaultUser || len(boot.acl[DefaultACLPath]) == 0 {
		t.Fatal("phase 2 ran after a section delete failed")
	}
}

func TestUnadoptCommitFailureReportsOnlyProvenConfigAndSkipsPhase2(t *testing.T) {
	owned := []Section{
		{Config: "wireless", Section: "oowrt_wlan1_radio0"},
		{Config: "network", Section: "oowrt_bv2"},
	}
	controller := newFaultConfigCaller(owned...)
	controller.commitErr["network"] = errors.New("I/O error")
	boot := armedBoot()
	rep, err := testAdopter().Unadopt(context.Background(), controller, boot, owned)
	if err == nil {
		t.Fatal("an unproved commit reported success")
	}
	if controller.sections["wireless"]["oowrt_wlan1_radio0"] {
		t.Fatal("the successful first config was not committed")
	}
	if !controller.sections["network"]["oowrt_bv2"] {
		t.Fatal("the failed config was claimed deleted")
	}
	if len(rep.Reverted) != 1 || rep.Reverted[0].Config != "wireless" ||
		len(rep.ConfigRemains) != 1 || rep.ConfigRemains[0].Config != "network" ||
		rep.ConfigRevertComplete {
		t.Fatalf("commit-failure report is not exact: %+v", rep)
	}
	if boot.login != DefaultUser || len(boot.acl[DefaultACLPath]) == 0 {
		t.Fatal("phase 2 ran after a config commit was unproved")
	}
	commands := strings.Join(rep.CleanupCommands(), "\n")
	if !strings.Contains(commands, "uci -q delete network.oowrt_bv2") ||
		strings.Contains(commands, "wireless.oowrt_wlan1_radio0") {
		t.Fatalf("cleanup recipe does not match the remaining config: %q", commands)
	}
}
