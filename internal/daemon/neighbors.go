package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/roaming"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Distributing 802.11k neighbour lists across the fleet.
//
// internal/roaming has the reasoning for why this exists and why the controller
// relays hostapd's own element rather than building one. This file is the part
// that talks to devices, and its own decisions are below.
//
// # Why it reconciles instead of applying
//
// Every other write in this system goes through the apply engine: stage, apply
// with a rollback armed, check health, confirm. That machinery exists because a
// bad UCI change can sever the network and a device must be able to undo it
// alone.
//
// None of that applies here. `rrm_nr_set` writes runtime state in hostapd, not
// config; it cannot be rolled back because there is nothing to roll back to
// except the empty list the AP already had; and the worst outcome of getting it
// wrong is that a client scans more channels than it needed to. Routing this
// through the apply engine would arm a rollback timer, hold the global apply
// lock that serialises the whole fleet, and demand a health check for a change
// that cannot make a device unhealthy. So it reconciles: read, compare, write
// what differs, and report.
//
// # Why the current list is read back rather than remembered
//
// The tempting optimisation is to record what was last pushed and skip the
// read. It does not survive contact with the device. Measured on the reference
// hardware: `wifi reload` after editing one section restarted that BSS and
// cleared its neighbour list, while the *other* BSS on the same device — whose
// config had not changed — kept its list intact. So neither "an apply clears
// everything" nor "an apply clears nothing" is true, and a controller that
// assumed either would leave APs silently empty.
//
// Reading `rrm_nr_list` back makes the operation idempotent against any cause
// of loss, including ones nobody has thought of: a hostapd crash, an operator's
// own `wifi reload`, a device rebooting between cycles. It costs one call per
// BSS, in the same batched request as the read that has to happen anyway.

// neighbourInterval is the backstop cadence.
//
// Neighbour data changes when a BSS appears, disappears or moves channel. None
// of those is fast, so this is deliberately slow — it shares the 15-minute
// rhythm the collector already uses for the board identity and the radio list,
// for the same reason: these are facts that change on the timescale of someone
// walking to a cupboard, not of a poll.
//
// The cost at this cadence is what makes the feature free. Per device per
// cycle: one `iwinfo.devices` call, one batched request carrying two calls per
// wireless interface, and — only when something actually differs — one more
// batched request to push. Against DEVICE-BUDGET's ceiling of one request per
// minute that is under a tenth of the allowance, and in the steady state where
// nothing has moved the third request never happens at all.
const neighbourInterval = 15 * time.Minute

// neighbourDeviceTimeout bounds one device's share of a cycle. A device that
// has gone away must not stop the rest of the fleet from being reconciled —
// which is the same rule Preview follows, for the same reason.
const neighbourDeviceTimeout = 20 * time.Second

var errNeighbourReconcileAdmission = errors.New(
	"daemon: neighbour reconciliation blocked by controller operation admission",
)

