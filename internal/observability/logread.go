package observability

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxLogEntries = 4096
	maxLogMessage = 4096
)

// LogEntry is the exact non-streaming OpenWrt log.read row.
type LogEntry struct {
	Message  string `json:"msg"`
	ID       uint32 `json:"id"`
	Priority uint32 `json:"priority"`
	Source   uint32 `json:"source"`
	TimeMS   uint64 `json:"time"`
}

func (e LogEntry) Severity() uint32 { return e.Priority & 7 }
func (e LogEntry) Facility() uint32 { return e.Priority >> 3 }
func (e LogEntry) SourceID() string { return strconv.FormatUint(uint64(e.ID), 10) }

func DecodeLogRead(raw json.RawMessage) ([]LogEntry, error) {
	var response struct {
		Log []LogEntry `json:"log"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode log.read: %w", err)
	}
	if len(response.Log) > maxLogEntries {
		return nil, fmt.Errorf("decode log.read: %d rows exceeds %d", len(response.Log), maxLogEntries)
	}
	for i, entry := range response.Log {
		if !utf8.ValidString(entry.Message) || len(entry.Message) > maxLogMessage {
			return nil, fmt.Errorf("decode log.read: row %d has an invalid message", i)
		}
		if entry.TimeMS == 0 {
			return nil, fmt.Errorf("decode log.read: row %d has no realtime timestamp", i)
		}
		if entry.TimeMS > math.MaxInt64 {
			return nil, fmt.Errorf("decode log.read: row %d timestamp overflows int64", i)
		}
		if i > 0 && response.Log[i-1].ID != entry.ID &&
			response.Log[i-1].ID+1 != entry.ID {
			return nil, fmt.Errorf("decode log.read: rows %d and %d are not contiguous", i-1, i)
		}
	}
	return response.Log, nil
}

// LogEpoch distinguishes logd restarts from a u32 ID wrap. Generation handles
// an ID regression even when logd was replaced too quickly to observe a PID.
type LogEpoch struct {
	BootID string
	PID    int64
}

type LogCursor struct {
	Epoch      LogEpoch
	Generation uint64
	LastID     uint32
	LastTimeMS uint64
	Valid      bool
}

type LogAdvance struct {
	Entries        []LogEntry
	Cursor         LogCursor
	Reset          bool
	Gap            bool
	ClockRegressed bool
	Reason         string
}

func (c LogCursor) SourceBoot() string {
	return c.Epoch.BootID + ":" + strconv.FormatInt(c.Epoch.PID, 10) + ":" +
		strconv.FormatUint(c.Generation, 10)
}

func (c LogCursor) Position() string {
	return strconv.FormatUint(uint64(c.LastID), 10) + ":" + strconv.FormatUint(c.LastTimeMS, 10)
}

func ParseLogCursor(sourceBoot, position string) (LogCursor, error) {
	bootParts := strings.Split(sourceBoot, ":")
	positionParts := strings.Split(position, ":")
	if len(bootParts) != 3 || len(positionParts) != 2 || strings.TrimSpace(bootParts[0]) == "" {
		return LogCursor{}, fmt.Errorf("invalid log cursor encoding")
	}
	pid, err := strconv.ParseInt(bootParts[1], 10, 64)
	if err != nil || pid <= 0 {
		return LogCursor{}, fmt.Errorf("invalid log cursor pid")
	}
	generation, err := strconv.ParseUint(bootParts[2], 10, 64)
	if err != nil {
		return LogCursor{}, fmt.Errorf("invalid log cursor generation")
	}
	id, err := strconv.ParseUint(positionParts[0], 10, 32)
	if err != nil {
		return LogCursor{}, fmt.Errorf("invalid log cursor id")
	}
	ts, err := strconv.ParseUint(positionParts[1], 10, 64)
	if err != nil {
		return LogCursor{}, fmt.Errorf("invalid log cursor timestamp")
	}
	return LogCursor{Epoch: LogEpoch{BootID: bootParts[0], PID: pid}, Generation: generation,
		LastID: uint32(id), LastTimeMS: ts, Valid: true}, nil
}

// AdvanceLogCursor filters overlap from logd's ring-shaped reads and records
// resets/gaps explicitly. It never turns a missing interval into an empty one.
func AdvanceLogCursor(cursor LogCursor, epoch LogEpoch, rows []LogEntry) LogAdvance {
	out := LogAdvance{Cursor: cursor}
	if len(rows) == 0 {
		return out
	}
	if !cursor.Valid || cursor.Epoch != epoch {
		out.Reset = cursor.Valid
		if out.Reset {
			out.Reason = "log producer restarted"
		}
		out.Entries = rows
		out.Cursor = nextCursor(cursor, epoch, rows[len(rows)-1], out.Reset)
		return out
	}

	for i, row := range rows {
		if row.ID != cursor.LastID || row.TimeMS != cursor.LastTimeMS {
			continue
		}
		out.Entries = rows[i+1:]
		if len(out.Entries) > 0 && out.Entries[0].ID != cursor.LastID+1 {
			out.Gap, out.Reason = true, "log ring skipped IDs"
		}
		out.Cursor = cursor
		if len(out.Entries) > 0 {
			last := out.Entries[len(out.Entries)-1]
			out.Cursor = nextCursor(cursor, epoch, last, wrapped(cursor.LastID, last.ID))
		}
		markClockRegression(&out, cursor)
		return out
	}

	// An ID match with a different producer timestamp is reuse after an
	// unobserved logd restart, not overlap with the durable cursor.
	for _, row := range rows {
		if row.ID == cursor.LastID {
			out.Reset = true
			out.Reason = "log ID was reused"
			out.Entries = rows
			out.Cursor = nextCursor(cursor, epoch, rows[len(rows)-1], true)
			return out
		}
	}

	last := rows[len(rows)-1]
	wrap := wrapped(cursor.LastID, last.ID)
	if last.ID < cursor.LastID && !wrap {
		out.Reset = true
		out.Reason = "log ID regressed"
		out.Entries = rows
		out.Cursor = nextCursor(cursor, epoch, last, true)
		return out
	}

	out.Entries = rows
	out.Gap = rows[0].ID != cursor.LastID+1
	if out.Gap {
		out.Reason = "log ring no longer contains the cursor"
	}
	out.Cursor = nextCursor(cursor, epoch, last, wrap)
	markClockRegression(&out, cursor)
	return out
}

func markClockRegression(out *LogAdvance, cursor LogCursor) {
	if len(out.Entries) == 0 || out.Entries[0].TimeMS >= cursor.LastTimeMS {
		return
	}
	out.ClockRegressed = true
	if out.Reason == "" {
		out.Reason = "router log clock moved backward"
	}
}

func nextCursor(prev LogCursor, epoch LogEpoch, last LogEntry, reset bool) LogCursor {
	generation := prev.Generation
	if reset {
		generation++
	}
	return LogCursor{Epoch: epoch, Generation: generation, LastID: last.ID,
		LastTimeMS: last.TimeMS, Valid: true}
}

func wrapped(before, after uint32) bool {
	return before >= 0xf0000000 && after <= 0x0fffffff
}
