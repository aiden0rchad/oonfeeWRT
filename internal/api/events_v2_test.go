package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestEventsKeysetScopesAndDetail(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	for _, event := range []store.Event{
		{TS: 100, Category: "system", Severity: "info", Event: "old"},
		{TS: 101, Category: "audit", Severity: "warning", Event: "audit"},
		{TS: 102, Category: "device", Severity: "error", Event: "new"},
	} {
		if err := h.db.LogEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	w := h.do(http.MethodGet, "/api/v1/events?scope=general&limit=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var page struct {
		Events []store.Event     `json:"events"`
		Total  int               `json:"total"`
		Next   store.EventCursor `json:"next_before"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Events) != 1 || page.Events[0].Event != "new" || page.Next.ID == 0 {
		t.Fatalf("page=%+v", page)
	}
	if err := h.db.LogEvent(ctx, store.Event{TS: 103, Category: "system", Severity: "info", Event: "later"}); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/events?scope=general&limit=10&before_ts=%d&before_id=%d",
		page.Next.TS, page.Next.ID)
	w = h.do(http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("next status=%d body=%s", w.Code, w.Body.String())
	}
	var next struct {
		Events []store.Event `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if len(next.Events) != 1 || next.Events[0].Event != "old" {
		t.Fatalf("next=%+v", next.Events)
	}
	w = h.do(http.MethodGet, fmt.Sprintf("/api/v1/events/%d", page.Events[0].ID), nil)
	if w.Code != http.StatusOK || !json.Valid(w.Body.Bytes()) {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	w = h.do(http.MethodGet, "/api/v1/events?scope=invalid", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid scope status=%d", w.Code)
	}
	w = h.do(http.MethodGet, "/api/v1/events?scope=general&before_ts=1", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("partial cursor status=%d", w.Code)
	}
}

func TestGeneralEventCoverageDistinguishesUnobservedEmptyAndStale(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	device := observabilityDevice(t, h, 1, "aa:bb:cc:00:20:01", "Living room AP",
		[]string{"ap"}, now.Unix())
	type response struct {
		Events   []store.Event `json:"events"`
		Coverage eventCoverage `json:"coverage"`
	}
	read := func(scope string) response {
		t.Helper()
		w := h.do(http.MethodGet, "/api/v1/events?scope="+scope, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var got response
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	missing := read("general").Coverage
	if missing.Complete || missing.ExpectedDevices != 1 || missing.ObservedDevices != 0 ||
		len(missing.Gaps) != 1 {
		t.Fatalf("missing coverage=%+v", missing)
	}
	if err := h.db.SaveIngestCursor(ctx, store.IngestCursor{
		DeviceID: device.ID, Source: "openwrt-logd", BootID: "boot:1:0",
		Cursor: "empty", UpdatedAt: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	empty := read("general")
	if !empty.Coverage.Complete || empty.Coverage.ObservedDevices != 1 ||
		len(empty.Events) != 0 || len(empty.Coverage.Gaps) != 0 {
		t.Fatalf("observed-empty coverage=%+v events=%+v", empty.Coverage, empty.Events)
	}
	if err := h.db.SaveIngestCursor(ctx, store.IngestCursor{
		DeviceID: device.ID, Source: "openwrt-logd", BootID: "boot:1:0",
		Cursor: "empty", UpdatedAt: now.Add(-4 * time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	stale := read("general").Coverage
	if stale.Complete || stale.ObservedDevices != 1 || len(stale.Gaps) != 1 {
		t.Fatalf("stale coverage=%+v", stale)
	}
	if audit := read("audit").Coverage; !audit.Complete || len(audit.Gaps) != 0 {
		t.Fatalf("audit coverage=%+v", audit)
	}
	if err := h.db.SaveIngestCursor(ctx, store.IngestCursor{
		DeviceID: device.ID, Source: "openwrt-logd", BootID: "boot:1:0",
		Cursor: "20:200000", UpdatedAt: now.UnixMilli(), ContinuityGapAt: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	discontinuous := read("general").Coverage
	if discontinuous.Complete || discontinuous.ObservedDevices != 1 ||
		len(discontinuous.Gaps) != 1 ||
		!strings.Contains(discontinuous.Gaps[0], "continuity") {
		t.Fatalf("continuity gap coverage=%+v", discontinuous)
	}
}

func TestGeneralEventCoverageDisclosesFreshCapPruning(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	h.srv.Now = func() time.Time { return now }
	device := observabilityDevice(t, h, 1, "aa:bb:cc:00:20:02", "Office AP",
		[]string{"ap"}, now.Unix())
	for id := 1; id <= 3; id++ {
		if err := h.db.LogEvent(ctx, store.Event{
			TS: now.Unix(), DeviceID: &device.ID, Category: "system", Severity: "info",
			Event: "openwrt.log", Source: "openwrt-logd", SourceBoot: "boot:1:0",
			SourceID: fmt.Sprint(id), IngestedAt: now.UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.db.SaveIngestCursor(ctx, store.IngestCursor{
		DeviceID: device.ID, Source: "openwrt-logd", BootID: "boot:1:0",
		Cursor: "3:1000", UpdatedAt: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Prune(ctx, now, store.Retention{MaxOpenWRTEventsPerDevice: 2}); err != nil {
		t.Fatal(err)
	}
	w := h.do(http.MethodGet, "/api/v1/events?scope=general", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Coverage eventCoverage `json:"coverage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Coverage.Complete || len(got.Coverage.Gaps) != 1 ||
		!strings.Contains(got.Coverage.Gaps[0], "continuity") {
		t.Fatalf("cap-pruned coverage=%+v", got.Coverage)
	}
}
