package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
)

// Process owns the runtime passphrase across controlled in-process restarts.
// Each daemon cycle gets a disposable copy; Close clears the retained buffer.
type Process struct {
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	pass    []byte
	closed  bool
	acquire passphraseSource
}

func NewProcess(cfg Config, log *slog.Logger) *Process {
	return &Process{cfg: cfg, log: log, acquire: func(firstRun bool) ([]byte, error) {
		return passphraseFor(cfg, firstRun, os.Stdin, os.Stderr)
	}}
}

// Open binds the listener before acquiring the passphrase on the first cycle.
// Later cycles reuse the same process-owned passphrase without prompting or
// rereading its file.
func (p *Process) Open(ctx context.Context) (*Daemon, error) {
	if p == nil {
		return nil, errors.New("daemon: process lifecycle is nil")
	}
	return open(ctx, p.cfg, p.log, p.runtimePassphrase)
}

func (p *Process) runtimePassphrase(firstRun bool) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("daemon: process lifecycle is closed")
	}
	if len(p.pass) != 0 {
		return bytes.Clone(p.pass), nil
	}
	if p.acquire == nil {
		return nil, errors.New("daemon: runtime passphrase source is unavailable")
	}
	pass, err := p.acquire(firstRun)
	if err != nil {
		clear(pass)
		return nil, err
	}
	p.pass = bytes.Clone(pass)
	return pass, nil
}

// Close clears the only passphrase retained across daemon cycles.
func (p *Process) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	clear(p.pass)
	p.pass = nil
	p.closed = true
	p.mu.Unlock()
}
