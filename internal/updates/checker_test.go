package updates

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clientFunc func(*http.Request) (*http.Response, error)

func (f clientFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func githubResponse(status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{
		StatusCode: status,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func published(tag string) string {
	return `{"tag_name":"` + tag + `","draft":false,"prerelease":false,"body":"ignored"}`
}

func TestVersionComparisonAndUnknownBuilds(t *testing.T) {
	tests := []struct {
		name, current, latest string
		wantState             State
		wantUpdate            bool
		wantRequest           bool
	}{
		{"patch", "v0.1.5", "v0.1.6", StateAvailable, true, true},
		{"same", "v0.1.8", "v0.1.8", StateCurrent, false, true},
		{"same rebuilt image", "v0.1.8+image.20260918", "v0.1.8", StateCurrent, false, true},
		{"git describe same base", "v0.1.8-14-gabcdef0", "v0.1.8", StateCurrent, false, true},
		{"dirty git describe same base", "v0.1.8-14-gabcdef0-dirty", "v0.1.8", StateCurrent, false, true},
		{"lower latest", "v0.1.9", "v0.1.8", StateCurrent, false, true},
		{"numeric patch", "v0.1.9", "v0.1.10", StateAvailable, true, true},
		{"minor", "v0.9.9", "v0.10.0", StateAvailable, true, true},
		{"major", "v9.99.99", "v10.0.0", StateAvailable, true, true},
		{"uppercase current prefix", "V1.2.3", "v1.2.4", StateAvailable, true, true},
		{"unprefixed current", "1.2.3", "v1.2.4", StateAvailable, true, true},
		{"dev", "dev", "v9.9.9", StateUnknown, false, false},
		{"sha only", "abcdef012345", "v9.9.9", StateUnknown, false, false},
		{"prerelease current", "v1.2.3-rc.1", "v1.2.4", StateUnknown, false, false},
		{"leading zero current", "v1.02.3", "v1.2.4", StateUnknown, false, false},
		{"overflow current", "v18446744073709551616.0.0", "v2.0.0", StateUnknown, false, false},
		{"malformed current", "release-1.2", "v1.2.3", StateUnknown, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			checker := NewCheckerWithClient(clientFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return githubResponse(http.StatusOK, published(tc.latest), nil), nil
			}), func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) })
			got := checker.Check(context.Background(), tc.current)
			if got.State != tc.wantState || got.UpdateAvailable != tc.wantUpdate {
				t.Fatalf("status=%+v want state=%s update=%v", got, tc.wantState, tc.wantUpdate)
			}
			if calls != boolInt(tc.wantRequest) {
				t.Fatalf("requests=%d want=%d", calls, boolInt(tc.wantRequest))
			}
			if tc.wantRequest && got.LatestVersion == "" {
				t.Fatal("valid check omitted latest version")
			}
			if !tc.wantRequest && (got.CheckedAt != nil || got.LatestTag != "") {
				t.Fatalf("unknown build claimed a check: %+v", got)
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestRefreshBypassesSuccessTTLButKeepsFloorAndConditionalRequest(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	calls := 0
	checker := NewCheckerWithClient(clientFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			if req.Header.Get("If-None-Match") != `"release"` {
				t.Fatal("manual check lost conditional request")
			}
			return githubResponse(http.StatusNotModified, "", nil), nil
		}
		return githubResponse(http.StatusOK, published("v1.2.4"), http.Header{"Etag": {`"release"`}}), nil
	}), func() time.Time { return now })
	first := checker.Check(context.Background(), "v1.2.3")
	now = now.Add(MinManualInterval - time.Nanosecond)
	if got := checker.Refresh(context.Background(), "v1.2.3"); calls != 1 || !got.CheckedAt.Equal(*first.CheckedAt) {
		t.Fatalf("manual floor: calls=%d status=%+v", calls, got)
	}
	now = now.Add(time.Nanosecond)
	if got := checker.Check(context.Background(), "v1.2.3"); calls != 1 || !got.CheckedAt.Equal(*first.CheckedAt) {
		t.Fatalf("automatic TTL: calls=%d status=%+v", calls, got)
	}
	got := checker.Refresh(context.Background(), "v1.2.3")
	if calls != 2 || !got.CheckedAt.Equal(now) || got.LatestTag != "v1.2.4" || got.NextCheckAt.Sub(now) != SuccessTTL {
		t.Fatalf("fresh manual check: calls=%d status=%+v", calls, got)
	}
	checker.Refresh(context.Background(), "v1.2.3")
	if calls != 2 {
		t.Fatal("repeated manual check bypassed floor")
	}
}

