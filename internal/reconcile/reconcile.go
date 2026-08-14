// Package reconcile joins the pieces: read what is on the device, render what
// the site model wants, diff the two, apply the difference, and remember what
// we own.
//
// It is the only package that both reads a device and writes the store, which
// is deliberate — the ordering constraints between those two live in one place
// rather than being rediscovered at each call site. The sharpest of them:
// ownership is recorded only after an apply is CONFIRMED, because a change the
// device reverts thirty seconds later was never ours.
package reconcile

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/render"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Drift is a section we own whose value on the device no longer matches what we
// last applied — someone edited it in LuCI or over SSH.
//
// This is surfaced, never silently corrected. The operator may have had a good
// reason, and a controller that quietly reverts human edits is worse than one
// that does not notice them.
type Drift struct {
	Config  string
	Section string
	Option  string
	Ours    string // what we last applied
	Theirs  string // what is there now
}

func (d Drift) String() string {
	return fmt.Sprintf("%s.%s.%s: we applied %q, device has %q",
		d.Config, d.Section, d.Option, d.Ours, d.Theirs)
}

// DevicePlan is the answer to "what would change on this device", which is both
// the preview the operator approves and the thing the engine executes.
type DevicePlan struct {
	Device model.Device
	Doc    render.Doc
	Report render.Report
	Drift  []Drift
	Plan   applyengine.Plan

	// Existing is what the device had when we looked, kept so Apply can record
	// ownership without re-reading.
	Existing render.Existing
}

// Empty reports that the device already matches the model.
func (p *DevicePlan) Empty() bool { return len(p.Plan.Ops) == 0 }

// Blocked reports a conflict that must be resolved by a human before anything
// is applied to this device.
func (p *DevicePlan) Blocked() bool { return p.Report.HasConflicts() }

// Reconciler owns the read-render-apply-record cycle for one site.
type Reconciler struct {
	Store  *store.DB
	Engine *applyengine.Engine
	Now    func() time.Time
}

// New returns a Reconciler with sensible defaults.
func New(db *store.DB) *Reconciler {
	return &Reconciler{Store: db, Engine: applyengine.New(), Now: time.Now}
}

// ReadExisting loads the device's current wireless config.
//
// The renderer needs this to tell our sections from a human's, so a preview
// built without it can only ever be provisional — which is why Plan takes it
// rather than assuming an empty device.
func ReadExisting(ctx context.Context, c *ubus.Client) (render.Existing, error) {
	// Decoded as `any`, not as string, because a real device does not return
	// only strings. Measured on OpenWrt 25.12.5: every UCI option is a string,
	// but the section metadata is not — `.anonymous` is a JSON bool and
	// `.index` is a number. Decoding straight into map[string]string therefore
	// failed the WHOLE read with "cannot unmarshal bool", so every device
	// reported as unplannable. The mock returned strings throughout, which is
	// why this survived until a preview ran against hardware.
	var out struct {
		Values map[string]map[string]any `json:"values"`
	}
	if err := c.Call(ctx, "uci", "get", map[string]any{"config": "wireless"}, &out); err != nil {
		return render.Existing{}, fmt.Errorf("reconcile: read wireless config: %w", err)
	}
	ifaces := map[string]map[string]string{}
	for name, vals := range out.Values {
		flat := flatten(vals)
		if flat[".type"] == "wifi-iface" {
			ifaces[name] = flat
		}
	}
	return render.Existing{WifiIfaces: ifaces}, nil
}

// flatten coerces one section's values to the strings the renderer compares.
//
// UCI itself stores everything as text, so the options we care about arrive as
// strings and pass through untouched. The other cases are the section metadata
// and list options, and each is rendered the way UCI would render it rather
// than dropped — a key that vanished here would read downstream as "the device
// does not have this option", which is a different claim entirely.
func flatten(vals map[string]any) map[string]string {
	out := make(map[string]string, len(vals))
	for k, v := range vals {
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			out[k] = strconv.FormatBool(t)
		case float64:
			// JSON has one number type; UCI indices are integers.
			out[k] = strconv.FormatFloat(t, 'f', -1, 64)
		case []any:
			// A UCI list. Space-joined, which is how `uci get` renders one.
			parts := make([]string, 0, len(t))
			for _, e := range t {
				parts = append(parts, fmt.Sprint(e))
			}
			out[k] = strings.Join(parts, " ")
		case nil:
			out[k] = ""
		default:
			out[k] = fmt.Sprint(t)
		}
	}
	return out
}

