package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControllerLogRotatesPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), controllerLogName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &rotatingLog{path: path, maxBytes: 48, backups: 2}
	if err := r.open(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := r.Write([]byte(`{"msg":"bounded-record"}` + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".1", ".2"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatalf("stat %s: %v", suffix, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode=%#o, want 0600", suffix, got)
		}
		if info.Size() > 48 {
			t.Errorf("%s size=%d, want <=48", suffix, info.Size())
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected third backup: %v", err)
	}
}

func TestControllerLogDropsOversizedRecordWithoutLeakingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), controllerLogName)
	r := &rotatingLog{path: path, maxBytes: 2 * controllerLogMaxRecord, backups: 1}
	if err := r.open(); err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("private-value", controllerLogMaxRecord/4)
	if n, err := r.Write([]byte(secret)); err != nil || n != len(secret) {
		t.Fatalf("write count=%d err=%v", n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("private-value")) {
		t.Fatal("oversized record content reached the controller log")
	}
	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(body), &row); err != nil {
		t.Fatalf("replacement is not JSON: %v", err)
	}
	if row["msg"] != "controller log record omitted: too large" {
		t.Fatalf("replacement=%v", row)
	}
}

func TestMirroredHandlerPreservesPrimaryLevel(t *testing.T) {
	var primary, structured bytes.Buffer
	log := slog.New(mirroredHandler{
		primary:   slog.NewTextHandler(&primary, &slog.HandlerOptions{Level: slog.LevelWarn}),
		secondary: slog.NewJSONHandler(&structured, &slog.HandlerOptions{Level: slog.LevelDebug}),
	})
	log.Debug("hidden")
	log.WarnContext(context.Background(), "kept", "count", 2)
	if strings.Contains(primary.String(), "hidden") || strings.Contains(structured.String(), "hidden") {
		t.Fatal("secondary handler bypassed the configured primary log level")
	}
	if !strings.Contains(primary.String(), "kept") {
		t.Fatal("primary log did not receive warning")
	}
	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(structured.Bytes()), &row); err != nil {
		t.Fatalf("structured log is not JSON: %v", err)
	}
	if row["msg"] != "kept" || row["count"] != float64(2) {
		t.Fatalf("structured row=%v", row)
	}
}

func TestControllerLogRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, controllerLogName)); err != nil {
		t.Fatal(err)
	}
	if _, err := openControllerLog(dir); err == nil {
		t.Fatal("opened a symbolic-link controller log")
	}
}

func TestControllerLogRefusesNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, controllerLogName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := openControllerLog(dir); err == nil {
		t.Fatal("opened a non-regular controller log")
	}
}

func TestControllerLogTightensExistingBackups(t *testing.T) {
	dir := t.TempDir()
	for _, suffix := range []string{"", ".1", ".2", ".3"} {
		if err := os.WriteFile(filepath.Join(dir, controllerLogName)+suffix, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := openControllerLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, suffix := range []string{"", ".1", ".2", ".3"} {
		info, err := os.Stat(filepath.Join(dir, controllerLogName) + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("backup %s mode=%#o, want 0600", suffix, got)
		}
	}
}

func TestControllerLogTailIsBoundedOrderedAndNeverFollowsLinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, controllerLogName)
	if err := os.WriteFile(path+".3", []byte("oldest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "outside")
	if err := os.WriteFile(secret, []byte("must-not-follow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, path+".1"); err != nil {
		t.Fatal(err)
	}
	r := &rotatingLog{path: path, maxBytes: controllerLogMaxBytes, backups: 3}
	if err := r.open(); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Write([]byte("newest\n")); err != nil {
		t.Fatal(err)
	}
	body, gaps, err := r.Tail(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "oldest\nnewest\n" || bytes.Contains(body, []byte("must-not-follow")) {
		t.Fatalf("tail=%q", body)
	}
	joined := strings.Join(gaps, "\n")
	if !strings.Contains(joined, "backup-2") || !strings.Contains(joined, "backup-1") {
		t.Fatalf("gaps=%v", gaps)
	}
	bounded, gaps, err := r.Tail(4)
	if err != nil || string(bounded) != "est\n" || !strings.Contains(strings.Join(gaps, "\n"), "truncated") {
		t.Fatalf("bounded=%q gaps=%v err=%v", bounded, gaps, err)
	}
}
