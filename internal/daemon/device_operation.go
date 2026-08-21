package daemon

import (
	"context"
	"errors"
	"sync"
)

var errDeviceIdentityChanged = errors.New(
	"device now identifies differently after waiting; refusing a stale apply or operation")

// deviceOperationGate serialises destructive operations per device.
//
// A buffered channel is the mutex so waiting can select on ctx.Done. users
// includes the holder and waiters; the final release removes the map entry so a
// fleet that changes over time does not leave one lock behind per historical id.
type deviceOperationGate struct {
	mu      sync.Mutex
	entries map[int64]*deviceOperation
}

type deviceOperation struct {
	token      chan struct{}
	users      int
	generation uint64
}

func (g *deviceOperationGate) acquire(ctx context.Context, deviceID int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	g.mu.Lock()
	if g.entries == nil {
		g.entries = make(map[int64]*deviceOperation)
	}
	op := g.entries[deviceID]
	if op == nil {
		op = &deviceOperation{token: make(chan struct{}, 1)}
		g.entries[deviceID] = op
	}
	op.users++
	generation := op.generation
	g.mu.Unlock()

	select {
	case op.token <- struct{}{}:
		// If cancellation raced with admission, do not start a destructive
		// operation the caller has already abandoned.
		if err := ctx.Err(); err != nil {
			g.done(deviceID, op, true)
			return nil, err
		}
		g.mu.Lock()
		stale := op.generation != generation
		g.mu.Unlock()
		if stale {
			g.done(deviceID, op, true)
			return nil, errDeviceIdentityChanged
		}
	case <-ctx.Done():
		g.done(deviceID, op, false)
		return nil, ctx.Err()
	}

	var once sync.Once
	return func() {
		once.Do(func() { g.done(deviceID, op, true) })
	}, nil
}

// invalidate fences waiters captured before a device row was deleted. It must
// not remove the entry: the current un-adopt holder and every waiter retain the
// same pointer, and replacing it would allow two operations on one reusable ID
// to run concurrently.
func (g *deviceOperationGate) invalidate(deviceID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if op := g.entries[deviceID]; op != nil {
		op.generation++
	}
}

func (g *deviceOperationGate) done(deviceID int64, op *deviceOperation, held bool) {
	if held {
		<-op.token
	}
	g.mu.Lock()
	op.users--
	if op.users == 0 && g.entries[deviceID] == op {
		delete(g.entries, deviceID)
	}
	g.mu.Unlock()
}
