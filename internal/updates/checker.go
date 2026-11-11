// Package updates checks the latest published oonfeeWRT controller release.
package updates

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	Endpoint          = "https://api.github.com/repos/aiden0rchad/oonfeeWRT/releases/latest"
	ReleaseOrigin     = "https://github.com/aiden0rchad/oonfeeWRT/releases/tag/"
	SuccessTTL        = 12 * time.Hour
	MinManualInterval = 60 * time.Second
	FailureBackoff    = 15 * time.Minute
	maxBodyBytes      = 1 << 20
	maxTagBytes       = 128
	maxETagBytes      = 256
)

type State string

const (
	StateAvailable   State = "available"
	StateCurrent     State = "current"
	StateUnknown     State = "unknown"
	StateUnavailable State = "unavailable"
)

// StatusError is deliberately independent of upstream response text.
type StatusError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Status is the stable API projection of one cached update check.
type Status struct {
	State           State        `json:"state"`
	InstallKind     string       `json:"install_kind"`
	CurrentVersion  string       `json:"current_version"`
	LatestVersion   string       `json:"latest_version,omitempty"`
	LatestTag       string       `json:"latest_tag,omitempty"`
	ReleaseURL      string       `json:"release_url,omitempty"`
	UpdateAvailable bool         `json:"update_available"`
	CheckedAt       *time.Time   `json:"checked_at,omitempty"`
	NextCheckAt     *time.Time   `json:"next_check_at,omitempty"`
	Message         string       `json:"message"`
	Error           *StatusError `json:"error,omitempty"`
}

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type release struct {
	tag     string
	version version
}

type attempt struct {
	release     *release
	checkedAt   time.Time
	nextCheckAt time.Time
	err         *StatusError
}

// Checker lazily performs and process-caches release checks. It is safe for
// concurrent use; one request owns an upstream check and the others wait for
// its result or their own cancellation.
type Checker struct {
	mu sync.Mutex

	client HTTPClient
	now    func() time.Time

	inFlight      chan struct{}
	last          attempt
	haveLast      bool
	lastStartedAt time.Time

	// A failed refresh replaces the public attempt, but the last valid release
	// and ETag remain private so a later conditional request can use a 304.
	lastRelease *release
	etag        string
}

func NewChecker() *Checker {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("updates: redirects are not permitted")
		},
	}
	return NewCheckerWithClient(client, time.Now)
}

// NewCheckerWithClient supplies the HTTP and clock seams used by tests.
func NewCheckerWithClient(client HTTPClient, now func() time.Time) *Checker {
	if client == nil {
		panic("updates: nil HTTP client")
	}
	if now == nil {
		panic("updates: nil clock")
	}
	return &Checker{client: client, now: now}
}

// Check returns an explanatory status rather than an HTTP-shaped error. An
// unrecognised development version never causes an outbound request.
func (c *Checker) Check(ctx context.Context, current string) Status {
	return c.check(ctx, current, false)
}

// Refresh bypasses the success TTL, but respects backoff and the manual floor.
func (c *Checker) Refresh(ctx context.Context, current string) Status {
	return c.check(ctx, current, true)
}

func (c *Checker) check(ctx context.Context, current string, force bool) Status {
	currentVersion, kind, ok := parseCurrent(current)
	if !ok {
		return Status{
			State: StateUnknown, CurrentVersion: current,
			Message: "The running executable does not report a stable numeric release version, so the controller did not contact GitHub or infer update availability.",
		}
	}

	for {
		if ctx.Err() != nil {
			return cancelledStatus(current)
		}
		now := c.now().UTC()
		c.mu.Lock()
		if c.haveLast && now.Before(c.last.nextCheckAt) &&
			(!force || c.last.err != nil || now.Before(c.last.checkedAt.Add(MinManualInterval))) {
			last := cloneAttempt(c.last)
			c.mu.Unlock()
			return statusFor(current, currentVersion, kind, last)
		}
		if c.inFlight != nil {
			wait := c.inFlight
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return cancelledStatus(current)
			}
		}

		// Cancelling an upstream request cannot evade the manual floor.
		if force && !c.lastStartedAt.IsZero() && now.Before(c.lastStartedAt.Add(MinManualInterval)) {
			last, haveLast := cloneAttempt(c.last), c.haveLast
			c.mu.Unlock()
			if haveLast {
				return statusFor(current, currentVersion, kind, last)
			}
			return cancelledStatus(current)
		}
		c.lastStartedAt = now
		wait := make(chan struct{})
		c.inFlight = wait
		previous := cloneRelease(c.lastRelease)
		etag := c.etag
		c.mu.Unlock()

		nextRelease, nextETag, failure, retryAt := c.fetch(ctx, previous, etag)
		checkedAt := c.now().UTC()
		nextCheckAt := checkedAt.Add(SuccessTTL)
		if failure != nil {
			nextCheckAt = checkedAt.Add(FailureBackoff)
			if retryAt.After(nextCheckAt) {
				nextCheckAt = retryAt
			}
		}
		last := attempt{release: nextRelease, checkedAt: checkedAt, nextCheckAt: nextCheckAt, err: failure}

		c.mu.Lock()
		if ctx.Err() != nil {
			close(wait)
			c.inFlight = nil
			c.mu.Unlock()
			return cancelledStatus(current)
		}
		c.last = cloneAttempt(last)
		c.haveLast = true
		if failure == nil && nextRelease != nil {
			c.lastRelease = cloneRelease(nextRelease)
			c.etag = nextETag
		}
		close(wait)
		c.inFlight = nil
		c.mu.Unlock()
		return statusFor(current, currentVersion, kind, last)
	}
}

