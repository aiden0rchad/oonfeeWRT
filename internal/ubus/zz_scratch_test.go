package ubus

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestScratchErrorCodeClassification(t *testing.T) {
	codes := map[int]string{
		-32000: "Object not found",
		-32001: "Session not found",
		-32002: "Access denied",
		-32003: "ubus request timed out",
		-32004: "Operation not supported",
		-32005: "Unknown error",
		-32006: "Connection failed",
		-32601: "Method not found",
	}
	for code, msg := range codes {
		raw := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":` +
			itoa(code) + `,"message":"` + msg + `"}}`)
		var r rpcResponse
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		_, _, err := decodeFrame("hostapd.wlan0", "get_status", &r)
		var de *DeniedError
		var pe *ProtocolError
		t.Logf("code %d: err=%T (%v) denied=%v protocol=%v IsPermanent=%v",
			code, err, err, errors.As(err, &de), errors.As(err, &pe), IsPermanent(err))
	}
}

func itoa(i int) string {
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// Does the batch retry path lose calls when IDs grow a digit?
func TestScratchRebuildCanShrinkChunk(t *testing.T) {
	c := New(Options{Host: "127.0.0.1:1"})
	// Pack calls so the chunk lands within a couple of bytes of the ceiling.
	pad := make([]byte, 0)
	_ = pad
	calls := make([]Invocation, 0, 4000)
	for i := 0; i < 4000; i++ {
		calls = append(calls, Invocation{Object: "uci", Method: "get",
			Args: map[string]any{"config": "network"}})
	}
	end1, buf1, _ := c.buildChunk(calls, 0)
	t.Logf("first build: end=%d bytes=%d (limit %d)", end1, len(buf1), maxBatchBytes)
	chunk := calls[:end1]
	// Simulate the retry: rebuild the same chunk after nextID has advanced.
	end2, buf2, _ := c.buildChunk(chunk, 0)
	t.Logf("rebuild:     end=%d of %d, bytes=%d", end2, len(chunk), len(buf2))
	if end2 != len(chunk) {
		t.Errorf("rebuild dropped %d calls; resendChunk will report a length mismatch",
			len(chunk)-end2)
	}
}
