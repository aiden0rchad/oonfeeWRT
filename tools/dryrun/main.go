// Command dryrun previews the live site model against real devices without
// the daemon, and writes nothing anywhere.
//
// It exists because the mock cannot answer the questions that matter. The
// suite was green while the reference Archer C6 was being marked blind about
// its own wired layout — a board that had reported that layout perfectly well
// — and no test in the repo could have shown it, because no test knows what a
// swconfig board reports. Pointing the real renderer at the real devices found
// it in one run (§5av).
//
// Read-only by construction: it opens the store, renders, and plans. It never
// stages, applies, commits or writes. Safe to run against a live fleet while
// the daemon is up.
//
//	go run ./tools/dryrun /path/to/.run/oonfeewrt.db
//
// Logs in as root with an empty password, which is what the lab devices use.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	db, err := store.Open(ctx, "sqlite", os.Args[1])
	must(err)
	defer db.Close()

	site, err := db.Site(ctx)
	must(err)
	fmt.Printf("site %q: %d networks, %d WLANs, %d groups, %d meshes, %d uplinks\n",
		site.UUID[:8], len(site.Networks), len(site.WLANs), len(site.Groups),
		len(site.Meshes), len(site.Uplinks))
	if errs := site.Validate(); len(errs) > 0 {
		fmt.Println("  site invalid:", errs[0])
	}

	devs, err := db.Devices(ctx)
	must(err)
	for _, d := range devs {
		if d.CapsJSON == "" || d.CapsJSON == "{}" {
			continue
		}
		fmt.Printf("\n=== %s (%s) role=%s ===\n", d.Name, d.Host, d.Role)
		var caps capability.Registry
		must(json.Unmarshal([]byte(d.CapsJSON), &caps))
		fmt.Printf("  radios=%d survey=%s mesh=%s uplink=%s\n",
			len(caps.Radios), caps.State(capability.FeatSurvey),
			caps.State(capability.FeatMesh), caps.State(capability.FeatWirelessUplink))

		c := ubus.New(ubus.Options{Host: d.Host, Timeout: 10 * time.Second})
		if err := c.Login(ctx, "root", ""); err != nil {
			fmt.Println("  login failed:", err)
			continue
		}
		existing, err := reconcile.ReadExisting(ctx, c)
		if err != nil {
			fmt.Println("  ReadExisting:", err)
			c.Close()
			continue
		}
		doc, rep, err := render.Render(site, model.Device{
			ID: d.ID, Name: d.Name, Role: model.RoleOf(d.Role)}, &caps, existing)
		if err != nil {
			fmt.Println("  render:", err)
			c.Close()
			continue
		}
		fmt.Printf("  rendered %d sections; blind=%v retain=%d\n",
			len(doc.Sections), doc.Blind, len(doc.Retain))
		for _, cf := range rep.Conflicts {
			fmt.Printf("  CONFLICT %s.%s: %s\n", cf.Config, cf.Section, cf.Reason)
		}
		for _, om := range rep.Omissions {
			fmt.Printf("  omit[%-12s] %s: %.100s\n", om.Kind, om.WLAN, om.Reason)
		}
		for _, w := range rep.Warnings {
			fmt.Printf("  WARN %s: %.90s\n", w.DefectID, w.Summary)
		}
		plan := doc.Plan(existing)
		prune := doc.Prune(existing)
		fmt.Printf("  PLAN: %d set/add ops, %d prune deletes\n", len(plan.Ops), len(prune))
		for _, op := range plan.Ops {
			fmt.Printf("    %s %s.%s opt=%s\n", op.Kind, op.Config, op.Section, op.Option)
		}
		for _, op := range prune {
			fmt.Printf("    DELETE %s.%s\n", op.Config, op.Section)
		}
		// Does the device hold any of our list options in the malformed shape?
		for _, s := range doc.Sections {
			cur, ok := existing.In(s.Config)[s.Name]
			if !ok {
				continue
			}
			for k := range s.Lists {
				isList, known := render.StoredAsList(cur, k)
				fmt.Printf("    shape %s.%s.%s list=%v known=%v\n",
					s.Config, s.Name, k, isList, known)
			}
		}
		c.Close()
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
