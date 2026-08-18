// Command applyone applies the site model to ONE device, with the rollback
// armed, using the same reconcile/applyengine path the daemon uses.
//
// One device per invocation, named explicitly, so a fleet cannot be changed by
// a fat-fingered argument.
//
//	go run ./tools/applyone <db> <device-host>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"

	_ "modernc.org/sqlite"
)

func main() {
	ctx := context.Background()
	db, err := store.Open(ctx, "sqlite", os.Args[1])
	must(err)
	defer db.Close()
	host := os.Args[2]

	site, err := db.Site(ctx)
	must(err)
	devs, err := db.Devices(ctx)
	must(err)

	for _, d := range devs {
		if d.Host != host {
			continue
		}
		var caps capability.Registry
		must(json.Unmarshal([]byte(d.CapsJSON), &caps))
		c := ubus.New(ubus.Options{Host: d.Host, Timeout: 20 * time.Second})
		must(c.Login(ctx, "root", ""))
		defer c.Close()

		r := reconcile.New(db)
		plan, err := r.PlanDevice(ctx, c, site,
			model.Device{ID: d.ID, Name: d.Name, Role: model.RoleOf(d.Role)}, &caps)
		must(err)
		if plan.Blocked() {
			fmt.Println("BLOCKED:", plan.Report.Conflicts[0].Reason)
			return
		}
		fmt.Printf("%s: %d op(s)\n", d.Name, len(plan.Plan.Ops))
		for _, op := range plan.Plan.Ops {
			fmt.Printf("  %s %s.%s %s\n", op.Kind, op.Config, op.Section, op.Option)
		}
		// No early return on an empty plan. Apply handles that case and it is
		// not a no-op: it records what the device already carries, which is
		// how a re-adopted device gets an ownership record at all.

		// The SSIDs that must be on the air when this is over. Same idea as
		// daemon.healthCheck: verify what was written actually appeared.
		want := map[string]bool{}
		for _, s := range plan.Doc.Sections {
			if s.Config == "wireless" && s.Values["ssid"] != "" {
				want[s.Values["ssid"]] = true
			}
		}
		health := func(hctx context.Context, hc *ubus.Client) error {
			var devs struct {
				Devices []string `json:"devices"`
			}
			if err := hc.Call(hctx, "iwinfo", "devices", nil, &devs); err != nil {
				return fmt.Errorf("health: iwinfo.devices: %w", err)
			}
			on := map[string]bool{}
			for _, iface := range devs.Devices {
				var info struct {
					SSID string `json:"ssid"`
				}
				if err := hc.Call(hctx, "iwinfo", "info",
					map[string]any{"device": iface}, &info); err != nil {
					continue
				}
				if info.SSID != "" {
					on[info.SSID] = true
				}
			}
			for ssid := range want {
				if !on[ssid] {
					return fmt.Errorf("health: %q is not on the air after the apply", ssid)
				}
			}
			fmt.Printf("  health: on air %v\n", keys(on))
			return nil
		}

		start := time.Now()
		res, err := r.Apply(ctx, c, d.ID, plan, health)
		fmt.Printf("\noutcome=%s stranded=%v after %s\n  reason: %s\n",
			res.Outcome, res.Stranded, time.Since(start).Round(time.Millisecond), res.Reason)
		if res.HealthErr != nil {
			fmt.Println("  health error:", res.HealthErr)
		}
		if err != nil {
			fmt.Println("  error:", err)
			os.Exit(1)
		}
		if res.Outcome != applyengine.Applied {
			os.Exit(2)
		}
		return
	}
	fmt.Println("no adopted device with host", host)
	os.Exit(1)
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
