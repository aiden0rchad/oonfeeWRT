package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/observability"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

type fakeLogIngestStore struct {
	cursors  map[int64]store.IngestCursor
	attempts [][]store.Event
	latest   []store.Event
	failNext bool
}

func (f *fakeLogIngestStore) LatestClientAssociationEvents(context.Context) ([]store.Event, error) {
	return append([]store.Event(nil), f.latest...), nil
}

func (f *fakeLogIngestStore) LoadIngestCursor(_ context.Context, deviceID int64,
	_ string) (store.IngestCursor, error) {
	if cursor, ok := f.cursors[deviceID]; ok {
		return cursor, nil
	}
	return store.IngestCursor{}, store.ErrNotFound
}

func (f *fakeLogIngestStore) AppendEventsAndCursor(_ context.Context, events []store.Event,
	cursor store.IngestCursor) (int, error) {
	f.attempts = append(f.attempts, append([]store.Event(nil), events...))
	if f.failNext {
		f.failNext = false
		return 0, errors.New("injected commit failure")
	}
	if f.cursors == nil {
		f.cursors = map[int64]store.IngestCursor{}
	}
	f.cursors[cursor.DeviceID] = cursor
	return len(events), nil
}

func TestLogIngestCursorAndRoamStateAdvanceOnlyAfterCommit(t *testing.T) {
	ctx := context.Background()
	db := &fakeLogIngestStore{cursors: map[int64]store.IngestCursor{}}
	ingestor := newLogIngestor(db)
	epoch := observability.LogEpoch{
		BootID: "11111111-2222-4333-8444-555555555555", PID: 81,
	}

	connectOne := observability.LogEntry{ID: 1, TimeMS: 100_000, Priority: 30,
		Message: "phy0-ap0: AP-STA-CONNECTED 02:00:00:00:00:01"}
	if err := ingestor.record(ctx, 1, time.UnixMilli(101_000), epoch,
		[]observability.LogEntry{connectOne}); err != nil {
		t.Fatal(err)
	}
	if got := db.attempts[0][0].Event; got != "client.connect" {
		t.Fatalf("first event=%q", got)
	}

	connectTwo := observability.LogEntry{ID: 7, TimeMS: 110_000, Priority: 30,
		Message: "phy1-ap0: AP-STA-CONNECTED 02:00:00:00:00:01"}
	db.failNext = true
	if err := ingestor.record(ctx, 2, time.UnixMilli(111_000), epoch,
		[]observability.LogEntry{connectTwo}); err == nil {
		t.Fatal("commit failure was ignored")
	}
	if _, ok := db.cursors[2]; ok {
		t.Fatal("failed page advanced the durable cursor")
	}
	if err := ingestor.record(ctx, 2, time.UnixMilli(112_000), epoch,
		[]observability.LogEntry{connectTwo}); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range db.attempts[1:] {
		if len(attempt) != 1 || attempt[0].Event != "client.roam" {
			t.Fatalf("retry lost roam correlation: %+v", attempt)
		}
		detail := attempt[0].Detail.(map[string]any)
		if detail["from_device_id"] != int64(1) || detail["to_device_id"] != int64(2) {
			t.Fatalf("roam detail=%+v", detail)
		}
	}
}

func TestLogIngestPersistsObservedEmptyCoverageAndResumesAfterRestart(t *testing.T) {
	ctx := context.Background()
	epoch := observability.LogEpoch{
		BootID: "11111111-2222-4333-8444-555555555555", PID: 81,
	}
	db := &fakeLogIngestStore{cursors: map[int64]store.IngestCursor{}}
	if err := newLogIngestor(db).record(ctx, 1, time.UnixMilli(100_000), epoch, nil); err != nil {
		t.Fatal(err)
	}
	if len(db.attempts) != 1 || len(db.attempts[0]) != 0 ||
		db.cursors[1].Cursor != emptyLogCursorPosition || db.cursors[1].UpdatedAt != 100_000 {
		t.Fatalf("empty observation was not persisted distinctly: attempts=%+v cursor=%+v", db.attempts, db.cursors[1])
	}

	row := observability.LogEntry{ID: 9, TimeMS: 101_000, Priority: 30, Message: "ordinary row"}
	if err := newLogIngestor(db).record(ctx, 1, time.UnixMilli(102_000), epoch,
		[]observability.LogEntry{row}); err != nil {
		t.Fatal(err)
	}
	if len(db.attempts) != 2 || len(db.attempts[1]) != 1 ||
		db.cursors[1].Cursor != "9:101000" || db.cursors[1].ContinuityGapAt != 102_000 {
		t.Fatalf("first row after empty restart was not ingested: attempts=%+v cursor=%+v", db.attempts, db.cursors[1])
	}
	detail := db.attempts[1][0].Detail.(map[string]any)
	if detail["ingest_gap"] != true ||
		detail["continuity_reason"] != "continuity after an observed-empty log page cannot be proven" {
		t.Fatalf("first page after empty did not disclose possible ring loss: %+v", detail)
	}
}

