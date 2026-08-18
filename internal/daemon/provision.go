package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Provisioning: turning the site model into config on devices.
//
// Two entry points, and the split between them is the whole point of the
// pending-changes flow. Preview reads every device and reports what WOULD
// change, touching nothing. Apply executes what Preview described. An operator
// sees the second only after reading the first.
//
// Nothing here re-derives intent. The site model is the intent, internal/render
// turns it into UCI, internal/reconcile diffs and applies it, and this file is
// only the ordering and the fan-out across a fleet.

// previewTimeout bounds a whole preview. It reads every adopted device, and a
// device that has gone away must not hold the screen open indefinitely.
const previewTimeout = 60 * time.Second

// Preview reports what applying the site model would change, per device.
func (d *Daemon) Preview(ctx context.Context) (*api.PreviewResult, error) {
	ctx, cancel := context.WithTimeout(ctx, previewTimeout)
	defer cancel()

	site, err := d.Store.Site(ctx)
	if err != nil {
		return nil, err
	}
	out := &api.PreviewResult{SiteName: site.Name, Devices: []api.DevicePreview{}}

	// Site-level validation first. A bad site model produces one clear error
	// here instead of the same confusing failure once per device.
	for _, e := range site.Validate() {
		out.SiteErrors = append(out.SiteErrors, e.Error())
	}
	if len(out.SiteErrors) > 0 {
		return out, nil
	}

	devices, err := d.Store.Devices(ctx)
	if err != nil {
		return nil, err
	}
	for _, dev := range applyOrder(devices) {
		if !dev.Adopted() {
			continue
		}
		out.Devices = append(out.Devices, d.previewDevice(ctx, site, dev))
	}
	return out, nil
}

// previewDevice plans one device, converting every failure into a reported
// state rather than an error that would hide the rest of the fleet.
//
// A device that is unreachable during a preview is a fact the operator needs —
// applying to the others is usually still right — so it becomes a row that says
// so, not a 502 for the whole screen.
func (d *Daemon) previewDevice(ctx context.Context, site model.Site, dev *store.Device) api.DevicePreview {
	p := api.DevicePreview{DeviceID: dev.ID, Name: dev.Name,
		Role: string(model.RoleOf(dev.Role))}

	caps, err := deviceCaps(dev)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	c, err := d.Connect(ctx, dev)
	if err != nil {
		p.Error = fmt.Sprintf("could not reach this device: %v", err)
		return p
	}
	defer c.Close()

	r := reconcile.New(d.Store)
	plan, err := r.PlanDevice(ctx, c, site, model.Device{
		ID: dev.ID, Name: dev.Name, Role: model.RoleOf(dev.Role),
	}, caps)
	if err != nil {
		p.Error = err.Error()
		return p
	}

	p.Changes = summarise(plan)
	p.Blocked = plan.Blocked()
	p.TouchesTraversal = touchesTraversal(plan)
	for _, cf := range plan.Report.Conflicts {
		p.Conflicts = append(p.Conflicts,
			fmt.Sprintf("%s.%s: %s", cf.Config, cf.Section, cf.Reason))
	}
	p.Omitted, p.Cautions, p.Undetermined = splitOmissions(plan.Report.Omissions)
	for _, w := range plan.Report.Warnings {
		p.DriverDefects = append(p.DriverDefects, api.DriverDefect{
			WLAN: w.WLAN, DefectID: w.DefectID, Summary: w.Summary,
			Detail: w.Detail, Confidence: w.Confidence, Severity: w.Severity,
			Mitigation: w.Mitigation, Source: w.Source,
		})
	}
	for _, dr := range plan.Drift {
		p.Drift = append(p.Drift, dr.String())
	}
	ssidOf := map[int]string{}
	for _, w := range site.WLANs {
		ssidOf[w.ID] = w.SSID
	}
	for _, ov := range site.Overrides[dev.ID] {
		p.Deviations = append(p.Deviations, ov.Describe(ssidOf[ov.WLANID]))
	}
	sort.Strings(p.Deviations)

	if needsExplanation(p) {
		p.CapabilityCause = d.recentCapabilityLoss(ctx, dev.ID)
	}
	return p
}

