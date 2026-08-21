// Package collector is the poll loop: it asks each adopted device for its state
// on a schedule, and hands the results to a sink.
//
// Every number in here comes from DEVICE-BUDGET, which comes from measurement
// on a real device, and the package exists to make those numbers structural
// rather than advisory. The rules it enforces:
//
//   - Two rates. Baseline ~60 s always; focused ~5–10 s only while someone is
//     looking at that device. When the last viewer leaves, it drops back.
//   - One request per poll. Batched, because a 60 s poll never reuses its
//     connection — uhttpd drops keep-alive at 20 s — so an unbatched call costs
//     a whole handshake.
//   - Stagger, don't stampede. Ten devices at 60 s is one request every 6 s.
//   - Back off on evidence, and fail quiet. A struggling router must not be
//     hammered by its manager.
//   - Never poll during an apply.
//
// The device computes nothing. It returns raw state and every derivation
// happens here, on hardware that has cycles to spare.
package collector

import (
	"context"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/radio"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Defaults from DEVICE-BUDGET §2 and §4.
const (
	DefaultBaseline = 60 * time.Second

	// DefaultFocused is 10 s, not the 8 s it used to be. DEVICE-BUDGET §4.2
	// gives a design range of "~5–10 s", but §2's table — the one headed "these
	// are test criteria, not aspirations" — caps the observed tier at one
	// request per 10 s. 8 s is 7.5 requests/min against a ceiling of 6, so the
	// shipped default did not meet the shipped budget. The budget harness said
	// so; 5 s remains available to anyone who lowers it deliberately.
	DefaultFocused = 10 * time.Second

	// DefaultMaxInterval caps both adaptive backoff paths unless an operator has
	// explicitly selected a slower per-device baseline. An otherwise
	// unreachable device is retried at least this often, so one that comes back
	// is noticed without a restart.
	DefaultMaxInterval = 10 * time.Minute

	// DefaultSlowPoll is the response time above which a device is treated as
	// struggling. A focused poll measured 194 ms on a healthy class A device, so
	// this is several times worse than normal rather than merely slower.
	DefaultSlowPoll = 1500 * time.Millisecond

	// DefaultLoadLimit is the 1-minute load average above which we widen the
	// interval. Above this the device has a real problem of its own, and our
	// polling is the one load on it we can choose to reduce.
	DefaultLoadLimit = 5.0

	// maxWiden bounds evidence-based widening to 8× the tier interval.
	maxWiden = 3
)

// Sink receives completed polls, including failed ones.
//
// It is called inline from the device's own goroutine, so an implementation
// that blocks delays that device's next poll — and only that device's. Failed
// polls are delivered too: an unreachable device is a fact worth recording, and
// a sink that only ever hears about successes cannot tell "fine" from "gone".
type Sink interface {
	Observe(ctx context.Context, snap Snapshot)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(ctx context.Context, snap Snapshot)

func (f SinkFunc) Observe(ctx context.Context, s Snapshot) { f(ctx, s) }

// Connect opens a logged-in session to a device. The collector calls it on the
// first poll and again whenever it has dropped a client, so it must be safe to
// call repeatedly.
type Connect func(ctx context.Context) (*ubus.Client, error)

// Target is one device to poll.
type Target struct {
	DeviceID int64
	MAC      string
	Name     string
	// ConnectionKey identifies the endpoint, trust pin, and credential used by
	// Connect. It must not contain those values themselves.
	ConnectionKey string
	// Class is the device's capability class ("A", "B", "C"). It selects the
	// measured per-poll CPU cost; an unmeasured class reports none rather than
	// borrowing another class's number.
	Class string
	// Gateway is desired-state authority to run site-wide WAN probes from this
	// device. Hardware labels and interface names are not substitutes: AP-only
	// devices must never each emit a competing "site" series.
	Gateway bool
	// AirtimeSplit is true only when the stored capability probe proved that
	// iwinfo's rx_time/tx_time counters track reality. Presence of the fields is
	// not proof: some drivers return plausible-looking counters that never move.
	AirtimeSplit bool
	// Baseline overrides the collector-wide baseline interval for this device.
	// Zero uses the default.
	//
	// It can only make polling CHEAPER, never more expensive: a value below the
	// collector default is clamped up. DEVICE-BUDGET's ceiling is a promise
	// about what the controller does to a device, and a per-device knob that
	// could raise the rate would turn that promise into a suggestion — the
	// budget harness measures the default and would never see the override.
	Baseline time.Duration
	Connect  Connect
}

// Options tune the collector. Zero values take the documented defaults.
type Options struct {
	Baseline    time.Duration
	Focused     time.Duration
	MaxInterval time.Duration
	SlowPoll    time.Duration
	LoadLimit   float64
	Log         *slog.Logger

	// Now is injectable so tests can drive the schedule without sleeping.
	Now func() time.Time
}

// Collector polls a set of devices.
type Collector struct {
	opts Options
	sink Sink
	log  *slog.Logger

	mu      sync.Mutex
	pollers map[int64]*poller
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// New builds a Collector. Nothing polls until Start.
func New(sink Sink, opts Options) *Collector {
	if opts.Baseline <= 0 {
		opts.Baseline = DefaultBaseline
	}
	if opts.Focused <= 0 {
		opts.Focused = DefaultFocused
	}
	if opts.MaxInterval <= 0 {
		opts.MaxInterval = DefaultMaxInterval
	}
	if opts.SlowPoll <= 0 {
		opts.SlowPoll = DefaultSlowPoll
	}
	if opts.LoadLimit <= 0 {
		opts.LoadLimit = DefaultLoadLimit
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Collector{opts: opts, sink: sink, log: opts.Log, pollers: map[int64]*poller{}}
}

// Start begins polling. Devices added later start immediately.
func (c *Collector) Start(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.started = true
	for _, p := range c.pollers {
		c.launch(p)
	}
}

// Stop halts polling and waits for the in-flight polls to finish.
//
// It waits rather than abandoning: a poll holds a session on the device, and
// leaving one mid-flight during shutdown is how a device accumulates sessions
// it will only release on the 300 s idle timer.
func (c *Collector) Stop() {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return
	}
	c.started = false
	cancel := c.cancel
	c.mu.Unlock()

	cancel()
	c.wg.Wait()

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pollers {
		p.closeClient()
	}
}

// Add registers a device. Adding one that is already registered replaces its
// target — the address or name may have changed — and keeps its schedule.
func (c *Collector) Add(t Target) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.pollers[t.DeviceID]; ok {
		p.mu.Lock()
		changed := p.target.MAC != t.MAC || p.target.ConnectionKey == "" ||
			t.ConnectionKey == "" || p.target.ConnectionKey != t.ConnectionKey
		scheduleChanged := p.target.Gateway != t.Gateway || p.target.Baseline != t.Baseline
		p.target = t
		p.mu.Unlock()
		if changed {
			// The endpoint/auth identity changed, or the caller could not prove it
			// stayed the same. A cached token/socket is not portable across either.
			p.closeClient()
		}
		if scheduleChanged && c.started {
			p.pokeSchedule()
		}
		return
	}
	p := newPoller(c, t)
	c.pollers[t.DeviceID] = p
	if c.started {
		c.launch(p)
	}
}

// RefreshAccess forces the next poll to authenticate again and retry every
// access-dependent slow source. rpcd scopes are fixed at session creation, and
// denied optional calls still stamp their cadence; dropping only the token
// would otherwise retain the old denial for up to fifteen minutes.
func (c *Collector) RefreshAccess(deviceID int64) bool {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return false
	}
	p.mu.Lock()
	p.dropClientLocked()
	p.ifaceAt = time.Time{}
	p.ifaceRefetchAt = time.Time{}
	p.meshAt = time.Time{}
	p.radioAt = time.Time{}
	p.boardAt = time.Time{}
	p.logAt = time.Time{}
	p.wanProbeAt = time.Time{}
	p.topologyAt = time.Time{}
	p.netAt = time.Time{}
	p.mu.Unlock()
	return true
}

// Remove stops polling a device — un-adoption, or removal from the inventory.
func (c *Collector) Remove(deviceID int64) {
	c.mu.Lock()
	p, ok := c.pollers[deviceID]
	delete(c.pollers, deviceID)
	c.mu.Unlock()
	if !ok {
		return
	}
	p.stop()
	// Give the session back rather than leaving it to idle out over the next
	// 300 s. Best effort: a device removed because it is gone will not answer,
	// and that is not a failure worth reporting.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.mu.Lock()
		client := p.client
		p.client = nil
		p.mu.Unlock()
		if client != nil {
			client.Destroy(ctx)
		}
	}()
}