func TestLogIngestEmptyRingImmediatelyPersistsContinuityLoss(t *testing.T) {
	ctx := context.Background()
	epochA := observability.LogEpoch{
		BootID: "11111111-2222-4333-8444-555555555555", PID: 81,
	}
	epochB := observability.LogEpoch{
		BootID: "66666666-7777-4888-8999-aaaaaaaaaaaa", PID: 82,
	}
	for _, tc := range []struct {
		name  string
		empty observability.LogEpoch
	}{
		{name: "same producer epoch", empty: epochA},
		{name: "new producer epoch", empty: epochB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeLogIngestStore{cursors: map[int64]store.IngestCursor{}}
			ingestor := newLogIngestor(db)
			row := observability.LogEntry{ID: 1, TimeMS: 100_000, Priority: 30,
				Message: "ordinary row"}
			if err := ingestor.record(ctx, 1, time.UnixMilli(101_000), epochA,
				[]observability.LogEntry{row}); err != nil {
				t.Fatal(err)
			}

			db.failNext = true
			if err := ingestor.record(ctx, 1, time.UnixMilli(102_000), tc.empty, nil); err == nil {
				t.Fatal("failed empty-page commit advanced continuity state")
			}
			if got := db.cursors[1]; got.Cursor != "1:100000" || got.ContinuityGapAt != 0 {
				t.Fatalf("failed commit changed cursor: %+v", got)
			}
			if err := ingestor.record(ctx, 1, time.UnixMilli(103_000), tc.empty, nil); err != nil {
				t.Fatal(err)
			}
			got := db.cursors[1]
			wantBoot := (observability.LogCursor{Epoch: tc.empty, Generation: 1}).SourceBoot()
			if got.Cursor != emptyLogCursorPosition || got.BootID != wantBoot ||
				got.ContinuityGapAt != 103_000 {
				t.Fatalf("empty ring cursor=%+v, want boot=%q gap=103000", got, wantBoot)
			}
		})
	}
}

func TestLogIngestMarksGapsAndRedactsMessages(t *testing.T) {
	epoch := observability.LogEpoch{
		BootID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", PID: 9,
	}
	db := &fakeLogIngestStore{cursors: map[int64]store.IngestCursor{
		3: {DeviceID: 3, Source: openWRTLogSource, BootID: epoch.BootID + ":9:0",
			Cursor: "10:100000", UpdatedAt: 100_000},
	}}
	ingestor := newLogIngestor(db)
	rows := []observability.LogEntry{
		{ID: 20, TimeMS: 120_000, Priority: 27,
			Message: "daemon: password=secret token=abc Authorization=Bearer-value"},
		{ID: 21, TimeMS: 121_000, Priority: 28, Message: "ordinary warning"},
		{ID: 22, TimeMS: 122_000, Priority: 30,
			Message: `{"password":"persist-sentinel","wpa_passphrase":"persist-sentinel-two"}`},
		{ID: 23, TimeMS: 123_000, Priority: 30,
			Message: "-----BEGIN OPEN" + "SSH PRIVATE KEY-----"},
	}
	if err := ingestor.record(context.Background(), 3, time.UnixMilli(122_000), epoch, rows); err != nil {
		t.Fatal(err)
	}
	if len(db.attempts) != 1 || len(db.attempts[0]) != 4 {
		t.Fatalf("attempts=%+v", db.attempts)
	}
	detail := db.attempts[0][0].Detail.(map[string]any)
	if detail["ingest_gap"] != true || detail["continuity_reason"] == "" {
		t.Fatalf("gap detail=%+v", detail)
	}
	message := detail["message"].(string)
	if strings.Contains(message, "secret") || strings.Contains(message, "abc") ||
		strings.Contains(message, "Bearer-value") {
		t.Fatalf("sensitive message survived redaction: %q", message)
	}
	if db.attempts[0][0].Severity != "error" || db.attempts[0][1].Severity != "warning" {
		t.Fatalf("severities=%q,%q", db.attempts[0][0].Severity, db.attempts[0][1].Severity)
	}
	for _, event := range db.attempts[0] {
		blob, err := json.Marshal(event.Detail)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(blob), "persist-sentinel") || strings.Contains(string(blob), "PRIVATE KEY") {
			t.Fatalf("durable detail retained secret material: %s", blob)
		}
	}
}