// PlanDevice produces the preview without changing anything.
func (r *Reconciler) PlanDevice(ctx context.Context, c *ubus.Client, site model.Site,
	dev model.Device, caps *capability.Registry) (*DevicePlan, error) {

	existing, err := ReadExisting(ctx, c)
	if err != nil {
		return nil, err
	}
	doc, report, err := render.Render(site, dev, caps, existing)
	if err != nil {
		return nil, err
	}

	p := &DevicePlan{Device: dev, Doc: doc, Report: report, Existing: existing}
	p.Drift = detectDrift(doc, existing)

	// A conflict means a human owns something we would have to touch. Produce
	// no operations at all — a partial apply around a conflict is how you end
	// up with half a WLAN.
	if report.HasConflicts() {
		return p, nil
	}

	plan := doc.Plan(existing)
	plan.Ops = append(plan.Ops, doc.Prune(existing)...)
	p.Plan = plan
	return p, nil
}

// detectDrift compares what we last rendered against what the device holds, for
// the keys we manage.
//
// Only our keys: the device adds defaults of its own, and hostapd writes state
// back into some sections. Comparing whole sections would report drift
// constantly and train the operator to ignore it.
func detectDrift(doc render.Doc, existing render.Existing) []Drift {
	var out []Drift
	for _, s := range doc.Sections {
		current, present := existing.WifiIfaces[s.Name]
		if !present || current[render.OwnershipTag] != "1" {
			continue // not ours yet, or not ours at all: not drift
		}
		keys := make([]string, 0, len(s.Values))
		for k := range s.Values {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic reporting
		for _, k := range keys {
			if have, ok := current[k]; ok && have != s.Values[k] {
				out = append(out, Drift{
					Config: s.Config, Section: s.Name, Option: k,
					Ours: s.Values[k], Theirs: have,
				})
			}
		}
	}
	return out
}

// Apply executes a plan and records ownership if — and only if — the device
// confirmed it.
//
// The ordering is the point. applyengine reports three outcomes, and only
// Applied means the change is permanent; recording ownership on Reverted would
// leave the reconciler believing it owns config that is not there, and on
// Unknown it would paper over the one state that needs a human.
func (r *Reconciler) Apply(ctx context.Context, c *ubus.Client, deviceID int64,
	p *DevicePlan, health applyengine.HealthCheck) (applyengine.Result, error) {

	if p.Blocked() {
		return applyengine.Result{}, fmt.Errorf(
			"reconcile: device has %d unresolved conflict(s); refusing to apply: %s",
			len(p.Report.Conflicts), p.Report.Conflicts[0].Reason)
	}
	if p.Empty() {
		return applyengine.Result{Outcome: applyengine.Applied,
			Reason: "already matches the site model; nothing to do"}, nil
	}

	res, err := r.Engine.Apply(ctx, c, p.Plan, health)
	r.logOutcome(ctx, deviceID, p, res, err)
	if err != nil {
		return res, err
	}
	if res.Outcome != applyengine.Applied {
		return res, nil
	}

	owned := make([]store.OwnedSection, 0, len(p.Doc.Sections))
	now := r.now().Unix()
	for _, s := range p.Doc.Sections {
		owned = append(owned, store.OwnedSection{
			DeviceID: deviceID, Config: s.Config, Section: s.Name,
			RenderedHash: s.Hash(), AppliedAt: now,
		})
	}
	if err := r.Store.RecordOwned(ctx, owned); err != nil {
		return res, fmt.Errorf("reconcile: applied but could not record ownership: %w", err)
	}
	return res, nil
}

// logOutcome writes an audit event for every apply, including the ones that
// failed. An apply nobody can account for afterwards is worse than one that
// failed loudly.
func (r *Reconciler) logOutcome(ctx context.Context, deviceID int64,
	p *DevicePlan, res applyengine.Result, applyErr error) {

	sev := "info"
	switch {
	case applyErr != nil, res.Outcome == applyengine.Unknown:
		sev = "error"
	case res.Outcome == applyengine.Reverted:
		sev = "warning"
	}
	detail := map[string]any{
		"outcome":   string(res.Outcome),
		"reason":    res.Reason,
		"ops":       len(p.Plan.Ops),
		"sections":  len(p.Doc.Sections),
		"omissions": p.Report.Omissions,
		"stranded":  res.Stranded,
	}
	if applyErr != nil {
		detail["error"] = applyErr.Error()
	}
	if res.HealthErr != nil {
		detail["health_error"] = res.HealthErr.Error()
	}
	id := deviceID
	_ = r.Store.LogEvent(ctx, store.Event{
		DeviceID: &id, Category: "audit", Severity: sev,
		Event: "config.apply", Detail: detail,
	})
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
