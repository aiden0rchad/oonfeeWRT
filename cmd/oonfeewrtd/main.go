// Command oonfeewrtd is the oonfeeWRT controller.
//
// Configuration comes from the environment (IMPLEMENTATION §11), with flags for
// the same values so a checkout is usable without exporting anything. Secrets
// never come from either: the passphrase is prompted on a terminal, or read
// from a file named by OONFEE_PASSPHRASE_FILE.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/daemon"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
var version = "dev"

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet when configuration is what failed, so
		// this path writes plainly to stderr rather than assuming one.
		fmt.Fprintln(os.Stderr, "oonfeewrtd:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := daemon.FromEnv()
	if err != nil {
		return err
	}
	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		logLevel    = flag.String("log-level", "info", "debug|info|warn|error")
	)
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir,
		"data directory (database and keyring); env "+daemon.EnvDataDir)
	flag.StringVar(&cfg.Listen, "listen", cfg.Listen,
		"HTTP bind address; env "+daemon.EnvListen)
	flag.StringVar(&cfg.PassphraseFile, "passphrase-file", cfg.PassphraseFile,
		"read the operator passphrase from this file instead of prompting; env "+
			daemon.EnvPassphraseFile)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}
	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// SIGINT and SIGTERM cancel the root context, which is what Serve watches.
	// signal.NotifyContext restores the default disposition on stop, so a second
	// signal during a slow shutdown kills the process — which is the behaviour an
	// operator expects when they press Ctrl-C twice.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("oonfeewrtd starting", "version", version, "data_dir", cfg.DataDir)
	d, err := daemon.Open(ctx, cfg, log)
	if err != nil {
		return err
	}
	if err := d.StartCollector(ctx, collector.Options{}); err != nil {
		return err
	}
	d.StartMaintenance(ctx)
	if err := d.Serve(ctx); err != nil {
		return err
	}
	log.Info("stopped cleanly")
	return nil
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, errors.New("log level must be one of debug, info, warn, error")
	}
	return l, nil
}