func TestRefreshHonorsFailureBackoffAndRateLimit(t *testing.T) {
	for _, retry := range []time.Duration{FailureBackoff, time.Hour} {
		t.Run(retry.String(), func(t *testing.T) {
			now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
			calls := 0
			checker := NewCheckerWithClient(clientFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return githubResponse(http.StatusTooManyRequests, "PRIVATE", http.Header{
					"Retry-After": {strconv.Itoa(int(retry.Seconds()))},
				}), nil
			}), func() time.Time { return now })
			first := checker.Refresh(context.Background(), "v1.2.3")
			now = now.Add(retry - time.Nanosecond)
			got := checker.Refresh(context.Background(), "v1.2.3")
			if calls != 1 || got.Error == nil || !got.CheckedAt.Equal(*first.CheckedAt) {
				t.Fatalf("backoff bypassed: calls=%d status=%+v", calls, got)
			}
			now = now.Add(time.Nanosecond)
			checker.Refresh(context.Background(), "v1.2.3")
			if calls != 2 {
				t.Fatal("manual check did not resume after backoff")
			}
		})
	}
}

func TestRefreshUnknownBuildNeverContactsGitHub(t *testing.T) {
	checker := NewCheckerWithClient(clientFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unknown version contacted GitHub")
		return nil, nil
	}), time.Now)
	for _, current := range []string{"", "dev", "abcdef0", "v1.2.3-rc.1"} {
		if got := checker.Refresh(context.Background(), current); got.State != StateUnknown || got.CheckedAt != nil {
			t.Fatalf("unknown status=%+v", got)
		}
	}
}

func TestCancelledManualCheckCannotBypassFloor(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	checker := NewCheckerWithClient(clientFunc(func(*http.Request) (*http.Response, error) {
		calls++
		cancel()
		return nil, context.Canceled
	}), func() time.Time { return now })
	checker.Refresh(ctx, "v1.2.3")
	checker.Refresh(context.Background(), "v1.2.3")
	if calls != 1 {
		t.Fatal("cancelled manual check bypassed floor")
	}
}

func TestRequestIsFixedBoundedAndIdentityFree(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	checker := NewCheckerWithClient(clientFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != Endpoint {
			t.Fatalf("request=%s %s", req.Method, req.URL)
		}
		if req.Header.Get("Accept") != "application/vnd.github+json" ||
			req.Header.Get("X-GitHub-Api-Version") != "2026-03-10" ||
			req.Header.Get("User-Agent") != "oonfeeWRT update checker" {
			t.Fatalf("headers=%v", req.Header)
		}
		for _, forbidden := range []string{"Authorization", "Cookie", "X-Device-ID", "X-Controller-ID"} {
			if req.Header.Get(forbidden) != "" {
				t.Fatalf("sent %s", forbidden)
			}
		}
		return githubResponse(http.StatusOK, published("v1.2.4"), http.Header{"Etag": {`"release-1"`}}), nil
	}), func() time.Time { return now })
	got := checker.Check(context.Background(), "v1.2.3")
	if got.ReleaseURL != ReleaseOrigin+"v1.2.4" || got.CheckedAt == nil || got.NextCheckAt == nil ||
		got.NextCheckAt.Sub(*got.CheckedAt) != SuccessTTL {
		t.Fatalf("status=%+v", got)
	}

	defaultClient, ok := NewChecker().client.(*http.Client)
	if !ok || defaultClient.Timeout > 10*time.Second || defaultClient.Timeout <= 0 {
		t.Fatalf("default timeout=%v", defaultClient.Timeout)
	}
	if defaultClient.CheckRedirect(nil, nil) == nil {
		t.Fatal("redirect was accepted")
	}
}

