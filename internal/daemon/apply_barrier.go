package daemon

import (
	"context"
	"sync"
	"time"
)

// applyBarrier counts applies that are currently running, so shutdown can wait
// for them rather than abandoning a device with a rollback timer armed.
//
// It is a counter and a broadcast rather than a sync.WaitGroup because Wait
// needs a deadline: waiting forever turns a stuck apply into a container that
// will not stop, and the supervisor's SIGKILL is a worse ending than a logged
// timeout that names what was still running.
type applyBarrier struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // created lazily by wait, closed when n reaches zero
}

type trackedApplyContextKey struct{}

// begin registers an apply and returns the function that ends it. The returned
// function is safe to call more than once, so it can be deferred and also called
// on an early return path without double-counting.
func (b *applyBarrier) begin() func() {
	b.mu.Lock()
	b.n++
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.n--
			if b.n == 0 && b.idle != nil {
				close(b.idle)
				b.idle = nil
			}
			b.mu.Unlock()
		})
	}
}

func (b *applyBarrier) inFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// wait blocks until no applies are running or d elapses, reporting whether the
// applies finished.
func (b *applyBarrier) wait(d time.Duration) bool {
	b.mu.Lock()
	if b.n == 0 {
		b.mu.Unlock()
		return true
	}
	if b.idle == nil {
		b.idle = make(chan struct{})
	}
	ch := b.idle
	b.mu.Unlock()

	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

// TrackApply runs fn as a counted apply, with a context deliberately detached
// from the caller's cancellation.
//
// Detaching is the point. An apply that has reached the APPLY step has armed a
// rollback on the device, and that timer expires whether this process is still
// interested or not — so cancelling the work because an HTTP client hung up, or
// because the daemon received SIGTERM, does not stop the change. It only removes
// the one party who could confirm it, converting a healthy change into a revert
// a minute later. Shutdown therefore waits for these instead of cancelling them.
//
// The detached context still carries a deadline of ApplyDrain, so a wedged apply
// cannot hold shutdown open indefinitely.
//
// deviceID quiesces that device's polling for the duration; pass 0 for work not
// scoped to a single device.
func (d *Daemon) TrackApply(ctx context.Context, deviceID int64, fn func(context.Context) error) error {
	end := d.applies.begin()
	defer end()

	// DEVICE-BUDGET §4.6: never poll during an apply. Wiring it here rather
	// than at each call site means an apply cannot forget — and forgetting
	// would mean a read landing between staged operations, seeing a config that
	// is neither the old one nor the new one.
	if deviceID != 0 {
		if c := d.collectorRef(); c != nil {
			defer c.Quiesce(deviceID)()
		}
	}

	base := context.WithoutCancel(ctx)
	var actx context.Context
	var cancel context.CancelFunc
	if tracked, _ := ctx.Value(trackedApplyContextKey{}).(bool); tracked {
		// A nested per-device tracker is part of the already-counted fleet run.
		// Keep the outer deadline; resetting ApplyDrain here would let each
		// device silently buy a fresh full drain budget.
		if deadline, ok := ctx.Deadline(); ok {
			actx, cancel = context.WithDeadline(base, deadline)
		} else {
			actx, cancel = context.WithTimeout(base, d.Config.ApplyDrain)
		}
	} else {
		// The first tracker intentionally ignores the caller's cancellation and
		// deadline: an armed router rollback outlives an HTTP request.
		actx, cancel = context.WithTimeout(base, d.Config.ApplyDrain)
	}
	defer cancel()
	actx = context.WithValue(actx, trackedApplyContextKey{}, true)
	return fn(actx)
}
