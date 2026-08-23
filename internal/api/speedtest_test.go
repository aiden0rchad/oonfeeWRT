package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/speedtest"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

type controlledSpeedRunner struct {
	started    chan struct{}
	release    chan struct{}
	once       sync.Once
	result     speedtest.Measurement
	descriptor *speedtest.Descriptor
}

type heldAfterCancelSpeedRunner struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *heldAfterCancelSpeedRunner) Descriptor() speedtest.Descriptor {
	return (&controlledSpeedRunner{}).Descriptor()
}

func (r *heldAfterCancelSpeedRunner) Run(_ context.Context,
	progress func(speedtest.Progress)) (speedtest.Measurement, error) {
	r.once.Do(func() { close(r.started) })
	progress(speedtest.Progress{Phase: "download", Percent: 25})
	<-r.release
	return speedtest.Measurement{}, nil
}

func (r *controlledSpeedRunner) Descriptor() speedtest.Descriptor {
	if r.descriptor != nil {
		return *r.descriptor
	}
	return speedtest.Descriptor{Provider: "local-test", Method: "controller-http-single-stream-v1",
		Provenance: "controller-host", Endpoint: "http://127.0.0.1",
		DownloadEndpoint: "http://127.0.0.1/down", UploadEndpoint: "http://127.0.0.1/up",
		EstimatedBytes: 3000, MaxDuration: 2 * time.Second}
}

func (r *controlledSpeedRunner) Run(ctx context.Context, progress func(speedtest.Progress)) (speedtest.Measurement, error) {
	r.once.Do(func() { close(r.started) })
	progress(speedtest.Progress{Phase: "download", Percent: 25, BytesDownloaded: 10})
	select {
	case <-r.release:
		return r.result, nil
	case <-ctx.Done():
		return speedtest.Measurement{BytesDownloaded: 10}, ctx.Err()
	}
}

func installSpeedRunner(h *harness, runner speedtest.Runner) {
	h.srv.SpeedTests = speedtest.New(h.db, runner,
		func(ctx context.Context, event, severity string, job speedtest.Job) error {
			return h.db.LogEvent(ctx, store.Event{Category: "audit", Severity: severity,
				Event: event, Detail: map[string]any{"job_id": job.ID}})
		}, quiet())
	h.mux = h.srv.Routes()
}

type speedFleetSpy struct{ calls atomic.Int64 }

func (f *speedFleetSpy) call()                             { f.calls.Add(1) }
func (f *speedFleetSpy) Focus(int64) func()                { f.call(); return func() {} }
func (f *speedFleetSpy) Tier(int64) (collector.Tier, bool) { f.call(); return "", false }
func (f *speedFleetSpy) Quiesced(int64) bool               { f.call(); return false }
func (f *speedFleetSpy) Overhead(int64) (collector.Overhead, bool) {
	f.call()
	return collector.Overhead{}, false
}
func (f *speedFleetSpy) Degraded(int64) ([]collector.Degradation, bool) { f.call(); return nil, false }
func (f *speedFleetSpy) Broadcasting(int64) ([]collector.AP, bool)      { f.call(); return nil, false }
func (f *speedFleetSpy) IfaceSections(int64) (map[string]string, bool)  { f.call(); return nil, false }
func (f *speedFleetSpy) IfaceModes(int64) (map[string]string, bool)     { f.call(); return nil, false }
func (f *speedFleetSpy) LiveClients(int64) (int, bool)                  { f.call(); return 0, false }
func (f *speedFleetSpy) LiveStations(int64) (collector.LiveStationSet, bool) {
	f.call()
	return nil, false
}
func (f *speedFleetSpy) LivePresence(int64) (collector.ClientPresenceState, bool) {
	f.call()
	return collector.ClientPresenceState{}, false
}

