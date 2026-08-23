package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestOperationGateReportsStableBoundedConflicts(t *testing.T) {
	var gate operationGate
	releases := make([]func(), 0, 4)
	for _, kind := range []operationKind{
		operationDiagnostics, operationApply, operationRFScan, operationApply,
	} {
		release, err := gate.begin(kind)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}

	_, conflicts, err := gate.beginExclusive()
	if !errors.Is(err, errOperationAdmissionBusy) {
		t.Fatalf("exclusive error = %v", err)
	}
	want := []string{"apply", "rf_scan", "diagnostics"}
	if !reflect.DeepEqual(conflicts, want) {
		t.Fatalf("conflicts = %v, want %v", conflicts, want)
	}
	if len(conflicts) > int(operationKindCount) {
		t.Fatalf("conflicts are not bounded: %d", len(conflicts))
	}

	for _, release := range releases {
		release()
		release()
	}
	if !gate.wait(time.Second) {
		t.Fatal("idempotent releases did not drain the gate")
	}
}

func TestOperationGateExclusiveRejectsOrdinaryAndSecondExclusive(t *testing.T) {
	var gate operationGate
	release, conflicts, err := gate.beginExclusive()
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("exclusive admission = conflicts %v, error %v", conflicts, err)
	}
	if _, err := gate.begin(operationAdopt); !errors.Is(err, errOperationAdmissionExclusive) {
		t.Fatalf("ordinary admission error = %v", err)
	}
	if _, conflicts, err := gate.beginExclusive(); !errors.Is(err, errOperationAdmissionExclusive) ||
		!reflect.DeepEqual(conflicts, []string{restoreOperationName}) {
		t.Fatalf("second exclusive = conflicts %v, error %v", conflicts, err)
	}
	if gate.wait(time.Millisecond) {
		t.Fatal("exclusive holder reported idle")
	}
	release()
	release()
	if !gate.wait(time.Second) {
		t.Fatal("exclusive release did not drain the gate")
	}
}

func TestOperationGateBackupBlocksExclusiveUntilTerminalRelease(t *testing.T) {
	var gate operationGate
	release, err := gate.begin(operationBackup)
	if err != nil {
		t.Fatal(err)
	}
	if _, conflicts, err := gate.beginExclusive(); !errors.Is(err, errOperationAdmissionBusy) ||
		!reflect.DeepEqual(conflicts, []string{"backup"}) {
		t.Fatalf("exclusive during backup = conflicts %v, error %v", conflicts, err)
	}
	release()
	exclusive, conflicts, err := gate.beginExclusive()
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("exclusive after backup = conflicts %v, error %v", conflicts, err)
	}
	exclusive()
}

func TestOperationGateRestorePrepareUpgradeIsAtomic(t *testing.T) {
	var gate operationGate
	prepareRelease, err := gate.begin(operationRestorePrepare)
	if err != nil {
		t.Fatal(err)
	}
	backupRelease, err := gate.begin(operationBackup)
	if err != nil {
		t.Fatal(err)
	}
	if _, conflicts, err := gate.upgrade(operationRestorePrepare); !errors.Is(err, errOperationAdmissionBusy) || !reflect.DeepEqual(conflicts, []string{"backup"}) {
		t.Fatalf("failed upgrade conflicts=%v err=%v", conflicts, err)
	}
	if gate.active[operationRestorePrepare] != 1 {
		t.Fatalf("failed upgrade consumed prepare lease: %d", gate.active[operationRestorePrepare])
	}
	backupRelease()
	exclusiveRelease, conflicts, err := gate.upgrade(operationRestorePrepare)
	if err != nil || len(conflicts) != 0 || !gate.exclusive || gate.active[operationRestorePrepare] != 0 {
		t.Fatalf("successful upgrade exclusive=%v active=%d conflicts=%v err=%v",
			gate.exclusive, gate.active[operationRestorePrepare], conflicts, err)
	}
	if _, err := gate.begin(operationDiagnostics); !errors.Is(err, errOperationAdmissionExclusive) {
		t.Fatalf("upgraded exclusive admitted operation: %v", err)
	}
	exclusiveRelease()
	if !gate.wait(time.Second) {
		t.Fatal("upgraded lease did not drain")
	}
	_ = prepareRelease // consumed by the successful atomic upgrade.

	var suppressed operationGate
	release, err := suppressed.begin(operationRestorePrepare)
	if err != nil {
		t.Fatal(err)
	}
	suppressed.setSuppression(true)
	if _, _, err := suppressed.upgrade(operationRestorePrepare); !errors.Is(err, errOperationRouterSuppressed) ||
		suppressed.active[operationRestorePrepare] != 1 {
		t.Fatalf("suppressed upgrade err=%v active=%d", err, suppressed.active[operationRestorePrepare])
	}
	release()
}

