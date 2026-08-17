package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Environment variables, per IMPLEMENTATION §11's runtime contract.
const (
	EnvDataDir        = "OONFEE_DATA_DIR"
	EnvListen         = "OONFEE_LISTEN"
	EnvPassphraseFile = "OONFEE_PASSPHRASE_FILE"

	// EnvPassphraseRejected is not a setting. It is a name an operator might
	// reasonably guess, and finding it set is a strong signal that a secret is
	// sitting in the environment — so it is refused loudly rather than ignored
	// silently. See Config.load.
	EnvPassphraseRejected = "OONFEE_PASSPHRASE"
)

// Defaults matching deploy/docker-compose.yml, so a container with no
// environment at all comes up on the documented paths and port.
const (
	DefaultDataDir = "/data"
	DefaultListen  = ":8080"
	DBFileName     = "oonfeewrt.db"
)

// Config is everything the daemon needs to start. It is deliberately small:
// anything an operator can change at runtime belongs in the database, not here,
// because a setting in the environment needs a restart to move.
type Config struct {
	// DataDir holds the database and the keyring. One volume, per §11.
	DataDir string

	// Listen is the HTTP bind address.
	Listen string

	// PassphraseFile, when set, is read instead of prompting. This is the
	// unattended-boot tradeoff documented in secrets.ReadPassphraseFile: the
	// passphrase stops being a second factor and becomes a file permission.
	PassphraseFile string

	// ShutdownGrace bounds how long SIGTERM waits for HTTP to drain. Applies
	// get their own, longer budget — see ApplyDrain.
	ShutdownGrace time.Duration

	// ApplyDrain is the budget for an apply — EVERY apply, not only at shutdown.
	//
	// The name says shutdown and the truth is wider, which cost an afternoon to
	// find, so it is spelled out here: TrackApply gives every apply a context
	// with this deadline, always. Tuning it down to make SIGTERM snappier
	// silently caps every write to every device.
	//
	// It must not be short, for two reasons that point the same way. At
	// shutdown, an apply past the APPLY step has a rollback armed on the device
	// and that timer keeps running whether this process exists or not; exiting
	// early leaves a device that reverts a good change seconds later with
	// nobody left to confirm it. And in normal operation, a budget below
	// applyengine.MinApplyBudget() — the rollback window plus the grace period
	// after it — expires while the device's own timer is still running, so an
	// apply that needed the full window ends Unknown and Stranded with the
	// change still applied. That is the engine's one alarming outcome, produced
	// by a config knob rather than by anything the device did.
	//
	// Validate refuses zero; a positive value below the floor is allowed but
	// warned about at startup, because a short drain is legitimate in tests
	// that exercise shutdown itself.
	ApplyDrain time.Duration
}

// DefaultConfig returns the configuration before any environment is applied.
func DefaultConfig() Config {
	return Config{
		DataDir:       DefaultDataDir,
		Listen:        DefaultListen,
		ShutdownGrace: 10 * time.Second,
		ApplyDrain:    3 * time.Minute,
	}
}

// FromEnv builds a Config from the process environment, over the defaults.
func FromEnv() (Config, error) {
	c := DefaultConfig()
	if err := c.load(os.LookupEnv); err != nil {
		return Config{}, err
	}
	return c, nil
}

// load is separated from FromEnv so tests can drive it without mutating the
// process environment, which is global state that leaks between parallel tests.
func (c *Config) load(lookup func(string) (string, bool)) error {
	if v, ok := lookup(EnvPassphraseRejected); ok && v != "" {
		return fmt.Errorf("%s is set: the passphrase must never be passed as an "+
			"environment value — it is readable from /proc, inherited by every child "+
			"process, and captured in crash reports and `docker inspect`. Use %s to "+
			"point at a file with mode 600 instead",
			EnvPassphraseRejected, EnvPassphraseFile)
	}
	if v, ok := lookup(EnvDataDir); ok && v != "" {
		c.DataDir = v
	}
	if v, ok := lookup(EnvListen); ok && v != "" {
		c.Listen = v
	}
	if v, ok := lookup(EnvPassphraseFile); ok && v != "" {
		c.PassphraseFile = v
	}
	return c.Validate()
}

// Validate checks what can be checked without touching the filesystem.
func (c *Config) Validate() error {
	switch {
	case c.DataDir == "":
		return errors.New("daemon: data directory is empty")
	case !filepath.IsAbs(c.DataDir):
		// Absolute only. A relative path resolves against a working directory
		// the container image does not promise, and the failure mode is a second
		// empty database rather than an error.
		return fmt.Errorf("daemon: data directory %q must be an absolute path", c.DataDir)
	case c.Listen == "":
		return errors.New("daemon: listen address is empty")
	case c.ShutdownGrace <= 0:
		return errors.New("daemon: shutdown grace must be positive")
	case c.ApplyDrain <= 0:
		return errors.New("daemon: apply drain must be positive")
	}
	return nil
}

// DBPath is where the SQLite database lives.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, DBFileName) }