// needsExplanation reports whether this row has something a recent capability
// change might account for.
//
// A named predicate rather than an inline condition because the tempting
// simplification — always attach the last change — is wrong, and wrong in a way
// that looks like an improvement. A device whose plan is clean does not need to
// be told its radio list changed last week; that is noise on the one screen
// where noise costs most, since it is the screen someone reads immediately
// before writing to their network.
func needsExplanation(p api.DevicePreview) bool {
	return len(p.Omitted) > 0 || p.Blocked || len(p.Conflicts) > 0
}

// summarise renders a plan's operations as lines an operator can read.
//
// Deliberately not a raw op dump: "add wifi-iface oowrt_wlan3_radio0" tells
// someone what changed, and a JSON blob of forty UCI options does not.
func summarise(p *reconcile.DevicePlan) []api.Change {
	out := make([]api.Change, 0, len(p.Plan.Ops))
	for _, op := range p.Plan.Ops {
		c := api.Change{Config: op.Config, Section: op.Section}
		switch op.Kind {
		case applyengine.OpAdd:
			c.Action = "create"
		case applyengine.OpSet:
			c.Action = "update"
		case applyengine.OpDelete:
			c.Action = "remove"
		default:
			c.Action = string(op.Kind)
		}
		// The passphrase is never put in a preview. An operator who wants to
		// check it can look at the WLAN; a diff screen is exactly the kind of
		// thing that ends up in a screenshot in a support thread.
		keys := make([]string, 0, len(op.Values))
		for k := range op.Values {
			if k == "key" || k == "wpa_psk" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		c.Options = keys
		if _, hasKey := op.Values["key"]; hasKey {
			c.TouchesKey = true
		}
		out = append(out, c)
	}
	return out
}

// ApplySite pushes the site model to the fleet.
//
// Serial, in dependency order, aborting on the first failure — IMPLEMENTATION
// §6. Serial rather than parallel because a failure part-way through a parallel
// fan-out leaves an operator with no idea which devices took the change, and
// because the gateway must be last: it is the path the controller's own traffic
// takes to reach everything else, so breaking it first would strand the rest of
// the queue mid-apply with rollback timers armed and nobody able to confirm.
func (d *Daemon) ApplySite(ctx context.Context, req api.ApplyRequest) (*api.ApplyResult, error) {
	site, err := d.Store.Site(ctx)
	if err != nil {
		return nil, err
	}
	if errs := site.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("the site model is not valid: %s", errs[0])
	}
	devices, err := d.Store.Devices(ctx)
	if err != nil {
		return nil, err
	}
	want := map[int64]bool{}
	for _, id := range req.DeviceIDs {
		want[id] = true
	}

	out := &api.ApplyResult{Devices: []api.DeviceApply{}}
	for _, dev := range applyOrder(devices) {
		if !dev.Adopted() || (len(want) > 0 && !want[dev.ID]) {
			continue
		}
		res := d.applyDevice(ctx, site, dev, req.AcknowledgeTraversal)
		out.Devices = append(out.Devices, res)
		// An apply reconfigures the radios, which is the one thing the
		// 15-minute interface cadence assumes does not happen between reads.
		// Without this, a mesh applied now has its interface a few seconds
		// later while the cached list still says that section has none — and
		// the health readout calls that a critical fault, for up to fifteen
		// minutes, after every successful mesh apply. Measured on hardware.
		if res.Changes > 0 {
			if c := d.collectorRef(); c != nil {
				c.Rediscover(dev.ID)
			}
		}
		if res.Outcome != string(applyengine.Applied) {
			// First failure stops the queue. Continuing would apply a
			// half-consistent site — some APs on the new SSID, some on the old
			// — which is worse than stopping somewhere an operator can see.
			out.Aborted = true
			out.AbortedAfter = dev.Name
			break
		}
	}

	// An apply that reconfigures a BSS restarts it, and a restarted BSS comes
	// back with an empty 802.11k neighbour list. Measured on the reference
	// hardware: `wifi reload` cleared the list on the BSS whose section had
	// changed and left the untouched BSS on the same radio holding its own, so
	// this cannot be predicted from the plan — only re-read.
	//
	// Detached from the request's context because it must outlive the HTTP
	// response, and quietly: an operator applied a site model, and a warning
	// about neighbour lists would read as though their apply had failed. The
	// fifteen-minute loop is the backstop if this cycle does not land.
	d.nudgeNeighbours()

	return out, nil
}