func (c *Collector) launch(p *poller) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		p.run(c.ctx)
	}()
}

// Focus raises a device to the focused rate and returns the release function.
//
// It is reference-counted because two operators may have the same device open,
// and the first one closing a tab must not drop the other back to 60 s polling.
// The returned function is idempotent.
//
// Focusing also pokes the device to poll now. A UI screen that opened to a
// spinner for up to a minute would be indistinguishable from a broken one.
func (c *Collector) Focus(deviceID int64) (release func()) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return func() {}
	}
	return p.addFocus()
}

// Quiesce suspends polling of a device and returns the release function.
//
// DEVICE-BUDGET §4.6: never poll during an apply. Not for politeness — an apply
// is a sequence of session-scoped staged operations, and reads interleaved with
// it see a config that is neither the old one nor the new one. The returned
// function is idempotent, so it is safe to defer and also call early.
func (c *Collector) Quiesce(deviceID int64) (release func()) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return func() {}
	}
	return p.addQuiesce()
}

// Rediscover forces the next poll of a device to re-read its interface list.
//
// The list normally rides a 15-minute cadence, because interfaces change only
// when someone reconfigures the radios. An apply IS someone reconfiguring the
// radios, and until this existed the consequence was sharp: a mesh applied at
// 12:00 has its interface a few seconds later, while the cached list — fetched
// moments before, when the interface did not yet exist — says there is a
// configured section with no interface. That is the §5q signature, and the
// controller would have reported a critical fault that had already resolved,
// for up to fifteen minutes, after every successful mesh apply.
//
// Found on hardware the first time the mesh health readout met a real device.
//
// Cheap: it does not fetch anything, it just makes the next scheduled poll ask.
func (c *Collector) Rediscover(deviceID int64) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ifaceAt = time.Time{}
	p.ifaceRefetchAt = p.c.now().Add(ifaceSettleDelay)
}

// ifaceSettleDelay is how long after an apply to re-read the interface list a
// SECOND time.
//
// Measured: an 802.11s interface appears about four to six seconds after the
// apply that configures it returns (§5r). An immediate re-read is therefore not
// enough on its own — it lands in the gap, caches "this section has no
// interface", and holds that for the full fifteen-minute cadence. Which is the
// §5q signature, reported as a critical fault, about a backhaul that came up
// fine two seconds later.
//
// Both re-reads happen rather than just the delayed one: a change that alters
// config without creating an interface is visible immediately, and waiting to
// notice it would be a regression for the common case to fix the rare one.
const ifaceSettleDelay = 10 * time.Second

// Quiesced reports that polling is suspended for a device, which the UI shows
// as "paused during a configuration change" rather than as a gap in the data.
func (c *Collector) Quiesced(deviceID int64) bool {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.quiesce > 0
}

