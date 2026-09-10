// Package processlock gives the daemon and write-capable maintenance tools
// exclusive ownership of one controller data directory.
package processlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

const FileName = ".oonfeewrt.lock"

var ErrInUse = errors.New("controller data directory is already in use")

type Lock struct {
	file *os.File
	once sync.Once
	err  error
}

// Acquire takes a non-blocking advisory lock. The file contains no data and is
// kept in the data directory so containers need no writable parent directory.
func Acquire(dataDir string) (*Lock, error) {
	path := filepath.Join(dataDir, FileName)
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open controller process lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrInUse
		}
		return nil, fmt.Errorf("acquire controller process lock: %w", err)
	}
	return &Lock{file: file}, nil
}

// Close releases the lock. It is safe to call more than once.
func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.file == nil {
			return
		}
		l.err = errors.Join(unix.Flock(int(l.file.Fd()), unix.LOCK_UN), l.file.Close())
	})
	return l.err
}
