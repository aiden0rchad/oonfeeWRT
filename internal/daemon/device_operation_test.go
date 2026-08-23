package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/telemetry"
)

func waitForGateUsers(t *testing.T, g *deviceOperationGate, deviceID int64, want int) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		g.mu.Lock()
		got := 0
		if op := g.entries[deviceID]; op != nil {
			got = op.users
		}
		g.mu.Unlock()
		if got == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("device %d gate users = %d, want %d", deviceID, got, want)
		case <-tick.C:
		}
	}
}

func gateEntryCount(g *deviceOperationGate) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.entries)
}

func TestDeviceOperationGateSerialisesPerDeviceWithoutBlockingTheFleet(t *testing.T) {
	var g deviceOperationGate
	ctx := context.Background()
	releaseFirst, err := g.acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	type admission struct {
		release func()
		err     error
	}
	second := make(chan admission, 1)
	go func() {
		release, err := g.acquire(ctx, 1)
		second <- admission{release: release, err: err}
	}()
	waitForGateUsers(t, &g, 1, 2)
	select {
	case got := <-second:
		if got.release != nil {
			got.release()
		}
		t.Fatal("a second operation entered the same device before release")
	default:
	}

	// A separate router has a separate lock and is admitted immediately.
	releaseOther, err := g.acquire(ctx, 2)
	if err != nil {
		t.Fatalf("different device was blocked: %v", err)
	}
	releaseOther()

	releaseFirst()
	select {
	case got := <-second:
		if got.err != nil {
			t.Fatalf("queued operation: %v", got.err)
		}
		got.release()
	case <-time.After(2 * time.Second):
		t.Fatal("queued operation was not admitted after release")
	}
	if got := gateEntryCount(&g); got != 0 {
		t.Fatalf("%d device-operation gate entries leaked after release", got)
	}
}

func TestDeviceOperationGateCancellationDoesNotLeakOrHoldTheDevice(t *testing.T) {
	var g deviceOperationGate
	release, err := g.acquire(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := g.acquire(waitCtx, 7)
		result <- err
	}()
	waitForGateUsers(t, &g, 7, 2)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v, want context.Canceled", err)
	}
	waitForGateUsers(t, &g, 7, 1)
	release()
	if got := gateEntryCount(&g); got != 0 {
		t.Fatalf("%d gate entries leaked after cancellation", got)
	}

	// The canceled waiter did not strand a token or poison the next operation.
	next, err := g.acquire(context.Background(), 7)
	if err != nil {
		t.Fatalf("device stayed locked after cancellation: %v", err)
	}
	next()
}