func TestLogIngestClassifiesOnlyTheKnownIPv6RAWarning(t *testing.T) {
	deviceID := int64(7)
	correlator := observability.NewAssociationCorrelator()
	for _, tc := range []struct {
		name     string
		priority uint32
		message  string
		want     string
	}{
		{
			name: "current OpenWrt wording", priority: 28,
			message: "odhcpd[81]: No default route present, setting ra_lifetime to 0!",
			want:    store.EventOpenWRTIPv6RANoDefaultRoute,
		},
		{
			name: "alternate OpenWrt wording", priority: 28,
			message: "odhcpd: No default route present, overriding ra_lifetime!",
			want:    store.EventOpenWRTIPv6RANoDefaultRoute,
		},
		{
			name: "same words at error priority remain raw", priority: 27,
			message: "odhcpd[81]: No default route present, setting ra_lifetime to 0!",
			want:    "openwrt.log",
		},
		{
			name: "other odhcpd warning remains raw", priority: 28,
			message: "odhcpd[81]: unable to send router advertisement",
			want:    "openwrt.log",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := logStoreEvent(deviceID, 101_000, "boot:81:0", observability.LogEntry{
				ID: 9, TimeMS: 100_000, Priority: tc.priority, Message: tc.message,
			}, correlator)
			if event.Event != tc.want {
				t.Fatalf("event=%q, want %q", event.Event, tc.want)
			}
			if tc.want == store.EventOpenWRTIPv6RANoDefaultRoute {
				detail := event.Detail.(map[string]any)
				if event.Severity != "warning" ||
					event.SourceID != store.EventOpenWRTIPv6RANoDefaultRouteSourceID ||
					detail["priority"] != uint32(28) ||
					detail["occurrences"] != 1 || detail["condition"] != "ipv6_ra_no_default_route" ||
					detail["message"] != tc.message {
					t.Fatalf("classified event lost source truth: %+v", event)
				}
			}
		})
	}
}

func TestLogIngestCoalescingAdvancesAcrossUint32Wrap(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "log wrap condition"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	device := &store.Device{MAC: "02:00:00:00:00:71", Host: "192.0.2.71", Name: "wrap"}
	if err := d.Store.UpsertDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	epoch := observability.LogEpoch{
		BootID: "11111111-2222-4333-8444-555555555555", PID: 81,
	}
	message := "odhcpd[81]: No default route present, setting ra_lifetime to 0!"
	row := func(id uint32, at uint64) observability.LogEntry {
		return observability.LogEntry{ID: id, TimeMS: at, Priority: 28, Message: message}
	}
	ingestor := newLogIngestor(d.Store)
	initial := row(math.MaxUint32-1, 100_000)
	if err := ingestor.record(ctx, device.ID, time.UnixMilli(101_000), epoch,
		[]observability.LogEntry{initial}); err != nil {
		t.Fatal(err)
	}
	page := []observability.LogEntry{
		initial,
		row(math.MaxUint32, 101_000),
		row(0, 102_000),
		row(1, 103_000),
	}
	if err := ingestor.record(ctx, device.ID, time.UnixMilli(104_000), epoch, page); err != nil {
		t.Fatal(err)
	}
	if err := ingestor.record(ctx, device.ID, time.UnixMilli(105_000), epoch, page); err != nil {
		t.Fatal(err)
	}

	wrappedBoot := (observability.LogCursor{Epoch: epoch, Generation: 1}).SourceBoot()
	var count int64
	var lastID string
	if err := d.Store.SQL().QueryRowContext(ctx, `
SELECT json_extract(detail_json,'$.occurrences'), json_extract(detail_json,'$.last_source_id')
  FROM events WHERE event=? AND source_boot=?`,
		store.EventOpenWRTIPv6RANoDefaultRoute, wrappedBoot).Scan(&count, &lastID); err != nil {
		t.Fatal(err)
	}
	if count != 3 || lastID != "1" {
		t.Fatalf("wrapped condition occurrences=%d last_source_id=%q", count, lastID)
	}
	cursor, err := d.Store.LoadIngestCursor(ctx, device.ID, openWRTLogSource)
	if err != nil || cursor.BootID != wrappedBoot || cursor.Cursor != "1:103000" {
		t.Fatalf("wrapped cursor=%+v err=%v", cursor, err)
	}
}