// StartNeighbourReconciler runs the distribution on a slow loop until ctx ends.
//
// It runs one cycle immediately. A controller that has just started has no idea
// what any AP's neighbour list holds — and the most likely reason it is
// starting is that something restarted, which is exactly when those lists are
// empty. Waiting a quarter of an hour to find out would make every restart a
// quarter of an hour of slow roaming.
//
// Failures are logged and never retried faster. The next tick is the retry, and
// a reconciler that speeds up when a device is unreachable is a reconciler that
// hammers whatever is already struggling.
func (d *Daemon) StartNeighbourReconciler(ctx context.Context) {
	if d == nil || d.RouterWritesSuppressed() {
		return
	}
	d.mu.Lock()
	if d.neighbourDone != nil {
		d.mu.Unlock()
		return
	}
	nctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	d.neighbourStop, d.neighbourDone = cancel, done
	d.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(neighbourInterval)
		defer t.Stop()
		for {
			d.reconcileNeighboursOnce(nctx)
			select {
			case <-nctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	d.Log.Info("802.11k neighbour distribution started", "interval", neighbourInterval)
}

func (d *Daemon) stopNeighbourReconciler(ctx context.Context) error {
	d.mu.Lock()
	cancel, done := d.neighbourStop, d.neighbourDone
	d.neighbourStop, d.neighbourDone = nil, nil
	d.mu.Unlock()
	if done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Daemon) startNeighbourAfterResume() {
	if d == nil || d.lifetimeCtx == nil || d.RouterWritesSuppressed() {
		return
	}
	d.StartNeighbourReconciler(d.lifetimeCtx)
}

func (d *Daemon) reconcileNeighboursOnce(ctx context.Context) {
	res, err := d.DistributeNeighbours(ctx)
	if err != nil {
		if errors.Is(err, errNeighbourReconcileAdmission) {
			return
		}
		d.Log.Warn("could not distribute 802.11k neighbour lists", "err", err)
		d.rememberNeighbourRun(nil, err)
		return
	}
	d.rememberNeighbourRun(res, nil)
	// Only a cycle that changed something is worth a line. In the steady state
	// this runs every fifteen minutes forever and has nothing to say, and a log
	// that reports "0 updated" ninety-six times a day is a log nobody reads.
	if res.Updated > 0 {
		d.Log.Info("distributed 802.11k neighbour lists",
			"updated", res.Updated, "unchanged", res.Unchanged,
			"ssids", res.SSIDs)
	}
	for _, dev := range res.Devices {
		if dev.Error != "" {
			d.Log.Warn("could not distribute neighbour lists to a device",
				"device", dev.Name, "err", dev.Error)
		}
	}
}

// bssObservation is one BSS as read from its own AP, plus what that AP
// currently believes about its neighbours.
type bssObservation struct {
	own     roaming.Neighbour
	current []roaming.Neighbour
	ok      bool
}

// DistributeNeighbours reconciles 802.11k neighbour lists across the fleet.
//
// It is safe to call at any time and does nothing when the fleet has not
// moved. Errors reaching one device are reported and do not stop the others:
// a fleet where one AP is unplugged is still better served by updating the
// rest than by refusing to update anything.
func (d *Daemon) DistributeNeighbours(ctx context.Context) (*api.NeighbourResult, error) {
	release, ok := d.beginNeighbourReconcileOperation()
	if !ok {
		return nil, errNeighbourReconcileAdmission
	}
	defer release()

	site, err := d.Store.Site(ctx)
	if err != nil {
		return nil, err
	}
	devices, err := d.Store.Devices(ctx)
	if err != nil {
		return nil, err
	}

	// Only SSIDs the controller manages, and only those with 802.11k asked for.
	//
	// Both halves matter. Pushing to an SSID we do not own would stomp whatever
	// an operator configured by hand on their own network — and a controller
	// that silently rewrites hand-made config is the thing ARCHITECTURE §0
	// forbids. And an SSID whose WLAN has KV switched off is one where the
	// renderer wrote no `rrm_neighbor_report`, so the AP will not answer a
	// client's request anyway; filling a list it cannot use is work that looks
	// like a feature and is not.
	managed := map[string]bool{}
	for _, w := range site.WLANs {
		if w.Enabled && w.Roaming.KV {
			managed[w.SSID] = true
		}
	}

	res := &api.NeighbourResult{SSIDs: sortedKeys(managed), Devices: []api.NeighbourDevice{}}
	if len(managed) == 0 {
		res.Note = "no managed WLAN asks for 802.11k, so there is nothing to " +
			"distribute. Enable neighbour reports on a WLAN to use this"
		return res, nil
	}

	// Phase one: read. Nobody can be told about their neighbours until every
	// AP has been asked what it is, so this is a genuine barrier rather than
	// something that could be pipelined per device.
	obs := map[roaming.Target]*bssObservation{}
	complete := true
	for _, dev := range devices {
		row := d.readNeighbourState(ctx, dev, managed, obs)
		if row == nil {
			continue
		}
		// A device that errored may be carrying BSSes this cycle cannot see.
		// A device that was SKIPPED is a different thing: it was reached, or it
		// was ruled out by its own capability record, and either way its APs
		// are not silently missing from the table.
		if row.Error != "" {
			complete = false
		}
		res.Devices = append(res.Devices, *row)
	}

	// Phase two: compute. Pure, and the only part with rules worth arguing
	// about — they are in internal/roaming with their tests.
	all := make([]roaming.Neighbour, 0, len(obs))
	for _, o := range obs {
		all = append(all, o.own)
	}
	desired := roaming.Distribute(all)

	// Phase three: push what differs, grouped per device so one device's
	// interfaces travel in one request.
	//
	// A BSS whose list already matches is counted here and not contacted. That
	// is the steady state — a fleet nobody has touched since the last cycle —
	// and it is why the feature costs a push request only when something has
	// genuinely moved.
	byDevice := map[int64][]roaming.Target{}
	for i := range res.Devices {
		row := &res.Devices[i]
		for _, b := range row.BSSes {
			tgt := roaming.Target{DeviceID: row.DeviceID, Iface: b.Iface}
			o := obs[tgt]
			if o == nil || !o.ok {
				continue
			}
			want, planned := desired[tgt]
			if !planned {
				// Distribute deduplicates by BSSID, so a BSS reachable under
				// two device rows is planned once. The two-value lookup is
				// load-bearing: a target with no plan is covered elsewhere,
				// whereas a target planned with an EMPTY list is the last AP of
				// an SSID being told to clear stale neighbours. Treating the
				// first as the second pushes an empty list over a correct one.
				row.Unchanged++
				continue
			}
			if !complete {
				// Some device could not be answered for, so this table is not
				// the whole fleet. Add and refresh, never remove — see
				// roaming.Union.
				want = roaming.Union(o.current, want)
				desired[tgt] = want
			}
			if roaming.SameSet(o.current, want) {
				row.Unchanged++
				continue
			}
			byDevice[row.DeviceID] = append(byDevice[row.DeviceID], tgt)
		}
	}
	for i := range res.Devices {
		row := &res.Devices[i]
		if targets := byDevice[row.DeviceID]; len(targets) > 0 {
			d.pushNeighbours(ctx, devices, row, targets, desired, obs)
		}
	}

	for _, row := range res.Devices {
		res.Updated += row.Updated
		res.Unchanged += row.Unchanged
	}
	return res, nil
}

func (d *Daemon) beginNeighbourReconcileOperation() (func(), bool) {
	if d.api == nil {
		return func() {}, true
	}
	return d.api.BeginNeighbourReconcileOperation()
}

// readNeighbourState asks one device what its APs are and what they currently
// know, filling obs. It returns nil for a device this feature does not apply
// to, so that the result lists the fleet it actually acted on rather than every
// row in the database.
func (d *Daemon) readNeighbourState(ctx context.Context, dev *store.Device,
	managed map[string]bool, obs map[roaming.Target]*bssObservation) *api.NeighbourDevice {

	if !dev.Adopted() || !deviceFunctions(dev).Wireless() {
		return nil
	}
	row := &api.NeighbourDevice{DeviceID: dev.ID, Name: dev.Name}

	caps, err := deviceCaps(dev)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	// The three-state gate, and the one place it earns its keep here.
	//
	// NotObservable on this feature almost always means one thing: the device
	// was adopted before the ACL carried the rrm_* grants, and the ACL is
	// written to a device exactly twice in its life. Reporting that as "this
	// device cannot" would send an operator hunting for a hardware limit that
	// does not exist, so the message names the actual remedy.
	st := caps.State(capability.FeatNeighborReport)
	switch {
	case st == capability.Present:
	case !st.Decided():
		// Covers both "the check was refused" and "no answer was ever
		// recorded". They lead to the same remedy here, and neither is a
		// statement that the hostapd lacks the methods — which is what the
		// default branch below says, and what Unknown used to be told.
		row.Skipped = "this device has not been shown to accept neighbour " +
			"lists. Most often that is an ACL or a capability record written " +
			"before this feature existed — re-adopt the device to refresh it, " +
			"then re-probe"
		return row
	default:
		row.Skipped = "this device's hostapd does not carry the 802.11k " +
			"neighbour-report methods"
		return row
	}

	dctx, cancel := context.WithTimeout(ctx, neighbourDeviceTimeout)
	defer cancel()

	c, err := d.Connect(dctx, dev)
	if err != nil {
		row.Error = fmt.Sprintf("could not reach this device: %v", err)
		return row
	}
	defer c.Close()

	var devs struct {
		Devices []string `json:"devices"`
	}
	if err := c.Call(dctx, "iwinfo", "devices", nil, &devs); err != nil {
		row.Error = fmt.Sprintf("could not list wireless interfaces: %v", err)
		return row
	}
	sort.Strings(devs.Devices)

	// Two calls per interface in one request. Interfaces that are not APs —
	// a mesh point, a station — have no hostapd object and simply fail their
	// pair, which is cheaper and more robust than asking a separate question
	// about each interface's mode first.
	calls := make([]ubus.Invocation, 0, 2*len(devs.Devices))
	for _, iface := range devs.Devices {
		calls = append(calls,
			ubus.Invocation{Object: "hostapd." + iface, Method: "rrm_nr_get_own"},
			ubus.Invocation{Object: "hostapd." + iface, Method: "rrm_nr_list"})
	}
	results, err := c.Batch(dctx, calls)
	if err != nil {
		row.Error = fmt.Sprintf("could not read neighbour state: %v", err)
		return row
	}
	d.noteExternalRequests(dev.ID, c)

	for i, iface := range devs.Devices {
		if 2*i+1 >= len(results) {
			break
		}
		own, ok := decodeOwnNR(results[2*i].Data, dev.ID, iface)
		if !ok || !managed[own.SSID] {
			continue
		}
		o := &bssObservation{own: own, ok: true}
		o.current = decodeNRList(results[2*i+1].Data)
		obs[roaming.Target{DeviceID: dev.ID, Iface: iface}] = o
		row.BSSes = append(row.BSSes, api.NeighbourBSS{
			Iface: iface, SSID: own.SSID, BSSID: own.BSSID,
			Neighbours: len(o.current),
		})
	}
	if len(row.BSSes) == 0 && row.Error == "" {
		row.Skipped = "no interface on this device carries a managed WLAN that " +
			"asks for 802.11k"
	}
	return row
}

// pushNeighbours writes the computed lists for one device.
func (d *Daemon) pushNeighbours(ctx context.Context, devices []*store.Device,
	row *api.NeighbourDevice, targets []roaming.Target,
	desired map[roaming.Target][]roaming.Neighbour, obs map[roaming.Target]*bssObservation) {

	var dev *store.Device
	for _, cand := range devices {
		if cand.ID == row.DeviceID {
			dev = cand
			break
		}
	}
	if dev == nil {
		return
	}

	dctx, cancel := context.WithTimeout(ctx, neighbourDeviceTimeout)
	defer cancel()

	c, err := d.Connect(dctx, dev)
	if err != nil {
		row.Error = fmt.Sprintf("could not reach this device to push: %v", err)
		return
	}
	defer c.Close()

	sort.Slice(targets, func(i, j int) bool { return targets[i].Iface < targets[j].Iface })

	calls := make([]ubus.Invocation, 0, len(targets))
	for _, tgt := range targets {
		calls = append(calls, ubus.Invocation{
			Object: "hostapd." + tgt.Iface, Method: "rrm_nr_set",
			Args: map[string]any{"list": wireList(desired[tgt])},
		})
	}
	results, err := c.Batch(dctx, calls)
	if err != nil {
		row.Error = fmt.Sprintf("could not set neighbour lists: %v", err)
		return
	}
	d.noteExternalRequests(dev.ID, c)

	for i, tgt := range targets {
		if i < len(results) && results[i].Err == nil {
			row.Updated++
			markBSS(row, tgt.Iface, len(desired[tgt]), "")
			continue
		}
		msg := "unknown error"
		if i < len(results) && results[i].Err != nil {
			msg = results[i].Err.Error()
		}
		// The count reported for a failed push is what the AP still has, not
		// what it was meant to get. Reporting the intended number beside a
		// failure would put a figure on screen that is true of nothing.
		markBSS(row, tgt.Iface, len(obs[tgt].current), msg)
	}
}

// markBSS records the outcome against the interface it belongs to. The row is
// what an operator reads, and "3 updated" without saying which three is a
// number rather than an answer.
func markBSS(row *api.NeighbourDevice, iface string, n int, failure string) {
	for i := range row.BSSes {
		if row.BSSes[i].Iface != iface {
			continue
		}
		row.BSSes[i].Neighbours = n
		row.BSSes[i].Changed = failure == ""
		row.BSSes[i].Failed = failure
		return
	}
}

// wireList renders neighbours in hostapd's wire shape: an array of
// [bssid, ssid, nr] triples. Measured against both reference devices — the
// element is relayed as the hex string its own AP produced, never rebuilt.
func wireList(ns []roaming.Neighbour) [][]string {
	out := make([][]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, []string{n.BSSID, n.SSID, n.NR})
	}
	return out
}

// decodeOwnNR reads one `rrm_nr_get_own` reply.
//
// The reply is `{"value": [bssid, ssid, nr]}` — a positional triple rather than
// an object, so the length check is the validation. A short array from a
// firmware that shapes this differently must read as "could not tell", never as
// a neighbour with empty fields: relaying one of those makes an AP answer a
// client with a candidate it cannot scan for.
func decodeOwnNR(raw json.RawMessage, deviceID int64, iface string) (roaming.Neighbour, bool) {
	var reply struct {
		Value []string `json:"value"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &reply) != nil || len(reply.Value) < 3 {
		return roaming.Neighbour{}, false
	}
	n := roaming.Neighbour{
		DeviceID: deviceID, Iface: iface,
		BSSID: reply.Value[0], SSID: reply.Value[1], NR: reply.Value[2],
	}
	return n, n.Valid()
}

// decodeNRList reads `rrm_nr_list`. Entries that do not decode are dropped
// rather than failing the read: the list is only ever compared against the
// desired one, and an undecodable entry means "not what we want", which is
// exactly what dropping it produces.
func decodeNRList(raw json.RawMessage) []roaming.Neighbour {
	var reply struct {
		List [][]string `json:"list"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &reply) != nil {
		return nil
	}
	out := make([]roaming.Neighbour, 0, len(reply.List))
	for _, e := range reply.List {
		if len(e) < 3 {
			continue
		}
		out = append(out, roaming.Neighbour{BSSID: e[0], SSID: e[1], NR: e[2]})
	}
	return out
}

// noteExternalRequests attributes this conversation to the device's Management
// Overhead readout.
//
// The same rule discovery follows: a request the controller makes and does not
// count is how a readout stops being trustworthy. "Negligible, therefore
// uncounted" is a decision an operator never got to check.
func (d *Daemon) noteExternalRequests(deviceID int64, c *ubus.Client) {
	if col := d.collectorRef(); col != nil {
		col.NoteExternalRequest(deviceID, c.BytesOut())
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// nudgeNeighbours re-runs the distribution out of band, after something that
// changed which APs exist or what they are carrying.
//
// Three callers, and each one leaves the fleet's neighbour lists wrong in a
// different direction until this runs: an apply restarts reconfigured BSSes and
// empties theirs, an adoption adds an AP nobody knows about and which knows
// nobody, and an un-adoption leaves a departed AP in every remaining list —
// telling clients to consider roaming to something that is no longer part of
// this network.
//
// Detached from the caller's context because it must outlive the HTTP response,
// and quiet on failure: an operator who just applied a site model or adopted a
// device would read a neighbour-list warning as though that had failed. The
// periodic cycle is the retry.
func (d *Daemon) nudgeNeighbours() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), previewTimeout)
		defer cancel()
		if res, err := d.DistributeNeighbours(ctx); err != nil {
			if errors.Is(err, errNeighbourReconcileAdmission) {
				return
			}
			d.Log.Debug("could not refresh 802.11k neighbour lists; the "+
				"periodic cycle will retry", "err", err)
		} else if res.Updated > 0 {
			d.Log.Info("refreshed 802.11k neighbour lists",
				"updated", res.Updated, "unchanged", res.Unchanged)
		}
	}()
}

