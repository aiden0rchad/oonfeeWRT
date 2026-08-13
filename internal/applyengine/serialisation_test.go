package applyengine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// uci.apply is globally serialised on the device: while one session's rollback
// is armed, another session's armed apply is refused with status 6. That is
// good news for safety — two controllers cannot clobber each other's snapshot —
// but it creates a trap, because status 6 there means "an apply is already
// armed" and NOT an authorization failure. Routing it to the ACL-error path
// would report a transient scheduling conflict as a permissions problem and
// stop retrying something that will succeed in ninety seconds.
func TestSecondArmedApplyIsRefusedAndTheFirstStillConfirms(t *testing.T) {
	ctx := context.Background()
	a := dial(t)
	b := dial(t)
	seed(t, a, "oonfeewrt_probe", "BASE_SER")
	seed(t, b, "oonfeewrt_probe2", "BASE_SER2")

	eng := New()
	eng.ConfirmInterval = 200 * time.Millisecond
	eng.RevertGrace = 500 * time.Millisecond

	// A applies and holds the window open by blocking inside its health check
	// until B has tried.
	bTried := make(chan struct{})
	aDone := make(chan Result, 1)
	go func() {
		res, err := eng.Apply(ctx, a, Plan{
			Timeout: 20 * time.Second,
			Ops: []Op{{Kind: OpSet, Config: "oonfeewrt_probe", Section: "probe",
				Values: map[string]string{"marker": "A_WINS"}}},
		}, func(context.Context, *ubus.Client) error {
			<-bTried // keep A's rollback armed while B attempts its apply
			return nil
		})
		if err != nil {
			t.Errorf("A's apply: %v", err)
		}
		aDone <- res
	}()

	// Give A time to arm its rollback before B tries.
	time.Sleep(700 * time.Millisecond)

	_, errB := eng.Apply(ctx, b, Plan{
		Timeout: 5 * time.Second,
		Ops: []Op{{Kind: OpSet, Config: "oonfeewrt_probe2", Section: "probe",
			Values: map[string]string{"marker": "B_LOSES"}}},
	}, func(context.Context, *ubus.Client) error { return nil })
	close(bTried)

	if errB == nil {
		t.Fatal("B's apply should have been refused while A's rollback was armed")
	}
	if !strings.Contains(errB.Error(), "already armed") {
		t.Fatalf("the refusal must be reported as a scheduling conflict, not a "+
			"permissions failure; got: %v", errB)
	}

	res := <-aDone
	if res.Outcome != Applied {
		t.Fatalf("A's apply should still confirm normally; got %s", res)
	}

	// And B left nothing staged behind to ride along on its next apply.
	var changes struct {
		Changes map[string][]any `json:"changes"`
	}
	if err := b.Call(ctx, "uci", "changes", struct{}{}, &changes); err != nil {
		t.Fatalf("uci.changes: %v", err)
	}
	if len(changes.Changes) != 0 {
		t.Errorf("a refused apply must not leave a staged delta behind, got %v",
			changes.Changes)
	}
}
