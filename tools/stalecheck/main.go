package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"

	_ "modernc.org/sqlite"
)

// What an operator turns OFF in the UI, and what the device is then told.
func main() {
	ctx := context.Background()
	db, _ := store.Open(ctx, "sqlite", os.Args[1])
	defer db.Close()
	site, _ := db.Site(ctx)
	devs, _ := db.Devices(ctx)

	mods := []struct {
		name string
		mod  func(*model.WLAN)
	}{
		{"turn 802.11r OFF", func(w *model.WLAN) { w.Roaming.FT = false }},
		{"turn 802.11k/v OFF", func(w *model.WLAN) { w.Roaming.KV = false }},
		{"turn client isolation ON then OFF", func(w *model.WLAN) { w.Options.Isolate = false }},
		{"switch security to OPEN", func(w *model.WLAN) { w.Security.Mode = model.SecNone }},
	}

	for _, d := range devs {
		if d.CapsJSON == "" {
			continue
		}
		var caps capability.Registry
		json.Unmarshal([]byte(d.CapsJSON), &caps)
		c := ubus.New(ubus.Options{Host: d.Host, Timeout: 10 * time.Second})
		if err := c.Login(ctx, "root", ""); err != nil {
			continue
		}
		existing, err := reconcile.ReadExisting(ctx, c)
		c.Close()
		if err != nil {
			continue
		}
		fmt.Printf("\n=== %s ===\n", d.Name)
		cur := existing.In("wireless")["oowrt_wlan1_radio0"]
		fmt.Printf("  device currently holds: ieee80211r=%q md=%q ieee80211k=%q ieee80211w=%q isolate=%q\n",
			cur["ieee80211r"], cur["mobility_domain"], cur["ieee80211k"],
			cur["ieee80211w"], cur["isolate"])

		for _, m := range mods {
			s2 := site
			s2.WLANs = append([]model.WLAN(nil), site.WLANs...)
			m.mod(&s2.WLANs[0])
			doc, _, err := render.Render(s2, model.Device{
				ID: d.ID, Name: d.Name, Role: model.RoleOf(d.Role)}, &caps, existing)
			if err != nil {
				fmt.Printf("  %-36s render error: %v\n", m.name, err)
				continue
			}
			ops := doc.Plan(existing).Ops
			// What the plan as a whole does to each option the device holds:
			// an OpSet carries the new value, an OpDelete removes it. Anything
			// in neither is left exactly as it is.
			written := map[string]bool{}
			deleted := map[string]bool{}
			var sets, dels int
			for _, op := range ops {
				switch op.Kind {
				case applyengine.OpDelete:
					dels++
					if op.Option != "" {
						deleted[op.Option] = true
					}
				default:
					sets++
					for k := range op.Values {
						written[k] = true
					}
				}
			}
			fmt.Printf("  %-36s -> %d op(s) (%d set, %d delete)\n",
				m.name, len(ops), sets, dels)
			var stale []string
			for _, k := range []string{"ieee80211r", "mobility_domain",
				"reassociation_deadline", "ft_over_ds", "ieee80211k",
				"rrm_neighbor_report", "bss_transition", "wnm_sleep_mode",
				"ieee80211w", "isolate", "hidden", "maxassoc", "key"} {
				if cur[k] == "" || cur[k] == "0" {
					continue
				}
				if !written[k] && !deleted[k] {
					stale = append(stale, fmt.Sprintf("%s=%s", k, cur[k]))
				}
			}
			if len(stale) > 0 {
				fmt.Printf("      LEFT ON DEVICE, neither written nor deleted: %v\n", stale)
			}
			if len(deleted) > 0 {
				var d []string
				for k := range deleted {
					d = append(d, k)
				}
				sort.Strings(d)
				fmt.Printf("      cleared from the device: %v\n", d)
			}
		}
	}
}
