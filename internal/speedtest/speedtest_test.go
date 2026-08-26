package speedtest

import (
	"context"
	"sync"
	"testing"
	"time"
)

type memoryRepo struct {
	mu               sync.Mutex
	job              *Job
	createMaxHistory int
	finishMaxHistory int
	finished         chan struct{}
}

func (r *memoryRepo) CreateSpeedTest(_ context.Context, job Job, maxHistory int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.job = &job
	r.createMaxHistory = maxHistory
	return nil
}

func (r *memoryRepo) SpeedTest(context.Context, string) (*Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.job == nil {
		return nil, ErrNotFound
	}
	job := *r.job
	return &job, nil
}

func (r *memoryRepo) SpeedTests(context.Context, int) ([]Job, error) { return nil, nil }

func (r *memoryRepo) ActiveSpeedTest(context.Context) (*Job, error) {
	return r.SpeedTest(context.Background(), "")
}

func (r *memoryRepo) StartSpeedTest(_ context.Context, _ string, at int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.job.State, r.job.Phase, r.job.StartedAt = "running", "idle-latency", &at
	return nil
}

func (r *memoryRepo) UpdateSpeedTestProgress(context.Context, string, Progress) error { return nil }
func (r *memoryRepo) RequestSpeedTestCancel(context.Context, string) error            { return nil }

func (r *memoryRepo) FinishSpeedTest(_ context.Context, _, state string, _ Measurement,
	detail string, at int64, maxHistory int) error {
	r.mu.Lock()
	r.job.State, r.job.FinishedAt = state, &at
	r.finishMaxHistory = maxHistory
	if detail != "" {
		r.job.Error = &detail
	}
	r.mu.Unlock()
	close(r.finished)
	return nil
}

type instantRunner struct{ descriptor Descriptor }

func (r instantRunner) Descriptor() Descriptor { return r.descriptor }
func (instantRunner) Run(context.Context, func(Progress)) (Measurement, error) {
	return Measurement{}, nil
}

func TestManagerCloseWithZeroTimeoutReportsIdle(t *testing.T) {
	manager := New(nil, nil, nil, nil)
	if !manager.Close(0) {
		t.Fatal("idle manager reported undrained with a zero timeout")
	}
}

func TestManagerClampsBackwardClock(t *testing.T) {
	d := Descriptor{Provider: "test", Method: "controller-http-single-stream-v1",
		Provenance: "controller-host", Endpoint: "http://127.0.0.1",
		DownloadEndpoint: "http://127.0.0.1/down", UploadEndpoint: "http://127.0.0.1/up",
		EstimatedBytes: 2, MaxDuration: time.Second}
	repo := &memoryRepo{finished: make(chan struct{})}
	manager := New(repo, instantRunner{descriptor: d}, nil, nil)
	times := []int64{1_000, 1, 2}
	manager.now = func() time.Time {
		at := times[0]
		times = times[1:]
		return time.UnixMilli(at)
	}
	job, err := manager.Start(context.Background(), true, d.PlanID(), 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if job.StartedAt == nil || job.CreatedAt != 1_000 || *job.StartedAt != job.CreatedAt {
		t.Fatalf("started job=%+v", job)
	}
	select {
	case <-repo.finished:
	case <-time.After(time.Second):
		t.Fatal("job did not finish")
	}
	stored, err := repo.SpeedTest(context.Background(), job.ID)
	if err != nil || stored.FinishedAt == nil || *stored.FinishedAt != job.CreatedAt {
		t.Fatalf("finished job=%+v err=%v", stored, err)
	}
	if repo.createMaxHistory != 3 || repo.finishMaxHistory != 3 {
		t.Fatalf("retention create=%d finish=%d, want 3", repo.createMaxHistory, repo.finishMaxHistory)
	}
	if !manager.Close(time.Second) {
		t.Fatal("manager did not drain")
	}
}