// Overhead is what the controller is costing one device.
//
// DEVICE-BUDGET §7 asks for this to be SHOWN, not just measured: "surfacing our
// own cost is both the honest thing to do and a real feature — it turns 'is this
// thing slowing down my router?' from an anxiety into a number the user can read
// and act on." UniFi can afford not to show it because it owns the hardware. We
// do not.
type Overhead struct {
	DeviceID int64         `json:"device_id"`
	Tier     Tier          `json:"tier"`
	Interval time.Duration `json:"-"`
	// IntervalSeconds is the CURRENT interval, including any backoff or
	// evidence-based widening — not the configured one, which would understate
	// a device we have deliberately backed off from.
	IntervalSeconds float64 `json:"interval_seconds"`
	PollsPerMinute  float64 `json:"polls_per_minute"`
	Requests        int64   `json:"requests"`
	BytesOut        int64   `json:"bytes_out"`
	Polls           int64   `json:"polls"`
	Failures        int64   `json:"failed_polls"`
	Since           int64   `json:"since"`
	// RequestsPerMinute is the rate actually observed, which is the number the
	// budget is written in.
	RequestsPerMinute float64 `json:"requests_per_minute"`
	// NonPollRequests is every request that was not a poll — session logins,
	// and anything that escaped the batch. Logins amortise to nothing; a number
	// that grows with the poll count means a call is being made outside the
	// batch, which is a defect and not a rate to be averaged away.
	NonPollRequests int64 `json:"non_poll_requests"`
	Quiesced        bool  `json:"quiesced"`

	// CPUMillisPerPoll is what one poll of the current tier costs this device,
	// in milliseconds of its own CPU. Nil when the device's class has never
	// been measured — see cpucost.go.
	CPUMillisPerPoll *float64 `json:"cpu_ms_per_poll,omitempty"`
	// CPUPercentOfCore is that cost at the rate this device is ACTUALLY being
	// polled, including any backoff or widening. Nil for the same reason.
	CPUPercentOfCore *float64 `json:"cpu_percent_of_core,omitempty"`
	// CPUBasis always says where the figure came from, or why there is none. A
	// derived number that does not announce itself gets read as a measurement.
	CPUBasis string `json:"cpu_basis"`
}

// Overhead reports the controller's cost for one device.
// Degraded reports the standing limitations of the last poll of a device: the
// calls that were refused or unreadable, and what each one costs.
//
// Standing is the operative word. A degradation is a property of the device's
// ACL, its driver or its firmware, not an event — it will be identical on the
// next poll and the one after. That is why they are logged at debug rather than
// raised per poll, and it is also why they have to be READABLE somewhere: a
// limitation the controller knows about and never shows is one the operator
// discovers from a number being quietly wrong.
//
// The particular case that prompted this: without luci-rpc.getWirelessDevices
// the poll cannot tell a mesh point from an access point, so it falls back to
// treating every interface as an AP and counts a mesh backhaul's peers as
// clients. The fallback is the right one — the alternative silently stops
// counting real clients — but it must not be invisible.
func (c *Collector) Degraded(deviceID int64) ([]Degradation, bool) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.degradedKnown {
		return nil, false
	}
	out := make([]Degradation, len(p.degraded))
	copy(out, p.degraded)
	return out, true
}

// Broadcasting is every BSS the last poll saw on this device, including the
// ones this controller does not manage.
//
// Worth surfacing precisely because the controller leaves foreign config alone:
// an AP adopted with SSIDs already on it keeps broadcasting them, correctly and
// invisibly. An operator who cannot see them from here cannot tell an SSID they
// forgot about from one that is not there, and the first is a security
// question.
func (c *Collector) Broadcasting(deviceID int64) ([]AP, bool) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return nil, false
	}
	// One definition of "known", shared with the write side. Two copies of this
	// rule is how they drift apart.
	return p.snapshotAPs()
}

// recordAPsLocked stores what this poll saw broadcasting. Caller holds p.mu.
//
// Gated on having ASKED, not on having found something. snap.APs may be
// legitimately empty — a device with its radios off broadcasts nothing — and
// that is an observation, not a reason to keep yesterday's list.
func (p *poller) recordAPsLocked(snap Snapshot) {
	if snap.APsFresh {
		p.aps, p.apsKnown = snap.APs, true
	}
}

// recordAPs is the locking wrapper, used by tests.
func (p *poller) recordAPs(snap Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recordAPsLocked(snap)
}

// snapshotAPs returns what is known, for tests and for Broadcasting.
func (p *poller) snapshotAPs() ([]AP, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.apsKnown {
		return nil, false
	}
	out := make([]AP, len(p.aps))
	copy(out, p.aps)
	return out, true
}

// everListedIfaces reports that some poll has successfully read this device's
// wireless interface list. Until one has, an empty AP set means "not asked".
func (p *poller) everListedIfaces() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.ifaceAt.IsZero()
}

// IfaceSections maps this device's wireless interfaces to the UCI wifi-iface
// section that created each one, and reports whether the map is known at all.
//
// The bool is the three-state rule. A device whose ACL refuses
// luci-rpc.getWirelessDevices, or one no poll has yet read the interface list
// from, returns false — which must NOT be read as "no interface has a section".
// Provenance is decided from this, and deciding it from silence would tell an
// operator their own SSID is foreign.
func (c *Collector) IfaceSections(deviceID int64) (map[string]string, bool) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ifaceSections == nil {
		return nil, false
	}
	out := make(map[string]string, len(p.ifaceSections))
	for k, v := range p.ifaceSections {
		out[k] = v
	}
	return out, true
}

// IfaceModes is each wireless interface's CONFIGURED mode. False means no poll
// has read it, which must not be taken as "everything is an AP" by anything
// that would then advise an operator to disable it.
func (c *Collector) IfaceModes(deviceID int64) (map[string]string, bool) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ifaceModes == nil {
		return nil, false
	}
	out := make(map[string]string, len(p.ifaceModes))
	for k, v := range p.ifaceModes {
		out[k] = v
	}
	return out, true
}

// Radios returns the last reconciled stable UCI radio inventory. The bool is
// false until getWirelessDevices has answered at least once. Each channel list
// independently carries FrequenciesKnown, so an unavailable freqlist never
// becomes an empty or unrestricted plan.
func (c *Collector) Radios(deviceID int64) ([]radio.LiveState, bool) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.radiosKnown {
		return nil, false
	}
	return cloneRadioStates(p.radios), true
}