// The last neighbour cycle, kept so the screen can show what happened without
// making it happen.
//
// The card used to render nothing until an operator pressed "Distribute now" —
// on a feature whose own description says it runs automatically every fifteen
// minutes. So the only way to find out whether 802.11k was working was to
// trigger it, which is not an observation, and the fifteen-minute cycles that
// had been running all along left no trace anywhere a user looks.
//
// In memory rather than in the database on purpose. This is the state of a
// process, not a fact about the fleet: after a restart the honest answer is
// "no cycle has run since this controller started", and the first one lands
// within fifteen minutes. Persisting it would let a stale row outlive the
// truth it described.
type neighbourRun struct {
	Result *api.NeighbourResult
	Err    string
	At     time.Time
}

func (d *Daemon) rememberNeighbourRun(res *api.NeighbourResult, err error) {
	d.nbrMu.Lock()
	defer d.nbrMu.Unlock()
	run := &neighbourRun{Result: res, At: time.Now()}
	if err != nil {
		run.Err = err.Error()
	}
	d.lastNeighbourRun = run
}

// LastNeighbourRun reports the most recent cycle, or nil when none has run
// since start. Nil is a real answer and must not be rendered as "nothing to
// do": it means nobody has looked yet.
func (d *Daemon) LastNeighbourRun() (*api.NeighbourResult, string, time.Time, bool) {
	d.nbrMu.Lock()
	defer d.nbrMu.Unlock()
	if d.lastNeighbourRun == nil {
		return nil, "", time.Time{}, false
	}
	r := d.lastNeighbourRun
	return r.Result, r.Err, r.At, true
}