func (d *Daemon) applyDevice(ctx context.Context, site model.Site, dev *store.Device,
	ackTraversal bool) api.DeviceApply {
	out := api.DeviceApply{DeviceID: dev.ID, Name: dev.Name}

	caps, err := deviceCaps(dev)
	if err != nil {
		out.Outcome, out.Reason = "error", err.Error()
		return out
	}

	err = d.TrackApply(ctx, dev.ID, func(ctx context.Context) error {
		c, err := d.Connect(ctx, dev)
		if err != nil {
			return fmt.Errorf("could not reach this device: %w", err)
		}
		defer c.Close()

		r := reconcile.New(d.Store)
		plan, err := r.PlanDevice(ctx, c, site, model.Device{
			ID: dev.ID, Name: dev.Name, Role: model.RoleOf(dev.Role),
		}, caps)
		if err != nil {
			return err
		}
		if plan.Empty() && !plan.Blocked() {
			out.Outcome = string(applyengine.Applied)
			out.Reason = "already matches the site model"
			return nil
		}
		// Pass the acknowledgment down: the engine gates on it too, and a
		// preflight refusal at that depth reads as a bug rather than a policy.
		plan.Plan.AcknowledgeTraversal = ackTraversal
		if touchesTraversal(plan) && !ackTraversal {
			return fmt.Errorf("this change edits %s's network or firewall "+
				"configuration, which carries the path the controller reaches it "+
				"through. Re-run the apply with the traversal acknowledgment to "+
				"proceed — the change is applied with a rollback armed either way, "+
				"but you should know you are editing the road before driving "+
				"down it", dev.Name)
		}
		res, err := r.Apply(ctx, c, dev.ID, plan, healthCheck(plan))
		out.Outcome = string(res.Outcome)
		out.Reason = res.Reason
		out.Changes = len(plan.Plan.Ops)
		return err
	})
	if err != nil {
		out.Outcome = "error"
		if out.Reason == "" {
			out.Reason = err.Error()
		}
		return out
	}
	return out
}

