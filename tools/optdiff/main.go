// Command optdiff prints the option-level difference the current plan would
// make on each device. Read-only.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"

	_ "modernc.org/sqlite"
)

func main() {
	ctx := context.Background()
	db, _ := store.Open(ctx, "sqlite", os.Args[1])
	defer db.Close()
	site, _ := db.Site(ctx)
	devs, _ := db.Devices(ctx)
	for _, d := range devs {
		if d.CapsJSON == "" {
			continue
		}
		var caps capability.Registry
		json.Unmarshal([]byte(d.CapsJSON), &caps)
		c := ubus.New(ubus.Options{Host: d.Host, Timeout: 10 * time.Second})
		if c.Login(ctx, "root", "") != nil {
			continue
		}
		existing, err := reconcile.ReadExisting(ctx, c)
		c.Close()
		if err != nil {
			continue
		}
		doc, _, err := render.Render(site, model.Device{
			ID: d.ID, Name: d.Name, Role: model.RoleOf(d.Role)}, &caps, existing)
		if err != nil {
			continue
		}
		fmt.Printf("\n=== %s ===\n", d.Name)
		for _, s := range doc.Sections {
			cur := existing.In(s.Config)[s.Name]
			keys := make([]string, 0, len(s.Values))
			for k := range s.Values {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var changes []string
			for _, k := range keys {
				if k == "key" {
					if cur[k] != s.Values[k] {
						changes = append(changes, "key: <redacted change>")
					}
					continue
				}
				if cur[k] != s.Values[k] {
					changes = append(changes, fmt.Sprintf("%s: %q -> %q", k, cur[k], s.Values[k]))
				}
			}
			for _, k := range s.Manages {
				if _, w := s.Values[k]; w {
					continue
				}
				if _, on := cur[k]; on {
					changes = append(changes, fmt.Sprintf("%s: %q -> DELETED", k, cur[k]))
				}
			}
			if len(changes) == 0 {
				fmt.Printf("  %s: no change\n", s.Name)
				continue
			}
			fmt.Printf("  %s:\n", s.Name)
			for _, ch := range changes {
				fmt.Printf("      %s\n", ch)
			}
		}
	}
}