func cloneRelease(in *release) *release {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneAttempt(in attempt) attempt {
	in.release = cloneRelease(in.release)
	if in.err != nil {
		errCopy := *in.err
		in.err = &errCopy
	}
	return in
}

func cancelledStatus(current string) Status {
	return Status{
		State: StateUnavailable, CurrentVersion: current,
		Message: "The release check was cancelled before a current result was available.",
		Error:   &StatusError{Code: "check_cancelled", Message: "The release check was cancelled."},
	}
}

func statusFor(current string, currentVersion version, kind currentKind, last attempt) Status {
	checkedAt, nextCheckAt := last.checkedAt, last.nextCheckAt
	status := Status{
		State: StateUnavailable, CurrentVersion: current,
		CheckedAt: &checkedAt, NextCheckAt: &nextCheckAt,
		Message: "The controller could not confirm the latest published stable release. It is not reporting this installation as current.",
		Error:   last.err,
	}
	if last.err != nil || last.release == nil {
		return status
	}

	status.LatestVersion = last.release.version.String()
	status.LatestTag = last.release.tag
	status.ReleaseURL = ReleaseOrigin + url.PathEscape(last.release.tag)
	status.UpdateAvailable = last.release.version.compare(currentVersion) > 0
	status.Error = nil
	if status.UpdateAvailable {
		status.State = StateAvailable
		status.Message = "A newer oonfeeWRT release is available."
		return status
	}
	status.State = StateCurrent
	status.Message = "You're running the latest published stable release."
	if kind == currentGitDescribe {
		status.Message = "No release newer than this build's base version has been published."
	}
	return status
}

type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      *bool  `json:"draft"`
	Prerelease *bool  `json:"prerelease"`
}

func (c *Checker) fetch(ctx context.Context, previous *release, etag string) (*release, string, *StatusError, time.Time) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Endpoint, nil)
	if err != nil {
		return nil, "", safeFailure("request_failed"), time.Time{}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "oonfeeWRT update checker")
	if previous != nil && validETag(etag) {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		code := "request_failed"
		if ctx.Err() != nil {
			code = "check_cancelled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			code = "check_timeout"
		} else if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
			code = "check_timeout"
		}
		return nil, "", safeFailure(code), time.Time{}
	}
	if resp == nil {
		return nil, "", safeFailure("request_failed"), time.Time{}
	}
	defer resp.Body.Close()

	retryAt := retryTime(c.now().UTC(), resp.Header)
	if resp.StatusCode == http.StatusNotModified {
		if previous == nil {
			return nil, "", safeFailure("invalid_not_modified"), retryAt
		}
		return previous, retainedETag(headerValue(resp.Header, "ETag"), etag), nil, time.Time{}
	}
	if resp.StatusCode != http.StatusOK {
		code := "upstream_unavailable"
		if resp.StatusCode == http.StatusTooManyRequests ||
			(resp.StatusCode == http.StatusForbidden && strings.TrimSpace(headerValue(resp.Header, "X-RateLimit-Remaining")) == "0") {
			code = "rate_limited"
		}
		return nil, "", safeFailure(code), retryAt
	}
	if resp.ContentLength > maxBodyBytes {
		return nil, "", safeFailure("response_too_large"), retryAt
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", safeFailure("check_cancelled"), time.Time{}
		}
		return nil, "", safeFailure("response_read_failed"), retryAt
	}
	if ctx.Err() != nil {
		return nil, "", safeFailure("check_cancelled"), time.Time{}
	}
	if len(body) > maxBodyBytes {
		return nil, "", safeFailure("response_too_large"), retryAt
	}
	var decoded githubRelease
	if json.Unmarshal(body, &decoded) != nil || decoded.Draft == nil || decoded.Prerelease == nil ||
		*decoded.Draft || *decoded.Prerelease || len(decoded.TagName) == 0 || len(decoded.TagName) > maxTagBytes {
		return nil, "", safeFailure("invalid_release_metadata"), retryAt
	}
	latest, ok := parseReleaseTag(decoded.TagName)
	if !ok {
		return nil, "", safeFailure("invalid_release_version"), retryAt
	}
	return &release{tag: decoded.TagName, version: latest}, retainedETag(headerValue(resp.Header, "ETag"), ""), nil, time.Time{}
}

