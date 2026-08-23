package speedtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDownloadBytes  = int64(10 << 20)
	defaultUploadBytes    = int64(5 << 20)
	defaultLatencyRuns    = 5
	defaultMaxDuration    = 30 * time.Second
	maxDirectionBytes     = int64(64 << 20)
	maxConfiguredDuration = 60 * time.Second
)

type HTTPConfig struct {
	Provider      string
	DownloadURL   string
	UploadURL     string
	DownloadBytes int64
	UploadBytes   int64
	LatencyRuns   int
	MaxDuration   time.Duration
	Client        *http.Client
}

func DefaultHTTPConfig() HTTPConfig {
	// Cloudflare's official speedtest repository documents these as its
	// download/upload defaults. They remain configurable because this direct
	// controller HTTP method is not Cloudflare's browser measurement engine:
	// https://github.com/cloudflare/speedtest
	return HTTPConfig{
		Provider: "Cloudflare", DownloadURL: "https://speed.cloudflare.com/__down",
		UploadURL: "https://speed.cloudflare.com/__up", DownloadBytes: defaultDownloadBytes,
		UploadBytes: defaultUploadBytes, LatencyRuns: defaultLatencyRuns,
		MaxDuration: defaultMaxDuration,
	}
}

type HTTPRunner struct {
	cfg HTTPConfig
}

func NewHTTPRunner(cfg HTTPConfig) (*HTTPRunner, error) {
	if cfg.Provider == "" || cfg.DownloadBytes <= 0 || cfg.DownloadBytes > maxDirectionBytes ||
		cfg.UploadBytes <= 0 || cfg.UploadBytes > maxDirectionBytes ||
		cfg.LatencyRuns < 2 || cfg.LatencyRuns > 20 || cfg.MaxDuration <= 0 ||
		cfg.MaxDuration > maxConfiguredDuration {
		return nil, errors.New("speedtest: invalid HTTP runner bounds")
	}
	var origin *url.URL
	for _, raw := range []string{cfg.DownloadURL, cfg.UploadURL} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return nil, errors.New("speedtest: endpoint must be an HTTP(S) URL without credentials")
		}
		if origin == nil {
			origin = u
		} else if !strings.EqualFold(origin.Scheme, u.Scheme) || !strings.EqualFold(origin.Host, u.Host) {
			return nil, errors.New("speedtest: download and upload endpoints must use the same origin")
		}
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: cfg.MaxDuration}
	}
	client := *cfg.Client
	// The reviewed endpoint is part of the operator's acknowledgement. A
	// redirect must not silently send measured traffic somewhere else.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.Client = &client
	return &HTTPRunner{cfg: cfg}, nil
}

func (r *HTTPRunner) Descriptor() Descriptor {
	return Descriptor{Provider: r.cfg.Provider, Method: "controller-http-single-stream-v1",
		Provenance: "controller-host", Endpoint: endpointOrigin(r.cfg.DownloadURL),
		DownloadEndpoint: r.cfg.DownloadURL, UploadEndpoint: r.cfg.UploadURL,
		EstimatedBytes: r.cfg.DownloadBytes + r.cfg.UploadBytes, MaxDuration: r.cfg.MaxDuration}
}

