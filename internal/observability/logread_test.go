package observability

import (
	"encoding/json"
	"math"
	"testing"
)

func TestDecodeLogReadLiveShape(t *testing.T) {
	rows, err := DecodeLogRead(json.RawMessage(`{"log":[
		{"msg":"hostapd: AP-STA-CONNECTED","id":41,"priority":30,"source":1,"time":1787160000123},
		{"msg":"netifd: link up","id":42,"priority":29,"source":1,"time":1787160001123}
	]}`))
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0].Severity() != 6 || rows[0].Facility() != 3 || rows[0].SourceID() != "41" {
		t.Fatalf("decoded row=%+v", rows[0])
	}
}

func TestDecodeLogReadRejectsMalformedPages(t *testing.T) {
	for _, raw := range []string{
		`{"log":[{"id":1},{"id":3}]}`,
		`{"log":[{"id":1,"time":18446744073709551615}]}`,
		`{"log":[{"id":1,"time":0}]}`,
	} {
		if _, err := DecodeLogRead(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	tooMany := make([]LogEntry, maxLogEntries+1)
	raw, _ := json.Marshal(map[string]any{"log": tooMany})
	if _, err := DecodeLogRead(raw); err == nil {
		t.Fatal("accepted oversized page")
	}
	_ = math.MaxInt64
}

func TestAdvanceLogCursorOverlapGapResetAndWrap(t *testing.T) {
	epoch := LogEpoch{BootID: "boot", PID: 7}
	first := AdvanceLogCursor(LogCursor{}, epoch, []LogEntry{{ID: 10, TimeMS: 100}, {ID: 11, TimeMS: 110}})
	if len(first.Entries) != 2 || first.Reset || first.Gap || !first.Cursor.Valid {
		t.Fatalf("first=%+v", first)
	}
	overlap := AdvanceLogCursor(first.Cursor, epoch,
		[]LogEntry{{ID: 11, TimeMS: 110}, {ID: 12, TimeMS: 120}})
	if len(overlap.Entries) != 1 || overlap.Entries[0].ID != 12 || overlap.Gap || overlap.Reset {
		t.Fatalf("overlap=%+v", overlap)
	}
	gap := AdvanceLogCursor(overlap.Cursor, epoch,
		[]LogEntry{{ID: 15, TimeMS: 150}, {ID: 16, TimeMS: 160}})
	if !gap.Gap || gap.Reset || len(gap.Entries) != 2 {
		t.Fatalf("gap=%+v", gap)
	}
	restart := AdvanceLogCursor(gap.Cursor, LogEpoch{BootID: "boot", PID: 8},
		[]LogEntry{{ID: 0, TimeMS: 170}})
	if !restart.Reset || restart.Gap || restart.Cursor.Generation != gap.Cursor.Generation+1 {
		t.Fatalf("restart=%+v", restart)
	}

	wrapCursor := LogCursor{Epoch: epoch, LastID: math.MaxUint32, LastTimeMS: 200, Valid: true}
	wrap := AdvanceLogCursor(wrapCursor, epoch, []LogEntry{{ID: 0, TimeMS: 210}})
	if wrap.Reset || wrap.Gap || len(wrap.Entries) != 1 || wrap.Cursor.Generation != 1 {
		t.Fatalf("wrap=%+v", wrap)
	}
	overlapWrap := AdvanceLogCursor(wrapCursor, epoch,
		[]LogEntry{{ID: math.MaxUint32, TimeMS: 200}, {ID: 0, TimeMS: 210}})
	if overlapWrap.Reset || overlapWrap.Gap || len(overlapWrap.Entries) != 1 ||
		overlapWrap.Cursor.Generation != 1 {
		t.Fatalf("overlap wrap=%+v", overlapWrap)
	}
}

func TestAdvanceLogCursorRegressionWithoutPIDChangeIsReset(t *testing.T) {
	epoch := LogEpoch{BootID: "boot", PID: 7}
	cur := LogCursor{Epoch: epoch, Generation: 2, LastID: 900, LastTimeMS: 100, Valid: true}
	got := AdvanceLogCursor(cur, epoch, []LogEntry{{ID: 1, TimeMS: 200}})
	if !got.Reset || got.Gap || got.Cursor.Generation != 3 || got.Reason == "" {
		t.Fatalf("got=%+v", got)
	}
}

func TestAdvanceLogCursorAcceptsNewIDsAfterRouterClockStepsBackward(t *testing.T) {
	epoch := LogEpoch{BootID: "boot", PID: 7}
	cur := LogCursor{Epoch: epoch, Generation: 2, LastID: 10, LastTimeMS: 200, Valid: true}
	got := AdvanceLogCursor(cur, epoch, []LogEntry{{ID: 11, TimeMS: 100}})
	if got.Reset || got.Gap || !got.ClockRegressed || len(got.Entries) != 1 ||
		got.Cursor.LastID != 11 || got.Cursor.LastTimeMS != 100 {
		t.Fatalf("got=%+v", got)
	}
}

func TestAdvanceLogCursorRequiresIDAndTimestampForOverlap(t *testing.T) {
	epoch := LogEpoch{BootID: "boot", PID: 7}
	cur := LogCursor{Epoch: epoch, Generation: 2, LastID: 5, LastTimeMS: 100, Valid: true}
	got := AdvanceLogCursor(cur, epoch, []LogEntry{
		{ID: 0, TimeMS: 200}, {ID: 1, TimeMS: 201}, {ID: 2, TimeMS: 202},
		{ID: 3, TimeMS: 203}, {ID: 4, TimeMS: 204}, {ID: 5, TimeMS: 205},
	})
	if !got.Reset || got.Gap || len(got.Entries) != 6 || got.Cursor.Generation != 3 ||
		got.Reason == "" {
		t.Fatalf("got=%+v", got)
	}
}

func TestLogCursorStorageRoundTrip(t *testing.T) {
	want := LogCursor{Epoch: LogEpoch{BootID: "boot-id", PID: 72}, Generation: 3,
		LastID: 4294967295, LastTimeMS: 1787160000123, Valid: true}
	got, err := ParseLogCursor(want.SourceBoot(), want.Position())
	if err != nil || got != want {
		t.Fatalf("got=%+v err=%v want=%+v", got, err, want)
	}
	for _, bad := range [][2]string{{"", ""}, {"boot:x:0", "1:2"}, {"boot:1:0", "x:2"}} {
		if _, err := ParseLogCursor(bad[0], bad[1]); err == nil {
			t.Fatalf("accepted %#v", bad)
		}
	}
}
