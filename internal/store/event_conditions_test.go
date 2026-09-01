package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestIPv6RAConditionStatusesClassifyIndependentDeviceEvidence(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	windows := IPv6RAConditionWindows{
		Now: now, CursorFreshFor: 3 * time.Minute,
		RecentFor: 5 * time.Minute, QuietFor: 10 * time.Minute,
	}

	recent := addIPv6RAStatusDevice(t, db, 1)
	appendIPv6RAStatusCondition(t, db, recent.ID, "recent:1:0", 1,
		now.Add(-2*time.Minute), now.Add(-2*time.Minute), now, time.Time{})

	middle := addIPv6RAStatusDevice(t, db, 2)
	appendIPv6RAStatusCondition(t, db, middle.ID, "middle:1:0", 1,
		now.Add(-7*time.Minute), now.Add(-7*time.Minute), now, time.Time{})

	quiet := addIPv6RAStatusDevice(t, db, 3)
	appendIPv6RAStatusCondition(t, db, quiet.ID, "quiet:1:0", 1,
		now.Add(-12*time.Minute), now.Add(-12*time.Minute), now, time.Time{})

	stale := addIPv6RAStatusDevice(t, db, 4)
	appendIPv6RAStatusCondition(t, db, stale.ID, "stale:1:0", 1,
		now.Add(-12*time.Minute), now.Add(-12*time.Minute), now.Add(-4*time.Minute), time.Time{})

	missing := addIPv6RAStatusDevice(t, db, 5)
	appendIPv6RAStatusConditionWithoutCursor(t, db, missing.ID, "missing:1:0", 1,
		now.Add(-12*time.Minute), now.Add(-12*time.Minute))

	gap := addIPv6RAStatusDevice(t, db, 6)
	appendIPv6RAStatusCondition(t, db, gap.ID, "gap:1:0", 1,
		now.Add(-12*time.Minute), now.Add(-12*time.Minute), now, now.Add(-4*time.Minute))

	priorBoot := addIPv6RAStatusDevice(t, db, 7)
	appendIPv6RAStatusCondition(t, db, priorBoot.ID, "old:1:0", 1,
		now.Add(-12*time.Minute), now.Add(-12*time.Minute), now.Add(-12*time.Minute), time.Time{})
	saveIPv6RAStatusCursor(t, db, priorBoot.ID, "current:2:0", now, now.Add(-2*time.Minute))

	// Unrelated newer rows and event paging cannot crowd a condition out of this
	// dedicated query.
	for i := 0; i < 20; i++ {
		if err := db.LogEvent(ctx, Event{
			TS: now.Add(time.Duration(i) * time.Second).Unix(), Category: "system",
			Severity: "info", Event: fmt.Sprintf("unrelated.%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	statuses, err := db.IPv6RAConditionStatuses(ctx, windows)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]IPv6RAConditionState{
		recent.ID: IPv6RAConditionRecent, middle.ID: IPv6RAConditionUnknown,
		quiet.ID: IPv6RAConditionHistorical, stale.ID: IPv6RAConditionUnknown,
		missing.ID: IPv6RAConditionUnknown, gap.ID: IPv6RAConditionUnknown,
		priorBoot.ID: IPv6RAConditionUnknown,
	}
	if len(statuses) != len(want) {
		t.Fatalf("statuses=%+v, want %d devices", statuses, len(want))
	}
	for _, status := range statuses {
		if status.State != want[status.DeviceID] {
			t.Errorf("device %d state=%q, want %q", status.DeviceID, status.State,
				want[status.DeviceID])
		}
		if status.EventID <= 0 || status.Occurrences != 1 || status.LastObservedAt <= 0 {
			t.Errorf("device %d incomplete status=%+v", status.DeviceID, status)
		}
	}
}

func TestIPv6RAConditionStatusesWaitForQuietAfterProducerEpochChange(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	device := addIPv6RAStatusDevice(t, db, 10)
	appendIPv6RAStatusCondition(t, db, device.ID, "boot:old:0", 1,
		base.Add(-20*time.Minute), base.Add(-20*time.Minute), base.Add(-20*time.Minute), time.Time{})
	saveIPv6RAStatusCursor(t, db, device.ID, "boot:new:0", base, base.Add(-4*time.Minute))
	windows := IPv6RAConditionWindows{
		Now: base, CursorFreshFor: 3 * time.Minute,
		RecentFor: 5 * time.Minute, QuietFor: 10 * time.Minute,
	}

	statuses, err := db.IPv6RAConditionStatuses(ctx, windows)
	if err != nil || len(statuses) != 1 || statuses[0].State != IPv6RAConditionUnknown {
		t.Fatalf("unsettled epoch statuses=%+v err=%v", statuses, err)
	}

	windows.Now = base.Add(7 * time.Minute)
	saveIPv6RAStatusCursor(t, db, device.ID, "boot:new:0", windows.Now, base.Add(-4*time.Minute))
	statuses, err = db.IPv6RAConditionStatuses(ctx, windows)
	if err != nil || len(statuses) != 1 || statuses[0].State != IPv6RAConditionHistorical {
		t.Fatalf("settled epoch statuses=%+v err=%v", statuses, err)
	}
}

func TestIPv6RAConditionStatusesSettleGapAndReactivateOneRow(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	device := addIPv6RAStatusDevice(t, db, 8)
	boot := "boot:8:0"
	appendIPv6RAStatusCondition(t, db, device.ID, boot, 1,
		base.Add(-20*time.Minute), base.Add(-20*time.Minute), base, base.Add(-4*time.Minute))
	windows := IPv6RAConditionWindows{
		Now: base, CursorFreshFor: 3 * time.Minute,
		RecentFor: 5 * time.Minute, QuietFor: 10 * time.Minute,
	}

	statuses, err := db.IPv6RAConditionStatuses(ctx, windows)
	if err != nil || len(statuses) != 1 || statuses[0].State != IPv6RAConditionUnknown {
		t.Fatalf("unsettled gap statuses=%+v err=%v", statuses, err)
	}
	eventID := statuses[0].EventID

	windows.Now = base.Add(7 * time.Minute)
	saveIPv6RAStatusCursor(t, db, device.ID, boot, windows.Now, base.Add(-4*time.Minute))
	statuses, err = db.IPv6RAConditionStatuses(ctx, windows)
	if err != nil || len(statuses) != 1 || statuses[0].State != IPv6RAConditionHistorical {
		t.Fatalf("settled gap statuses=%+v err=%v", statuses, err)
	}

	appendIPv6RAStatusCondition(t, db, device.ID, boot, 2,
		windows.Now, windows.Now, windows.Now, base.Add(-4*time.Minute))
	statuses, err = db.IPv6RAConditionStatuses(ctx, windows)
	if err != nil || len(statuses) != 1 {
		t.Fatalf("reactivated statuses=%+v err=%v", statuses, err)
	}
	if statuses[0].State != IPv6RAConditionRecent || statuses[0].EventID != eventID ||
		statuses[0].Occurrences != 2 || statuses[0].LastObservedAt != windows.Now.UnixMilli() {
		t.Fatalf("reactivated status=%+v", statuses[0])
	}
}

func TestIPv6RAConditionStatusesUseNewestProducerEpoch(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	device := addIPv6RAStatusDevice(t, db, 9)
	appendIPv6RAStatusCondition(t, db, device.ID, "boot:old:0", 20,
		now.Add(-20*time.Minute), now.Add(-20*time.Minute), now.Add(-20*time.Minute), time.Time{})
	appendIPv6RAStatusCondition(t, db, device.ID, "boot:new:0", 1,
		now.Add(-time.Minute), now.Add(-time.Minute), now, now.Add(-2*time.Minute))

	statuses, err := db.IPv6RAConditionStatuses(ctx, IPv6RAConditionWindows{
		Now: now, CursorFreshFor: 3 * time.Minute,
		RecentFor: 5 * time.Minute, QuietFor: 10 * time.Minute,
	})
	if err != nil || len(statuses) != 1 {
		t.Fatalf("statuses=%+v err=%v", statuses, err)
	}
	if statuses[0].State != IPv6RAConditionRecent || statuses[0].Occurrences != 1 ||
		statuses[0].LastObservedAt != now.Add(-time.Minute).UnixMilli() {
		t.Fatalf("newest epoch status=%+v", statuses[0])
	}
}

func TestIPv6RAConditionStatusesValidateWindowsAndReturnEmptyList(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	valid := IPv6RAConditionWindows{
		Now: now, CursorFreshFor: 3 * time.Minute,
		RecentFor: 5 * time.Minute, QuietFor: 10 * time.Minute,
	}
	invalid := []IPv6RAConditionWindows{
		{},
		{Now: now, CursorFreshFor: 0, RecentFor: time.Minute, QuietFor: time.Minute},
		{Now: now, CursorFreshFor: time.Minute, RecentFor: 0, QuietFor: time.Minute},
		{Now: now, CursorFreshFor: time.Minute, RecentFor: time.Minute, QuietFor: 0},
		{Now: now, CursorFreshFor: time.Minute, RecentFor: 2 * time.Minute, QuietFor: time.Minute},
	}
	for _, windows := range invalid {
		if _, err := db.IPv6RAConditionStatuses(ctx, windows); err == nil {
			t.Errorf("windows %+v were accepted", windows)
		}
	}
	statuses, err := db.IPv6RAConditionStatuses(ctx, valid)
	if err != nil {
		t.Fatal(err)
	}
	if statuses == nil || len(statuses) != 0 {
		t.Fatalf("empty statuses=%+v", statuses)
	}
}

func addIPv6RAStatusDevice(t *testing.T, db *DB, suffix byte) *Device {
	t.Helper()
	device := &Device{
		MAC:  fmt.Sprintf("02:00:00:00:01:%02x", suffix),
		Host: fmt.Sprintf("192.0.2.%d", suffix), Name: fmt.Sprintf("router-%d", suffix),
	}
	if err := db.UpsertDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	return device
}

func appendIPv6RAStatusCondition(t *testing.T, db *DB, deviceID int64,
	boot string, sourceID uint32, sourceAt, ingestedAt, cursorAt, gapAt time.Time) {
	t.Helper()
	event := ipv6RAStatusConditionEvent(deviceID, boot, sourceID, sourceAt, ingestedAt)
	cursor := IngestCursor{
		DeviceID: deviceID, Source: "openwrt-logd", BootID: boot,
		Cursor:    fmt.Sprintf("%d:%d", sourceID, sourceAt.UnixMilli()),
		UpdatedAt: cursorAt.UnixMilli(),
	}
	if !gapAt.IsZero() {
		cursor.ContinuityGapAt = gapAt.UnixMilli()
	}
	if _, err := db.AppendEventsAndCursor(context.Background(), []Event{event}, cursor); err != nil {
		t.Fatal(err)
	}
}

func appendIPv6RAStatusConditionWithoutCursor(t *testing.T, db *DB, deviceID int64,
	boot string, sourceID uint32, sourceAt, ingestedAt time.Time) {
	t.Helper()
	if _, err := db.AppendEvent(context.Background(),
		ipv6RAStatusConditionEvent(deviceID, boot, sourceID, sourceAt, ingestedAt)); err != nil {
		t.Fatal(err)
	}
}

func ipv6RAStatusConditionEvent(deviceID int64, boot string, sourceID uint32,
	sourceAt, ingestedAt time.Time) Event {
	sourceTime := sourceAt.UnixMilli()
	return Event{
		TS: sourceAt.Unix(), IngestedAt: ingestedAt.UnixMilli(), DeviceID: &deviceID,
		Category: "system", Severity: "warning",
		Event: EventOpenWRTIPv6RANoDefaultRoute, Source: "openwrt-logd",
		SourceBoot: boot, SourceID: EventOpenWRTIPv6RANoDefaultRouteSourceID,
		Detail: map[string]any{
			"message":  "odhcpd[81]: No default route present, setting ra_lifetime to 0!",
			"priority": uint32(28), "condition": "ipv6_ra_no_default_route",
			"occurrences": 1, "source_time_ms": sourceTime,
			"first_source_time_ms": sourceTime, "last_source_time_ms": sourceTime,
			"first_source_id": fmt.Sprint(sourceID), "last_source_id": fmt.Sprint(sourceID),
			"address_family": "ipv6", "router_advertisement_lifetime": 0,
		},
	}
}

func saveIPv6RAStatusCursor(t *testing.T, db *DB, deviceID int64, boot string,
	updatedAt, gapAt time.Time) {
	t.Helper()
	cursor := IngestCursor{
		DeviceID: deviceID, Source: "openwrt-logd", BootID: boot,
		Cursor: "empty", UpdatedAt: updatedAt.UnixMilli(),
	}
	if !gapAt.IsZero() {
		cursor.ContinuityGapAt = gapAt.UnixMilli()
	}
	if err := db.SaveIngestCursor(context.Background(), cursor); err != nil {
		t.Fatal(err)
	}
}