func (r *HTTPRunner) Run(ctx context.Context, progress func(Progress)) (Measurement, error) {
	var result Measurement
	progress(Progress{Phase: "idle-latency", Percent: 5})
	latencies := make([]float64, 0, r.cfg.LatencyRuns)
	for i := 0; i < r.cfg.LatencyRuns; i++ {
		ms, err := r.latency(ctx)
		if err != nil {
			return result, fmt.Errorf("measure idle latency: %w", err)
		}
		latencies = append(latencies, ms)
	}
	latency, jitter := mean(latencies), meanDelta(latencies)
	result.IdleLatencyMS, result.IdleJitterMS = &latency, &jitter

	progress(Progress{Phase: "download", Percent: 20})
	bytes, elapsed, err := r.download(ctx)
	result.BytesDownloaded = bytes
	if err != nil {
		return result, fmt.Errorf("measure download: %w", err)
	}
	download, err := mbps(bytes, elapsed)
	if err != nil {
		return result, err
	}
	result.DownloadMbps = &download
	progress(Progress{Phase: "upload", Percent: 65, BytesDownloaded: bytes})

	bytes, elapsed, err = r.upload(ctx)
	result.BytesUploaded = bytes
	if err != nil {
		return result, fmt.Errorf("measure upload: %w", err)
	}
	upload, err := mbps(bytes, elapsed)
	if err != nil {
		return result, err
	}
	result.UploadMbps = &upload
	progress(Progress{Phase: "finalising", Percent: 95,
		BytesDownloaded: result.BytesDownloaded, BytesUploaded: bytes})
	// Loaded latency/jitter remain nil: this single-stream method does not run
	// simultaneous probes, and an invented zero would be materially misleading.
	return result, nil
}

func (r *HTTPRunner) latency(ctx context.Context) (float64, error) {
	u, _ := url.Parse(r.cfg.DownloadURL)
	q := u.Query()
	q.Set("bytes", "0")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Cache-Control", "no-store")
	started := time.Now()
	resp, err := r.cfg.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1)); err != nil {
		return 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, fmt.Errorf("endpoint returned %s", resp.Status)
	}
	return float64(time.Since(started)) / float64(time.Millisecond), nil
}

func (r *HTTPRunner) download(ctx context.Context) (int64, time.Duration, error) {
	u, _ := url.Parse(r.cfg.DownloadURL)
	q := u.Query()
	q.Set("bytes", strconv.FormatInt(r.cfg.DownloadBytes, 10))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Cache-Control", "no-store")
	started := time.Now()
	resp, err := r.cfg.Client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, 0, fmt.Errorf("endpoint returned %s", resp.Status)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, r.cfg.DownloadBytes+1))
	elapsed := time.Since(started)
	if err != nil {
		return n, elapsed, err
	}
	if n > r.cfg.DownloadBytes {
		return n, elapsed, errors.New("endpoint exceeded the download byte limit")
	}
	if n != r.cfg.DownloadBytes {
		return n, elapsed, fmt.Errorf("endpoint returned %d download bytes; expected %d", n, r.cfg.DownloadBytes)
	}
	return n, elapsed, nil
}

func (r *HTTPRunner) upload(ctx context.Context) (int64, time.Duration, error) {
	body := &countingReader{r: io.LimitReader(zeroReader{}, r.cfg.UploadBytes)}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.UploadURL,
		body)
	if err != nil {
		return 0, 0, err
	}
	req.ContentLength = r.cfg.UploadBytes
	req.Header.Set("Content-Type", "application/octet-stream")
	started := time.Now()
	resp, err := r.cfg.Client.Do(req)
	if err != nil {
		return body.n, time.Since(started), err
	}
	defer resp.Body.Close()
	_, copyErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	elapsed := time.Since(started)
	if copyErr != nil {
		return body.n, elapsed, copyErr
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return body.n, elapsed, fmt.Errorf("endpoint returned %s", resp.Status)
	}
	if body.n != r.cfg.UploadBytes {
		return body.n, elapsed, fmt.Errorf("endpoint consumed %d upload bytes; expected %d",
			body.n, r.cfg.UploadBytes)
	}
	return body.n, elapsed, nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func endpointOrigin(raw string) string {
	u, _ := url.Parse(raw)
	return u.Scheme + "://" + u.Host
}

func mean(values []float64) float64 {
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func meanDelta(values []float64) float64 {
	var total float64
	for i := 1; i < len(values); i++ {
		d := values[i] - values[i-1]
		if d < 0 {
			d = -d
		}
		total += d
	}
	return total / float64(len(values)-1)
}

func mbps(bytes int64, elapsed time.Duration) (float64, error) {
	if bytes <= 0 || elapsed <= 0 {
		return 0, errors.New("speedtest: invalid transfer measurement")
	}
	return float64(bytes*8) / elapsed.Seconds() / 1_000_000, nil
}