// RadioStatus describes the cache's source time and the most recent device
// poll. It lets callers retain useful last-known values without presenting an
// offline or overdue inventory as current.
func (c *Collector) RadioStatus(deviceID int64) (radio.CollectionStatus, bool) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return radio.CollectionStatus{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.radiosKnown {
		return radio.CollectionStatus{}, false
	}
	status := radio.CollectionStatus{ConsecutiveFailures: p.fails,
		LastPollOK: p.fails == 0 && !p.lastPoll.IsZero()}
	if !p.radioObservedAt.IsZero() {
		status.ObservedAt = p.radioObservedAt.UnixMilli()
	}
	if !p.lastPoll.IsZero() {
		status.LastPollAt = p.lastPoll.UnixMilli()
	}
	if !p.radioAttemptAt.IsZero() {
		status.LastSourceAttemptAt = p.radioAttemptAt.UnixMilli()
		status.LastSourceAttemptOK = p.radioAttemptOK
	}
	status.Stale = !status.LastPollOK || p.radioObservedAt.IsZero() ||
		!p.c.now().Before(p.radioObservedAt.Add(rediscoverInterval)) ||
		(!p.radioAttemptAt.IsZero() && !p.radioAttemptOK)
	return status, true
}

func (p *poller) reconcileRadios(snap *Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if snap.radioInventoryAsked {
		attemptAt := snap.At
		if attemptAt.IsZero() {
			attemptAt = p.c.now()
		}
		p.radioAttemptAt, p.radioAttemptOK = attemptAt, snap.radioInventoryOK
	}

	discardResult := map[string]bool{}
	if snap.radioInventory != nil {
		observedAt := snap.At
		if observedAt.IsZero() {
			observedAt = p.c.now()
		}
		previous := make(map[string]radio.LiveState, len(p.radios))
		for _, state := range p.radios {
			previous[state.Key] = state
		}
		inventory := append([]radio.InventoryRadio(nil), snap.radioInventory...)
		sort.Slice(inventory, func(i, j int) bool { return inventory[i].Key < inventory[j].Key })
		next := make([]radio.LiveState, 0, len(inventory))
		seen := map[string]bool{}
		targetsChanged := !p.radiosKnown || len(previous) != len(inventory)
		for _, item := range inventory {
			if seen[item.Key] {
				continue
			}
			seen[item.Key] = true
			state := radio.LiveState{InventoryRadio: cloneRadioInventory(item),
				InventoryObservedAt: observedAt.UnixMilli(), Frequencies: []radio.Frequency{}}
			if old, ok := previous[item.Key]; ok && sameRadioFrequencyIdentity(old.InventoryRadio, item) {
				state.Frequencies = cloneRadioFrequencies(old.Frequencies)
				state.FrequenciesKnown = old.FrequenciesKnown
				state.FrequenciesObservedAt = old.FrequenciesObservedAt
			} else {
				targetsChanged = true
				discardResult[item.Key] = true
			}
			next = append(next, state)
		}
		if len(next) != len(previous) {
			targetsChanged = true
		}
		p.radios, p.radiosKnown = next, true
		p.radioObservedAt = observedAt
		if targetsChanged {
			// A newly learned or moved radio gets its frequency list on the next
			// normal poll, rather than waiting a full slow interval.
			p.radioAt = time.Time{}
		}
	}

	if !p.radiosKnown {
		return
	}
	byKey := make(map[string]int, len(p.radios))
	for i := range p.radios {
		byKey[p.radios[i].Key] = i
	}
	for key := range snap.radioFrequencyAsked {
		if i, ok := byKey[key]; ok {
			p.radios[i].Frequencies = []radio.Frequency{}
			p.radios[i].FrequenciesKnown = false
			p.radios[i].FrequenciesObservedAt = 0
		}
	}
	frequencyObservedAt := snap.At
	if frequencyObservedAt.IsZero() {
		frequencyObservedAt = p.c.now()
	}
	for key, frequencies := range snap.radioFrequencies {
		if discardResult[key] {
			continue // result came from the pre-refresh runtime interface
		}
		if i, ok := byKey[key]; ok {
			p.radios[i].Frequencies = cloneRadioFrequencies(frequencies)
			p.radios[i].FrequenciesKnown = true
			p.radios[i].FrequenciesObservedAt = frequencyObservedAt.UnixMilli()
		}
	}
	snap.Radios = cloneRadioStates(p.radios)
	snap.RadiosKnown = true
	sourceAt := snap.At
	if sourceAt.IsZero() {
		sourceAt = p.c.now()
	}
	snap.RadiosStale = p.radioObservedAt.IsZero() ||
		!sourceAt.Before(p.radioObservedAt.Add(rediscoverInterval)) ||
		(!p.radioAttemptAt.IsZero() && !p.radioAttemptOK)
}

func sameRadioFrequencyIdentity(old, next radio.InventoryRadio) bool {
	return old.Key == next.Key && old.Band == next.Band &&
		preferredRadioInterface(old.Interfaces) == preferredRadioInterface(next.Interfaces)
}

func cloneRadioStates(states []radio.LiveState) []radio.LiveState {
	if states == nil {
		return nil
	}
	out := make([]radio.LiveState, len(states))
	for i, state := range states {
		out[i] = state
		out[i].InventoryRadio = cloneRadioInventory(state.InventoryRadio)
		out[i].Frequencies = cloneRadioFrequencies(state.Frequencies)
	}
	return out
}

func cloneRadioInventory(in radio.InventoryRadio) radio.InventoryRadio {
	out := in
	out.Up = clonePtr(in.Up)
	out.Disabled = clonePtr(in.Disabled)
	out.Pending = clonePtr(in.Pending)
	out.CurrentMHz = clonePtr(in.CurrentMHz)
	out.CurrentChannel = clonePtr(in.CurrentChannel)
	if in.Interfaces != nil {
		out.Interfaces = append([]radio.Interface(nil), in.Interfaces...)
	}
	return out
}

func cloneRadioFrequencies(in []radio.Frequency) []radio.Frequency {
	if in == nil {
		return nil
	}
	out := make([]radio.Frequency, len(in))
	for i, frequency := range in {
		out[i] = frequency
		out[i].Restricted = clonePtr(frequency.Restricted)
		out[i].Active = clonePtr(frequency.Active)
		if frequency.Flags != nil {
			out[i].Flags = append([]string(nil), frequency.Flags...)
		}
	}
	return out
}