func TestSuccessfulChecksCacheForTwelveHours(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	var calls int
	checker := NewCheckerWithClient(clientFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return githubResponse(http.StatusOK, published("v1.3.0"), nil), nil
	}), func() time.Time { return now })

	first := checker.Check(context.Background(), "v1.2.0")
	now = now.Add(SuccessTTL - time.Second)
	second := checker.Check(context.Background(), "v1.2.9")
	if calls != 1 || !first.UpdateAvailable || !second.UpdateAvailable {
		t.Fatalf("calls=%d first=%+v second=%+v", calls, first, second)
	}
	now = now.Add(time.Second)
	checker.Check(context.Background(), "v1.3.0")
	if calls != 2 {
		t.Fatalf("requests after expiry=%d", calls)
	}
}

func TestFailedRefreshHidesStaleReleaseAndRetainsConditionalMetadata(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	var calls int
	checker := NewCheckerWithClient(clientFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			return githubResponse(http.StatusOK, published("v1.3.0"), http.Header{"Etag": {`"release-1"`}}), nil
		case 2:
			if req.Header.Get("If-None-Match") != `"release-1"` {
				t.Fatalf("conditional header=%q", req.Header.Get("If-None-Match"))
			}
			return githubResponse(http.StatusBadGateway, "PRIVATE-UPSTREAM-BODY", nil), nil
		default:
			if req.Header.Get("If-None-Match") != `"release-1"` {
				t.Fatalf("retained conditional header=%q", req.Header.Get("If-None-Match"))
			}
			return githubResponse(http.StatusNotModified, "", nil), nil
		}
	}), func() time.Time { return now })

	if !checker.Check(context.Background(), "v1.2.0").UpdateAvailable {
		t.Fatal("initial release not available")
	}
	now = now.Add(SuccessTTL)
	failed := checker.Check(context.Background(), "v1.2.0")
	if failed.State != StateUnavailable || failed.LatestTag != "" || failed.ReleaseURL != "" ||
		failed.UpdateAvailable || strings.Contains(failed.Error.Message, "PRIVATE") {
		t.Fatalf("failed refresh leaked stale/upstream data: %+v", failed)
	}
	if cached := checker.Check(context.Background(), "v1.2.0"); cached.State != StateUnavailable || calls != 2 {
		t.Fatalf("failure not cached: calls=%d status=%+v", calls, cached)
	}
	now = now.Add(FailureBackoff)
	revalidated := checker.Check(context.Background(), "v1.2.0")
	if calls != 3 || revalidated.State != StateAvailable || revalidated.LatestTag != "v1.3.0" {
		t.Fatalf("304 revalidation=%+v calls=%d", revalidated, calls)
	}
}

func TestConcurrentChecksCoalesce(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	checker := NewCheckerWithClient(clientFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return githubResponse(http.StatusOK, published("v2.0.0"), nil), nil
	}), func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) })

	const count = 24
	results := make(chan Status, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				results <- checker.Check(context.Background(), "v1.0.0")
			} else {
				results <- checker.Refresh(context.Background(), "v1.0.0")
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(results)
	for result := range results {
		if result.State != StateAvailable {
			t.Fatalf("coalesced result=%+v", result)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d want=1", calls.Load())
	}
}

func TestWaitingCancellationDoesNotBlockOrCorruptOwner(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	checker := NewCheckerWithClient(clientFunc(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return githubResponse(http.StatusOK, published("v2.0.0"), nil), nil
	}), time.Now)
	owner := make(chan Status, 1)
	go func() { owner <- checker.Check(context.Background(), "v1.0.0") }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := checker.Check(ctx, "v1.0.0"); got.Error == nil || got.Error.Code != "check_cancelled" {
		t.Fatalf("cancelled waiter=%+v", got)
	}
	close(release)
	if got := <-owner; got.State != StateAvailable {
		t.Fatalf("owner=%+v", got)
	}
}

type observingContext struct {
	context.Context
	doneCalled chan struct{}
	once       sync.Once
}

func (c *observingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.doneCalled) })
	return c.Context.Done()
}

func TestOwnerCancellationReleasesFlightAndHealthyWaiterRetries(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	checker := NewCheckerWithClient(clientFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return githubResponse(http.StatusOK, published("v2.0.0"), nil), nil
	}), time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	owner := make(chan Status, 1)
	go func() { owner <- checker.Check(ctx, "v1.0.0") }()
	<-started
	waiting := &observingContext{Context: context.Background(), doneCalled: make(chan struct{})}
	healthy := make(chan Status, 1)
	go func() { healthy <- checker.Check(waiting, "v1.0.0") }()
	<-waiting.doneCalled
	cancel()
	first, second := <-owner, <-healthy
	if first.Error == nil || first.Error.Code != "check_cancelled" || second.State != StateAvailable || calls.Load() != 2 {
		t.Fatalf("owner=%+v waiter=%+v calls=%d", first, second, calls.Load())
	}
}