func TestOperationGateSuppressionBlocksOnlyRouterWrites(t *testing.T) {
	var gate operationGate
	gate.setSuppression(true)
	for _, kind := range []operationKind{
		operationApply, operationAdopt, operationUnadopt, operationRFScan,
		operationCapability, operationNeighbourReconcile,
	} {
		if _, err := gate.begin(kind); !errors.Is(err, errOperationRouterSuppressed) {
			t.Fatalf("router operation %s error=%v", operationKindNames[kind], err)
		}
	}
	for _, kind := range []operationKind{
		operationSpeedTest, operationDiagnostics, operationBackup, operationRestorePrepare,
	} {
		release, err := gate.begin(kind)
		if err != nil {
			t.Fatalf("non-router operation %s error=%v", operationKindNames[kind], err)
		}
		release()
	}
	gate.setSuppression(false)
	release, err := gate.begin(operationApply)
	if err != nil {
		t.Fatalf("cleared suppression still blocked apply: %v", err)
	}
	release()
}

func TestOperationGateCloseNeverWaitsForHolders(t *testing.T) {
	for _, test := range []struct {
		name  string
		begin func(*operationGate) (func(), error)
	}{
		{name: "ordinary", begin: func(g *operationGate) (func(), error) {
			return g.begin(operationSpeedTest)
		}},
		{name: "exclusive", begin: func(g *operationGate) (func(), error) {
			release, _, err := g.beginExclusive()
			return release, err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gate operationGate
			release, err := test.begin(&gate)
			if err != nil {
				t.Fatal(err)
			}
			closed := make(chan struct{})
			go func() {
				gate.close()
				close(closed)
			}()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("close blocked on an active holder")
			}
			if _, err := gate.begin(operationApply); !errors.Is(err, errOperationAdmissionClosed) {
				t.Fatalf("ordinary admission after close = %v", err)
			}
			if _, _, err := gate.beginExclusive(); !errors.Is(err, errOperationAdmissionClosed) {
				t.Fatalf("exclusive admission after close = %v", err)
			}
			release()
			if !gate.wait(time.Second) {
				t.Fatal("closed gate did not drain")
			}
		})
	}
}

func TestOperationGateRejectsInvalidKindWithoutChangingState(t *testing.T) {
	var gate operationGate
	if _, err := gate.begin(operationKindCount); !errors.Is(err, errOperationKindInvalid) {
		t.Fatalf("invalid kind error = %v", err)
	}
	release, conflicts, err := gate.beginExclusive()
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("exclusive after invalid kind = conflicts %v, error %v", conflicts, err)
	}
	release()
}

func TestOperationGateOrdinaryAndExclusiveAdmissionAreAtomic(t *testing.T) {
	type result struct {
		kind      string
		release   func()
		conflicts []string
		err       error
	}
	for range 200 {
		var gate operationGate
		start := make(chan struct{})
		hold := make(chan struct{})
		results := make(chan result, 2)
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			release, err := gate.begin(operationApply)
			results <- result{kind: "ordinary", release: release, err: err}
			<-hold
			if release != nil {
				release()
			}
		}()
		go func() {
			defer workers.Done()
			<-start
			release, conflicts, err := gate.beginExclusive()
			results <- result{kind: "exclusive", release: release, conflicts: conflicts, err: err}
			<-hold
			if release != nil {
				release()
			}
		}()
		close(start)
		first, second := <-results, <-results
		close(hold)
		workers.Wait()
		winners := 0
		for _, got := range []result{first, second} {
			if got.err == nil {
				winners++
				continue
			}
			switch got.kind {
			case "ordinary":
				if !errors.Is(got.err, errOperationAdmissionExclusive) {
					t.Fatalf("ordinary loser error = %v", got.err)
				}
			case "exclusive":
				if !errors.Is(got.err, errOperationAdmissionBusy) ||
					!reflect.DeepEqual(got.conflicts, []string{"apply"}) {
					t.Fatalf("exclusive loser = conflicts %v, error %v", got.conflicts, got.err)
				}
			}
		}
		if winners != 1 {
			t.Fatalf("simultaneous admissions produced %d winners: %#v %#v", winners, first, second)
		}
		if !gate.wait(time.Second) {
			t.Fatal("race iteration did not drain")
		}
	}
}

