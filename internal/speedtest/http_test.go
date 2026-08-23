package speedtest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testHTTPRunner(t *testing.T, handler http.Handler) *HTTPRunner {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	runner, err := NewHTTPRunner(HTTPConfig{Provider: "local-test",
		DownloadURL: server.URL + "/down", UploadURL: server.URL + "/up",
		DownloadBytes: 4096, UploadBytes: 2048, LatencyRuns: 3,
		MaxDuration: 2 * time.Second, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestDefaultHTTPDescriptorDisclosesExactEndpoints(t *testing.T) {
	runner, err := NewHTTPRunner(DefaultHTTPConfig())
	if err != nil {
		t.Fatal(err)
	}
	d := runner.Descriptor()
	if d.Endpoint != "https://speed.cloudflare.com" ||
		d.DownloadEndpoint != "https://speed.cloudflare.com/__down" ||
		d.UploadEndpoint != "https://speed.cloudflare.com/__up" ||
		d.EstimatedBytes != 15<<20 || d.MaxDuration != 30*time.Second {
		t.Fatalf("default descriptor=%+v", d)
	}
	if !strings.HasPrefix(d.PlanID(), "sha256:") || d.PlanID() != runner.Descriptor().PlanID() {
		t.Fatalf("unstable plan id %q", d.PlanID())
	}
	changed := d
	changed.UploadEndpoint += "?revision=2"
	if changed.PlanID() == d.PlanID() {
		t.Fatal("endpoint change did not invalidate the reviewed plan")
	}
}

func TestHTTPRunnerMeasuresBoundedLocalTransfers(t *testing.T) {
	var uploaded atomic.Int64
	runner := testHTTPRunner(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/down":
			n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
			_, _ = io.CopyN(w, zeroReader{}, n)
		case "/up":
			n, _ := io.Copy(io.Discard, r.Body)
			uploaded.Store(n)
		default:
			http.NotFound(w, r)
		}
	}))
	var phases []string
	result, err := runner.Run(context.Background(), func(p Progress) {
		phases = append(phases, p.Phase)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesDownloaded != 4096 || result.BytesUploaded != 2048 || uploaded.Load() != 2048 {
		t.Fatalf("bytes=(%d,%d) server_upload=%d", result.BytesDownloaded,
			result.BytesUploaded, uploaded.Load())
	}
	if result.DownloadMbps == nil || result.UploadMbps == nil ||
		result.IdleLatencyMS == nil || result.IdleJitterMS == nil {
		t.Fatalf("missing measured fields: %+v", result)
	}
	if result.LoadedLatencyMS != nil || result.LoadedJitterMS != nil {
		t.Fatalf("unsupported loaded metrics were invented: %+v", result)
	}
	if strings.Join(phases, ",") != "idle-latency,download,upload,finalising" {
		t.Fatalf("phases=%v", phases)
	}
	d := runner.Descriptor()
	if d.Provenance != "controller-host" || d.Endpoint == "" || d.EstimatedBytes != 6144 {
		t.Fatalf("descriptor=%+v", d)
	}
}

func TestHTTPRunnerRejectsShortAndOversizeDownloads(t *testing.T) {
	for _, test := range []struct {
		name  string
		delta int64
		want  string
	}{{"short", -1, "expected 4096"}, {"oversize", 1, "exceeded the download byte limit"}} {
		t.Run(test.name, func(t *testing.T) {
			runner := testHTTPRunner(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/down" {
					_, _ = io.Copy(io.Discard, r.Body)
					return
				}
				n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
				if n > 0 {
					n += test.delta
				}
				_, _ = io.CopyN(w, zeroReader{}, n)
			}))
			_, err := runner.Run(context.Background(), func(Progress) {})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestHTTPRunnerRefusesRedirectsEvenWithPermissiveInjectedClient(t *testing.T) {
	var targetCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer source.Close()
	client := source.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	runner, err := NewHTTPRunner(HTTPConfig{Provider: "local-test",
		DownloadURL: source.URL + "/down", UploadURL: source.URL + "/up",
		DownloadBytes: 100, UploadBytes: 100, LatencyRuns: 2,
		MaxDuration: time.Second, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), func(Progress) {}); err == nil ||
		!strings.Contains(err.Error(), "302 Found") {
		t.Fatalf("redirect error=%v", err)
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("followed disclosed-endpoint redirect %d time(s)", targetCalls.Load())
	}
}

func TestHTTPRunnerRejectsUndisclosedUploadOrigin(t *testing.T) {
	_, err := NewHTTPRunner(HTTPConfig{Provider: "test",
		DownloadURL: "https://speed.example/down", UploadURL: "https://upload.example/up",
		DownloadBytes: 1, UploadBytes: 1, LatencyRuns: 2, MaxDuration: time.Second})
	if err == nil || !strings.Contains(err.Error(), "same origin") {
		t.Fatalf("error=%v", err)
	}
}

func TestHTTPRunnerRejectsExcessiveConfiguredBounds(t *testing.T) {
	base := HTTPConfig{Provider: "test", DownloadURL: "https://speed.example/down",
		UploadURL: "https://speed.example/up", DownloadBytes: 1, UploadBytes: 1,
		LatencyRuns: 2, MaxDuration: time.Second}
	for _, mutate := range []func(*HTTPConfig){
		func(c *HTTPConfig) { c.DownloadBytes = maxDirectionBytes + 1 },
		func(c *HTTPConfig) { c.UploadBytes = maxDirectionBytes + 1 },
		func(c *HTTPConfig) { c.MaxDuration = maxConfiguredDuration + time.Second },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := NewHTTPRunner(cfg); err == nil || !strings.Contains(err.Error(), "bounds") {
			t.Fatalf("accepted excessive config: %+v err=%v", cfg, err)
		}
	}
}

func TestHTTPRunnerReportsBytesReadOnFailedUpload(t *testing.T) {
	runner := testHTTPRunner(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down" {
			n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
			_, _ = io.CopyN(w, zeroReader{}, n)
			return
		}
		_, _ = io.CopyN(io.Discard, r.Body, 256)
		http.Error(w, "failed", http.StatusServiceUnavailable)
	}))
	result, err := runner.Run(context.Background(), func(Progress) {})
	if err == nil || result.BytesUploaded <= 0 || result.BytesUploaded > 2048 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestHTTPRunnerRejectsSuccessfulShortUpload(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			_, _ = io.CopyN(io.Discard, r.Body, 17)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK",
			Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(nil)), Request: r}, nil
	})}
	runner, err := NewHTTPRunner(HTTPConfig{Provider: "local-test",
		DownloadURL: "http://127.0.0.1/down", UploadURL: "http://127.0.0.1/up",
		DownloadBytes: 100, UploadBytes: 100, LatencyRuns: 2,
		MaxDuration: time.Second, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	n, _, err := runner.upload(context.Background())
	if n != 17 || err == nil || !strings.Contains(err.Error(), "expected 100") {
		t.Fatalf("bytes=%d error=%v", n, err)
	}
}

func TestMbpsRejectsImpossibleMeasurements(t *testing.T) {
	if _, err := mbps(1, 0); err == nil {
		t.Fatal("accepted zero elapsed time")
	}
}