func TestSpeedTestAPIRequiresConsentAndNeverTouchesFleet(t *testing.T) {
	h := newHarness(t)
	h.setup()
	runner := &controlledSpeedRunner{started: make(chan struct{}), release: make(chan struct{})}
	installSpeedRunner(h, runner)
	spy := &speedFleetSpy{}
	h.srv.Fleet = spy

	list := h.do(http.MethodGet, "/api/v1/speedtests", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	body := h.json(list)
	test, ok := body["test"].(map[string]any)
	if !ok || test["provider"] != "local-test" || test["provenance"] != "controller-host" ||
		test["endpoint"] != "http://127.0.0.1" || test["download_endpoint"] != "http://127.0.0.1/down" ||
		test["upload_endpoint"] != "http://127.0.0.1/up" || test["max_duration_seconds"] != float64(2) {
		t.Fatalf("pre-consent descriptor=%v", body["test"])
	}
	planID, _ := test["plan_id"].(string)
	if !strings.HasPrefix(planID, "sha256:") {
		t.Fatalf("plan_id=%q", planID)
	}
	if body["active"] != nil {
		t.Fatalf("fresh active=%v", body["active"])
	}
	if denied := h.do(http.MethodPost, "/api/v1/speedtests",
		map[string]any{"acknowledge_data_use": false}); denied.Code != http.StatusBadRequest {
		t.Fatalf("unacknowledged: %d %s", denied.Code, denied.Body.String())
	}
	if missing := h.do(http.MethodPost, "/api/v1/speedtests",
		map[string]any{"acknowledge_data_use": true}); missing.Code != http.StatusBadRequest {
		t.Fatalf("missing plan: %d %s", missing.Code, missing.Body.String())
	}
	changedPlan := runner.Descriptor()
	changedPlan.UploadEndpoint += "?revision=2"
	runner.descriptor = &changedPlan
	if stale := h.do(http.MethodPost, "/api/v1/speedtests",
		map[string]any{"acknowledge_data_use": true, "plan_id": planID}); stale.Code != http.StatusConflict {
		t.Fatalf("stale plan: %d %s", stale.Code, stale.Body.String())
	}
	if active, err := h.db.ActiveSpeedTest(context.Background()); err != nil || active != nil {
		t.Fatalf("stale consent created job=%+v err=%v", active, err)
	}
	select {
	case <-runner.started:
		t.Fatal("stale consent invoked runner")
	default:
	}
	fresh := h.json(h.do(http.MethodGet, "/api/v1/speedtests", nil))["test"].(map[string]any)
	freshPlanID := fresh["plan_id"].(string)
	if freshPlanID == planID {
		t.Fatal("descriptor change did not invalidate plan_id")
	}
	started := h.do(http.MethodPost, "/api/v1/speedtests",
		map[string]any{"acknowledge_data_use": true, "plan_id": freshPlanID})
	if started.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", started.Code, started.Body.String())
	}
	job := h.json(started)
	id, _ := job["id"].(string)
	if id == "" || job["state"] != "running" {
		t.Fatalf("job=%v", job)
	}
	<-runner.started
	conflict := h.do(http.MethodPost, "/api/v1/speedtests",
		map[string]any{"acknowledge_data_use": true, "plan_id": freshPlanID})
	if conflict.Code != http.StatusConflict || h.json(conflict)["active"] == nil {
		t.Fatalf("conflict: %d %s", conflict.Code, conflict.Body.String())
	}
	cancelled := h.do(http.MethodPost, "/api/v1/speedtests/"+id+"/cancel", map[string]any{})
	cancelState := h.json(cancelled)["state"]
	if cancelled.Code != http.StatusAccepted || (cancelState != "cancelling" && cancelState != "failed") {
		t.Fatalf("cancel: %d %s", cancelled.Code, cancelled.Body.String())
	}
	got := waitSpeedJob(t, h.db, id, "failed")
	if got.Error == nil || *got.Error != "cancelled by operator" {
		t.Fatalf("cancelled job=%+v", got)
	}
	if !h.srv.WaitForOperations(2 * time.Second) {
		t.Fatalf("terminal job did not release lease: %v", h.srv.ActiveOperations())
	}
	if spy.calls.Load() != 0 {
		t.Fatalf("controller speed test made %d Fleet call(s)", spy.calls.Load())
	}
	events, err := h.db.RecentEvents(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"speedtest.start": false, "speedtest.cancel": false, "speedtest.cancelled": false}
	for _, event := range events {
		if _, ok := want[event.Event]; ok {
			want[event.Event] = true
		}
		if event.Event == "speedtest.failure" {
			t.Fatal("operator cancellation was audited as a system failure")
		}
	}
	for event, found := range want {
		if !found {
			t.Errorf("missing audit event %s", event)
		}
	}
}