func clonePtr[T any](value *T) *T {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func (c *Collector) Overhead(deviceID int64) (Overhead, bool) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return Overhead{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	o := Overhead{
		DeviceID: deviceID,
		Tier:     p.tierLocked(),
		Interval: p.nextLocked(),
		Polls:    p.polls,
		Failures: p.failures,
		Since:    p.startedAt.Unix(),
		Quiesced: p.quiesce > 0,
	}
	o.IntervalSeconds = o.Interval.Seconds()
	o.Requests, o.BytesOut = p.requestsBase, p.bytesBase
	if p.client != nil {
		o.Requests += p.client.Requests()
		o.BytesOut += p.client.BytesOut()
	}
	// Requests beyond one per poll: session logins, and anything that escaped
	// the batch. The second is a defect, so the number is worth surfacing
	// rather than folding into a rate that hides it.
	o.NonPollRequests = o.Requests - o.Polls
	if o.NonPollRequests < 0 {
		o.NonPollRequests = 0
	}
	if mins := c.now().Sub(p.startedAt).Minutes(); mins > 0 {
		o.RequestsPerMinute = float64(o.Requests) / mins
		o.PollsPerMinute = float64(o.Polls) / mins
	}

	// Attributable CPU, derived from the measured per-poll cost and the rate
	// this device is actually being polled at — not the configured rate, which
	// would understate a device we have backed off from and overstate one that
	// is being widened.
	if ms, ok := cpuCost(p.target.Class, o.Tier); ok && o.IntervalSeconds > 0 {
		perPoll := ms
		pct := ms / (o.IntervalSeconds * 1000) * 100
		o.CPUMillisPerPoll = &perPoll
		o.CPUPercentOfCore = &pct
		o.CPUBasis = CPUBasis
	} else {
		o.CPUBasis = CPUUnmeasured
	}
	return o, true
}

// NoteExternalRequest attributes a request made outside the poll loop to a
// device, so the Management Overhead readout counts it.
//
// The discovery sweep is the case this exists for. It probes by address with
// its own HTTP client rather than the device's ubus client, so its one request
// per address would otherwise be invisible in a readout that claims to say what
// the controller costs this device. One request per operator-initiated scan is
// negligible — and "negligible, therefore uncounted" is exactly how a readout
// stops being trustworthy, so it is counted instead.
//
// It lands in NonPollRequests, which is where a request that is not a poll
// belongs.
func (c *Collector) NoteExternalRequest(deviceID int64, bytesOut int64) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requestsBase++
	p.bytesBase += bytesOut
}

// Tier reports how a device is currently being polled, for the UI.
func (c *Collector) Tier(deviceID int64) (Tier, bool) {
	c.mu.Lock()
	p := c.pollers[deviceID]
	c.mu.Unlock()
	if p == nil {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tierLocked(), true
}

func (c *Collector) now() time.Time {
	if c.opts.Now != nil {
		return c.opts.Now()
	}
	return time.Now()
}

// ---- per-device poller ----

type poller struct {
	c          *Collector
	wake       chan struct{}
	reschedule chan struct{}
	done       chan struct{}
	stopMu     sync.Once
	// cycleMu makes Quiesce a boundary for a cycle that already passed the
	// quiesce check. It is held through snapshot emission, so an apply cannot
	// begin while an older poll is still publishing pre-apply state. A cycle
	// takes cycleMu before emitMu; stop takes only emitMu, never the reverse.
	cycleMu sync.Mutex
	// emitMu makes Remove a boundary: when it returns, no in-flight or later
	// poll can still publish a snapshot under a device ID that may be reused.
	emitMu sync.RWMutex

	mu      sync.Mutex
	target  Target
	client  *ubus.Client
	ifaces  []string
	ifaceAt time.Time
	// ifaceRefetchAt schedules a SECOND re-read after an apply. See Rediscover:
	// an interface can take seconds to appear, and an immediate re-read alone
	// caches the moment before it did.
	ifaceRefetchAt time.Time
	// meshAt is the mesh peer read's own timer. Separate from ifaceAt because
	// the two are consumer and producer: see needMeshPeers.
	meshAt time.Time
	// degraded is the last poll's list of refused or unreadable calls, kept so
	// a standing limitation can be shown rather than only logged. degradedKnown
	// separates "the last poll found none" from "no poll has completed".
	degraded      []Degradation
	degradedKnown bool
	// aps is what the last poll saw BROADCASTING, whether or not this
	// controller put it there. apsKnown separates "the last poll saw none" from
	// "no poll has looked".
	aps      []AP
	apsKnown bool
	// ifaceModes is each wireless interface's configured mode, cached beside
	// the interface list and refreshed with it.
	ifaceModes map[string]string
	// ifaceSections is each wireless interface's UCI wifi-iface section, cached
	// beside the modes and refreshed with them. It is what makes "did this
	// controller write that BSS?" answerable at all: the poll sees interfaces
	// and SSIDs, and only this says which section produced one.
	ifaceSections   map[string]string
	ifaceRadios     map[string]string
	radios          []radio.LiveState
	radiosKnown     bool
	radioAt         time.Time
	radioObservedAt time.Time
	radioAttemptAt  time.Time
	radioAttemptOK  bool
	boardAt         time.Time
	logAt           time.Time
	// logInterval is fixed at one minute in production. Tests shorten it while
	// preserving the ratio to a modeled slow full baseline.
	logInterval time.Duration
	wanProbeAt  time.Time
	// wanInterval is zero in production, which means the fixed one-minute
	// contract. Tests shorten it without exposing a rate-raising option.
	wanInterval          time.Duration
	topologyAt           time.Time
	topologyBridges      []string
	topologyBridgesKnown bool

	// networks are the device's IPv4 subnets, refreshed on the slow cadence and
	// stamped onto every poll in between. Without carrying them forward, only
	// one poll in fifteen minutes could scope its own hosts, and the other
	// fourteen would record every client as "unknown".
	networks []Network
	netAt    time.Time
	focus    int
	quiesce  int

	// fails counts consecutive failed polls, driving exponential backoff.
	fails int

	// polls and failures are lifetime totals for the overhead readout, distinct
	// from fails, which resets on every success.
	polls     int64
	failures  int64
	startedAt time.Time

	// requestsBase and bytesBase carry the counts of clients we have dropped.
	// The counters live on the ubus client, so a reconnect would otherwise
	// silently reset the device-facing cost the UI shows — and a device that
	// reconnects often is exactly the one whose cost you want to see.
	requestsBase int64
	bytesBase    int64

	// widen counts evidence-based interval doublings: the device told us, by
	// its load or its latency, that it is busy. Distinct from fails, because
	// "slow" and "gone" deserve different recovery — this one decays gently
	// on each good poll instead of resetting, so a device that is merely
	// borderline does not oscillate between rates every interval.
	widen int

	lastPoll time.Time
}

func newPoller(c *Collector, t Target) *poller {
	return &poller{
		c:          c,
		target:     t,
		startedAt:  c.now(),
		wake:       make(chan struct{}, 1),
		reschedule: make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
}

func (p *poller) run(ctx context.Context) {
	initial := p.stagger()
	timer := time.NewTimer(initial)
	defer timer.Stop()
	// Full and auxiliary deadlines share one timer and one goroutine. This keeps
	// requests serialized while allowing slow-baseline devices to retain
	// one-minute log continuity (and gateway WAN samples) without re-running the
	// full poll.
	nextIsFull := true
	fullDue := time.Now().Add(initial)
	for {
		doFull := nextIsFull
		doWork := true
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		case <-timer.C:
		case <-p.wake:
			doFull = true
		case <-p.reschedule:
			doWork = false
		}
		if !doWork {
			fullDue = time.Now().Add(p.next())
		} else if doFull {
			started := time.Now()
			p.tick(ctx)
			// Cadence is start-to-start. Besides matching the advertised rate,
			// this lets the full one-minute gateway poll absorb the WAN probe
			// instead of a probe-only request racing it just after completion.
			fullDue = started.Add(p.next())
		} else {
			p.tickAux(ctx)
		}
		delay, full := p.nextWake(fullDue)
		nextIsFull = full
		// Go 1.23 made Timer channels synchronous, so Reset needs no drain
		// dance: a stale value cannot be waiting in the channel.
		timer.Reset(delay)
	}
}

func (p *poller) nextWake(fullDue time.Time) (time.Duration, bool) {
	fullDelay := time.Until(fullDue)
	if fullDelay < 0 {
		fullDelay = 0
	}
	auxDelay, enabled := p.nextAuxDelay()
	if enabled {
		difference := fullDelay - auxDelay
		if difference < 0 {
			difference = -difference
		}
		if difference <= p.auxCoalesceWindow() {
			// Let a nearby full poll carry the probe in its existing batch. This
			// preserves one request/minute at the default cadence.
			return auxDelay, true
		}
	}
	if enabled && auxDelay < fullDelay {
		return auxDelay, false
	}
	return fullDelay, true
}

func (p *poller) auxCoalesceWindow() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	window := min(5*time.Second, p.logIntervalLocked()/4)
	if p.target.Gateway {
		window = min(window, p.wanIntervalLocked()/4)
	}
	return window
}