// healthCheck verifies the change is actually good, while the rollback timer is
// still armed.
//
// Runtime state only, never uci.get: inside the confirm window a uci read is
// overlaid with the applying session's own staged delta, so it will happily
// confirm a change that is not really on the device (IMPLEMENTATION §6). The
// SSIDs are read from hostapd, which is both the cheap source and the one that
// answers — `network.wireless status` is unreachable through rpcd.
func healthCheck(plan *reconcile.DevicePlan) applyengine.HealthCheck {
	wantSSIDs := map[string]bool{}
	for _, s := range plan.Doc.Sections {
		if s.Config == "wireless" && s.Values["ssid"] != "" && s.Values["disabled"] != "1" {
			wantSSIDs[s.Values["ssid"]] = true
		}
	}
	// The SSIDs this change REMOVES, so their disappearance is verified rather
	// than assumed.
	//
	// The check used to confirm only that what it wrote had appeared. An apply
	// that deleted a WLAN therefore passed on "the lan interface is up", and
	// the controller recorded the network as gone without ever asking.
	//
	// What this DOES catch: hostapd still holding a BSS it was told to drop —
	// a reload that did not happen, a section deleted from uci while the
	// running config kept it, an apply that reported success and changed
	// nothing.
	//
	// What it does NOT catch, and the comment here said otherwise until it was
	// checked: §0's actual failure. readSSIDs reads iwinfo and hostapd, which
	// is the CONTROL PLANE — the very source that lied for fourteen hours while
	// the radio beaconed wrt-cleanroom. If the firmware keeps transmitting an
	// SSID hostapd has dropped, this gate is told it is gone and agrees.
	//
	// Only a scan answers that, which is what internal/onair is for. It is
	// deliberately not wired in here: a scan takes a radio off-channel for
	// seconds inside a 90-second rollback window, and onair's own rule is that
	// Unheard is not proof of absence — so it would buy an expensive maybe.
	// The honest summary is that this closes the config-plane half of §0 and
	// leaves the firmware half to a check an operator runs.
	//
	// Read from the section's PREVIOUS values — the plan carries what the
	// device had — because a delete op names a section, not an SSID.
	goneSSIDs := map[string]bool{}
	for _, op := range plan.Plan.Ops {
		if op.Config != "wireless" || op.Option != "" {
			continue
		}
		// A section DELETED, and a section whose ssid is being CHANGED. The
		// second is the rename case and it makes the same claim: after this
		// applies, the old name is not on the air. Checking only deletes would
		// leave a renamed WLAN able to keep broadcasting its old SSID while the
		// controller recorded the new one — §0's failure with an extra step.
		switch op.Kind {
		case applyengine.OpDelete:
		case applyengine.OpSet, applyengine.OpAdd:
			if _, changing := op.Values["ssid"]; !changing {
				continue
			}
		default:
			continue
		}
		prev, ok := plan.Existing.In("wireless")[op.Section]
		if !ok {
			continue
		}
		if ssid := prev["ssid"]; ssid != "" && !wantSSIDs[ssid] {
			// Not one we are also writing: an SSID moved between sections, or
			// kept under a new section name, is still meant to be on the air.
			goneSSIDs[ssid] = true
		}
	}
	return func(ctx context.Context, verify *ubus.Client) error {
		// Interfaces first: a change that took the network down is the failure
		// this whole mechanism exists to catch, and it is cheap to see.
		var dump struct {
			Interface []struct {
				Name string `json:"interface"`
				Up   bool   `json:"up"`
			} `json:"interface"`
		}
		if err := verify.Call(ctx, "network.interface", "dump", struct{}{}, &dump); err != nil {
			return fmt.Errorf("health: could not read interface state: %w", err)
		}
		for _, i := range dump.Interface {
			if i.Name == "lan" && !i.Up {
				return fmt.Errorf("health: the lan interface is down after this change")
			}
		}
		if len(wantSSIDs) == 0 && len(goneSSIDs) == 0 {
			return nil
		}

		// Then the SSIDs, from hostapd — the source that answers. Polled
		// rather than read once: bringing up a new BSS is asynchronous, and
		// checking the instant uci.apply returns asks the question before the
		// hardware has had a chance to answer it. Measured on the reference
		// device, a new BSS appears about a second after the reload; the
		// budget here is generous against that so a slow device is not
		// reported as a broken one, and it is bounded so a genuinely broken
		// one still reverts well inside the rollback window.
		var found map[string]bool
		deadline := time.Now().Add(ssidSettleTimeout)
		for {
			found = readSSIDs(ctx, verify)
			if missingFrom(found, wantSSIDs) == nil && stillOn(found, goneSSIDs) == nil {
				return nil
			}
			if time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}

		if lingering := stillOn(found, goneSSIDs); lingering != nil {
			return fmt.Errorf("health: after %s, hostapd is still carrying %d "+
				"SSID(s) this change removed: %v — the config no longer has "+
				"them, so letting the device revert rather than recording them "+
				"as gone (note this reads hostapd, not the air: a radio can "+
				"keep transmitting an SSID hostapd has dropped, which the "+
				"on-air check finds and this cannot)",
				ssidSettleTimeout, len(lingering), lingering)
		}
		missing := missingFrom(found, wantSSIDs)
		// The error names what WAS found, not only what was not. "expected
		// Home, the radios are broadcasting [OtherNet]" points at the problem;
		// "Home is missing" leaves someone to go and look for themselves.
		on := make([]string, 0, len(found))
		for s := range found {
			on = append(on, s)
		}
		sort.Strings(on)
		return fmt.Errorf("health: after %s, %d SSID(s) this change should have "+
			"brought up are not broadcasting: %v (the radios are currently "+
			"carrying %v) — letting the device revert",
			ssidSettleTimeout, len(missing), missing, on)
	}
}

// ssidSettleTimeout is how long a new BSS is given to appear.
//
// Well inside the rollback window (90 s), so a device that never brings the
// SSID up still reverts on its own with time to spare.
const ssidSettleTimeout = 20 * time.Second

