package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPServerLimits(t *testing.T) {
	d, err := Open(context.Background(), testConfig(t, "pass"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if d.http.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %s, want 1m", d.http.IdleTimeout)
	}
	if d.http.MaxHeaderBytes != 64<<10 {
		t.Errorf("MaxHeaderBytes = %d, want %d", d.http.MaxHeaderBytes, 64<<10)
	}
	if d.http.ReadTimeout != 0 || d.http.WriteTimeout != 0 {
		t.Errorf("stream-breaking timeouts set: ReadTimeout=%s WriteTimeout=%s",
			d.http.ReadTimeout, d.http.WriteTimeout)
	}
}

func TestSecurityHeadersCoverEveryHTTPSurface(t *testing.T) {
	d := &Daemon{Log: quietLogger()}
	handler := d.routes()

	for _, path := range []string{"/healthz", "/api/v1/not-a-route", "/"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			for name, want := range map[string]string{
				"X-Content-Type-Options": "nosniff",
				"X-Frame-Options":        "DENY",
				"Referrer-Policy":        "no-referrer",
			} {
				if got := rec.Header().Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}