func (p *poller) nextAuxDelay() (time.Duration, bool) {
	logDelay := p.nextLogDelay()
	wanDelay, wanEnabled := p.nextWANDelay()
	if wanEnabled && wanDelay < logDelay {
		return wanDelay, true
	}
	return logDelay, true
}

func (p *poller) nextLogDelay() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.quiesce > 0 {
		return clamp(p.c.opts.Focused, time.Second, p.c.opts.MaxInterval)
	}
	interval := p.logIntervalLocked()
	if p.logAt.IsZero() {
		return interval
	}
	elapsed := p.c.now().Sub(p.logAt)
	if elapsed < 0 || elapsed >= interval {
		return 0
	}
	return interval - elapsed
}

func (p *poller) nextWANDelay() (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.target.Gateway {
		return 0, false
	}
	if p.quiesce > 0 {
		return clamp(p.c.opts.Focused, time.Second, p.c.opts.MaxInterval), true
	}
	interval := p.wanIntervalLocked()
	if p.wanProbeAt.IsZero() {
		return interval, true
	}
	elapsed := p.c.now().Sub(p.wanProbeAt)
	if elapsed < 0 {
		return 0, true
	}
	if elapsed >= interval {
		// A failed full poll already has its own retry schedule. Do not add an
		// immediate second dial; the independent WAN attempt resumes next minute.
		if p.fails > 0 {
			return interval, true
		}
		return 0, true
	}
	return interval - elapsed, true
}

// stagger spreads devices across the baseline interval, deterministically from
// the MAC so the spread survives a restart and does not need coordination.
//
// DEVICE-BUDGET §4.4: ten devices at 60 s should be one request every 6 s, not
// ten requests every 60 s. The stampede matters less for the controller than for
// a shared uplink and for the operator reading a graph where everything moves at
// once.
func (p *poller) stagger() time.Duration {
	h := fnv.New32a()
	p.mu.Lock()
	_, _ = h.Write([]byte(p.target.MAC))
	p.mu.Unlock()
	return time.Duration(uint64(h.Sum32()) % uint64(p.c.opts.Baseline))
}