func TestQueuedApplyRevalidatesIdentityAfterUnadopt(t *testing.T) {
	ctx := context.Background()
	d := openDaemon(t)
	blob, err := d.Keys.SealCredential("02:00:00:00:0e:01", "root", "good")
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1)
	dev := &store.Device{
		MAC: "02:00:00:00:0e:01", Host: "127.0.0.1:1", Name: "removed-first",
		Role: "ap", AdoptedAt: &at, CredEnc: blob, CapsJSON: `{"Class":"A"}`,
	}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	stale, err := d.Store.DeviceByID(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the operation slot as the un-adopt winner, delete the row, then reuse
	// its SQLite id for another device before the already-captured apply proceeds.
	// Reloading by id is not enough: it must revalidate the stable MAC too.
	releaseUnadopt, err := d.deviceOps.acquire(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	applied := make(chan api.DeviceApply, 1)
	go func() {
		applied <- d.applyDevice(ctx, model.Site{}, stale, false)
	}()
	waitForGateUsers(t, &d.deviceOps, dev.ID, 2)
	if err := d.deleteDevice(ctx, dev.ID); err != nil {
		releaseUnadopt()
		t.Fatal(err)
	}
	replacementBlob, err := d.Keys.SealCredential("02:00:00:00:0e:99", "root", "good")
	if err != nil {
		releaseUnadopt()
		t.Fatal(err)
	}
	replacement := &store.Device{
		MAC: "02:00:00:00:0e:99", Host: "127.0.0.1:1", Name: "replacement",
		Role: "ap", AdoptedAt: &at, CredEnc: replacementBlob, CapsJSON: `{"Class":"A"}`,
	}
	if err := d.Store.UpsertDevice(ctx, replacement); err != nil {
		releaseUnadopt()
		t.Fatal(err)
	}
	if replacement.ID != dev.ID {
		releaseUnadopt()
		t.Fatalf("fixture did not reuse device id: old=%d new=%d", dev.ID, replacement.ID)
	}
	releaseUnadopt()

	var got api.DeviceApply
	select {
	case got = <-applied:
	case <-time.After(2 * time.Second):
		t.Fatal("queued apply did not finish after un-adopt released the device")
	}
	if got.Outcome != "error" || !strings.Contains(got.Reason, "now identifies") ||
		!strings.Contains(got.Reason, "refusing a stale apply") {
		t.Fatalf("stale apply was not stopped by identity revalidation: %+v", got)
	}
	if n := d.applies.inFlight(); n != 0 {
		t.Fatalf("global apply barrier leaked %d operation(s)", n)
	}
	if got := gateEntryCount(&d.deviceOps); got != 0 {
		t.Fatalf("%d per-device gate entries leaked", got)
	}
}

func TestQueuedReprobeIsFencedWhenDeletedIDIsReused(t *testing.T) {
	ctx := context.Background()
	d := openDaemon(t)
	at := int64(1)
	oldMAC := "02:00:00:00:0e:21"
	oldCredential, err := d.Keys.SealCredential(oldMAC, "root", "old")
	if err != nil {
		t.Fatal(err)
	}
	old := &store.Device{MAC: oldMAC, Host: "127.0.0.1:1", Name: "old",
		Role: "ap", Scheme: "http", AdoptedAt: &at, CredEnc: oldCredential,
		CapsJSON: `{"Class":"A"}`}
	if err := d.Store.UpsertDevice(ctx, old); err != nil {
		t.Fatal(err)
	}

	// This holder represents un-adopt after it has admitted the deletion. The
	// reprobe joins the old generation and waits behind it.
	releaseDelete, err := d.deviceOps.acquire(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	reprobeDone := make(chan error, 1)
	go func() {
		_, err := d.Reprobe(ctx, old.ID)
		reprobeDone <- err
	}()
	waitForGateUsers(t, &d.deviceOps, old.ID, 2)

	if err := d.deleteDevice(ctx, old.ID); err != nil {
		releaseDelete()
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "replacement must not be contacted", http.StatusInternalServerError)
	}))
	defer server.Close()
	replacementMAC := "02:00:00:00:0e:22"
	replacementCredential, err := d.Keys.SealCredential(replacementMAC, "root", "new")
	if err != nil {
		releaseDelete()
		t.Fatal(err)
	}
	replacement := &store.Device{MAC: replacementMAC,
		Host: strings.TrimPrefix(server.URL, "http://"), Name: "replacement",
		Role: "ap", Scheme: "http", AdoptedAt: &at, CredEnc: replacementCredential,
		CapsJSON: `{"Class":"A"}`}
	if err := d.Store.UpsertDevice(ctx, replacement); err != nil {
		releaseDelete()
		t.Fatal(err)
	}
	if replacement.ID != old.ID {
		releaseDelete()
		t.Fatalf("fixture did not reuse device id: old=%d replacement=%d", old.ID, replacement.ID)
	}
	releaseDelete()

	select {
	case err := <-reprobeDone:
		if !errors.Is(err, errDeviceIdentityChanged) {
			t.Fatalf("queued reprobe error = %v, want identity fence", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued reprobe did not leave the gate")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("stale reprobe made %d request(s) to the replacement", got)
	}
	if got := gateEntryCount(&d.deviceOps); got != 0 {
		t.Fatalf("%d per-device gate entries leaked", got)
	}
}

func TestDeleteDevicePurgesTelemetryBeforeSQLiteReusesItsID(t *testing.T) {
	ctx := context.Background()
	d := openDaemon(t)
	at := int64(1)
	old := &store.Device{
		MAC: "02:00:00:00:0e:11", Host: "192.0.2.11", Name: "old",
		Role: "ap", AdoptedAt: &at,
	}
	if err := d.Store.UpsertDevice(ctx, old); err != nil {
		t.Fatal(err)
	}
	oldID := old.ID
	if err := d.Store.LogEvent(ctx, store.Event{DeviceID: &oldID,
		Category: "device", Severity: "info", Event: "old-device-history"}); err != nil {
		t.Fatal(err)
	}
	validFrom := time.Now().Add(-time.Minute).UnixMilli()
	edge := model.TopologyEdge{
		ChildNode: "device:02:00:00:00:0e:11", ParentNode: "synthetic:internet",
		Medium: "uplink", Confidence: "measured", ValidFrom: validFrom, LastSeen: validFrom,
		Evidence: []model.TopologyEvidence{}, Ambiguities: []string{},
	}
	if err := d.Store.SaveTopologyEdge(ctx, &edge); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Truncate(telemetry.DefaultWindow).Add(-telemetry.DefaultWindow)
	key := telemetry.SeriesKey{DeviceID: old.ID, Kind: telemetry.KindLoad1}
	d.Samples.Gauge(key, base.Add(time.Second).Unix(), 1)
	if err := d.deleteDevice(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	replacement := &store.Device{
		MAC: "02:00:00:00:0e:12", Host: "192.0.2.12", Name: "replacement",
		Role: "ap", AdoptedAt: &at,
	}
	if err := d.Store.UpsertDevice(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.ID != old.ID {
		t.Fatalf("fixture did not reuse device id: old=%d replacement=%d", old.ID, replacement.ID)
	}
	active, err := d.Store.TopologyEdgesAt(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("removed device retained active topology: %+v", active)
	}
	history, truncated, err := d.Store.TopologyEdgesBetween(
		ctx, validFrom, time.Now().Add(time.Minute).UnixMilli(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("single device history was unexpectedly truncated")
	}
	if len(history) != 1 || history[0].ValidTo == nil {
		t.Fatalf("removed device topology history was not closed: %+v", history)
	}
	events, err := d.Store.QueryEvents(ctx, "device", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != "old-device-history" || events[0].DeviceID != nil {
		t.Fatalf("replacement inherited removed device event provenance: %+v", events)
	}
	d.Samples.Gauge(key, base.Add(2*time.Second).Unix(), 99)
	m := telemetry.NewMaintainer(d.Store, d.Samples, quietLogger())
	m.Lifecycle = &d.telemetryLifecycle
	m.Now = func() time.Time { return base.Add(2 * telemetry.DefaultWindow) }
	m.Tick(ctx)
	got, err := d.Store.QuerySeries(ctx, replacement.ID, string(telemetry.KindLoad1), "",
		base, base.Add(3*telemetry.DefaultWindow))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Points) != 1 || got.Points[0].Avg != 99 || got.Points[0].Cnt != 1 {
		t.Fatalf("replacement inherited removed device telemetry: %+v", got.Points)
	}
}

func TestQueuedApplyHonorsCancellationAndRemainsInTheGlobalDrain(t *testing.T) {
	d := openDaemon(t)
	blob, err := d.Keys.SealCredential("02:00:00:00:0e:03", "root", "good")
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1)
	dev := &store.Device{
		MAC: "02:00:00:00:0e:03", Host: "127.0.0.1:1", Name: "cancel-wait",
		Role: "ap", AdoptedAt: &at, CredEnc: blob, CapsJSON: `{"Class":"A"}`,
	}
	if err := d.Store.UpsertDevice(context.Background(), dev); err != nil {
		t.Fatal(err)
	}

	releaseHolder, err := d.deviceOps.acquire(context.Background(), dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan api.DeviceApply, 1)
	go func() {
		finished <- d.applyDevice(ctx, model.Site{}, dev, false)
	}()
	waitForGateUsers(t, &d.deviceOps, dev.ID, 2)
	if n := d.applies.inFlight(); n != 1 {
		releaseHolder()
		t.Fatalf("queued apply is absent from global drain: inFlight=%d", n)
	}
	cancel()

	select {
	case got := <-finished:
		if got.Outcome != "error" || !strings.Contains(got.Reason, context.Canceled.Error()) {
			t.Fatalf("canceled queued apply = %+v", got)
		}
	case <-time.After(2 * time.Second):
		releaseHolder()
		t.Fatal("queued apply ignored cancellation")
	}
	if n := d.applies.inFlight(); n != 0 {
		releaseHolder()
		t.Fatalf("global drain leaked canceled apply: inFlight=%d", n)
	}
	waitForGateUsers(t, &d.deviceOps, dev.ID, 1)
	releaseHolder()
	if got := gateEntryCount(&d.deviceOps); got != 0 {
		t.Fatalf("%d per-device gate entries leaked after cancellation", got)
	}
}

func TestUnadoptWaitingForApplyReadsTheFinalOwnershipLedger(t *testing.T) {
	ctx := context.Background()
	addr := startMock(t)
	d := openDaemon(t)
	dev := seedAP(t, d, "02:00:00:00:0e:02", "apply-first", addr, capability.Present)

	// Hold the slot as an apply and queue the real Unadopt call behind it. The
	// claim is recorded while the slot is held, exactly as a converged or
	// confirmed apply does at its end.
	releaseApply, err := d.deviceOps.acquire(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	type unadoptResult struct {
		result *api.UnadoptResult
		err    error
	}
	finished := make(chan unadoptResult, 1)
	go func() {
		result, err := d.Unadopt(ctx, api.UnadoptRequest{DeviceID: dev.ID})
		finished <- unadoptResult{result: result, err: err}
	}()
	waitForGateUsers(t, &d.deviceOps, dev.ID, 2)
	if err := d.Store.RecordOwned(ctx, []store.OwnedSection{{
		DeviceID: dev.ID, Config: "wireless", Section: "oowrt_after_apply",
		RenderedHash: "final", AppliedAt: 1,
	}}); err != nil {
		releaseApply()
		t.Fatal(err)
	}
	releaseApply()

	var got unadoptResult
	select {
	case got = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("un-adopt did not resume after apply released the device")
	}
	if !errors.Is(got.err, api.ErrOperatorRequired) {
		t.Fatalf("un-adopt error = %v, want operator credential request", got.err)
	}
	if got.result == nil || got.result.RevertedSections != 1 {
		t.Fatalf("un-adopt did not read the apply's final ownership claim: %+v", got.result)
	}
	if got := gateEntryCount(&d.deviceOps); got != 0 {
		t.Fatalf("%d per-device gate entries leaked", got)
	}
}
