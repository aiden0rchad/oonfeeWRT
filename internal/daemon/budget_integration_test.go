//go:build integration

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// The resource-budget harness (DEVICE-BUDGET §7): "a benchmark harness that
// adopts a device, runs baseline and focused polling, and asserts the §2
// numbers. Run it per release. A budget nobody measures is a wish."
//
//	OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt OONFEE_TEST_PASS=... \
//	OONFEE_BUDGET_MINUTES=60 \
//	go test -tags=integration ./internal/daemon/ -run TestBudget -v -timeout 90m
//
// Defaults to a short run so it is usable in a normal cycle; set
// OONFEE_BUDGET_MINUTES for the real thing. The flash-write assertion is the
// one that needs no scaling — a single write fails it at any duration.
//
// It measures device-side state over SSH, which the controller's own credential
// deliberately cannot reach. That is the point: a harness that could only see
// what the controller sees could not check whether the controller is lying.

func budgetEnv(t *testing.T) (host, user, pass string) {
	t.Helper()
	host = os.Getenv("OONFEE_TEST_HOST")
	user = os.Getenv("OONFEE_TEST_USER")
	pass = os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER")
	}
	return
}

// ssh runs a command on the device as root. Used only by the harness.
func sshRun(t *testing.T, host, cmd string) string {
	t.Helper()
	out, err := exec.Command("ssh", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=8",
		"root@"+host, cmd).Output()
	if err != nil {
		t.Skipf("the harness needs root SSH to the device to measure flash and "+
			"CPU (the controller credential deliberately cannot): %v", err)
	}
	return strings.TrimSpace(string(out))
}

// overlayState is what a flash write would change.
type overlayState struct {
	usedKB  int
	files   string
	writes  int64
	cpuIdle float64
	cpuTot  float64
}

func readOverlay(t *testing.T, host string) overlayState {
	t.Helper()
	var st overlayState
	// Used blocks on the overlay: a config commit lands here.
	if f := strings.Fields(sshRun(t, host, "df -k /overlay | tail -1")); len(f) > 3 {
		st.usedKB, _ = strconv.Atoi(f[2])
	}
	// The actual file list with mtimes — more precise than block counts, which
	// can stay flat across a small rewrite.
	st.files = sshRun(t, host,
		`find /overlay/upper -type f -newermt '1970-01-01' -exec sh -c 'echo "$1 $(date -r "$1" +%s)"' _ {} \; 2>/dev/null | sort`)
	// /proc/stat for attributable CPU.
	if f := strings.Fields(sshRun(t, host, "head -1 /proc/stat")); len(f) > 4 {
		var total float64
		for _, v := range f[1:] {
			n, _ := strconv.ParseFloat(v, 64)
			total += n
		}
		idle, _ := strconv.ParseFloat(f[4], 64)
		st.cpuIdle, st.cpuTot = idle, total
	}
	return st
}

