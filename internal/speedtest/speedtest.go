// Package speedtest runs explicit, bounded tests from the controller host.
// It has no router or fleet dependency by design.
package speedtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const MaxHistory = 50

var (
	ErrActive      = errors.New("a speed test is already active")
	ErrNotFound    = errors.New("speed test not found")
	ErrTerminal    = errors.New("speed test is already finished")
	ErrPlanChanged = errors.New("speed test plan changed; review it and acknowledge again")
)

type Descriptor struct {
	Provider         string        `json:"provider"`
	Method           string        `json:"method"`
	Provenance       string        `json:"provenance"`
	Endpoint         string        `json:"endpoint"`
	DownloadEndpoint string        `json:"download_endpoint"`
	UploadEndpoint   string        `json:"upload_endpoint"`
	EstimatedBytes   int64         `json:"estimated_bytes"`
	MaxDuration      time.Duration `json:"-"`
}

func (d Descriptor) PlanID() string {
	encoded, _ := json.Marshal(struct {
		Provider, Method, Provenance, Endpoint, DownloadEndpoint, UploadEndpoint string
		EstimatedBytes, MaxDurationNanoseconds                                   int64
	}{d.Provider, d.Method, d.Provenance, d.Endpoint, d.DownloadEndpoint, d.UploadEndpoint,
		d.EstimatedBytes, int64(d.MaxDuration)})
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type Measurement struct {
	DownloadMbps    *float64
	UploadMbps      *float64
	IdleLatencyMS   *float64
	IdleJitterMS    *float64
	LoadedLatencyMS *float64
	LoadedJitterMS  *float64
	BytesDownloaded int64
	BytesUploaded   int64
}

type Progress struct {
	Phase           string
	Percent         int
	BytesDownloaded int64
	BytesUploaded   int64
}

type Job struct {
	ID              string   `json:"id"`
	State           string   `json:"state"`
	Phase           string   `json:"phase"`
	ProgressPercent int      `json:"progress_percent"`
	Provider        string   `json:"provider"`
	Method          string   `json:"method"`
	Provenance      string   `json:"provenance"`
	Endpoint        string   `json:"endpoint"`
	EstimatedBytes  int64    `json:"estimated_bytes"`
	PlanID          string   `json:"plan_id"`
	ActorAdminID    int64    `json:"-"`
	ActorUsername   string   `json:"-"`
	CreatedAt       int64    `json:"created_at"`
	StartedAt       *int64   `json:"started_at"`
	FinishedAt      *int64   `json:"finished_at"`
	DownloadMbps    *float64 `json:"download_mbps"`
	UploadMbps      *float64 `json:"upload_mbps"`
	IdleLatencyMS   *float64 `json:"idle_latency_ms"`
	IdleJitterMS    *float64 `json:"idle_jitter_ms"`
	LoadedLatencyMS *float64 `json:"loaded_latency_ms"`
	LoadedJitterMS  *float64 `json:"loaded_jitter_ms"`
	BytesDownloaded int64    `json:"bytes_downloaded"`
	BytesUploaded   int64    `json:"bytes_uploaded"`
	Error           *string  `json:"error"`
}

type Repository interface {
	CreateSpeedTest(context.Context, Job, int) error
	SpeedTest(context.Context, string) (*Job, error)
	SpeedTests(context.Context, int) ([]Job, error)
	ActiveSpeedTest(context.Context) (*Job, error)
	StartSpeedTest(context.Context, string, int64) error
	UpdateSpeedTestProgress(context.Context, string, Progress) error
	RequestSpeedTestCancel(context.Context, string) error
	FinishSpeedTest(context.Context, string, string, Measurement, string, int64, int) error
}

type Runner interface {
	Descriptor() Descriptor
	Run(context.Context, func(Progress)) (Measurement, error)
}

type AuditFunc func(context.Context, string, string, Job) error

type Manager struct {
	repo   Repository
	runner Runner
	audit  AuditFunc
	log    *slog.Logger
	now    func() time.Time

	mu            sync.Mutex
	cancels       map[string]context.CancelFunc
	cancelReasons map[string]string
	closing       bool
	running       int
	wg            sync.WaitGroup
}

func New(repo Repository, runner Runner, audit AuditFunc, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{repo: repo, runner: runner, audit: audit, log: log,
		now: time.Now, cancels: make(map[string]context.CancelFunc),
		cancelReasons: make(map[string]string)}
}

func (m *Manager) Descriptor() Descriptor { return m.runner.Descriptor() }

func (m *Manager) Start(ctx context.Context, acknowledged bool, reviewedPlanID string,
	actorID int64, username string) (*Job, error) {
	return m.start(ctx, acknowledged, reviewedPlanID, actorID, username, nil)
}

// StartWithCompletion transfers complete to the manager only on success. The
// callback runs after terminal persistence, audit and in-memory cleanup.
func (m *Manager) StartWithCompletion(ctx context.Context, acknowledged bool, reviewedPlanID string,
	actorID int64, username string, complete func()) (*Job, error) {
	return m.start(ctx, acknowledged, reviewedPlanID, actorID, username, complete)
}

func (m *Manager) start(ctx context.Context, acknowledged bool, reviewedPlanID string,
	actorID int64, username string, complete func()) (*Job, error) {
	if !acknowledged {
		return nil, errors.New("data-use acknowledgement is required")
	}
	d := m.runner.Descriptor()
	if d.Provider == "" || d.Method == "" || d.Provenance != "controller-host" ||
		d.Endpoint == "" || d.DownloadEndpoint == "" || d.UploadEndpoint == "" ||
		d.EstimatedBytes <= 0 || d.MaxDuration <= 0 {
		return nil, errors.New("speed test runner has an invalid descriptor")
	}
	planID := d.PlanID()
	if reviewedPlanID != planID {
		return nil, ErrPlanChanged
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return nil, errors.New("speed test service is shutting down")
	}
	id, err := newID()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	job := Job{ID: id, State: "queued", Phase: "queued", Provider: d.Provider,
		Method: d.Method, Provenance: d.Provenance, Endpoint: d.Endpoint,
		EstimatedBytes: d.EstimatedBytes, PlanID: planID, ActorAdminID: actorID,
		ActorUsername: username, CreatedAt: m.now().UnixMilli()}
	if err := m.repo.CreateSpeedTest(ctx, job, MaxHistory); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(context.Background(), d.MaxDuration)
	m.cancels[id] = cancel
	started := atLeast(m.now().UnixMilli(), job.CreatedAt)
	if err := m.repo.StartSpeedTest(ctx, id, started); err != nil {
		cancel()
		delete(m.cancels, id)
		m.mu.Unlock()
		if finishErr := m.repo.FinishSpeedTest(context.Background(), id, "failed", Measurement{},
			"could not begin speed test: "+err.Error(),
			atLeast(m.now().UnixMilli(), job.CreatedAt), MaxHistory); finishErr == nil {
			if stored, readErr := m.repo.SpeedTest(context.Background(), id); readErr == nil {
				m.emitBackground("speedtest.failure", "error", *stored)
			}
		}
		return nil, err
	}
	job.State, job.Phase, job.ProgressPercent = "running", "idle-latency", 1
	job.StartedAt = &started
	m.running++
	m.wg.Add(1)
	m.mu.Unlock()

	m.emitBackground("speedtest.start", "info", job)
	go m.run(runCtx, job, complete)
	return &job, nil
}

func (m *Manager) run(ctx context.Context, job Job, complete func()) {
	defer func() {
		m.mu.Lock()
		m.running--
		m.mu.Unlock()
		m.wg.Done()
	}()
	if complete != nil {
		defer complete()
	}
	defer func() {
		m.mu.Lock()
		delete(m.cancels, job.ID)
		delete(m.cancelReasons, job.ID)
		m.mu.Unlock()
	}()
	result, err := m.runner.Run(ctx, func(p Progress) {
		if p.Percent < 0 {
			p.Percent = 0
		} else if p.Percent > 99 {
			p.Percent = 99
		}
		if updateErr := m.repo.UpdateSpeedTestProgress(context.Background(), job.ID, p); updateErr != nil &&
			!errors.Is(updateErr, ErrTerminal) {
			m.log.Warn("could not persist speed-test progress", "job", job.ID, "err", updateErr)
		}
	})
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if ctx.Err() != nil {
		m.mu.Lock()
		reason := m.cancelReasons[job.ID]
		m.mu.Unlock()
		if reason != "" {
			err = errors.New(reason)
		}
	}
	m.finish(job, result, err)
}

func (m *Manager) finish(job Job, result Measurement, runErr error) {
	state, detail, severity, event := "completed", "", "info", "speedtest.complete"
	if runErr != nil {
		state, detail, severity, event = "failed", runErr.Error(), "error", "speedtest.failure"
		if detail == "cancelled by operator" {
			severity, event = "warning", "speedtest.cancelled"
		}
	}
	floors := []int64{job.CreatedAt}
	if job.StartedAt != nil {
		floors = append(floors, *job.StartedAt)
	}
	finished := atLeast(m.now().UnixMilli(), floors...)
	if err := m.repo.FinishSpeedTest(context.Background(), job.ID, state, result, detail,
		finished, MaxHistory); err != nil {
		m.log.Error("could not persist speed-test outcome", "job", job.ID, "err", err)
		return
	}
	if stored, err := m.repo.SpeedTest(context.Background(), job.ID); err == nil && stored != nil {
		m.emitBackground(event, severity, *stored)
	}
}

func (m *Manager) Cancel(ctx context.Context, id string) (*Job, error) {
	if err := m.repo.RequestSpeedTestCancel(ctx, id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	cancel := m.cancels[id]
	if cancel != nil {
		m.cancelReasons[id] = "cancelled by operator"
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	job, err := m.repo.SpeedTest(readCtx, id)
	if err == nil && job != nil {
		m.emitBackground("speedtest.cancel", "warning", *job)
	}
	return job, err
}

func (m *Manager) Job(ctx context.Context, id string) (*Job, error) {
	return m.repo.SpeedTest(ctx, id)
}

func (m *Manager) List(ctx context.Context, limit int) ([]Job, *Job, error) {
	jobs, err := m.repo.SpeedTests(ctx, limit)
	if err != nil {
		return nil, nil, err
	}
	active, err := m.repo.ActiveSpeedTest(ctx)
	return jobs, active, err
}

// Close cancels all active work and waits up to timeout. A false result means
// the database must remain open: a runner goroutine may still persist outcome.
func (m *Manager) Close(timeout time.Duration) bool {
	m.mu.Lock()
	m.closing = true
	for id, cancel := range m.cancels {
		// Close is lifecycle cancellation, not an operator result.
		m.cancelReasons[id] = "controller shutting down"
		cancel()
	}
	idle := m.running == 0
	m.mu.Unlock()
	if idle {
		return true
	}
	if timeout <= 0 {
		return false
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (m *Manager) emit(ctx context.Context, event, severity string, job Job) {
	if m.audit != nil {
		if err := m.audit(ctx, event, severity, job); err != nil {
			m.log.Warn("could not write speed-test audit event", "event", event,
				"job", job.ID, "err", err)
		}
	}
}

func (m *Manager) emitBackground(event, severity string, job Job) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m.emit(ctx, event, severity, job)
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate speed-test id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func atLeast(value int64, floors ...int64) int64 {
	for _, floor := range floors {
		if value < floor {
			value = floor
		}
	}
	return value
}
