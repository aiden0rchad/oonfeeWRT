package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/daemon"
)

func TestHealthcheckURLUsesLoopbackForWildcardListeners(t *testing.T) {
	for _, tc := range []struct {
		listen string
		want   string
	}{
		{":8080", "http://127.0.0.1:8080/healthz"},
		{"0.0.0.0:8080", "http://127.0.0.1:8080/healthz"},
		{"[::]:8080", "http://[::1]:8080/healthz"},
		{"127.0.0.1:9090", "http://127.0.0.1:9090/healthz"},
	} {
		t.Run(tc.listen, func(t *testing.T) {
			got, err := healthcheckURL(tc.listen)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("URL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHealthcheckProbesOnlyTheHealthEndpoint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	called := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- r.URL.Path
		_, _ = w.Write([]byte("ok"))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	if err := healthcheck(ln.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if got := <-called; got != "/healthz" {
		t.Fatalf("requested %q, want /healthz", got)
	}
}

func TestHealthcheckModeDoesNotOpenControllerState(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	root := t.TempDir()
	state := filepath.Join(root, "missing-state")
	cfg := daemon.DefaultConfig()
	cfg.Listen = ln.Addr().String()
	cfg.DataDir = state
	cfg.PassphraseFile = filepath.Join(root, "missing-passphrase")
	if err := runWithConfig(cfg, []string{"-healthcheck"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("healthcheck opened controller state: %v", err)
	}
}

func TestHealthcheckRejectsNonHealthyResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"wrong status", http.StatusServiceUnavailable, "ok"},
		{"wrong body", http.StatusOK, "not ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			err = healthcheck(ln.Addr().String())
			if err == nil || !strings.Contains(err.Error(), "unexpected response") {
				t.Fatalf("error = %v, want unexpected response", err)
			}
		})
	}
}