func (p *poller) tick(ctx context.Context) {
	p.cycleMu.Lock()
	defer p.cycleMu.Unlock()

	p.mu.Lock()
	if p.quiesce > 0 {
		p.mu.Unlock()
		return // an apply owns this device; §4.6
	}
	tier := p.tierLocked()
	target := p.target
	ifaces := p.ifaces
	modes := p.ifaceModes
	ifaceRadios := cloneStringMap(p.ifaceRadios)
	p.mu.Unlock()

	// Bound the poll by its own interval so a slow device produces gaps rather
	// than a queue of overlapping requests.
	pctx, cancel := context.WithTimeout(ctx, p.pollTimeout(tier))
	defer cancel()

	client, err := p.dial(pctx, target)
	if err != nil {
		p.fail(ctx, Snapshot{
			DeviceID: target.DeviceID, MAC: target.MAC, Name: target.Name,
			Tier: tier, At: p.c.now(), Err: err,
		})
		return
	}
	snap := p.poll(pctx, client, target, tier, ifaces, modes)
	if snap.Err != nil {
		p.fail(ctx, snap)
		return
	}
	// The radio list, if this poll asked for it, for the next poll to use. A
	// device with no radios legitimately returns an empty list, which is why
	// IfacesFresh is separate from len(Ifaces) — "asked and there are none" and
	// "did not ask" are different, and only the first should update the cache.
	p.mu.Lock()
	p.degraded, p.degradedKnown = snap.Degraded, true
	p.recordAPsLocked(snap)
	p.mu.Unlock()

	if snap.IfacesFresh {
		p.mu.Lock()
		p.ifaces, p.ifaceAt = snap.Ifaces, p.c.now()
		// A new interface list may contain a mesh nobody has asked about yet.
		p.meshAt = time.Time{}
		// The scheduled second look has happened; do not repeat it.
		if !p.ifaceRefetchAt.IsZero() && !p.c.now().Before(p.ifaceRefetchAt) {
			p.ifaceRefetchAt = time.Time{}
		}
		// Modes are cached only when they were actually read. A device whose
		// ACL refuses getWirelessDevices keeps whatever it knew rather than
		// forgetting, and a device that has never answered keeps an empty map —
		// which servesClients reads as "assume AP", the prior behaviour.
		if snap.IfaceModes != nil {
			p.ifaceModes = snap.IfaceModes
		}
		// Same guard: a poll that did not read the interface list must not
		// erase a determination an earlier one made.
		if snap.IfaceSections != nil {
			p.ifaceSections = snap.IfaceSections
		}
		if snap.IfaceRadios != nil {
			p.ifaceRadios = snap.IfaceRadios
			ifaceRadios = cloneStringMap(snap.IfaceRadios)
		}
		p.mu.Unlock()
	}
	if snap.IfaceRadios == nil {
		snap.IfaceRadios = ifaceRadios
	}
	p.reconcileRadios(&snap)
	if snap.Topology.Cycle && topologySourceSuccessful(snap.Topology.Sources, TopologySourceNetworkDevices) {
		bridges := topologyBridgeNames(snap.Topology.NetworkDevices)
		p.mu.Lock()
		changed := !p.topologyBridgesKnown || !slices.Equal(p.topologyBridges, bridges)
		p.topologyBridges, p.topologyBridgesKnown = bridges, true
		if changed && len(bridges) > 0 {
			p.topologyAt = time.Time{}
		}
		p.mu.Unlock()
	}
	// The subnets, likewise. A device with no IPv4 address at all returns an
	// empty list legitimately, so freshness is decided by whether this poll
	// ASKED — p.needNetworks() at build time — not by the list being non-empty.
	p.mu.Lock()
	if snap.askedNetworks {
		p.networks = snap.Networks
	} else {
		// Carry the last known set onto this snapshot so the sink can scope the
		// hosts it just collected. netAt is stamped where the call is BUILT, so
		// a device that refuses the call does not re-ask on every poll.
		snap.Networks = p.networks
	}
	p.mu.Unlock()
	p.succeed(snap)
	p.emit(ctx, snap)
}

// tickAux runs only due router-log and gateway-probe work between full polls.
// It shares the cycle lock and session with tick, so it cannot race an apply or
// another request. It deliberately does not update reachability, radio
// freshness, client state, or the full-poll backoff.
func (p *poller) tickAux(ctx context.Context) {
	p.cycleMu.Lock()
	defer p.cycleMu.Unlock()

	p.mu.Lock()
	if p.quiesce > 0 {
		p.mu.Unlock()
		return
	}
	target := p.target
	p.mu.Unlock()
	calls, wan, logs := p.buildAuxCalls()
	if len(calls) == 0 {
		return
	}

	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client, err := p.dial(pctx, target)
	var snap Snapshot
	if err != nil {
		snap = Snapshot{DeviceID: target.DeviceID, MAC: target.MAC, Name: target.Name,
			Tier: Baseline, At: p.c.now(), WANOnly: wan, LogOnly: logs, Err: err}
	} else {
		snap = p.pollAux(pctx, client, target, calls, wan, logs)
	}
	p.mu.Lock()
	p.polls++
	if snap.Err != nil {
		p.failures++
		if p.client != nil {
			p.client.Close()
		}
	}
	p.mu.Unlock()
	p.emit(ctx, snap)
}