func TestAlreadyCancelledContextDoesNotClaimFlight(t *testing.T) {
	var calls atomic.Int32
	checker := NewCheckerWithClient(clientFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return githubResponse(http.StatusOK, published("v2.0.0"), nil), nil
	}), time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	first := checker.Check(ctx, "v1.0.0")
	second := checker.Check(context.Background(), "v1.0.0")
	if first.Error == nil || first.Error.Code != "check_cancelled" || second.State != StateAvailable || calls.Load() != 1 {
		t.Fatalf("cancelled=%+v healthy=%+v calls=%d", first, second, calls.Load())
	}
}

func TestCancelledRefreshRetainsPreviousReleaseAndETag(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	var calls atomic.Int32
	checker := NewCheckerWithClient(clientFunc(func(req *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return githubResponse(http.StatusOK, published("v2.0.0"), http.Header{"Etag": {`"release-2"`}}), nil
		case 2:
			close(started)
			<-req.Context().Done()
			return nil, req.Context().Err()
		default:
			if got := req.Header.Get("If-None-Match"); got != `"release-2"` {
				t.Fatalf("conditional header=%q", got)
			}
			return githubResponse(http.StatusNotModified, "", nil), nil
		}
	}), func() time.Time { return now })

	if got := checker.Check(context.Background(), "v1.0.0"); got.LatestTag != "v2.0.0" {
		t.Fatalf("initial=%+v", got)
	}
	now = now.Add(SuccessTTL)
	ctx, cancel := context.WithCancel(context.Background())
	refresh := make(chan Status, 1)
	go func() { refresh <- checker.Check(ctx, "v1.0.0") }()
	<-started
	cancel()
	if got := <-refresh; got.Error == nil || got.Error.Code != "check_cancelled" {
		t.Fatalf("cancelled refresh=%+v", got)
	}
	got := checker.Check(context.Background(), "v1.0.0")
	if got.State != StateAvailable || got.LatestTag != "v2.0.0" || calls.Load() != 3 {
		t.Fatalf("revalidated=%+v calls=%d", got, calls.Load())
	}
}

type contextReadCloser struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (r *contextReadCloser) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (*contextReadCloser) Close() error { return nil }

func TestCancellationDuringResponseReadDoesNotBackOff(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	checker := NewCheckerWithClient(clientFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &contextReadCloser{ctx: req.Context(), started: started}}, nil
		}
		return githubResponse(http.StatusOK, published("v2.0.0"), nil), nil
	}), time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan Status, 1)
	go func() { result <- checker.Check(ctx, "v1.0.0") }()
	<-started
	cancel()
	if got := <-result; got.Error == nil || got.Error.Code != "check_cancelled" {
		t.Fatalf("cancelled read=%+v", got)
	}
	if got := checker.Check(context.Background(), "v1.0.0"); got.State != StateAvailable || calls.Load() != 2 {
		t.Fatalf("healthy=%+v calls=%d", got, calls.Load())
	}
}

func TestFailuresBackOffAndHonorRateLimitHeaders(t *testing.T) {
	base := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		headers http.Header
		want    time.Duration
	}{
		{"minimum", nil, FailureBackoff},
		{"retry after", http.Header{"Retry-After": {"3600"}}, time.Hour},
		{"rate reset longer", http.Header{
			"Retry-After": {"3600"}, "X-RateLimit-Remaining": {"0"},
			"X-RateLimit-Reset": {strconvTime(base.Add(2 * time.Hour))},
		}, 2 * time.Hour},
		{"HTTP date", http.Header{"Retry-After": {base.Add(3 * time.Hour).Format(http.TimeFormat)}}, 3 * time.Hour},
		{"malformed", http.Header{"Retry-After": {"not-a-delay"}, "X-RateLimit-Remaining": {"0"}, "X-RateLimit-Reset": {"overflow-overflow"}}, FailureBackoff},
		{"overflow", http.Header{"Retry-After": {"999999999999999999999999"}, "X-RateLimit-Remaining": {"0"}, "X-RateLimit-Reset": {"9223372036854775807"}}, FailureBackoff},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checker := NewCheckerWithClient(clientFunc(func(*http.Request) (*http.Response, error) {
				return githubResponse(http.StatusTooManyRequests, "upstream details", tc.headers), nil
			}), func() time.Time { return base })
			got := checker.Check(context.Background(), "v1.0.0")
			if got.State != StateUnavailable || got.Error == nil || got.Error.Code != "rate_limited" ||
				got.NextCheckAt == nil || got.NextCheckAt.Sub(base) != tc.want {
				t.Fatalf("status=%+v want delay=%s", got, tc.want)
			}
		})
	}
}

