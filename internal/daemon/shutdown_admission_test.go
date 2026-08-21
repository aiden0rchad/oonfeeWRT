package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const queuedShutdownOperationID = "01962c09-7d62-7cd7-a1c2-450eba830894"

type heldApplyProvisioner struct {
	mu            sync.Mutex
	calls         int
	started       chan struct{}
	release       chan struct{}
	unexpectedRun chan struct{}
}

func (p *heldApplyProvisioner) Preview(context.Context) (*api.PreviewResult, error) {
	return &api.PreviewResult{}, nil
}

func (p *heldApplyProvisioner) ApplySite(context.Context, api.ApplyRequest) (*api.ApplyResult, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call != 1 {
		select {
		case p.unexpectedRun <- struct{}{}:
		default:
		}
		return nil, errors.New("queued Apply reached the provisioner during shutdown")
	}
	close(p.started)
	<-p.release
	return &api.ApplyResult{Devices: []api.DeviceApply{{
		DeviceID: 7, Name: "gateway", Outcome: "applied", Changes: 1,
	}}}, nil
}

func (p *heldApplyProvisioner) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type applyHTTPResult struct {
	status int
	body   []byte
	err    error
}

func setupShutdownClient(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client.Jar = jar
	resp, err := client.Post(base+"/api/v1/setup", "application/json",
		strings.NewReader(`{"username":"admin","password":"integration-test-password"}`))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup = %d: %s", resp.StatusCode, body)
	}
	var out struct {
		CSRF string `json:"csrf"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode setup: %v", err)
	}
	return out.CSRF
}

func postApplyForShutdown(client *http.Client, base, csrf, operationID string) applyHTTPResult {
	body := strings.NewReader(`{"operation_id":"` + operationID + `","preview_token":"pv-current"}`)
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/site/apply", body)
	if err != nil {
		return applyHTTPResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Oonfee-CSRF", csrf)
	resp, err := client.Do(req)
	if err != nil {
		return applyHTTPResult{err: err}
	}
	defer resp.Body.Close()
	blob, readErr := io.ReadAll(resp.Body)
	return applyHTTPResult{status: resp.StatusCode, body: blob, err: readErr}
}

func waitForApplyState(t *testing.T, db *store.DB, operationID string,
	want store.ApplyOperationState) *store.ApplyOperation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		op, err := db.ApplyOperation(context.Background(), operationID)
		if err == nil && op.State == want {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, err := db.ApplyOperation(context.Background(), operationID)
	t.Fatalf("operation %s = %+v, err=%v; want state %s", operationID, op, err, want)
	return nil
}

func TestShutdownRejectsQueuedApplyAndDrainsTerminalReceiptsBeforeDBClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testConfig(t, "pass")
	cfg.ShutdownGrace = 100 * time.Millisecond
	cfg.ApplyDrain = 2 * time.Second
	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()
	if resp, err := waitForHealthz(d.Addr()); err != nil {
		t.Fatalf("healthz: %v", err)
	} else {
		resp.Body.Close()
	}

	client := &http.Client{Timeout: 5 * time.Second}
	csrf := setupShutdownClient(t, client, "http://"+d.Addr())
	provision := &heldApplyProvisioner{
		started: make(chan struct{}), release: make(chan struct{}),
		unexpectedRun: make(chan struct{}, 1),
	}
	d.api.Provision = provision

	firstDone := make(chan applyHTTPResult, 1)
	go func() {
		firstDone <- postApplyForShutdown(client, "http://"+d.Addr(), csrf,
			daemonApplyOperationID)
	}()
	select {
	case <-provision.started:
	case <-time.After(time.Second):
		t.Fatal("first Apply did not start")
	}

	secondDone := make(chan applyHTTPResult, 1)
	go func() {
		secondDone <- postApplyForShutdown(client, "http://"+d.Addr(), csrf,
			queuedShutdownOperationID)
	}()
	waitForApplyState(t, d.Store, queuedShutdownOperationID, store.ApplyOperationQueued)
	if got := provision.callCount(); got != 1 {
		t.Fatalf("queued Apply reached provisioner before shutdown: calls=%d", got)
	}

	cancel()
	var second applyHTTPResult
	select {
	case second = <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("queued Apply was not woken by shutdown admission close")
	}
	if second.err != nil || second.status != http.StatusServiceUnavailable {
		t.Fatalf("queued Apply response = status %d err=%v body=%s",
			second.status, second.err, second.body)
	}
	var rejected map[string]any
	if err := json.Unmarshal(second.body, &rejected); err != nil {
		t.Fatalf("decode queued response: %v", err)
	}
	if rejected["write_state"] != store.ApplyWriteStateNone ||
		!strings.Contains(string(second.body), "nothing was written") {
		t.Fatalf("queued Apply response is not a definite no-write: %s", second.body)
	}
	queued := waitForApplyState(t, d.Store, queuedShutdownOperationID,
		store.ApplyOperationFailed)
	if queued.WriteState != store.ApplyWriteStateNone || queued.StartedAt != nil ||
		queued.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("queued durable receipt = %+v", queued)
	}
	select {
	case <-provision.unexpectedRun:
		t.Fatal("queued Apply started after shutdown began")
	default:
	}

	// Keep the first handler beyond HTTP Shutdown's grace. Serve must still wait
	// for its detached terminal receipt instead of closing SQLite underneath it.
	time.Sleep(cfg.ShutdownGrace + 50*time.Millisecond)
	select {
	case err := <-served:
		t.Fatalf("Serve returned before the accepted Apply finished: %v", err)
	default:
	}
	close(provision.release)
	select {
	case err := <-served:
		if err == nil || !strings.Contains(err.Error(), "HTTP did not drain") {
			t.Fatalf("Serve error after deliberate HTTP grace expiry = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not finish after the accepted Apply persisted")
	}
	<-firstDone // the connection may be closed; the durable receipt is authoritative.

	// A real daemon restart runs recovery. Both receipts must already be terminal,
	// so recovery preserves the applied first run and definite no-write second run.
	restarted, err := Open(context.Background(), cfg, quietLogger())
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer restarted.Close()
	first, err := restarted.Store.ApplyOperation(context.Background(), daemonApplyOperationID)
	if err != nil {
		t.Fatalf("first receipt after restart: %v", err)
	}
	if first.State != store.ApplyOperationCompleted ||
		first.WriteState != store.ApplyWriteStatePossible || first.StartedAt == nil {
		t.Fatalf("first receipt after restart = %+v", first)
	}
	secondAfterRestart, err := restarted.Store.ApplyOperation(context.Background(),
		queuedShutdownOperationID)
	if err != nil {
		t.Fatalf("queued receipt after restart: %v", err)
	}
	if secondAfterRestart.State != store.ApplyOperationFailed ||
		secondAfterRestart.WriteState != store.ApplyWriteStateNone ||
		secondAfterRestart.StartedAt != nil {
		t.Fatalf("queued receipt after restart = %+v", secondAfterRestart)
	}
	if got := provision.callCount(); got != 1 {
		t.Fatalf("provision calls = %d, want 1", got)
	}
}
