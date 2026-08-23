package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	controllerLogName      = "controller.jsonl"
	controllerLogMaxBytes  = 2 << 20
	controllerLogBackups   = 3
	controllerLogMaxRecord = 64 << 10
)

// mirroredHandler keeps the operator's existing logger while writing the same
// accepted records as JSON for a future diagnostics bundle.
type mirroredHandler struct {
	primary   slog.Handler
	secondary slog.Handler
}

func (h mirroredHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.primary.Enabled(ctx, level)
}

func (h mirroredHandler) Handle(ctx context.Context, record slog.Record) error {
	return errors.Join(h.primary.Handle(ctx, record), h.secondary.Handle(ctx, record))
}

func (h mirroredHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return mirroredHandler{h.primary.WithAttrs(attrs), h.secondary.WithAttrs(attrs)}
}

func (h mirroredHandler) WithGroup(name string) slog.Handler {
	return mirroredHandler{h.primary.WithGroup(name), h.secondary.WithGroup(name)}
}

type rotatingLog struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	size     int64
	maxBytes int64
	backups  int
}

func openControllerLog(dataDir string) (*rotatingLog, error) {
	path := filepath.Join(dataDir, controllerLogName)
	for i := 0; i <= controllerLogBackups; i++ {
		candidate := path
		if i > 0 {
			candidate += fmt.Sprintf(".%d", i)
		}
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect controller log: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("controller log %s is not a regular file", candidate)
		}
		if err := os.Chmod(candidate, 0o600); err != nil {
			return nil, fmt.Errorf("secure controller log %s: %w", candidate, err)
		}
	}
	r := &rotatingLog{path: path, maxBytes: controllerLogMaxBytes, backups: controllerLogBackups}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingLog) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open controller log: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("secure controller log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("inspect controller log size: %w", err)
	}
	r.file, r.size = f, info.Size()
	return nil
}

func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return 0, os.ErrClosed
	}
	originalLen := len(p)
	omitted := false
	if len(p) > controllerLogMaxRecord {
		p = []byte(fmt.Sprintf("{\"time\":%q,\"level\":\"WARN\",\"msg\":\"controller log record omitted: too large\"}\n", time.Now().UTC().Format(time.RFC3339Nano)))
		omitted = true
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	if err == nil && omitted {
		return originalLen, nil
	}
	return n, err
}

func (r *rotatingLog) rotate() error {
	if err := r.file.Close(); err != nil {
		return fmt.Errorf("close controller log for rotation: %w", err)
	}
	r.file = nil
	if err := os.Remove(r.path + fmt.Sprintf(".%d", r.backups)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove oldest controller log: %w", err)
	}
	for i := r.backups - 1; i >= 1; i-- {
		from, to := r.path+fmt.Sprintf(".%d", i), r.path+fmt.Sprintf(".%d", i+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate controller log: %w", err)
		}
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate controller log: %w", err)
	}
	r.size = 0
	return r.open()
}

func (r *rotatingLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

// Tail returns the retained log family oldest-to-newest without following
// links. Gaps are evidence: a missing or unsafe segment is reported rather
// than silently treated as an empty log.
func (r *rotatingLog) Tail(maxBytes int) ([]byte, []string, error) {
	if maxBytes <= 0 {
		return nil, nil, errors.New("controller log tail limit must be positive")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil, nil, os.ErrClosed
	}

	type segment struct {
		path  string
		label string
	}
	segments := make([]segment, 0, r.backups+1)
	for i := r.backups; i >= 1; i-- {
		segments = append(segments, segment{r.path + fmt.Sprintf(".%d", i), fmt.Sprintf("backup-%d", i)})
	}
	segments = append(segments, segment{r.path, "current"})

	var out []byte
	var gaps []string
	for _, segment := range segments {
		info, err := os.Lstat(segment.path)
		if errors.Is(err, os.ErrNotExist) {
			gaps = append(gaps, "controller log "+segment.label+" segment is unavailable")
			continue
		}
		if err != nil {
			gaps = append(gaps, "controller log "+segment.label+" segment could not be inspected")
			continue
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			gaps = append(gaps, "controller log "+segment.label+" segment is not a regular file")
			continue
		}
		f, err := os.Open(segment.path)
		if err != nil {
			gaps = append(gaps, "controller log "+segment.label+" segment could not be opened")
			continue
		}
		opened, statErr := f.Stat()
		if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			_ = f.Close()
			gaps = append(gaps, "controller log "+segment.label+" segment changed while opening")
			continue
		}
		segmentLimit := int64(controllerLogMaxBytes)
		if opened.Size() > segmentLimit {
			if _, err := f.Seek(-segmentLimit, io.SeekEnd); err != nil {
				_ = f.Close()
				gaps = append(gaps, "controller log "+segment.label+" segment could not be bounded")
				continue
			}
			gaps = append(gaps, "controller log "+segment.label+" segment was truncated")
		}
		part, readErr := io.ReadAll(io.LimitReader(f, segmentLimit))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil {
			gaps = append(gaps, "controller log "+segment.label+" segment could not be read")
			continue
		}
		out = append(out, part...)
	}
	if len(out) > maxBytes {
		out = append([]byte(nil), out[len(out)-maxBytes:]...)
		gaps = append(gaps, "controller log retained input was truncated")
	}
	return out, gaps, nil
}

func withControllerLog(log *slog.Logger, sink io.Writer) *slog.Logger {
	jsonHandler := slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(mirroredHandler{primary: log.Handler(), secondary: jsonHandler})
}