func TestBudgetHarness(t *testing.T) {
	host, user, pass := budgetEnv(t)
	minutes := 2
	if v := os.Getenv("OONFEE_BUDGET_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			minutes = n
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const mac = "60:38:e0:bu:dg:et"
	blob, err := d.Keys.SealCredential(mac, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1)
	dev := &store.Device{MAC: mac, Host: host, Name: "budget", Scheme: "http",
		AdoptedAt: &at, CredEnc: blob}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}

	// Shipped intervals, not test-shortened ones: the budget is stated for the
	// real cadence and measuring a faster one would prove nothing.
	if err := d.StartCollector(ctx, collector.Options{Log: quietLogger()}); err != nil {
		t.Fatalf("StartCollector: %v", err)
	}
	d.StartMaintenance(ctx)

	half := time.Duration(minutes) * time.Minute / 2
	t.Logf("running %d minute(s): %v idle, then %v with a screen open",
		minutes, half, half)

	before := readOverlay(t, host)
	idleStart := time.Now()

	// ---- idle: adopted, nobody looking ----
	time.Sleep(half)
	idleO, ok := d.collectorRef().Overhead(dev.ID)
	if !ok {
		t.Fatal("no overhead recorded for a polled device")
	}
	idleElapsed := time.Since(idleStart)

	// ---- observed: a device screen is open ----
	release := d.Focus(dev.ID)
	focusStart := time.Now()
	focusBase := idleO.Requests
	time.Sleep(half)
	focusO, _ := d.collectorRef().Overhead(dev.ID)
	release()
	focusElapsed := time.Since(focusStart)
	after := readOverlay(t, host)

	idleRPM := float64(idleO.Requests) / idleElapsed.Minutes()
	focusRPM := float64(focusO.Requests-focusBase) / focusElapsed.Minutes()
	// The budget is a STEADY-STATE rate, and a short run is dominated by the
	// one-time session login. Reporting both keeps that honest instead of
	// hiding it behind a slack factor: the poll rate is what the tier promises,
	// and non-poll requests are what it costs on top — logins amortise to
	// nothing, anything else is a call that escaped the batch.
	idlePPM := float64(idleO.Polls) / idleElapsed.Minutes()

	t.Logf("idle     : %.2f polls/min, %d non-poll request(s) total "+
		"(%d requests in %v = %.2f/min raw)",
		idlePPM, idleO.NonPollRequests, idleO.Requests,
		idleElapsed.Round(time.Second), idleRPM)
	t.Logf("observed : %d request(s) in %v = %.2f req/min (budget: <= 6.0)",
		focusO.Requests-focusBase, focusElapsed.Round(time.Second), focusRPM)
	t.Logf("projected steady state at these rates: idle %.2f req/min "+
		"(budget <= 1.0), i.e. %.0f requests/hour",
		idlePPM, idlePPM*60)
	t.Logf("polls    : %d total, %d failed", focusO.Polls, focusO.Failures)
	t.Logf("bytes out: %d (%.1f B/request)", focusO.BytesOut,
		float64(focusO.BytesOut)/float64(max64(focusO.Requests, 1)))

	// Attributable device CPU across the whole run.
	if after.cpuTot > before.cpuTot {
		busy := (after.cpuTot - before.cpuTot) - (after.cpuIdle - before.cpuIdle)
		pct := busy * 100 / (after.cpuTot - before.cpuTot)
		t.Logf("device CPU during the run: %.2f%% busy (all causes, not just ours)", pct)
	}

	// ---- the assertions ----

	// Network. DEVICE-BUDGET §2: <= 1 request/60 s idle, <= 1 request/10 s
	// observed.
	//
	// Asserted on the POLL rate, which is what the tier promises and what the
	// budget describes in steady state, plus a separate and much stronger check
	// that nothing but session setup happens outside a poll. A raw-rate
	// assertion with slack in it would pass a genuine leak on a long run and
	// fail a healthy one on a short run.
	if idlePPM > 1.05 {
		t.Errorf("idle poll rate %.2f/min exceeds the 1/60s budget", idlePPM)
	}
	if focusRPM > 6.3 {
		t.Errorf("observed rate %.2f req/min exceeds the 1/10s budget", focusRPM)
	}
	// One request per poll. A handful of logins is expected; a count that scales
	// with polls means a call escaped the batch, which is what took the idle
	// rate to 1.08 req/min before interface discovery moved inside it.
	if focusO.NonPollRequests > 5 {
		t.Errorf("%d requests were not polls across %d polls — something is "+
			"calling outside the batch", focusO.NonPollRequests, focusO.Polls)
	}
	// The tiers must actually differ, or the split is buying nothing.
	if focusRPM <= idleRPM {
		t.Errorf("observed rate %.2f is not above idle %.2f; focus did not engage",
			focusRPM, idleRPM)
	}

	// Flash writes. The hard rule, and the one assertion that needs no scaling:
	// a single write fails it at any duration.
	if before.files != after.files {
		t.Errorf("THE DEVICE'S FLASH WAS WRITTEN during a read-only poll run.\n"+
			"This is DEVICE-BUDGET §2's hard rule — these devices have NOR/NAND "+
			"with finite write cycles and no wear levelling worth trusting.\ndiff:\n%s",
			diffLines(before.files, after.files))
	}
	if after.usedKB > before.usedKB {
		t.Errorf("overlay usage grew %d KB during a read-only run",
			after.usedKB-before.usedKB)
	}

	// Polls must have actually succeeded — a run that failed every poll would
	// pass every budget above for the wrong reason.
	if focusO.Polls == 0 {
		t.Fatal("no polls completed; the budget numbers above mean nothing")
	}
	if focusO.Failures*2 > focusO.Polls {
		t.Errorf("%d of %d polls failed", focusO.Failures, focusO.Polls)
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// diffLines reports what changed, so a failure names the file rather than
// leaving someone to hunt for it.
func diffLines(before, after string) string {
	was := map[string]bool{}
	for _, l := range strings.Split(before, "\n") {
		was[l] = true
	}
	var out []string
	for _, l := range strings.Split(after, "\n") {
		if l != "" && !was[l] {
			out = append(out, "  + "+l)
		}
	}
	now := map[string]bool{}
	for _, l := range strings.Split(after, "\n") {
		now[l] = true
	}
	for _, l := range strings.Split(before, "\n") {
		if l != "" && !now[l] {
			out = append(out, "  - "+l)
		}
	}
	if len(out) == 0 {
		return "  (mtimes changed with no file added or removed)"
	}
	return fmt.Sprint(strings.Join(out, "\n"))
}