// dial returns the cached session, opening one if needed.
func (p *poller) dial(ctx context.Context, t Target) (*ubus.Client, error) {
	p.mu.Lock()
	if c := p.client; c != nil {
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	c, err := t.Connect(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	// Another tick cannot race here — one goroutine per device — but Add can
	// replace the target concurrently, so re-check rather than assume.
	if p.client == nil {
		p.client = c
	} else {
		c.Close()
		c = p.client
	}
	p.mu.Unlock()
	return c, nil
}

func (p *poller) fail(ctx context.Context, snap Snapshot) {
	p.mu.Lock()
	p.fails++
	p.polls++
	p.failures++
	n := p.fails
	// Keep the rpcd token after one hard failure, but stop a healthy keep-alive
	// socket from pinning the next poll to a stale route or endpoint.
	if n == 1 && p.client != nil {
		p.client.Close()
	}
	// Drop the session after repeated failures so the next attempt re-logs in.
	// One failure is not enough: the client already replays a single expired
	// session itself, and discarding it on every blip would turn a flaky link
	// into a login storm.
	if n >= 2 {
		p.dropClientLocked()
	}
	p.lastPoll = p.c.now()
	p.mu.Unlock()

	if n == 1 {
		p.c.log.Warn("poll failed", "device", snap.MAC, "err", snap.Err)
	} else {
		p.c.log.Debug("poll still failing", "device", snap.MAC,
			"consecutive", n, "err", snap.Err)
	}
	p.emit(ctx, snap)
}

func (p *poller) emit(ctx context.Context, snap Snapshot) {
	p.emitMu.RLock()
	defer p.emitMu.RUnlock()
	select {
	case <-p.done:
		return
	default:
		p.c.sink.Observe(ctx, snap)
	}
}

func (p *poller) succeed(snap Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails = 0
	p.polls++
	p.lastPoll = p.c.now()
	if snap.Board != nil {
		p.boardAt = p.c.now()
	} else if permanentlyDenied(snap, "system", "board") {
		// Refused rather than merely missed: stop asking every poll. Marking it
		// read schedules the next attempt for the normal refresh interval, which
		// is also the right cadence for noticing a widened ACL.
		p.boardAt = p.c.now()
	}

	// Evidence-based backoff, DEVICE-BUDGET §4.5. Widen on the device's own
	// symptoms — its load average, or how long it took to answer us — and
	// recover one step at a time so a device sitting near the threshold does not
	// flip rates every interval.
	switch {
	case snap.Load[0] >= p.c.opts.LoadLimit, snap.Duration >= p.c.opts.SlowPoll:
		if p.widen < maxWiden {
			p.widen++
			p.c.log.Info("widening poll interval; the device reports it is busy",
				"device", snap.MAC, "load1", snap.Load[0],
				"poll_ms", snap.Duration.Milliseconds(), "step", p.widen)
		}
	case p.widen > 0:
		p.widen--
	}
}

// next returns the delay before the following poll.
func (p *poller) next() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nextLocked()
}

// nextLocked is next() with the lock already held, so the overhead readout can
// report the interval a device is ACTUALLY on without taking it twice.
func (p *poller) nextLocked() time.Duration {
	base := p.baselineLocked()
	if p.focus > 0 {
		base = p.c.opts.Focused
	}
	// MaxInterval caps adaptive widening/backoff, not an operator's explicitly
	// slower baseline. Letting a 15-minute override collapse to the default
	// 10-minute cap silently raises the full-poll rate.
	ceiling := max(p.c.opts.MaxInterval, base)
	if p.quiesce > 0 {
		// Re-check soon rather than sleeping out a full interval: an apply plus
		// its confirm window is under two minutes, and polling should resume
		// promptly once it clears.
		return clamp(p.c.opts.Focused, time.Second, ceiling)
	}
	if p.fails > 0 {
		// Jitter first, clamp second. The other order lets a jittered interval
		// land up to 50% above MaxInterval, which quietly turns a documented
		// ceiling into an approximate one.
		return clamp(withJitter(base<<min(p.fails, 6)), base/2, ceiling)
	}
	return clamp(base<<p.widen, base, ceiling)
}

// baselineLocked is this device's baseline interval: its own override when it
// has one, otherwise the collector default. An override shorter than the
// default is ignored — see Target.Baseline.
func (p *poller) baselineLocked() time.Duration {
	if p.target.Baseline > p.c.opts.Baseline {
		return p.target.Baseline
	}
	return p.c.opts.Baseline
}

func (p *poller) pollTimeout(tier Tier) time.Duration {
	d := p.c.opts.Baseline
	if tier == Focused {
		d = p.c.opts.Focused
	}
	return clamp(d, 5*time.Second, 30*time.Second)
}

func (p *poller) tierLocked() Tier {
	if p.focus > 0 {
		return Focused
	}
	return Baseline
}

func (p *poller) addFocus() func() {
	p.mu.Lock()
	p.focus++
	first := p.focus == 1
	since := p.c.now().Sub(p.lastPoll)
	p.mu.Unlock()

	// Poke, but only if the data would be stale at the focused rate — otherwise
	// a UI that opens and closes repeatedly becomes its own load generator.
	if first && since >= p.c.opts.Focused {
		p.poke()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if p.focus > 0 {
				p.focus--
			}
			p.mu.Unlock()
		})
	}
}

func (p *poller) addQuiesce() func() {
	p.mu.Lock()
	p.quiesce++
	p.mu.Unlock()

	// A cycle that acquired cycleMu before the flag changed may finish, including
	// its sink emission. Waiting here makes return from Quiesce the point after
	// which no poll is in flight; cycles queued behind us see quiesce above.
	p.cycleMu.Lock()
	p.cycleMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if p.quiesce > 0 {
				p.quiesce--
			}
			resume := p.quiesce == 0
			p.mu.Unlock()
			if resume {
				p.poke() // the apply is done; refresh rather than wait it out
			}
		})
	}
}

func (p *poller) poke() {
	select {
	case p.wake <- struct{}{}:
	default: // already pending; one wake is enough
	}
}

func (p *poller) pokeSchedule() {
	select {
	case p.reschedule <- struct{}{}:
	default:
	}
}

func (p *poller) stop() {
	p.emitMu.Lock()
	p.stopMu.Do(func() { close(p.done) })
	p.emitMu.Unlock()
}

func (p *poller) closeClient() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropClientLocked()
}

// dropClientLocked discards the session, keeping what it cost.
//
// The counters live on the ubus client, so closing one without banking its
// totals loses every request and byte that session made. That is not merely a
// smaller number: Overhead derives NonPollRequests as Requests minus Polls, and
// polls are counted on the poller and survive. Discarding the requests while
// keeping the polls drives the difference negative, where it is clamped to
// zero — so the readout whose whole purpose is to surface calls that escaped
// the batch reports none, exactly when a session has just been thrown away.
//
// fail() has always banked them and closeClient() did not, which is the kind of
// difference two call sites are for. One place knows the rule now.
func (p *poller) dropClientLocked() {
	if p.client == nil {
		return
	}
	p.requestsBase += p.client.Requests()
	p.bytesBase += p.client.BytesOut()
	p.client.Close()
	p.client = nil
}

func clamp(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

// withJitter spreads retries so that devices which failed together — a switch
// reboot, a controller restart — do not come back in lockstep and re-create the
// stampede the stagger exists to prevent.
func withJitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d)))
}

// permanentlyDenied reports that a specific call in this snapshot failed in a
// way retrying cannot fix.
//
// The distinction is the one this project keeps relearning: a refused call is
// not a negative answer, and it is not a transient one either. Treating a
// permanent denial as transient means re-failing it forever; treating it as an
// answer means recording a fact that was never observed.
func permanentlyDenied(snap Snapshot, object, method string) bool {
	for _, d := range snap.Degraded {
		if d.Object == object && d.Method == method {
			return d.Permanent
		}
	}
	return false
}
