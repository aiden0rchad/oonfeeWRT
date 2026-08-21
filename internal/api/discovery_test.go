package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/discovery"
)

type stubScanner struct {
	result *ScanResult
	err    error
}

func (s *stubScanner) Plan(context.Context) (*ScanPlan, error) {
	return &ScanPlan{}, s.err
}

func (s *stubScanner) Scan(context.Context, ScanRequest) (*ScanResult, error) {
	return s.result, s.err
}

// The HTTP layer must preserve the structured failure. Returning 200 with the
// old summary fields is intentionally backward compatible, but stripping the
// additive field would make newer clients repeat the original false claim.
func TestScanReturnsStructuredNetworkFailures(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.srv.Scan = &stubScanner{result: &ScanResult{
		Found:    []DiscoveredDevice{},
		Swept:    254,
		Networks: []string{"192.168.1.0/24"},
		Failures: []discovery.NetworkFailure{{
			Network: "192.168.1.0/24", Reason: discovery.FailureUnreachable,
			Attempts: 254,
		}},
	}}

	w := h.do(http.MethodPost, "/api/v1/discovery/scan", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got ScanResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Failures) != 1 || got.Failures[0].Network != "192.168.1.0/24" ||
		got.Failures[0].Reason != discovery.FailureUnreachable ||
		got.Failures[0].Attempts != 254 {
		t.Fatalf("structured failure was lost: %+v", got.Failures)
	}
}