func TestSpeedTestAPICompletesWithNullableLoadedMetrics(t *testing.T) {
	h := newHarness(t)
	h.setup()
	download, upload, latency, jitter := 100.5, 20.25, 8.1, 1.2
	runner := &controlledSpeedRunner{started: make(chan struct{}), release: make(chan struct{}),
		result: speedtest.Measurement{DownloadMbps: &download, UploadMbps: &upload,
			IdleLatencyMS: &latency, IdleJitterMS: &jitter,
			BytesDownloaded: 2000, BytesUploaded: 1000}}
	close(runner.release)
	installSpeedRunner(h, runner)
	started := h.do(http.MethodPost, "/api/v1/speedtests",
		map[string]any{"acknowledge_data_use": true, "plan_id": runner.Descriptor().PlanID()})
	id := h.json(started)["id"].(string)
	got := waitSpeedJob(t, h.db, id, "completed")
	if got.DownloadMbps == nil || *got.DownloadMbps != download || got.LoadedLatencyMS != nil ||
		got.LoadedJitterMS != nil || got.ProgressPercent != 100 {
		t.Fatalf("completed=%+v", got)
	}
	res := h.do(http.MethodGet, "/api/v1/speedtests/"+id, nil)
	if res.Code != http.StatusOK || h.json(res)["loaded_latency_ms"] != nil {
		t.Fatalf("status: %d %s", res.Code, res.Body.String())
	}
}

func TestSpeedTestOwnsOperationLeaseThroughCancelAndTerminalAudit(t *testing.T) {
	h := newHarness(t)
	h.setup()
	runner := &heldAfterCancelSpeedRunner{started: make(chan struct{}), release: make(chan struct{})}
	installSpeedRunner(h, runner)

	exclusive, _, err := h.srv.operations.beginExclusive()
	if err != nil {
		t.Fatal(err)
	}
	blocked := h.do(http.MethodPost, "/api/v1/speedtests", map[string]any{
		"acknowledge_data_use": true, "plan_id": runner.Descriptor().PlanID(),
	})
	if blocked.Code != http.StatusServiceUnavailable || h.json(blocked)["code"] != "restore_in_progress" {
		t.Fatalf("blocked start = %d %s", blocked.Code, blocked.Body.String())
	}
	if active, err := h.db.ActiveSpeedTest(context.Background()); err != nil || active != nil {
		t.Fatalf("restore-blocked start created job=%+v err=%v", active, err)
	}
	exclusive()

	started := h.do(http.MethodPost, "/api/v1/speedtests", map[string]any{
		"acknowledge_data_use": true, "plan_id": runner.Descriptor().PlanID(),
	})
	if started.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", started.Code, started.Body.String())
	}
	id := h.json(started)["id"].(string)
	<-runner.started
	if got := h.srv.ActiveOperations(); len(got) != 1 || got[0] != "speed_test" {
		t.Fatalf("active operations = %v", got)
	}
	if _, conflicts, err := h.srv.operations.beginExclusive(); !errors.Is(err, errOperationAdmissionBusy) || len(conflicts) != 1 || conflicts[0] != "speed_test" {
		t.Fatalf("active job exclusive = conflicts %v, error %v", conflicts, err)
	}
	cancelled := h.do(http.MethodPost, "/api/v1/speedtests/"+id+"/cancel", map[string]any{})
	if cancelled.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d %s", cancelled.Code, cancelled.Body.String())
	}
	if got := h.srv.ActiveOperations(); len(got) != 1 || got[0] != "speed_test" {
		t.Fatalf("cancel response released job lease: %v", got)
	}
	close(runner.release)
	if !h.srv.WaitForOperations(2 * time.Second) {
		t.Fatalf("terminal job did not release lease: %v", h.srv.ActiveOperations())
	}
	job := waitSpeedJob(t, h.db, id, "failed")
	if job.Error == nil || *job.Error != "cancelled by operator" {
		t.Fatalf("terminal job = %+v", job)
	}
	events, err := h.db.RecentEvents(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		found = found || event.Event == "speedtest.cancelled"
	}
	if !found {
		t.Fatal("operation lease released before terminal audit")
	}
}

type cancelOnMutationRepo struct {
	*store.DB
	cancel context.CancelFunc
}

type cancelOnCancelRepo struct {
	*store.DB
	cancel context.CancelFunc
}

type gatedStartRepo struct {
	*store.DB
	entered chan struct{}
	release chan struct{}
}

func (r *gatedStartRepo) StartSpeedTest(ctx context.Context, id string, at int64) error {
	close(r.entered)
	<-r.release
	return r.DB.StartSpeedTest(ctx, id, at)
}

