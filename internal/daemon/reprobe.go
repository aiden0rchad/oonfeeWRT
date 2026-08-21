package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// Re-probing an adopted device.
//
// # Why this is not part of the poll
//
// A probe is a burst: tens of ubus calls, several of them the expensive ones
// the poll deliberately avoids. DEVICE-BUDGET's ceiling is one request per
// minute at baseline, and a probe folded into polling would blow it on every
// cycle to re-learn facts that change once a year. So it runs on the two
// occasions the answer can actually have changed — the firmware moved, or an
// operator asked — and quiesces the poller while it does, for the same reason
// an apply does: two conversations with one rpcd is how you get a read that
// belongs to neither.
//
// # Why the old record is not simply overwritten
//
// The valuable output is the *difference*. "This device now has a second radio"
// and "this device can no longer be asked about hostapd control" are the things
// an operator needs, and they are invisible in a replaced blob. capability.Diff
// produces them, and it is careful about the one distinction that matters: a
// check that stopped being possible is not a capability that stopped existing.

// reprobeTimeout bounds one probe. Generous — a probe is dozens of calls and a
// slow device on a busy channel is not a broken one — but bounded, because a
// hung probe holds the device quiesced and polling stops.
const reprobeTimeout = 90 * time.Second

// reprobeMinInterval is the floor between automatic re-probes of one device.
//
// A device that flaps its firmware string, or one whose board call returns
// something unstable, would otherwise trigger a probe burst on every poll. The
// operator-initiated path is not rate limited: someone watching a screen and
// pressing a button has a reason.
const reprobeMinInterval = 10 * time.Minute

// reprobeGate serialises probes per device and enforces the automatic floor.
type reprobeGate struct {
	mu      sync.Mutex
	running map[int64]bool
	last    map[int64]time.Time
}

func (g *reprobeGate) enter(id int64, auto bool, now time.Time, floor time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running == nil {
		g.running = map[int64]bool{}
		g.last = map[int64]time.Time{}
	}
	if g.running[id] {
		return false
	}
	if auto {
		if t, ok := g.last[id]; ok && now.Sub(t) < floor {
			return false
		}
	}
	g.running[id] = true
	g.last[id] = now
	return true
}

func (g *reprobeGate) leave(id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.running, id)
}

func (g *reprobeGate) forget(id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.last, id)
	// A correctly sequenced deletion holds deviceOps, so no probe can still be
	// running here. Keep a true entry if a direct test/support caller used
	// Untrack without that gate; allowing a second probe would be less safe than
	// retaining this one in-flight marker until leave removes it.
	if !g.running[id] {
		delete(g.running, id)
	}
}

// Reprobe re-interrogates one adopted device and records what changed.
//
// It replaces the stored registry whatever the diff says. The registry is a
// record of the last successful observation, and keeping an older one because
// it looked better would make the controller's model of the device diverge from
// the device on exactly the occasions it matters.
func (d *Daemon) Reprobe(ctx context.Context, deviceID int64) (*api.ReprobeResult, error) {
	return d.reprobe(ctx, deviceID, false)
}

