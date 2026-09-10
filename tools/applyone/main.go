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
	"reflect"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/toolstore"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: applyone <db> <device-host>")
		os.Exit(2)
	}
	ctx := context.Background()
	handle, err := toolstore.OpenWritable(ctx, os.Args[1])
	must(err)
	defer handle.Close()
	db := handle.DB
	host := os.Args[2]

	site, err := db.Site(ctx)
	must(err)
	devs, err := db.Devices(ctx)
	must(err)

	for _, d := range devs {
		if d.Host != host {
			continue
		}
		must(validateApplyFleet(devs, d))
		problems, err := db.PolicyMACScopeProblems(ctx, site)
		must(err)
		if len(problems) > 0 {
			must(fmt.Errorf("applyone: site policy is not safely renderable: %w", problems[0]))
		}
		var caps capability.Registry
		must(json.Unmarshal([]byte(d.CapsJSON), &caps))
		c := ubus.New(ubus.Options{Host: d.Host, Timeout: 20 * time.Second})
		must(c.Login(ctx, "root", ""))
		defer c.Close()

		r := reconcile.New(db)
		plan, err := r.PlanDevice(ctx, c, site, d.ModelDevice(), &caps)
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

		currentSite, err := db.Site(ctx)
		must(err)
		currentDevices, err := db.Devices(ctx)
		must(err)
		fresh := deviceByID(currentDevices, d.ID)
		must(validateApplyFleet(currentDevices, fresh))
		if fresh == nil || fresh.MAC != d.MAC || !reflect.DeepEqual(currentSite, site) ||
			!reflect.DeepEqual(*fresh, *d) {
			must(fmt.Errorf("applyone: controller state changed while planning; run the command again"))
		}
		d = fresh
		// Planning can take long enough for the collector to reclassify a client.
		// Re-prove MAC scope at the last boundary before reconcile may write.
		problems, err = db.PolicyMACScopeProblems(ctx, currentSite)
		must(err)
		if len(problems) > 0 {
			must(fmt.Errorf("applyone: site policy is no longer safely renderable: %w", problems[0]))
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

func deviceByID(devices []*store.Device, id int64) *store.Device {
	for _, device := range devices {
		if device != nil && device.ID == id {
			return device
		}
	}
	return nil
}

func validateApplyFleet(devices []*store.Device, target *store.Device) error {
	if err := validateApplyTarget(target); err != nil {
		return err
	}
	for _, device := range devices {
		if device == nil || !device.Adopted() {
			continue
		}
		if device.ManagementModeError != "" || device.FunctionError != "" {
			return fmt.Errorf("applyone: adopted fleet contains invalid device metadata; use the controller Preview and repair or re-adopt the affected device")
		}
	}
	return nil
}

func validateApplyTarget(device *store.Device) error {
	if device == nil {
		return fmt.Errorf("applyone: device is unavailable")
	}
	if !device.Adopted() {
		return fmt.Errorf("applyone: device %q is not adopted", device.Name)
	}
	if device.ManagementModeError != "" {
		return fmt.Errorf("applyone: device %q has invalid management mode: %s", device.Name, device.ManagementModeError)
	}
	if device.FunctionError != "" {
		return fmt.Errorf("applyone: device %q has invalid function state: %s", device.Name, device.FunctionError)
	}
	if !device.Configurable() {
		return fmt.Errorf("applyone: device %q is monitor-only; direct apply is disabled", device.Name)
	}
	return nil
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