func strconvTime(value time.Time) string { return strconv.FormatInt(value.Unix(), 10) }

func TestTransportTimeoutIsUnavailableAndSanitized(t *testing.T) {
	var calls atomic.Int32
	checker := NewCheckerWithClient(clientFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.Join(context.DeadlineExceeded, errors.New("SECRET-NETWORK-DETAIL"))
	}), time.Now)
	got := checker.Check(context.Background(), "v1.0.0")
	cached := checker.Check(context.Background(), "v1.0.0")
	if got.State != StateUnavailable || got.Error == nil || got.Error.Code != "check_timeout" ||
		strings.Contains(got.Error.Message, "SECRET") || got.UpdateAvailable || cached.Error == nil ||
		cached.Error.Code != "check_timeout" || calls.Load() != 1 {
		t.Fatalf("status=%+v cached=%+v calls=%d", got, cached, calls.Load())
	}
}

func TestMalformedAndUnsafeResponsesNeverClaimCurrent(t *testing.T) {
	tests := []struct {
		name string
		code int
		body string
	}{
		{"404", http.StatusNotFound, `{"message":"PRIVATE"}`},
		{"invalid JSON", http.StatusOK, `{`},
		{"missing flags", http.StatusOK, `{"tag_name":"v1.2.0"}`},
		{"draft", http.StatusOK, `{"tag_name":"v1.2.0","draft":true,"prerelease":false}`},
		{"prerelease flag", http.StatusOK, `{"tag_name":"v1.2.0","draft":false,"prerelease":true}`},
		{"prerelease tag", http.StatusOK, published("v1.2.0-rc.1")},
		{"build metadata tag", http.StatusOK, published("v1.2.0+build")},
		{"uppercase prefix tag", http.StatusOK, published("V1.2.0")},
		{"missing prefix tag", http.StatusOK, published("1.2.0")},
		{"leading zero tag", http.StatusOK, published("v1.02.0")},
		{"overflow tag", http.StatusOK, published("v18446744073709551616.0.0")},
		{"unsafe slash", http.StatusOK, published("v1.2.0/../../elsewhere")},
		{"unsafe query", http.StatusOK, published("v1.2.0?next")},
		{"empty tag", http.StatusOK, published("")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checker := NewCheckerWithClient(clientFunc(func(*http.Request) (*http.Response, error) {
				return githubResponse(tc.code, tc.body, nil), nil
			}), time.Now)
			got := checker.Check(context.Background(), "v1.0.0")
			if got.State != StateUnavailable || got.UpdateAvailable || got.LatestTag != "" || got.ReleaseURL != "" || got.Error == nil {
				t.Fatalf("status=%+v", got)
			}
		})
	}
}

func TestOversizeAndUnprimedNotModifiedAreRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp *http.Response
		code string
	}{
		{"oversize body", githubResponse(http.StatusOK, strings.Repeat("x", maxBodyBytes+1), nil), "response_too_large"},
		{"oversize content length", func() *http.Response {
			response := githubResponse(http.StatusOK, published("v2.0.0"), nil)
			response.ContentLength = maxBodyBytes + 1
			return response
		}(), "response_too_large"},
		{"unprimed 304", githubResponse(http.StatusNotModified, "", nil), "invalid_not_modified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := NewCheckerWithClient(clientFunc(func(*http.Request) (*http.Response, error) {
				return tc.resp, nil
			}), time.Now)
			got := checker.Check(context.Background(), "v1.0.0")
			if got.Error == nil || got.Error.Code != tc.code || got.State != StateUnavailable {
				t.Fatalf("status=%+v", got)
			}
		})
	}
}
