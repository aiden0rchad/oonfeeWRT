//go:build integration

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// The resource-budget harness (DEVICE-BUDGET §7) runs the shipped idle and
// focused cadence against a proven class-C device. The full gate is explicit:
// once 60 or more minutes are requested, missing credentials, SSH, or probe
// evidence fail the test instead of turning a release gate into a passing SKIP.
//
// Secure credential source (preferred):
//
//	OONFEE_BUDGET_SOURCE_DATA_DIR=/path/to/controller-data \
//	OONFEE_BUDGET_DEVICE_MAC=aa:bb:cc:dd:ee:ff \
//	OONFEE_BUDGET_PASSPHRASE_FILE=/path/to/mode-600-passphrase \
//	OONFEE_BUDGET_MINUTES=60 \
//	go test -count=1 -tags=integration ./internal/daemon/ \
//	  -run '^TestBudgetHarness$' -v -timeout 90m
//
// Explicit throwaway-lab fallback:
//
//	OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt OONFEE_TEST_PASS=... \
//	OONFEE_BUDGET_MINUTES=60 \
//	go test -count=1 -tags=integration ./internal/daemon/ \
//	  -run '^TestBudgetHarness$' -v -timeout 90m

type budgetTarget struct {
	mac, host, name       string
	port                  int
	scheme, certFP        string
	user, pass            string
	credentialDescription string
}

