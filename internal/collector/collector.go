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
	DefaultFocused  = 8 * time.Second

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
	boardAt time.Time
	focus   int
	quiesce int

	// fails counts consecutive failed polls, driving exponential backoff.
	fails int

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
		c:      c,
		target: t,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
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
	stale := p.ifaceAt.IsZero() || p.c.now().Sub(p.ifaceAt) >= rediscoverInterval
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
	if stale {
		if found, err := p.discoverIfaces(pctx, client); err == nil {
			ifaces = found
			p.mu.Lock()
			p.ifaces, p.ifaceAt = found, p.c.now()
			p.mu.Unlock()
		} else {
			// Not fatal: a device with no radios is a legitimate target, and a
			// denied grant must not stop the rest of the poll. It is recorded on
			// the snapshot below so it cannot pass for "no wireless".
			p.c.log.Debug("could not list wireless interfaces",
				"device", target.MAC, "err", err)
		}
	}

	snap := p.poll(pctx, client, tier, ifaces)
	if snap.Err != nil {
		p.fail(ctx, snap)
		return
	}
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
	n := p.fails
	// Drop the session after repeated failures so the next attempt re-logs in.
	// One failure is not enough: the client already replays a single expired
	// session itself, and discarding it on every blip would turn a flaky link
	// into a login storm.
	if n >= 2 && p.client != nil {
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

	base := p.c.opts.Baseline
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