func TestLogIngestRebasesAssociationCorrelationAfterClockOrProducerReset(t *testing.T) {
	ctx := context.Background()
	epoch := observability.LogEpoch{BootID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", PID: 9}
	db := &fakeLogIngestStore{cursors: map[int64]store.IngestCursor{}}
	ingestor := newLogIngestor(db)
	mac := "02:00:00:00:00:04"
	connect := observability.LogEntry{ID: 10, TimeMS: 200_000, Priority: 30,
		Message: "phy0-ap0: AP-STA-CONNECTED " + mac}
	if err := ingestor.record(ctx, 1, time.UnixMilli(201_000), epoch,
		[]observability.LogEntry{connect}); err != nil {
		t.Fatal(err)
	}
	disconnect := observability.LogEntry{ID: 11, TimeMS: 100_000, Priority: 30,
		Message: "phy0-ap0: AP-STA-DISCONNECTED " + mac}
	if err := ingestor.record(ctx, 1, time.UnixMilli(202_000), epoch,
		[]observability.LogEntry{disconnect}); err != nil {
		t.Fatal(err)
	}
	clockEvent := db.attempts[1][0]
	clockDetail := clockEvent.Detail.(map[string]any)
	if clockEvent.Event != "client.disconnect" || clockEvent.Action != "disconnect" ||
		clockDetail["producer_clock_regressed"] != true ||
		clockDetail["association_correlation_reset"] != true {
		t.Fatalf("backward-clock event=%+v", clockEvent)
	}

	restarted := observability.LogEntry{ID: 0, TimeMS: 50_000, Priority: 30,
		Message: "phy1-ap0: AP-STA-CONNECTED " + mac}
	newEpoch := observability.LogEpoch{BootID: epoch.BootID, PID: 10}
	if err := ingestor.record(ctx, 1, time.UnixMilli(203_000), newEpoch,
		[]observability.LogEntry{restarted}); err != nil {
		t.Fatal(err)
	}
	restartEvent := db.attempts[2][0]
	restartDetail := restartEvent.Detail.(map[string]any)
	if restartEvent.Event != "client.connect" || restartEvent.Action != "connect" ||
		restartDetail["producer_reset"] != true ||
		restartDetail["association_correlation_reset"] != true {
		t.Fatalf("producer-reset event=%+v", restartEvent)
	}
}

func TestLogIngestRestoresLatestDurableAssociation(t *testing.T) {
	oldDevice := int64(11)
	db := &fakeLogIngestStore{cursors: map[int64]store.IngestCursor{}, latest: []store.Event{{
		TS: 90, DeviceID: &oldDevice, ClientMAC: "02:00:00:00:00:09",
		Action: string(observability.WirelessConnect), InIface: "phy0-ap0",
	}}}
	ingestor := newLogIngestor(db)
	epoch := observability.LogEpoch{
		BootID: "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff", PID: 10,
	}
	row := observability.LogEntry{ID: 1, TimeMS: 100_000, Priority: 30,
		Message: "phy1-ap0: AP-STA-CONNECTED 02:00:00:00:00:09"}
	if err := ingestor.record(context.Background(), 12, time.UnixMilli(101_000), epoch,
		[]observability.LogEntry{row}); err != nil {
		t.Fatal(err)
	}
	if got := db.attempts[0][0].Event; got != "client.roam" {
		t.Fatalf("event=%q, want restored roam", got)
	}
}

func TestLogIngestRestartIgnoresLateCrossDeviceRowsAtMillisecondPrecision(t *testing.T) {
	currentDevice := int64(2)
	db := &fakeLogIngestStore{cursors: map[int64]store.IngestCursor{}, latest: []store.Event{{
		TS: 110, DeviceID: &currentDevice, ClientMAC: "02:00:00:00:00:09",
		Action: string(observability.WirelessConnect), InIface: "phy1-ap0",
		Detail: map[string]any{"source_time_ms": uint64(110_500)},
	}}}
	ingestor := newLogIngestor(db)
	epoch := observability.LogEpoch{
		BootID: "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff", PID: 10,
	}
	late := observability.LogEntry{ID: 1, TimeMS: 110_400, Priority: 30,
		Message: "phy0-ap0: AP-STA-CONNECTED 02:00:00:00:00:09"}
	if err := ingestor.record(context.Background(), 1, time.UnixMilli(111_000), epoch,
		[]observability.LogEntry{late}); err != nil {
		t.Fatal(err)
	}
	if got := db.attempts[0][0]; got.Action != "" || got.Event != "openwrt.log" {
		t.Fatalf("late raw row remained authoritative: %+v", got)
	}
	next := observability.LogEntry{ID: 1, TimeMS: 110_600, Priority: 30,
		Message: "phy2-ap0: AP-STA-CONNECTED 02:00:00:00:00:09"}
	if err := ingestor.record(context.Background(), 3, time.UnixMilli(112_000), epoch,
		[]observability.LogEntry{next}); err != nil {
		t.Fatal(err)
	}
	got := db.attempts[1][0]
	if got.Event != "client.roam" || got.Detail.(map[string]any)["from_device_id"] != int64(2) {
		t.Fatalf("restart state was rewound: %+v", got)
	}
}
