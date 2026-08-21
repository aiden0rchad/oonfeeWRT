package store

import (
	"context"
	"testing"
)

func TestEventKeysetScopesRemainStableAcrossInserts(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	for _, event := range []Event{
		{TS: 100, Category: "system", Severity: "info", Event: "oldest"},
		{TS: 101, Category: "audit", Severity: "info", Event: "audit"},
		{TS: 102, Category: "device", Severity: "warning", Event: "middle"},
		{TS: 103, Category: "system", Severity: "error", Event: "newest"},
	} {
		if err := db.LogEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	page, err := db.QueryEventsKeyset(ctx, EventQuery{Scope: "general", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].Event != "newest" || page[1].Event != "middle" {
		t.Fatalf("first page=%+v", page)
	}
	before := &EventCursor{TS: page[1].TS, ID: page[1].ID}
	if err := db.LogEvent(ctx, Event{TS: 104, Category: "system", Severity: "info", Event: "later insert"}); err != nil {
		t.Fatal(err)
	}
	next, err := db.QueryEventsKeyset(ctx, EventQuery{Scope: "general", Before: before, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].Event != "oldest" {
		t.Fatalf("stable next page=%+v", next)
	}
	audit, err := db.QueryEventsKeyset(ctx, EventQuery{Scope: "audit", Limit: 10})
	if err != nil || len(audit) != 1 || audit[0].Event != "audit" {
		t.Fatalf("audit page=%+v err=%v", audit, err)
	}
	got, err := db.EventByID(ctx, audit[0].ID)
	if err != nil || got.Event != "audit" {
		t.Fatalf("event detail=%+v err=%v", got, err)
	}
	if _, err := db.EventByID(ctx, 999999); err != ErrNotFound {
		t.Fatalf("missing detail err=%v", err)
	}
}

func TestEventScopedFacetsUseWholeScopedResult(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	for _, event := range []Event{
		{TS: 1, Category: "audit", Severity: "error", Event: "a"},
		{TS: 2, Category: "system", Severity: "error", Event: "b"},
		{TS: 3, Category: "device", Severity: "info", Event: "c"},
	} {
		if err := db.LogEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	cats, sevs, total, err := db.EventFacetsScoped(ctx, EventQuery{Scope: "general"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(cats) != 2 || len(sevs) != 2 {
		t.Fatalf("facets cats=%+v sevs=%+v total=%d", cats, sevs, total)
	}
	_, _, total, err = db.EventFacetsScoped(ctx, EventQuery{Scope: "audit"})
	if err != nil || total != 1 {
		t.Fatalf("audit total=%d err=%v", total, err)
	}
	if _, err := db.QueryEventsKeyset(ctx, EventQuery{Scope: "invented"}); err == nil {
		t.Fatal("invalid scope accepted")
	}
}
