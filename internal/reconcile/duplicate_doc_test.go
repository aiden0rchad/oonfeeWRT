package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

func TestApplyRejectsDuplicateDesiredSectionsBeforeSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nonempty bool
	}{
		{name: "nonempty plan", nonempty: true},
		{name: "empty plan", nonempty: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := dial(t)
			r, db := newReconciler(t)
			dev := device(t, db)
			name := "oowrt_wlan99_radio0"
			sec := render.Section{
				Config: "wireless", Type: "wifi-iface", Name: name,
				Values: map[string]string{
					render.OwnershipTag: "1", "device": "radio0",
					"mode": "ap", "ssid": "duplicate-guard",
				},
			}
			if err := db.ReplaceOwned(ctx, dev.ID, []store.OwnedSection{{
				DeviceID: dev.ID, Config: "wireless", Section: "sentinel",
				RenderedHash: "keep", AppliedAt: 123,
			}}); err != nil {
				t.Fatal(err)
			}
			p := &DevicePlan{
				Device: model.Device{ID: dev.ID},
				Doc:    render.Doc{DeviceID: dev.ID, Sections: []render.Section{sec, sec}},
				Existing: render.NewExisting(map[string]map[string]map[string]string{
					"wireless": {},
				}),
			}
			if tc.nonempty {
				p.Plan.Ops = []applyengine.Op{{
					Kind: applyengine.OpAdd, Config: sec.Config, Type: sec.Type,
					Name: sec.Name, Section: sec.Name, Values: sec.Values,
				}}
			}
			healthRan := false
			requests := c.Requests()
			res, err := r.Apply(ctx, c, dev.ID, p, func(context.Context, *ubus.Client) error {
				healthRan = true
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "duplicate desired section") ||
				!strings.Contains(err.Error(), "wireless."+name) {
				t.Fatalf("duplicate document error = %v", err)
			}
			if res.Outcome != "" {
				t.Errorf("preflight rejection claimed router outcome %q", res.Outcome)
			}
			if healthRan {
				t.Error("duplicate document reached runtime health")
			}
			if got := c.Requests(); got != requests {
				t.Errorf("duplicate document made %d ubus request(s)", got-requests)
			}
			owned, ownedErr := db.OwnedSections(ctx, dev.ID)
			if ownedErr != nil || len(owned) != 1 || owned[0].Section != "sentinel" ||
				owned[0].RenderedHash != "keep" || owned[0].AppliedAt != 123 {
				t.Errorf("duplicate rejection changed ownership: owned=%+v err=%v", owned, ownedErr)
			}
		})
	}
}

func TestSharedPHYTargetsApplyAndRemainIdempotent(t *testing.T) {
	ctx := context.Background()
	c := dial(t)
	r, db := newReconciler(t)
	dev := device(t, db)
	site := siteWLAN(dev.ID, 98)
	caps := capability.NewRegistry()
	caps.Radios = []capability.Radio{
		{Section: "radio0", Device: "phy0.0-ap0", Phy: "phy0", Frequency: 2412},
		{Section: "radio1", Device: "phy0.1-ap0", Phy: "phy0", Frequency: 5180},
	}

	p, err := r.PlanDevice(ctx, c, site, model.Device{ID: dev.ID}, caps)
	if err != nil {
		t.Fatal(err)
	}
	if p.Empty() || p.Blocked() {
		t.Fatalf("shared-PHY plan empty=%v blocked=%v report=%+v", p.Empty(), p.Blocked(), p.Report)
	}
	for name, target := range map[string]string{
		"oowrt_wlan98_radio0": "radio0",
		"oowrt_wlan98_radio1": "radio1",
	} {
		found := false
		for _, section := range p.Doc.Sections {
			if section.Config == "wireless" && section.Name == name && section.Values["device"] == target {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s targeting %s: %+v", name, target, p.Doc.Sections)
		}
	}
	res, err := r.Apply(ctx, c, dev.ID, p, func(context.Context, *ubus.Client) error { return nil })
	if err != nil || res.Outcome != applyengine.Applied {
		t.Fatalf("Apply: result=%+v err=%v", res, err)
	}
	owned, err := db.OwnedSections(ctx, dev.ID)
	if err != nil || len(owned) != 2 {
		t.Fatalf("owned after apply = %+v, err=%v", owned, err)
	}

	again, err := r.PlanDevice(ctx, c, site, model.Device{ID: dev.ID}, caps)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("already matching shared-PHY plan has ops: %+v", again.Plan.Ops)
	}
	res, err = r.Apply(ctx, c, dev.ID, again, func(context.Context, *ubus.Client) error { return nil })
	if err != nil || res.Outcome != applyengine.Applied {
		t.Fatalf("idempotent Apply: result=%+v err=%v", res, err)
	}
	owned, err = db.OwnedSections(ctx, dev.ID)
	if err != nil || len(owned) != 2 {
		t.Fatalf("owned after no-op = %+v, err=%v", owned, err)
	}
}