func (d *Daemon) reprobe(ctx context.Context, deviceID int64, auto bool) (*api.ReprobeResult, error) {
	if !d.reprobes.enter(deviceID, auto, time.Now(), reprobeMinInterval) {
		return nil, errReprobeBusy
	}
	defer d.reprobes.leave(deviceID)

	// Capabilities and firmware are provisioning inputs. Share the destructive
	// per-device gate with apply and un-adopt so a probe cannot replace them
	// between a bound re-plan and its write. Reload after admission because the
	// row may have changed while this probe waited.
	release, err := d.deviceOps.acquire(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	defer release()
	dev, err := d.Store.DeviceByID(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	if !dev.Adopted() {
		return nil, fmt.Errorf("daemon: %s is not adopted, so there is no "+
			"credential to probe it with", dev.Name)
	}

	// The previous record, decoded before anything is overwritten. A device
	// with no readable record is not an error here — that is the first probe,
	// and Diff treats a nil old registry as first observation rather than as a
	// device that just gained everything it has.
	var before *capability.Registry
	if dev.CapsJSON != "" && dev.CapsJSON != "{}" {
		var r capability.Registry
		if err := json.Unmarshal([]byte(dev.CapsJSON), &r); err == nil {
			before = &r
		} else {
			d.Log.Warn("the stored capability record could not be decoded; "+
				"this probe will be treated as the first",
				"device", dev.MAC, "err", err)
		}
	}

	// Never poll and probe at once — the same rule an apply follows, for the
	// same reason.
	if c := d.collectorRef(); c != nil {
		defer c.Quiesce(deviceID)()
	}

	pctx, cancel := context.WithTimeout(ctx, reprobeTimeout)
	defer cancel()

	client, err := d.Connect(pctx, dev)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	after, err := capability.Probe(pctx, client)
	if err != nil {
		return nil, fmt.Errorf("daemon: probe %s: %w", dev.Name, err)
	}

	if err := d.Store.SetCapabilities(ctx, deviceID, after, string(after.Class)); err != nil {
		return nil, fmt.Errorf("daemon: probed %s but could not store the "+
			"result: %w", dev.Name, err)
	}
	// The firmware column is what the automatic trigger compares against. Not
	// updating it here means a probe that succeeds still leaves the device
	// looking changed, and the next poll triggers another one.
	if err := d.Store.SetFirmware(ctx, deviceID, after.Board.Release); err != nil {
		d.Log.Debug("could not record firmware after a probe",
			"device", dev.MAC, "err", err)
	}
	// The collector target carries capability gates. Replace it from the stored
	// row now so a re-probe takes effect without a controller restart.
	if fresh, err := d.Store.DeviceByID(ctx, deviceID); err != nil {
		d.Log.Warn("could not refresh collector target after a probe",
			"device", dev.MAC, "err", err)
	} else {
		d.Track(fresh)
	}

	changes := capability.Diff(before, after)
	res := &api.ReprobeResult{
		DeviceID: deviceID, Name: dev.Name, Summary: after.Summary(),
		Changes: changes, Registry: after, Unchanged: len(changes) == 0,
		RoleFit: functionFit(deviceFunctions(dev), after),
	}

	d.logReprobe(ctx, dev, res, auto)
	return res, nil
}

// errReprobeBusy is the API's sentinel, re-exported here so the two packages
// cannot drift into two errors that mean the same thing but compare unequal —
// which would turn a 429 into a 502 and tell an operator their device failed
// when nothing did.
var errReprobeBusy = api.ErrReprobeBusy

// logReprobe records the outcome, splitting it by what a reader may conclude.
//
// Actionable changes are a warning: something the controller renders against
// moved. Visibility changes are info, because the device did not change — and
// logging "hostapd-control lost" at warning level for a narrowed ACL would send
// someone looking for a hardware fault that is not there.
func (d *Daemon) logReprobe(ctx context.Context, dev *store.Device,
	res *api.ReprobeResult, auto bool) {
	id := dev.ID
	// Every successful probe is recorded, changes or not.
	//
	// Without this there is no way to say "nothing has changed since", and the
	// preview's capability panel had no way to be cleared: it reads the newest
	// capabilities_changed event, and a probe that found nothing wrote nothing,
	// so a stale loss stayed on screen for good. The operator re-probed exactly
	// as advised and the screen did not move — advice that cannot work, which
	// is the failure STATUS §6 warns about and this was written while fixing.
	_ = d.Store.LogEvent(ctx, store.Event{
		DeviceID: &id, Category: "device", Severity: "info",
		Event:  EventCapabilitiesProbed,
		Detail: map[string]any{"automatic": auto, "unchanged": res.Unchanged},
	})
	if res.Unchanged {
		d.Log.Info("capability probe found no changes",
			"device", dev.MAC, "automatic", auto)
		return
	}

	actionable := capability.Actionable(res.Changes)
	severity := "info"
	if len(actionable) > 0 {
		severity = "warning"
	}
	details := make([]map[string]any, 0, len(res.Changes))
	for _, c := range res.Changes {
		details = append(details, map[string]any{
			"kind": c.Kind, "name": c.Name, "effect": string(c.Effect),
			"from": c.From, "to": c.To, "detail": c.Detail,
		})
		d.Log.Log(ctx, levelFor(c.Effect), "capability change",
			"device", dev.MAC, "change", c.String())
	}
	_ = d.Store.LogEvent(ctx, store.Event{
		DeviceID: &id, Category: "device", Severity: severity,
		Event: EventCapabilitiesChanged,
		Detail: map[string]any{
			"automatic": auto, "summary": res.Summary,
			"actionable": len(actionable), "changes": details,
		},
	})
}

// EventCapabilitiesChanged is the event a re-probe writes. Named because the
// preview reads it back to explain itself.
const EventCapabilitiesChanged = "device.capabilities_changed"

// EventCapabilitiesProbed is written by EVERY successful probe, including one
// that found nothing. It is what makes "nothing has changed since" a
// statement the preview can check, and therefore what makes re-probing a
// remedy rather than a suggestion.
const EventCapabilitiesProbed = "device.capabilities_probed"

// recentCapabilityLoss finds the newest re-probe that changed something the
// controller renders against.
//
// It exists to answer a question the preview screen could not: an operator sees
// "this WLAN was not rendered: device has no 5 GHz radio" and cannot tell
// whether they configured the wrong band or the radio disappeared on Tuesday.
// The first is their mistake, the second is a fault, and the same sentence
// describes both.
//
// Only actionable changes qualify. A capability that merely became unobservable
// did not stop the device doing anything, and offering it as the cause of an
// omission would send someone to fix an ACL that is not the problem.
//
// Returns nil for anything it cannot answer: no probe, no actionable change, an
// undecodable detail blob. A wrong explanation is worse than none, because an
// operator will act on it.
func (d *Daemon) recentCapabilityLoss(ctx context.Context, deviceID int64) *api.CapabilityCause {
	// Only the MOST RECENT capability event, and only if it is still actionable.
	//
	// This used to read five and return the first actionable one, skipping past
	// anything newer that was not — so a loss stayed pinned to the preview
	// until five further capability events pushed it out. A clean re-probe did
	// not supersede it, which makes "re-probe to settle it" advice that cannot
	// work: the newest probe says the device is fine and the screen keeps
	// quoting an older one.
	//
	// The latest probe is the current word on the device. If it found nothing
	// actionable, there is nothing here to explain the omissions above.
	//
	// Observed on the reference WRT3200ACM: a 39-hour-old event claiming
	// "radio radio0 is gone" was still being offered as the probable cause of
	// two VLAN omissions, about a radio that was up and carrying the SSID.
	events, err := d.Store.DeviceEvents(ctx, deviceID, EventCapabilitiesChanged, 1)
	if err != nil {
		d.Log.Debug("could not read capability history",
			"device", deviceID, "err", err)
		return nil
	}
	// A probe that ran later and found nothing settles it.
	//
	// Reading only the newest capabilities_changed event is not enough, because
	// an unchanged probe writes no such event — so the newest one stays the
	// newest for ever and the panel could never be cleared. The probe log is
	// the other half: if the device has been looked at since, what the older
	// probe saw is no longer the current word on it.
	probed, err := d.Store.DeviceEvents(ctx, deviceID, EventCapabilitiesProbed, 1)
	if err == nil && len(probed) > 0 && len(events) > 0 &&
		probed[0].TS > events[0].TS {
		return nil
	}
	for _, e := range events {
		blob, ok := e.Detail.(json.RawMessage)
		if !ok {
			continue
		}
		var detail struct {
			Actionable int `json:"actionable"`
			Changes    []struct {
				Effect string `json:"effect"`
				Detail string `json:"detail"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(blob, &detail); err != nil || detail.Actionable == 0 {
			continue
		}
		cause := &api.CapabilityCause{At: e.TS}
		for _, c := range detail.Changes {
			if capability.Effect(c.Effect).Actionable() {
				cause.Changes = append(cause.Changes, c.Detail)
			}
		}
		if len(cause.Changes) == 0 {
			// The count said actionable but no change survived the filter: the
			// two disagree, so trust neither.
			continue
		}
		return cause
	}
	return nil
}

// levelFor keeps the log level honest about what a line means. A visibility
// change at warning level sends someone looking for a hardware fault that is
// not there; a lost radio at debug level is never seen at all.
func levelFor(e capability.Effect) slog.Level {
	if e.Actionable() {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// reprobeAfterFirmwareChange is the automatic trigger.
//
// Detached from the poll's context on purpose: the poll callback returns
// immediately and its context dies with it, while the probe has to outlive it.
// Failures are logged and not retried here — the next firmware-change detection
// will try again, and a probe that retries in a loop against a device that is
// refusing is the opposite of a budget.
func (d *Daemon) reprobeAfterFirmwareChange(deviceID int64, mac, from, to string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), reprobeTimeout)
		defer cancel()

		res, err := d.reprobe(ctx, deviceID, true)
		switch {
		case errors.Is(err, errReprobeBusy):
			// Not a failure. Two polls can see the same change.
			return
		case err != nil:
			d.Log.Warn("could not re-probe after a firmware change; the stored "+
				"capability record still describes the previous build",
				"device", mac, "from", from, "to", to, "err", err)
			id := deviceID
			_ = d.Store.LogEvent(context.Background(), store.Event{
				DeviceID: &id, Category: "device", Severity: "warning",
				Event: "device.reprobe_failed",
				Detail: map[string]any{
					"from": from, "to": to, "error": err.Error(),
					"consequence": "the capability record still describes the " +
						"previous firmware; re-probe from the device screen " +
						"once it is reachable",
				},
			})
		case res.Unchanged:
			d.Log.Info("firmware changed but capabilities did not",
				"device", mac, "from", from, "to", to)
		}
	}()
}
