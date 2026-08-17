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
	"sync"
	"time"

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

	// DefaultMaxInterval caps both backoff paths. An unreachable device is
	// retried at least this often, so one that comes back is noticed without a
	// restart.
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
	// Class is the device's capability class ("A", "B", "C"). It selects the
	// measured per-poll CPU cost; an unmeasured class reports none rather than
	// borrowing another class's number.
	Class string
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
		changed := p.target.MAC != t.MAC
		p.target = t
		p.mu.Unlock()
		if changed {
			// The address behind this device changed. The cached session points
			// at the old one, and a session token is not portable between hosts.
			p.closeClient()
		}
		return
	}
	p := newPoller(c, t)
	c.pollers[t.DeviceID] = p
	if c.started {
		c.launch(p)
	}
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.apsKnown {
		return nil, false
	}
	out := make([]AP, len(p.aps))
	copy(out, p.aps)
	return out, true
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
	c      *Collector
	wake   chan struct{}
	done   chan struct{}
	stopMu sync.Once

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
	boardAt    time.Time

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
		c:         c,
		target:    t,
		startedAt: c.now(),
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

func (p *poller) run(ctx context.Context) {
	timer := time.NewTimer(p.stagger())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		case <-timer.C:
		case <-p.wake:
		}
		p.tick(ctx)
		// Go 1.23 made Timer channels synchronous, so Reset needs no drain
		// dance: a stale value cannot be waiting in the channel.
		timer.Reset(p.next())
	}
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
	p.mu.Lock()
	if p.quiesce > 0 {
		p.mu.Unlock()
		return // an apply owns this device; §4.6
	}
	tier := p.tierLocked()
	target := p.target
	ifaces := p.ifaces
	modes := p.ifaceModes
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
	snap := p.poll(pctx, client, tier, ifaces, modes)
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
	if len(snap.APs) > 0 {
		p.aps, p.apsKnown = snap.APs, true
	}
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
	p.c.sink.Observe(ctx, snap)
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
	// Drop the session after repeated failures so the next attempt re-logs in.
	// One failure is not enough: the client already replays a single expired
	// session itself, and discarding it on every blip would turn a flaky link
	// into a login storm.
	if n >= 2 && p.client != nil {
		p.requestsBase += p.client.Requests()
		p.bytesBase += p.client.BytesOut()
		p.client.Close()
		p.client = nil
	}
	p.lastPoll = p.c.now()
	p.mu.Unlock()

	if n == 1 {
		p.c.log.Warn("poll failed", "device", snap.MAC, "err", snap.Err)
	} else {
		p.c.log.Debug("poll still failing", "device", snap.MAC,
			"consecutive", n, "err", snap.Err)
	}
	p.c.sink.Observe(ctx, snap)
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
	if p.quiesce > 0 {
		// Re-check soon rather than sleeping out a full interval: an apply plus
		// its confirm window is under two minutes, and polling should resume
		// promptly once it clears.
		return clamp(p.c.opts.Focused, time.Second, p.c.opts.MaxInterval)
	}
	if p.fails > 0 {
		// Jitter first, clamp second. The other order lets a jittered interval
		// land up to 50% above MaxInterval, which quietly turns a documented
		// ceiling into an approximate one.
		return clamp(withJitter(base<<min(p.fails, 6)), base/2, p.c.opts.MaxInterval)
	}
	return clamp(base<<p.widen, base, p.c.opts.MaxInterval)
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

func (p *poller) stop() {
	p.stopMu.Do(func() { close(p.done) })
}

func (p *poller) closeClient() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		p.client.Close()
		p.client = nil
	}
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