func budgetUnavailable(t *testing.T, full bool, format string, args ...any) {
	t.Helper()
	if full {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

func budgetTargetFromEnv(ctx context.Context, t *testing.T, run budgetRun) budgetTarget {
	t.Helper()
	dataDir, dataOK := os.LookupEnv("OONFEE_BUDGET_SOURCE_DATA_DIR")
	mac, macOK := os.LookupEnv("OONFEE_BUDGET_DEVICE_MAC")
	passFile, passOK := os.LookupEnv("OONFEE_BUDGET_PASSPHRASE_FILE")
	anySource := dataOK || macOK || passOK
	allSource := dataOK && dataDir != "" && macOK && mac != "" && passOK && passFile != ""
	if anySource && !allSource {
		t.Fatal("secure credential mode requires OONFEE_BUDGET_SOURCE_DATA_DIR, " +
			"OONFEE_BUDGET_DEVICE_MAC, and OONFEE_BUDGET_PASSPHRASE_FILE together")
	}
	if allSource {
		passphrase, err := secrets.ReadPassphraseFile(passFile)
		if err != nil {
			t.Fatalf("read source keyring passphrase: %v", err)
		}
		defer clear(passphrase)
		keeper, err := secrets.Open(secrets.DefaultPath(dataDir), passphrase)
		if err != nil {
			t.Fatalf("open source credential keyring: %v", err)
		}
		defer keeper.Close()
		db, err := store.OpenReadOnly(ctx, driverName,
			filepath.Join(dataDir, DBFileName), keeper)
		if err != nil {
			t.Fatalf("open source controller database read-only: %v", err)
		}
		defer db.Close()
		dev, err := db.DeviceByMAC(ctx, mac)
		if err != nil {
			t.Fatalf("read source device %s: %v", mac, err)
		}
		if !dev.Adopted() {
			t.Fatalf("source device %s is not adopted; no scoped controller credential is available", mac)
		}
		user, password, err := keeper.OpenCredential(dev.MAC, dev.CredEnc)
		if err != nil {
			t.Fatalf("open source device credential: %v", err)
		}
		return budgetTarget{
			mac: dev.MAC, host: dev.Host, port: dev.Port, scheme: dev.Scheme,
			certFP: dev.CertFP, name: dev.Name, user: user, pass: password,
			credentialDescription: "read-only source database/keyring",
		}
	}

	host, hostOK := os.LookupEnv("OONFEE_TEST_HOST")
	user, userOK := os.LookupEnv("OONFEE_TEST_USER")
	password, passwordOK := os.LookupEnv("OONFEE_TEST_PASS")
	if !hostOK || host == "" || !userOK || user == "" || !passwordOK {
		budgetUnavailable(t, run.full, "set all of OONFEE_TEST_HOST, OONFEE_TEST_USER, "+
			"and OONFEE_TEST_PASS, or use the three OONFEE_BUDGET_SOURCE_* variables")
		return budgetTarget{}
	}
	return budgetTarget{
		mac: "02:00:00:00:00:c0", host: host, scheme: "http", name: "budget",
		user: user, pass: password, credentialDescription: "explicit test environment",
	}
}

type budgetSSH struct {
	t          *testing.T
	host       string
	knownHosts string
	full       bool
}

func newBudgetSSH(t *testing.T, host string, full bool) budgetSSH {
	t.Helper()
	return budgetSSH{t: t, host: host, full: full,
		knownHosts: filepath.Join(t.TempDir(), "known_hosts")}
}

// sshRun is retained unchanged for the adoption integration test in this
// build-tagged package. The budget gate uses budgetSSH instead.
func sshRun(t *testing.T, host, cmd string) string {
	t.Helper()
	out, err := exec.Command("ssh", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=8",
		"root@"+host, cmd).Output()
	if err != nil {
		t.Skipf("the integration test needs root SSH to the device: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// run executes a read-only measurement command as root. Host-key acceptance is
// isolated to the test temp directory; the operator's known_hosts is untouched.
func (s budgetSSH) run(cmd string) string {
	s.t.Helper()
	c := exec.Command("ssh", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile="+s.knownHosts,
		"-o", "ConnectTimeout=8", "root@"+s.host, cmd)
	out, err := c.Output()
	if err != nil {
		detail := ""
		if exit, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(string(exit.Stderr))
		}
		budgetUnavailable(s.t, s.full, "root SSH measurement failed: %v: %s",
			err, detail)
		return ""
	}
	return strings.TrimSpace(string(out))
}

type resourceState struct {
	cpu cpuCounters
	mem memoryCounters
}

func readResources(t *testing.T, ssh budgetSSH) resourceState {
	t.Helper()
	out := ssh.run("head -n 1 /proc/stat; printf '\\n__OONFEE_MEM__\\n'; cat /proc/meminfo")
	parts := strings.SplitN(out, "\n__OONFEE_MEM__\n", 2)
	if len(parts) != 2 {
		t.Fatalf("resource measurement returned no memory marker")
	}
	cpu, err := parseCPUCounters(strings.TrimSpace(parts[0]))
	if err != nil {
		t.Fatal(err)
	}
	mem, err := parseMemoryCounters(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	return resourceState{cpu: cpu, mem: mem}
}

func logResources(t *testing.T, label string, before, after resourceState) {
	t.Helper()
	if pct, ok := cpuBusyPercent(before.cpu, after.cpu); ok {
		t.Logf("%-9s CPU: %.2f%% whole-device busy (observational; not attributable)", label, pct)
	} else {
		t.Logf("%-9s CPU: unavailable (device counters did not advance monotonically)", label)
	}
	start, end := before.mem.usedKB(), after.mem.usedKB()
	t.Logf("%-9s RAM: %d -> %d KiB used (%+d KiB, whole-device observation; not attributable)",
		label, start, end, end-start)
}

type flashState struct {
	usedKB                    int64
	source, mountOptions      string
	paths, metadata, contents string
	hashAlgorithm             string
	writes, sectorsWritten    uint64
	writeCounter              bool
}

const flashSnapshotCommand = `set -eu
opts=$(awk '$2 == "/overlay" { print $4; exit }' /proc/mounts)
case ",$opts," in
  *,noatime,*) ;;
  *) echo "overlay is not mounted noatime; hashing it could itself write atime" >&2; exit 42 ;;
esac
printf '__OONFEE_MOUNT__\n%s\n' "$opts"
printf '__OONFEE_DF__\n'
df -k /overlay | tail -n 1
printf '__OONFEE_PATHS__\n'
find /overlay/upper -print 2>/dev/null | sed '1d' | sort
printf '__OONFEE_METADATA__\n'
if stat -c '%s|%Y|%n' /overlay/upper >/dev/null 2>&1; then
  find /overlay/upper -type f -exec stat -c '%s|%Y|%n' {} \; 2>/dev/null | sort
else
  find /overlay/upper -type f -exec sh -c 'printf "%s|%s|%s\n" "$(wc -c < "$1")" "$(date -r "$1" +%s)" "$1"' _ {} \; 2>/dev/null | sort
fi
printf '__OONFEE_CONTENT__\n'
if command -v sha256sum >/dev/null 2>&1; then
  echo sha256sum
  find /overlay/upper -type f -exec sha256sum {} \; 2>/dev/null | sort
else
  echo cksum
  find /overlay/upper -type f -exec cksum {} \; 2>/dev/null | sort
fi
printf '__OONFEE_DISKSTATS__\n'
cat /proc/diskstats 2>/dev/null || true`

func splitFlashSections(out string) (map[string]string, error) {
	want := map[string]bool{
		"__OONFEE_MOUNT__": true, "__OONFEE_DF__": true,
		"__OONFEE_PATHS__": true, "__OONFEE_METADATA__": true,
		"__OONFEE_CONTENT__": true, "__OONFEE_DISKSTATS__": true,
	}
	sections := map[string][]string{}
	current := ""
	for _, line := range strings.Split(out, "\n") {
		if want[line] {
			current = line
			if _, ok := sections[current]; !ok {
				sections[current] = nil
			}
			continue
		}
		if current != "" {
			sections[current] = append(sections[current], line)
		}
	}
	result := map[string]string{}
	for marker := range want {
		if _, ok := sections[marker]; !ok {
			return nil, fmt.Errorf("flash snapshot returned no %s section", marker)
		}
		result[marker] = strings.TrimSpace(strings.Join(sections[marker], "\n"))
	}
	return result, nil
}

func diskWrites(stats, source string) (writes, sectors uint64, ok bool) {
	name := filepath.Base(strings.TrimPrefix(source, "/dev/"))
	for _, line := range strings.Split(stats, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[2] != name {
			continue
		}
		w, werr := strconv.ParseUint(f[7], 10, 64)
		s, serr := strconv.ParseUint(f[9], 10, 64)
		if werr == nil && serr == nil {
			return w, s, true
		}
	}
	return 0, 0, false
}

func readFlash(t *testing.T, ssh budgetSSH) flashState {
	t.Helper()
	sections, err := splitFlashSections(ssh.run(flashSnapshotCommand))
	if err != nil {
		t.Fatal(err)
	}
	df := strings.Fields(sections["__OONFEE_DF__"])
	if len(df) < 4 {
		t.Fatalf("malformed df output for /overlay: %q", sections["__OONFEE_DF__"])
	}
	used, err := strconv.ParseInt(df[2], 10, 64)
	if err != nil {
		t.Fatalf("malformed used-block count %q: %v", df[2], err)
	}
	content := strings.SplitN(sections["__OONFEE_CONTENT__"], "\n", 2)
	if len(content) == 0 || content[0] == "" {
		t.Fatal("flash snapshot did not report a content-hash algorithm")
	}
	st := flashState{usedKB: used, source: df[0],
		mountOptions: sections["__OONFEE_MOUNT__"], hashAlgorithm: content[0],
		paths: sections["__OONFEE_PATHS__"], metadata: sections["__OONFEE_METADATA__"]}
	if len(content) == 2 {
		st.contents = content[1]
	}
	st.writes, st.sectorsWritten, st.writeCounter = diskWrites(
		sections["__OONFEE_DISKSTATS__"], st.source)
	return st
}

func assertNoFlashChange(t *testing.T, phase string, before, after flashState) {
	t.Helper()
	if before.source != after.source || before.hashAlgorithm != after.hashAlgorithm {
		t.Errorf("flash measurement basis changed during %s: source %q -> %q, hash %q -> %q",
			phase, before.source, after.source, before.hashAlgorithm, after.hashAlgorithm)
	}
	for _, part := range []struct {
		name        string
		before, now string
	}{
		{"path inventory", before.paths, after.paths},
		{"size/mtime metadata", before.metadata, after.metadata},
		{"content hashes", before.contents, after.contents},
	} {
		if part.before != part.now {
			t.Errorf("FLASH CHANGED during %s (%s):\n%s", phase, part.name,
				diffLines(part.before, part.now))
		}
	}
	if before.usedKB != after.usedKB {
		t.Errorf("overlay usage changed during %s: %d -> %d KiB", phase,
			before.usedKB, after.usedKB)
	}
	if before.writeCounter && after.writeCounter &&
		(after.writes != before.writes || after.sectorsWritten != before.sectorsWritten) {
		t.Errorf("overlay block-device write counters changed during %s: writes %d -> %d, sectors %d -> %d",
			phase, before.writes, after.writes, before.sectorsWritten, after.sectorsWritten)
	}
}

func TestBudgetHarness(t *testing.T) {
	run, err := parseBudgetRun(os.Getenv("OONFEE_BUDGET_MINUTES"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := budgetTargetFromEnv(ctx, t, run)
	ssh := newBudgetSSH(t, target.host, run.full)
	ssh.run("true") // validate root measurement access before contacting the target via ubus

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	blob, err := d.Keys.SealCredential(target.mac, target.user, target.pass)
	if err != nil {
		t.Fatal(err)
	}
	target.pass = ""
	at := int64(1)
	dev := &store.Device{MAC: target.mac, Host: target.host, Port: target.port,
		Scheme: target.scheme, CertFP: target.certFP, Name: target.name,
		AdoptedAt: &at, CredEnc: blob}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}

	// Prove the target rather than trusting an environment label. Probe uses the
	// exact scoped credential that the collector will use and is read-only.
	c, err := d.Connect(ctx, dev)
	if err != nil {
		t.Fatalf("connect for class-C capability proof: %v", err)
	}
	caps, probeErr := capability.Probe(ctx, c)
	c.Close()
	if probeErr != nil {
		t.Fatalf("capability proof failed: %v", probeErr)
	}
	if caps.Class != capability.ClassC {
		t.Fatalf("budget gate requires a probed class-C target; %s (%s, %s) classified %s",
			caps.Board.Model, caps.Board.System, caps.Board.Target, caps.Class)
	}
	if err := d.Store.SetCapabilities(ctx, dev.ID, caps, string(caps.Class)); err != nil {
		t.Fatalf("record temporary capability proof: %v", err)
	}
	t.Logf("verified class C: %s (%s, target %s); credential: %s",
		caps.Board.Model, caps.Board.System, caps.Board.Target, target.credentialDescription)

	half := time.Duration(run.minutes) * time.Minute / 2
	t.Logf("running %d minute(s): %v idle, then %v focused", run.minutes, half, half)

	flashBefore := readFlash(t, ssh)
	resourceBefore := readResources(t, ssh)
	idleStart := time.Now()
	if err := d.StartCollector(ctx, collector.Options{Log: quietLogger()}); err != nil {
		t.Fatalf("StartCollector: %v", err)
	}
	d.StartMaintenance(ctx)
	time.Sleep(half)
	idleElapsed := time.Since(idleStart)
	resourceMid := readResources(t, ssh)
	idleO, ok := d.collectorRef().Overhead(dev.ID)
	if !ok {
		t.Fatal("no overhead recorded for a polled device")
	}
	flashMid := readFlash(t, ssh)
	resourceFocusStart := readResources(t, ssh)
	focusBase, ok := d.collectorRef().Overhead(dev.ID)
	if !ok {
		t.Fatal("device overhead disappeared before focused phase")
	}

	focusStart := time.Now()
	release := d.Focus(dev.ID)
	time.Sleep(half)
	focusElapsed := time.Since(focusStart)
	resourceAfter := readResources(t, ssh)
	focusO, ok := d.collectorRef().Overhead(dev.ID)
	release()
	if !ok {
		t.Fatal("focused device overhead disappeared")
	}
	flashAfter := readFlash(t, ssh)

	idleRPM := float64(idleO.Requests) / idleElapsed.Minutes()
	idlePPM := float64(idleO.Polls) / idleElapsed.Minutes()
	idleSPM := float64(idleO.Polls-idleO.Failures) / idleElapsed.Minutes()
	focusRequests := focusO.Requests - focusBase.Requests
	focusPolls := focusO.Polls - focusBase.Polls
	focusFailures := focusO.Failures - focusBase.Failures
	focusRPM := float64(focusRequests) / focusElapsed.Minutes()
	focusPPM := float64(focusPolls) / focusElapsed.Minutes()
	focusSPM := float64(focusPolls-focusFailures) / focusElapsed.Minutes()

	t.Logf("idle     : %.2f polls/min (%.2f successful), %d non-poll request(s) total "+
		"(%d requests in %v = %.2f/min raw)", idlePPM, idleSPM,
		idleO.NonPollRequests, idleO.Requests, idleElapsed.Round(time.Second), idleRPM)
	t.Logf("focused  : %.2f polls/min (%.2f successful); %d request(s) in %v = %.2f req/min",
		focusPPM, focusSPM, focusRequests, focusElapsed.Round(time.Second), focusRPM)
	t.Logf("polls    : %d total, %d failed", focusO.Polls, focusO.Failures)
	t.Logf("bytes out: %d (%.1f B/request)", focusO.BytesOut,
		float64(focusO.BytesOut)/float64(max64(focusO.Requests, 1)))
	logResources(t, "idle", resourceBefore, resourceMid)
	logResources(t, "focused", resourceFocusStart, resourceAfter)
	if flashBefore.writeCounter {
		t.Logf("flash    : %s endpoint snapshots plus cumulative %s write counters",
			flashBefore.hashAlgorithm, flashBefore.source)
	} else {
		t.Logf("flash    : %s endpoint snapshots; %s has no stock /proc/diskstats counter",
			flashBefore.hashAlgorithm, flashBefore.source)
	}

	if idleSPM < 0.90 || idlePPM > 1.05 {
		t.Errorf("idle cadence is outside the gate: %.2f successful polls/min (min 0.90), %.2f attempts/min (max 1.05)",
			idleSPM, idlePPM)
	}
	if focusSPM < 5.70 || focusRPM > 6.30 {
		t.Errorf("focused cadence is outside the gate: %.2f successful polls/min (min 5.70), %.2f req/min (max 6.30)",
			focusSPM, focusRPM)
	}
	if focusO.NonPollRequests > 5 {
		t.Errorf("%d requests were not polls across %d polls — something is calling outside the batch",
			focusO.NonPollRequests, focusO.Polls)
	}
	if focusRPM <= idleRPM {
		t.Errorf("focused rate %.2f is not above idle %.2f; focus did not engage", focusRPM, idleRPM)
	}

	assertNoFlashChange(t, "idle phase", flashBefore, flashMid)
	assertNoFlashChange(t, "focused phase", flashMid, flashAfter)

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
		return "  (snapshot records changed without an added or removed line)"
	}
	return strings.Join(out, "\n")
}