func TestRestoreExclusiveBlocksOperationRoutesBeforeWorkStarts(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		body  any
		setup func(*harness) func() bool
	}{
		{"apply", "/api/v1/site/apply", applyOperationBody(testApplyOperationID, "pv-current"),
			func(h *harness) func() bool {
				p := &recordingProvisioner{}
				h.srv.Provision = p
				return func() bool { return applyCallCount(p) != 0 }
			}},
		{"adopt", "/api/v1/devices/adopt", map[string]any{
			"host": "192.0.2.1", "username": "root", "password": "secret",
			"acknowledge_router_changes": true,
		}, func(h *harness) func() bool {
			e := &stubEnroller{}
			h.srv.Enroll = e
			return func() bool { return len(e.adopted) != 0 }
		}},
		{"unadopt", "/api/v1/devices/7/unadopt", nil, func(h *harness) func() bool {
			e := &stubEnroller{}
			h.srv.Enroll = e
			return func() bool { return len(e.unadopt) != 0 }
		}},
		{"acl_refresh", "/api/v1/devices/7/refresh-acl", map[string]any{
			"username": "root", "password": "secret", "acknowledge_router_changes": true,
		}, func(h *harness) func() bool {
			e := &stubEnroller{}
			h.srv.Enroll = e
			return func() bool { return len(e.refreshed) != 0 }
		}},
		{"lldp", "/api/v1/devices/7/capabilities/lldp", map[string]any{
			"action": "diagnose", "username": "root", "password": "secret",
			"acknowledge_read_only_diagnostics": true,
		}, func(h *harness) func() bool {
			e := &stubEnroller{}
			h.srv.Enroll = e
			return func() bool { return len(e.lldp) != 0 }
		}},
		{"reprobe", "/api/v1/devices/7/reprobe", nil, func(h *harness) func() bool {
			r := &stubReprober{res: &ReprobeResult{DeviceID: 7, Unchanged: true}}
			h.srv.Reprobe = r
			return func() bool { return r.last != 0 }
		}},
		{"rf_scan", "/api/v1/devices/7/radios/radio0/scan",
			map[string]any{"acknowledge_disruption": true}, func(h *harness) func() bool {
				scanner := &radioScannerStub{}
				h.srv.RadioScan = scanner
				return func() bool { return scanner.calls != 0 }
			}},
		{"on_air", "/api/v1/site/verify-on-air", nil, func(h *harness) func() bool {
			calls := 0
			h.srv.OnAir = func(context.Context) (*OnAirResult, error) {
				calls++
				return &OnAirResult{}, nil
			}
			return func() bool { return calls != 0 }
		}},
		{"neighbour_reconcile", "/api/v1/roaming/neighbours", nil,
			func(h *harness) func() bool {
				calls := 0
				h.srv.Neighbours = func(context.Context) (*NeighbourResult, error) {
					calls++
					return &NeighbourResult{}, nil
				}
				return func() bool { return calls != 0 }
			}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			h.setup()
			called := test.setup(h)
			release, conflicts, err := h.srv.operations.beginExclusive()
			if err != nil || len(conflicts) != 0 {
				t.Fatalf("exclusive admission = conflicts %v, error %v", conflicts, err)
			}
			t.Cleanup(release)

			w := h.do(http.MethodPost, test.path, test.body)
			if w.Code != http.StatusServiceUnavailable || h.json(w)["code"] != "restore_in_progress" {
				t.Fatalf("blocked response = %d %s", w.Code, w.Body.String())
			}
			if called() {
				t.Fatal("restore-blocked request reached its collaborator")
			}
			if test.name == "apply" {
				if _, err := h.db.ApplyOperation(context.Background(), testApplyOperationID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("restore-blocked Apply created receipt: %v", err)
				}
			}
		})
	}
}

func TestOnAirHoldsRFScanLeaseThroughExecution(t *testing.T) {
	h := newHarness(t)
	h.setup()
	entered, finish := make(chan struct{}), make(chan struct{})
	h.srv.OnAir = func(context.Context) (*OnAirResult, error) {
		close(entered)
		<-finish
		return &OnAirResult{}, nil
	}
	done := make(chan int, 1)
	go func() {
		done <- h.do(http.MethodPost, "/api/v1/site/verify-on-air", nil).Code
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("on-air check did not start")
	}
	if _, conflicts, err := h.srv.operations.beginExclusive(); !errors.Is(err, errOperationAdmissionBusy) || !reflect.DeepEqual(conflicts, []string{"rf_scan"}) {
		t.Fatalf("exclusive during on-air check = conflicts %v, error %v", conflicts, err)
	}
	close(finish)
	select {
	case status := <-done:
		if status != http.StatusOK {
			t.Fatalf("on-air response status = %d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("on-air check did not finish")
	}
	release, conflicts, err := h.srv.operations.beginExclusive()
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("exclusive after on-air check = conflicts %v, error %v", conflicts, err)
	}
	release()
}