func TestImmediateCancelCannotLeaveQueuedJobActive(t *testing.T) {
	h := newHarness(t)
	repo := &gatedStartRepo{DB: h.db, entered: make(chan struct{}), release: make(chan struct{})}
	runner := &controlledSpeedRunner{started: make(chan struct{}), release: make(chan struct{})}
	audits := make(chan string, 4)
	manager := speedtest.New(repo, runner, func(_ context.Context, event, _ string, _ speedtest.Job) error {
		audits <- event
		return nil
	}, quiet())
	startErr := make(chan error, 1)
	go func() {
		_, err := manager.Start(context.Background(), true, runner.Descriptor().PlanID(), 1, "admin")
		startErr <- err
	}()
	<-repo.entered
	active, err := h.db.ActiveSpeedTest(context.Background())
	if err != nil || active == nil || active.State != "queued" {
		t.Fatalf("queued=%+v err=%v", active, err)
	}
	cancelDone := make(chan error, 1)
	go func() {
		_, err := manager.Cancel(context.Background(), active.ID)
		cancelDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		job, _ := h.db.SpeedTest(context.Background(), active.ID)
		if job != nil && job.State == "cancelling" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancel did not persist while start was gated")
		}
		time.Sleep(time.Millisecond)
	}
	close(repo.release)
	if err := <-startErr; !errors.Is(err, speedtest.ErrTerminal) {
		t.Fatalf("start error=%v", err)
	}
	if err := <-cancelDone; err != nil {
		t.Fatalf("cancel error=%v", err)
	}
	got := waitSpeedJob(t, h.db, active.ID, "failed")
	if got.Error == nil || !strings.Contains(*got.Error, "could not begin speed test") {
		t.Fatalf("terminal job=%+v", got)
	}
	if active, err := h.db.ActiveSpeedTest(context.Background()); err != nil || active != nil {
		t.Fatalf("active after race=%+v err=%v", active, err)
	}
	foundFailure := false
	for len(audits) > 0 {
		if <-audits == "speedtest.failure" {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatal("cancel/start race was not audited as a failed start")
	}
}

func (r *cancelOnMutationRepo) StartSpeedTest(ctx context.Context, id string, at int64) error {
	err := r.DB.StartSpeedTest(ctx, id, at)
	r.cancel()
	return err
}

func (r *cancelOnCancelRepo) RequestSpeedTestCancel(ctx context.Context, id string) error {
	err := r.DB.RequestSpeedTestCancel(ctx, id)
	r.cancel()
	return err
}

func TestSpeedTestAuditSurvivesCancelledRequestContextAndCloseDrains(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	repo := &cancelOnMutationRepo{DB: h.db, cancel: cancel}
	runner := &controlledSpeedRunner{started: make(chan struct{}), release: make(chan struct{})}
	audited := make(chan error, 1)
	manager := speedtest.New(repo, runner, func(ctx context.Context, _, _ string, _ speedtest.Job) error {
		audited <- ctx.Err()
		return nil
	}, quiet())
	job, err := manager.Start(ctx, true, runner.Descriptor().PlanID(), 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := <-audited; err != nil {
		t.Fatalf("audit inherited cancelled request context: %v", err)
	}
	if !manager.Close(time.Second) {
		t.Fatal("manager did not drain cancelled runner")
	}
	got, err := h.db.SpeedTest(context.Background(), job.ID)
	if err != nil || got.State != "failed" || got.Error == nil || *got.Error != "controller shutting down" {
		t.Fatalf("shutdown job=%+v err=%v", got, err)
	}
}

func TestSpeedTestCancelAuditSurvivesCancelledRequestContext(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	repo := &cancelOnCancelRepo{DB: h.db, cancel: cancel}
	runner := &controlledSpeedRunner{started: make(chan struct{}), release: make(chan struct{})}
	type auditResult struct {
		event string
		err   error
	}
	audited := make(chan auditResult, 4)
	manager := speedtest.New(repo, runner,
		func(ctx context.Context, event, _ string, _ speedtest.Job) error {
			audited <- auditResult{event: event, err: ctx.Err()}
			return nil
		}, quiet())
	job, err := manager.Start(context.Background(), true, runner.Descriptor().PlanID(), 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	if _, err := manager.Cancel(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case got := <-audited:
			if got.event == "speedtest.cancel" {
				if got.err != nil {
					t.Fatalf("cancel audit inherited request context: %v", got.err)
				}
				if !manager.Close(time.Second) {
					t.Fatal("manager did not drain")
				}
				return
			}
		case <-deadline:
			t.Fatal("missing speedtest.cancel audit")
		}
	}
}

func waitSpeedJob(t *testing.T, db *store.DB, id, state string) *speedtest.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := db.SpeedTest(context.Background(), id)
		if err == nil && job.State == state {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	job, err := db.SpeedTest(context.Background(), id)
	t.Fatalf("job %s state=%+v err=%v, want %s", id, job, err, state)
	return nil
}