// readSSIDs reports every SSID the device is currently broadcasting.
//
// A radio that publishes no hostapd object is skipped rather than treated as a
// failure: a device may legitimately have a radio that is not an AP, and the
// question being asked is "is this SSID on air somewhere", not "is every radio
// doing what I expect".
func readSSIDs(ctx context.Context, c *ubus.Client) map[string]bool {
	found := map[string]bool{}
	var devs struct {
		Devices []string `json:"devices"`
	}
	if err := c.Call(ctx, "iwinfo", "devices", struct{}{}, &devs); err != nil {
		return found
	}
	for _, iface := range devs.Devices {
		var st struct {
			SSID string `json:"ssid"`
		}
		if err := c.Call(ctx, "hostapd."+iface, "get_status", struct{}{}, &st); err != nil {
			continue
		}
		if st.SSID != "" {
			found[st.SSID] = true
		}
	}
	return found
}

func missingFrom(found, want map[string]bool) []string {
	var missing []string
	for ssid := range want {
		if !found[ssid] {
			missing = append(missing, ssid)
		}
	}
	sort.Strings(missing)
	return missing
}

// touchesTraversal delegates to the apply engine's own definition.
//
// Deliberately not a second copy. This layer warns the operator before they
// click; the engine refuses mechanically at preflight. Two guards for one
// concern is reasonable defence in depth, two DEFINITIONS of it is not — they
// drift, and then the warning and the refusal disagree about which changes are
// dangerous.
func touchesTraversal(p *reconcile.DevicePlan) bool {
	return applyengine.TouchesManagementPath(p.Plan)
}

// applyOrder puts gateways last.
//
// The controller's own traffic to every other device traverses the gateway, so
// applying to it first and breaking it would strand the rest of the queue —
// mid-apply, with rollback timers armed and no way to reach them to confirm.
// Within a role, order by ID so a run is reproducible and an operator reading
// two previews can compare them.
func applyOrder(devices []*store.Device) []*store.Device {
	out := append([]*store.Device(nil), devices...)
	sort.SliceStable(out, func(i, j int) bool {
		gi := model.RoleOf(out[i].Role).Routes()
		gj := model.RoleOf(out[j].Role).Routes()
		if gi != gj {
			return !gi
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// splitOmissions routes each omission to the list whose heading is true of it.
//
// One list under one heading was telling an operator that a layer-2 loop
// warning and a section kept in place because the device could not be read
// were both "not an error — the hardware or firmware cannot take it". The
// first describes a network that stops working; the second is the reverse of
// "left out".
//
// Split out so the routing is testable without a device, like batchVerdict and
// meshFromPackages. An unclassified omission falls to Omitted, whose heading
// asserts nothing about why.
func splitOmissions(oms []render.Omission) (omitted, cautions, undetermined []string) {
	for _, om := range oms {
		line := fmt.Sprintf("%s: %s", om.WLAN, om.Reason)
		switch om.Kind {
		case render.KindCaution:
			cautions = append(cautions, line)
		case render.KindUndetermined:
			undetermined = append(undetermined, line)
		default:
			omitted = append(omitted, line)
		}
	}
	return omitted, cautions, undetermined
}

// deviceCaps decodes the capability record the probe stored at adoption.
//
// A device with no readable record is refused rather than rendered with
// assumed capabilities: render gates options on what the probe found, and
// treating "we do not know" as "everything works" is how a device gets sent
// options its hostapd rejects.
func deviceCaps(dev *store.Device) (*capability.Registry, error) {
	if dev.CapsJSON == "" || dev.CapsJSON == "{}" {
		return nil, fmt.Errorf("this device has no capability record; "+
			"re-adopt %s so its radios and features are probed", dev.Name)
	}
	var caps capability.Registry
	if err := json.Unmarshal([]byte(dev.CapsJSON), &caps); err != nil {
		return nil, fmt.Errorf("this device's capability record is unreadable: %w", err)
	}
	return &caps, nil
}

// stillOn reports the SSIDs that should have stopped and have not.
//
// The mirror of missingFrom, and the half that was absent. Both read the same
// snapshot of what the radios are actually carrying.
func stillOn(found, gone map[string]bool) []string {
	var out []string
	for ssid := range gone {
		if found[ssid] {
			out = append(out, ssid)
		}
	}
	sort.Strings(out)
	return out
}
