package collector

import (
	"context"
	"testing"
	"time"
)

// Discarding a session must not discard what it cost.
//
// The counters live on the ubus client, so closing one without banking its
// totals loses every request and byte that session made. fail() has always
// banked them; closeClient() did not — and closeClient is what runs when a
// device's address changes under the same device id, because a session token is
// not portable between hosts.
//
// The consequence is worse than a smaller number. Overhead derives
// NonPollRequests as Requests minus Polls, and polls are counted on the poller,
// which survives. Drop the requests, keep the polls, and the difference goes
// negative and is clamped to zero — so the readout whose stated purpose is to
// surface calls that escaped the batch reports none, precisely when a session
// has just been thrown away.
func TestARediallDoesNotEraseWhatTheOldSessionCost(t *testing.T) {
	rec := newRecorder()
	c := New(rec, fastOptions())
	connect := mockConnect(t)
	c.Add(Target{DeviceID: 1, MAC: "aa:bb:cc:dd:ee:ff", Name: "ap1", Connect: connect})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	rec.nextWithAPs(t, 5*time.Second)
	before, ok := c.Overhead(1)
	if !ok {
		t.Fatal("no overhead recorded")
	}
	if before.Requests == 0 {
		t.Fatal("no requests were counted, so this test cannot detect losing them")
	}

	// The same device id at a new address: Add sees a different MAC and drops
	// the cached session, which is the real call site for closeClient.
	c.Add(Target{DeviceID: 1, MAC: "aa:bb:cc:dd:ee:00", Name: "ap1", Connect: connect})

	after, _ := c.Overhead(1)
	if after.Requests < before.Requests {
		t.Errorf("re-dialling lost %d request(s): %d before, %d after",
			before.Requests-after.Requests, before.Requests, after.Requests)
	}
	if after.BytesOut < before.BytesOut {
		t.Errorf("re-dialling lost %d byte(s) of accounted traffic",
			before.BytesOut-after.BytesOut)
	}
	// And the derived figure stays sane: polls survive the re-dial, so requests
	// must too, or this reads as "every request was a poll" on a device that
	// just threw a session away.
	if after.Requests < after.Polls {
		t.Errorf("requests (%d) fell below polls (%d), which zeroes the "+
			"escaped-call detector", after.Requests, after.Polls)
	}
}
