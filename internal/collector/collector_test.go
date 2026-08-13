package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// These run against tools/mock_ubus.py, which reproduces the measured device
// semantics — including the awkward ones this package exists to handle: the
// unsigned survey noise, hostapd's 100× rate scale, and ACL gaps that fail one
// call inside an otherwise good batch.

var mockAddr string

func TestMain(m *testing.M) {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mockAddr = fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command("python3", filepath.Join(root, "tools", "mock_ubus.py"),
		"--port", fmt.Sprint(port))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := waitReady(mockAddr, 10*time.Second); err != nil {
		_ = cmd.Process.Kill()
		fmt.Fprintln(os.Stderr, "mock not ready:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	os.Exit(code)
}

func repoRoot() (string, error) {
	dir, _ := os.Getwd()
	for range 6 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", errors.New("go.mod not found")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitReady(addr string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timeout")
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func mockConnect(t *testing.T) Connect {
	t.Helper()
	return func(ctx context.Context) (*ubus.Client, error) {
		c := ubus.New(ubus.Options{Host: mockAddr})
		if err := c.Login(ctx, "root", "good"); err != nil {
			return nil, err
		}
		t.Cleanup(c.Close)
		return c, nil
	}
}

// recorder collects snapshots and lets a test wait for the next one.
type recorder struct {
	mu   sync.Mutex
	snap []Snapshot
	ch   chan Snapshot
}

func newRecorder() *recorder { return &recorder{ch: make(chan Snapshot, 64)} }

func (r *recorder) Observe(_ context.Context, s Snapshot) {
	r.mu.Lock()
	r.snap = append(r.snap, s)
	r.mu.Unlock()
	select {
	case r.ch <- s:
	default:
	}
}

func (r *recorder) next(t *testing.T, within time.Duration) Snapshot {
	t.Helper()
	select {
	case s := <-r.ch:
		return s
	case <-time.After(within):
		t.Fatal("no snapshot arrived")
		return Snapshot{}
	}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.snap)
}

// fastOptions removes the stagger and the minute-long intervals so the schedule
// can be exercised in a test rather than only reasoned about.
func fastOptions() Options {
	return Options{
		Baseline: 80 * time.Millisecond,
		Focused:  20 * time.Millisecond,
		Log:      quiet(),
	}
}

func startCollector(t *testing.T, rec Sink, opts Options) *Collector {
	t.Helper()
	c := New(rec, opts)
	c.Add(Target{DeviceID: 1, MAC: "aa:bb:cc:dd:ee:ff", Name: "ap1", Connect: mockConnect(t)})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Start(ctx)
	t.Cleanup(c.Stop)
	return c
}

func TestBaselinePollShape(t *testing.T) {
	rec := newRecorder()
	startCollector(t, rec, fastOptions())

	snap := rec.next(t, 5*time.Second)
	if !snap.OK() {
		t.Fatalf("poll failed: %v", snap.Err)
	}
	if snap.Tier != Baseline {
		t.Errorf("tier = %q, want %q", snap.Tier, Baseline)
	}
	if len(snap.Degraded) != 0 {
		t.Errorf("unexpected degradations: %v", snap.Degraded)
	}
	if snap.Uptime == 0 {
		t.Error("uptime is zero")
	}
	// The mock reports load [8000, 9000, 8500] in 1/65536 units.
	if got := snap.Load[0]; got < 0.11 || got > 0.13 {
		t.Errorf("load1 = %v, want ~0.122 (the fixed-point scale is /65536)", got)
	}
	if snap.Board == nil {
		t.Fatal("the first poll did not read the board")
	}
	if snap.Board.Release.Description == "" {
		t.Error("board release description is empty")
	}
	if _, ok := snap.Interfaces["br-lan"]; !ok {
		t.Errorf("no interface counters: %v", snap.Interfaces)
	}
	if len(snap.APs) != 2 {
		t.Fatalf("got %d APs, want 2", len(snap.APs))
	}

	// Baseline must not pay for iwinfo: it is ~92% of a focused poll.
	if len(snap.Stations) != 0 || len(snap.Surveys) != 0 {
		t.Errorf("baseline poll fetched focused data: %d stations, %d surveys",
			len(snap.Stations), len(snap.Surveys))
	}
	for _, ap := range snap.APs {
		if ap.Clients == nil {
			t.Errorf("%s: client count missing", ap.Iface)
		}
		if ap.Airtime == nil {
			t.Errorf("%s: airtime missing", ap.Iface)
		}
	}
	if n, ok := snap.ClientCount(); !ok || n == 0 {
		t.Errorf("ClientCount = %d, %v; want a known non-zero total", n, ok)
	}
}

func TestFocusRaisesTheTierAndFetchesStations(t *testing.T) {
	rec := newRecorder()
	c := startCollector(t, rec, fastOptions())
	rec.next(t, 5*time.Second) // let the baseline poll land first

	release := c.Focus(1)
	defer release()
	if tier, _ := c.Tier(1); tier != Focused {
		t.Fatalf("tier after Focus = %q, want %q", tier, Focused)
	}

	var focused Snapshot
	deadline := time.After(5 * time.Second)
	for focused.Tier != Focused {
		select {
		case s := <-rec.ch:
			focused = s
		case <-deadline:
			t.Fatal("no focused snapshot arrived")
		}
	}
	if !focused.OK() {
		t.Fatalf("focused poll failed: %v", focused.Err)
	}
	if len(focused.Stations) == 0 {
		t.Error("focused poll returned no stations")
	}
	if len(focused.Surveys) == 0 {
		t.Fatal("focused poll returned no surveys")
	}
	// mwlwifi leaves rx_time uninitialised at a value beyond int64's range.
	// Decoding it as signed fails, and one decode error discards the whole
	// object — which would throw away the busy/active times, the only part of
	// the survey that works, to a field already known to be unusable.
	if focused.Surveys[0].ActiveTime == 0 {
		t.Error("survey lost its usable fields; the garbage rx_time took them with it")
	}
	for _, st := range focused.Stations {
		if st.Iface == "" {
			t.Error("a station is not attributed to an interface")
		}
	}

	release()
	// Focus is reference counted, so one release from one holder returns it.
	if tier, _ := c.Tier(1); tier != Baseline {
		t.Fatalf("tier after release = %q, want %q", tier, Baseline)
	}
}

func TestFocusIsReferenceCounted(t *testing.T) {
	rec := newRecorder()
	c := startCollector(t, rec, fastOptions())

	a := c.Focus(1)
	b := c.Focus(1)
	a()
	a() // idempotent
	if tier, _ := c.Tier(1); tier != Focused {
		t.Fatal("one viewer leaving dropped the device while another was still watching")
	}
	b()
	if tier, _ := c.Tier(1); tier != Baseline {
		t.Fatal("the last viewer leaving did not drop the device back to baseline")
	}
}

// DEVICE-BUDGET §4.6. Reads interleaved with an apply see a config that is
// neither the old one nor the new one.
func TestQuiesceStopsPolling(t *testing.T) {
	rec := newRecorder()
	c := startCollector(t, rec, fastOptions())
	rec.next(t, 5*time.Second)

	release := c.Quiesce(1)
	time.Sleep(50 * time.Millisecond) // let any in-flight poll finish
	before := rec.count()
	time.Sleep(400 * time.Millisecond) // several baseline intervals
	if after := rec.count(); after != before {
		t.Fatalf("%d polls happened while the device was quiesced", after-before)
	}

	release()
	release() // idempotent
	select {
	case <-rec.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("polling did not resume after the quiesce was released")
	}
}

// A denied call inside an otherwise good batch must degrade the snapshot, not
// discard it — and must never be recorded as a zero reading.
func TestPartialFailureDegradesRatherThanDiscards(t *testing.T) {
	ctx := context.Background()
	admin := ubus.New(ubus.Options{Host: mockAddr})
	if err := admin.Login(ctx, "root", "good"); err != nil {
		t.Fatalf("login: %v", err)
	}
	defer admin.Close()
	if err := admin.Call(ctx, "__test", "set_acl_gap", map[string]any{
		"pairs": []map[string]string{{"object": "hostapd.wlan0", "method": "get_clients"}},
	}, nil); err != nil {
		t.Skipf("mock does not support ACL-gap simulation: %v", err)
	}
	defer admin.Call(ctx, "__test", "set_acl_gap", map[string]any{"pairs": []any{}}, nil)

	rec := newRecorder()
	startCollector(t, rec, fastOptions())

	snap := rec.next(t, 5*time.Second)
	if !snap.OK() {
		t.Fatalf("one denied optional call failed the whole poll: %v", snap.Err)
	}
	if len(snap.Degraded) == 0 {
		t.Fatal("the denied call was not recorded as a degradation")
	}
	var found bool
	for _, d := range snap.Degraded {
		if d.Object == "hostapd.wlan0" && d.Method == "get_clients" {
			found = true
		}
	}
	if !found {
		t.Fatalf("degradations do not name the denied call: %v", snap.Degraded)
	}
	if snap.Complete() {
		t.Error("a snapshot with degradations reported itself complete")
	}

	// The critical property: the AP whose count could not be read reports
	// "unknown", and the fleet total refuses to be summed.
	for _, ap := range snap.APs {
		if ap.Iface == "wlan0" && ap.Clients != nil {
			t.Errorf("wlan0 reported %d clients from a call that was denied", *ap.Clients)
		}
	}
	if _, ok := snap.ClientCount(); ok {
		t.Error("ClientCount claimed a trustworthy total while one radio did not answer")
	}
	// The other radio still answered, so the poll was worth keeping.
	if snap.Uptime == 0 {
		t.Error("the rest of the poll was lost along with the denied call")
	}
}

// A device that cannot be reached must produce a snapshot saying so, not
// silence. A sink that only hears about successes cannot tell fine from gone.
func TestUnreachableDeviceIsReported(t *testing.T) {
	rec := newRecorder()
	opts := fastOptions()
	opts.MaxInterval = 200 * time.Millisecond
	c := New(rec, opts)
	c.Add(Target{DeviceID: 9, MAC: "de:ad:be:ef:00:01", Name: "gone",
		Connect: func(context.Context) (*ubus.Client, error) {
			return nil, errors.New("connection refused")
		}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	snap := rec.next(t, 5*time.Second)
	if snap.OK() {
		t.Fatal("an unreachable device produced a successful snapshot")
	}
	if snap.MAC != "de:ad:be:ef:00:01" {
		t.Errorf("snapshot is not attributed to the device: %+v", snap)
	}
	if snap.Uptime != 0 || len(snap.APs) != 0 {
		t.Error("a failed poll carried data")
	}
}

// ---- scheduling ----

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	c := New(newRecorder(), Options{
		Baseline: time.Second, Focused: 100 * time.Millisecond,
		MaxInterval: 8 * time.Second, Log: quiet(),
	})
	p := newPoller(c, Target{DeviceID: 1, MAC: "aa"})

	if got := p.next(); got != time.Second {
		t.Fatalf("healthy interval = %v, want 1s", got)
	}
	var last time.Duration
	for i := 1; i <= 10; i++ {
		p.fails = i
		got := p.next()
		if got > c.opts.MaxInterval {
			t.Fatalf("%d failures gave %v, above the %v cap", i, got, c.opts.MaxInterval)
		}
		if got < c.opts.Baseline/2 {
			t.Fatalf("%d failures gave %v, below the tier interval", i, got)
		}
		last = got
	}
	if last < 2*time.Second {
		t.Errorf("backoff did not grow: settled at %v", last)
	}
}

// §4.5: back off on evidence. A device reporting a high load average, or simply
// taking a long time to answer, gets polled less often — and recovers gradually
// so it does not oscillate between rates.
func TestEvidenceBasedWidening(t *testing.T) {
	c := New(newRecorder(), Options{
		Baseline: time.Second, MaxInterval: time.Hour,
		SlowPoll: 500 * time.Millisecond, LoadLimit: 5, Log: quiet(),
	})
	p := newPoller(c, Target{DeviceID: 1, MAC: "aa"})

	busy := Snapshot{Load: [3]float64{9, 9, 9}}
	for range 5 {
		p.succeed(busy)
	}
	if p.widen != maxWiden {
		t.Fatalf("widen = %d after five busy polls, want the %d cap", p.widen, maxWiden)
	}
	if got, want := p.next(), 8*time.Second; got != want {
		t.Fatalf("interval when busy = %v, want %v", got, want)
	}

	slow := Snapshot{Duration: time.Second}
	p.widen = 0
	p.succeed(slow)
	if p.widen != 1 {
		t.Errorf("a slow poll did not widen the interval: widen = %d", p.widen)
	}

	// Recovery is one step per good poll, not an immediate snap back.
	calm := Snapshot{Load: [3]float64{0.1}, Duration: time.Millisecond}
	p.widen = 3
	p.succeed(calm)
	if p.widen != 2 {
		t.Fatalf("widen = %d after one calm poll, want 2 (gradual recovery)", p.widen)
	}
}

// §4.4: ten devices at 60 s is one request every 6 s, not ten every 60 s.
func TestStaggerSpreadsDevices(t *testing.T) {
	c := New(newRecorder(), Options{Baseline: time.Minute, Log: quiet()})
	seen := map[time.Duration]int{}
	for i := range 10 {
		p := newPoller(c, Target{DeviceID: int64(i),
			MAC: fmt.Sprintf("aa:bb:cc:00:00:%02d", i)})
		d := p.stagger()
		if d < 0 || d >= time.Minute {
			t.Fatalf("stagger %v is outside the baseline interval", d)
		}
		seen[d]++
	}
	if len(seen) < 8 {
		t.Fatalf("ten devices produced only %d distinct offsets: %v", len(seen), seen)
	}

	// Deterministic: the spread must survive a restart, or every controller
	// bounce re-clusters the fleet.
	a := newPoller(c, Target{DeviceID: 1, MAC: "aa:bb:cc:dd:ee:ff"}).stagger()
	b := newPoller(c, Target{DeviceID: 1, MAC: "aa:bb:cc:dd:ee:ff"}).stagger()
	if a != b {
		t.Fatalf("stagger is not deterministic: %v then %v", a, b)
	}
}

func TestQuiescedPollerReschedulesSoon(t *testing.T) {
	c := New(newRecorder(), Options{
		Baseline: time.Hour, Focused: 5 * time.Second, MaxInterval: time.Hour,
		Log: quiet(),
	})
	p := newPoller(c, Target{DeviceID: 1, MAC: "aa"})
	p.quiesce = 1
	// Sleeping out a full baseline hour would leave the device unpolled long
	// after its apply finished.
	if got := p.next(); got > 10*time.Second {
		t.Fatalf("quiesced re-check interval = %v, want a short one", got)
	}
}

func TestAddAndRemove(t *testing.T) {
	rec := newRecorder()
	c := startCollector(t, rec, fastOptions())
	rec.next(t, 5*time.Second)

	c.Remove(1)
	if _, ok := c.Tier(1); ok {
		t.Fatal("a removed device is still registered")
	}
	time.Sleep(50 * time.Millisecond)
	before := rec.count()
	time.Sleep(300 * time.Millisecond)
	if after := rec.count(); after != before {
		t.Fatalf("%d polls happened after the device was removed", after-before)
	}

	// Focus and Quiesce on an unknown device must be no-ops, not panics: the UI
	// can hold a handle to a device that was un-adopted underneath it.
	c.Focus(999)()
	c.Quiesce(999)()
}

func TestStopIsIdempotent(t *testing.T) {
	c := New(newRecorder(), fastOptions())
	c.Stop() // never started
	c.Start(context.Background())
	c.Stop()
	c.Stop()
}

// ---- unit conversions the measurements forced ----

func TestSurveyNoiseIsUnwrapped(t *testing.T) {
	// iwinfo.survey reports noise unsigned while iwinfo.info reports it signed.
	// 161 is -95 dBm; a UI plotting 161 would be silently, badly wrong.
	if got := (Survey{Noise: 161}).NoiseDBm(); got != -95 {
		t.Errorf("NoiseDBm(161) = %d, want -95", got)
	}
	if got := (Survey{Noise: -95}).NoiseDBm(); got != -95 {
		t.Errorf("NoiseDBm(-95) = %d, want -95", got)
	}
}

func TestSurveyBusyPercent(t *testing.T) {
	s := Survey{ActiveTime: 19849, BusyTime: 495}
	if got := s.BusyPercent(); got < 2.4 || got > 2.6 {
		t.Errorf("BusyPercent = %v, want ~2.5", got)
	}
	if got := (Survey{}).BusyPercent(); got != 0 {
		t.Errorf("BusyPercent with no active time = %v, want 0 rather than a NaN", got)
	}
}

func TestAirtimeUtilizationIsNotAPercentage(t *testing.T) {
	// hostapd reports the 802.11 BSS-Load 0-255 scale. 172 is about 67%.
	if got := (Airtime{Utilization: 172}).UtilizationPercent(); got < 67 || got > 68 {
		t.Errorf("UtilizationPercent(172) = %v, want ~67.5", got)
	}
}

func TestMemoryUsedPrefersAvailable(t *testing.T) {
	// free+buffered+cached overstates pressure on a router, where the page cache
	// is most of RAM and is reclaimable.
	m := Memory{Total: 1000, Free: 100, Buffered: 200, Cached: 300, Available: 600}
	if got := m.Used(); got != 400 {
		t.Errorf("Used = %d, want 400 (total - available)", got)
	}
	old := Memory{Total: 1000, Free: 100, Buffered: 200, Cached: 300}
	if got := old.Used(); got != 400 {
		t.Errorf("Used without available = %d, want 400", got)
	}
}