func safeFailure(code string) *StatusError {
	messages := map[string]string{
		"check_cancelled":          "The release check was cancelled.",
		"check_timeout":            "The public release service did not respond before the check timed out.",
		"rate_limited":             "The public release service rate-limited this controller.",
		"invalid_not_modified":     "The public release service returned an unusable conditional response.",
		"response_too_large":       "The public release service returned an oversized response.",
		"response_read_failed":     "The public release response could not be read.",
		"invalid_release_metadata": "The public release service returned invalid release metadata.",
		"invalid_release_version":  "The published release tag is not a stable numeric version.",
		"upstream_unavailable":     "The public release service is currently unavailable.",
		"request_failed":           "The controller could not reach the public release service.",
	}
	message := messages[code]
	if message == "" {
		code, message = "request_failed", messages["request_failed"]
	}
	return &StatusError{Code: code, Message: message}
}

func retainedETag(candidate, fallback string) string {
	if validETag(candidate) {
		return candidate
	}
	if validETag(fallback) {
		return fallback
	}
	return ""
}

func validETag(value string) bool {
	if value == "" || len(value) > maxETagBytes || strings.ContainsAny(value, "\r\n") {
		return false
	}
	if strings.HasPrefix(value, "W/\"") {
		value = strings.TrimPrefix(value, "W/")
	}
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return false
	}
	for _, char := range []byte(value[1 : len(value)-1]) {
		if char < 0x21 || char > 0x7e || char == '"' {
			return false
		}
	}
	return true
}

func retryTime(now time.Time, header http.Header) time.Time {
	var retry time.Time
	value := strings.TrimSpace(headerValue(header, "Retry-After"))
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if candidate, ok := addSeconds(now, seconds); ok {
			retry = candidate
		}
	} else if candidate, err := http.ParseTime(value); err == nil && candidate.After(now) {
		retry = candidate.UTC()
	}

	if strings.TrimSpace(headerValue(header, "X-RateLimit-Remaining")) == "0" {
		if seconds, err := strconv.ParseInt(strings.TrimSpace(headerValue(header, "X-RateLimit-Reset")), 10, 64); err == nil {
			candidate := time.Unix(seconds, 0).UTC()
			if candidate.Year() <= 9999 && candidate.After(now) && candidate.After(retry) && candidate.Sub(now) > 0 {
				retry = candidate
			}
		}
	}
	return retry
}

func headerValue(header http.Header, name string) string {
	for key, values := range header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func addSeconds(now time.Time, seconds int64) (time.Time, bool) {
	if seconds > math.MaxInt64/int64(time.Second) {
		return time.Time{}, false
	}
	candidate := now.Add(time.Duration(seconds) * time.Second)
	return candidate, candidate.After(now) || seconds == 0
}

type version [3]uint64

func (v version) compare(other version) int {
	for i := range v {
		if v[i] < other[i] {
			return -1
		}
		if v[i] > other[i] {
			return 1
		}
	}
	return 0
}

func (v version) String() string {
	return strconv.FormatUint(v[0], 10) + "." + strconv.FormatUint(v[1], 10) + "." + strconv.FormatUint(v[2], 10)
}

func parseCurrentStable(input string) (version, bool) {
	var out version
	if input == "" || len(input) > maxTagBytes || strings.TrimSpace(input) != input {
		return out, false
	}
	value := input
	if value[0] == 'v' || value[0] == 'V' {
		value = value[1:]
	}
	base, metadata, hasMetadata := strings.Cut(value, "+")
	if hasMetadata && !validBuildMetadata(metadata) || strings.Contains(base, "-") {
		return out, false
	}
	return parseNumericVersion(base)
}

func parseReleaseTag(input string) (version, bool) {
	if len(input) < 2 || len(input) > maxTagBytes || strings.TrimSpace(input) != input || input[0] != 'v' {
		return version{}, false
	}
	return parseNumericVersion(input[1:])
}

func parseNumericVersion(input string) (version, bool) {
	var out version
	parts := strings.Split(input, ".")
	if len(parts) != len(out) {
		return out, false
	}
	for i, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return version{}, false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return version{}, false
			}
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return version{}, false
		}
		out[i] = n
	}
	return out, true
}

func validBuildMetadata(value string) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		for _, char := range identifier {
			if !(char >= '0' && char <= '9' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char == '-') {
				return false
			}
		}
	}
	return true
}

type currentKind uint8

const (
	currentStable currentKind = iota
	currentGitDescribe
)

var gitDescribe = regexp.MustCompile(`^([vV]?[0-9]+\.[0-9]+\.[0-9]+)-([0-9]+)-g([0-9A-Fa-f]{4,64})(-dirty)?$`)

func parseCurrent(input string) (version, currentKind, bool) {
	if parsed, ok := parseCurrentStable(input); ok {
		return parsed, currentStable, true
	}
	match := gitDescribe.FindStringSubmatch(input)
	if match == nil || len(match[2]) > 1 && match[2][0] == '0' {
		return version{}, currentStable, false
	}
	if _, err := strconv.ParseUint(match[2], 10, 64); err != nil {
		return version{}, currentStable, false
	}
	parsed, ok := parseCurrentStable(match[1])
	return parsed, currentGitDescribe, ok
}
