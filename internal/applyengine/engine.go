package applyengine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// HealthCheck decides whether an applied change is actually good. Returning an
// error means "do not confirm", and the device then reverts itself — the cheap
// failure path the APPLY→HEALTH→CONFIRM ordering exists to buy.
//
// Two hard rules, both from measurement:
//
//  1. Do NOT trust uci.apply's status. An apply that killed dnsmasq still
//     returned 0. Check the thing that matters — interfaces up, SSIDs on air, a
//     name resolving — not the call that changed it.
//  2. Do NOT use uci.get here. The client passed in is the APPLYING session,
//     because inside the confirmation window rpcd gives every new login that
//     same token, so an independent session does not exist yet. uci reads on it
//     are overlaid with its own staged delta and would bless a change that is
//     not really applied. Read RUNTIME state instead: network.interface,
//     iwinfo, hostapd, or a file.exec probe.
type HealthCheck func(ctx context.Context, verify *ubus.Client) error

// Engine applies plans to one device.
type Engine struct {
	// ConfirmInterval is how often uci.confirm is retried inside the window.
	ConfirmInterval time.Duration

	// RevertGrace is how long past the rollback timeout to wait before
	// verifying a revert. The device restores config a little before the
	// service is usable again — measured at ~12s between the two — so a
	// verifier that samples too eagerly misreads a good revert as a bad one.
	RevertGrace time.Duration

	// Now is injectable for tests.
	Now func() time.Time
}

// New returns an Engine with defaults drawn from measured device timing.
func New() *Engine {
	return &Engine{
		ConfirmInterval: 3 * time.Second,
		RevertGrace:     15 * time.Second,
		Now:             time.Now,
	}
}

// Apply runs the full state machine against one device.
//
// applier is the session that stages and applies; it is also the ONLY session
// that may confirm. verify must produce independent sessions for reading state.
func (e *Engine) Apply(ctx context.Context, applier *ubus.Client, plan Plan, health HealthCheck) (res Result, err error) {
	res = Result{Started: e.now()}
	// Named results, so this lands on what the caller receives rather than on
	// a local that every return path discards.
	defer func() { res.Finished = e.now() }()

	if len(plan.Ops) == 0 {
		return res, errors.New("applyengine: empty plan")
	}

	// ---- PREFLIGHT ----------------------------------------------------
	if err := e.preflight(ctx, applier, plan); err != nil {
		return res, err
	}

	// ---- STAGE --------------------------------------------------------
	// Staged only. Never commit here: uci.apply is what commits the delta
	// together with the rollback snapshot, so committing first leaves nothing
	// staged, makes the snapshot equal the new state, and silently protects
	// nothing.
	if err := e.stage(ctx, applier, plan); err != nil {
		// Batch runs every op, so a failure part-way leaves its predecessors
		// staged. Clear them, or they ride along on the next apply.
		e.discardStaged(ctx, applier, plan)
		return res, fmt.Errorf("stage: %w", err)
	}
	if err := e.verifyStaged(ctx, applier, plan); err != nil {
		_ = e.revertStaged(ctx, applier, plan)
		return res, err
	}

	// ---- APPLY --------------------------------------------------------
	// Hold the confirm window across apply→confirm: inside it the transport
	// must not re-authenticate, because a new token cannot confirm and the
	// change would revert while we believed we were recovering.
	endWindow := applier.BeginConfirmWindow()
	defer endWindow()

	timeout := plan.timeout()
	err = applier.Call(ctx, "uci", "apply", map[string]any{
		"rollback": true, "timeout": int(timeout.Seconds()),
	}, nil)
	if err != nil {
		_ = e.revertStaged(ctx, applier, plan)
		// status 6 here is NOT an authorization failure: uci.apply is globally
		// serialised and refuses a second armed apply while one is pending.
		var se *ubus.StatusError
		if errors.As(err, &se) && se.Status == ubus.StatusPermissionDenied {
			return res, fmt.Errorf("another apply is already armed on this device; retry after its window: %w", err)
		}
		return res, fmt.Errorf("apply: %w", err)
	}

	// ---- HEALTH (before confirm, timer still armed) --------------------
	healthErr := e.runHealth(ctx, applier, health)
	if healthErr != nil {
		// Do nothing. Declining to confirm is the whole point of the ordering:
		// the device reverts itself and we pay nothing.
		endWindow()
		out := e.awaitRevert(ctx, applier, plan, timeout,
			fmt.Sprintf("health check failed: %v", healthErr))
		e.discardStaged(ctx, applier, plan)
		out.HealthErr = healthErr
		out.Started = res.Started
		return out, nil
	}

	// ---- CONFIRM (on the applying session, always) ---------------------
	confirmErr := e.confirmPoll(ctx, applier, timeout)
	endWindow()
	if confirmErr == nil {
		res.Outcome = Applied
		res.Reason = "health passed and confirm landed"
		return res, nil
	}
	out := e.awaitRevert(ctx, applier, plan, timeout,
		fmt.Sprintf("confirm never landed: %v", confirmErr))
	e.discardStaged(ctx, applier, plan)
	out.Started = res.Started
	return out, nil
}

// discardStaged clears the rejected delta from the applying session.
//
// A fired rollback restores /etc/config but leaves the applying session's
// staged delta in place — and uci.apply is one all-or-nothing transaction
// across everything staged. So a rejected change left staged here rides along
// on the NEXT apply from this client and silently lands, minutes later, having
// already been reported as reverted. Reproduced before this call existed.
func (e *Engine) discardStaged(ctx context.Context, applier *ubus.Client, plan Plan) {
	for _, cfg := range plan.Configs() {
		_ = applier.Call(ctx, "uci", "revert", map[string]any{"config": cfg}, nil)
	}
}

// preflight refuses to touch a device someone else is mid-edit on.
func (e *Engine) preflight(ctx context.Context, c *ubus.Client, plan Plan) error {
	dirty, err := ForeignDirtyConfigs(ctx, c)
	if err != nil {
		// Not fatal on its own: the grant may be absent on an older ACL. But
		// say so, because the gate is off.
		return fmt.Errorf("preflight: cannot check for foreign edits "+
			"(grant file.list on /tmp/.uci): %w", err)
	}
	if len(dirty) > 0 {
		return &DirtyError{Configs: dirty}
	}
	if TouchesManagementPath(plan) && !plan.AcknowledgeTraversal {
		return errors.New("preflight: this change touches the management path; " +
			"set AcknowledgeTraversal to proceed")
	}
	return nil
}

// ForeignDirtyConfigs lists configs with uncommitted LuCI/SSH edits.
//
// uci.changes is useless for this: rpcd scopes staged deltas to the calling
// session's savedir, so a foreign edit is invisible there. /tmp/.uci is the
// system savedir, and each file is named for its config. Zero-length files are
// stale leftovers and must be filtered — three were present on a device with
// exactly one real pending change.
func ForeignDirtyConfigs(ctx context.Context, c *ubus.Client) ([]string, error) {
	var out struct {
		Entries []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			Type string `json:"type"`
		} `json:"entries"`
	}
	if err := c.Call(ctx, "file", "list", map[string]any{"path": "/tmp/.uci"}, &out); err != nil {
		var se *ubus.StatusError
		if errors.As(err, &se) && se.Status == ubus.StatusNotFound {
			return nil, nil // no savedir yet: nothing staged anywhere
		}
		return nil, err
	}
	var dirty []string
	for _, e := range out.Entries {
		if e.Size > 0 {
			dirty = append(dirty, e.Name)
		}
	}
	return dirty, nil
}

// touchesManagementPath is conservative: any change to the network config can
// move the address we are talking to.
// TouchesManagementPath reports a change to the configs that carry the path the
// controller reaches this device through.
//
// Exported so callers use THIS definition rather than writing their own. The
// daemon had its own copy briefly, with a wider set than this one had, and two
// guards that disagree about what they guard is worse than one — the operator
// gets warned about a change the engine then refuses for a different reason, or
// worse, is not warned about one it allows.
//
// `firewall` counts alongside `network`: a zone whose input policy is REJECT
// blocks the controller just as effectively as a broken interface does, and the
// zone we render for a new network defaults to exactly that.
func TouchesManagementPath(plan Plan) bool {
	for _, op := range plan.Ops {
		if op.Config == "network" || op.Config == "firewall" {
			return true
		}
	}
	return false
}

// withLists folds list options into the values map as JSON arrays, which is how
// rpcd's uci.set expresses `list foo 'a'` / `list foo 'b'`.
//
// The alternative — a single space-joined string — is accepted and stored and
// then silently ignored by the consumer. See Op.Lists.
func withLists(values map[string]string, lists map[string][]string) map[string]any {
	out := make(map[string]any, len(values)+len(lists))
	for k, v := range values {
		out[k] = v
	}
	for k, v := range lists {
		out[k] = v
	}
	return out
}

func (e *Engine) stage(ctx context.Context, c *ubus.Client, plan Plan) error {
	calls := make([]ubus.Invocation, 0, len(plan.Ops))
	for _, op := range plan.Ops {
		args := map[string]any{"config": op.Config}
		switch op.Kind {
		case OpAdd:
			args["type"] = op.Type
			if op.Name != "" {
				args["name"] = op.Name
			}
			args["values"] = withLists(withOwnership(op.Values), op.Lists)
		case OpSet:
			args["section"] = op.Section
			args["values"] = withLists(withOwnership(op.Values), op.Lists)
		case OpDelete:
			args["section"] = op.Section
			if op.Option != "" {
				args["option"] = op.Option
			}
		default:
			return fmt.Errorf("unknown op kind %q", op.Kind)
		}
		calls = append(calls, ubus.Invocation{
			Object: "uci", Method: string(op.Kind), Args: args})
	}
	results, err := c.Batch(ctx, calls)
	if err != nil {
		return err
	}
	for i, r := range results {
		if r.Err != nil {
			return fmt.Errorf("op %d (%s %s): %w", i, plan.Ops[i].Kind, plan.Ops[i].Config, r.Err)
		}
	}
	return nil
}

// withOwnership stamps every section we write. Sections without this tag are
// somebody else's and must never be rewritten.
func withOwnership(v map[string]string) map[string]string {
	out := make(map[string]string, len(v)+1)
	for k, val := range v {
		out[k] = val
	}
	out[OwnershipTag] = "1"
	return out
}

// verifyStaged checks the device staged what we asked, before we make it live.
func (e *Engine) verifyStaged(ctx context.Context, c *ubus.Client, plan Plan) error {
	var out struct {
		Changes map[string][]any `json:"changes"`
	}
	if err := c.Call(ctx, "uci", "changes", struct{}{}, &out); err != nil {
		return fmt.Errorf("verify staged: %w", err)
	}
	want := map[string]bool{}
	for _, cfg := range plan.Configs() {
		want[cfg] = true
		if len(out.Changes[cfg]) == 0 {
			return fmt.Errorf("verify staged: nothing staged for %q; refusing to apply", cfg)
		}
	}
	// apply commits EVERYTHING staged on this session, so an extra config in
	// the delta would be applied too. Refuse rather than carry it along.
	for cfg := range out.Changes {
		if !want[cfg] {
			return fmt.Errorf("verify staged: unexpected staged changes for %q; "+
				"apply is all-or-nothing and would commit them too", cfg)
		}
	}
	return nil
}

func (e *Engine) revertStaged(ctx context.Context, c *ubus.Client, plan Plan) error {
	for _, cfg := range plan.Configs() {
		_ = c.Call(ctx, "uci", "revert", map[string]any{"config": cfg}, nil)
	}
	return nil
}

// runHealth runs the caller's check while the rollback timer is still armed.
//
// It deliberately hands over the APPLYING session, because inside the window
// there is no other one to hand over: rpcd returns the applying token to every
// new login until the timer resolves. Attempting to "get a fresh session" here
// yields the same session and — if you then tidy it up — destroys the only
// thing that can confirm. That is not theoretical; it is what this engine did
// before, and it turned a healthy apply into a revert.
//
// The consequence for callers is in HealthCheck's doc: inside the window, check
// RUNTIME state (network.interface, iwinfo, hostapd, an exec probe), never
// uci.get, because uci reads are overlaid with this session's own staged delta
// and will happily confirm a change that is not really there.
func (e *Engine) runHealth(ctx context.Context, applier *ubus.Client, health HealthCheck) error {
	if health == nil {
		return nil
	}
	return health(ctx, applier)
}

// confirmPoll retries uci.confirm until it lands or the window closes.
//
// Always on the applying session. Reconnecting the socket is fine; the token,
// not the connection, is what confirm is bound to.
func (e *Engine) confirmPoll(ctx context.Context, applier *ubus.Client, timeout time.Duration) error {
	deadline := e.now().Add(timeout)
	var last error
	for e.now().Before(deadline) {
		err := applier.Call(ctx, "uci", "confirm", struct{}{}, nil)
		if err == nil {
			return nil
		}
		last = err
		// A wrong-session confirm returns status 6 and does NOT cancel the
		// timer; there is no recovery, so stop early rather than burn the
		// window on a call that cannot succeed.
		var se *ubus.StatusError
		if errors.As(err, &se) && se.Status == ubus.StatusPermissionDenied {
			return fmt.Errorf("confirm refused: not the applying session (%w)", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.ConfirmInterval):
		}
	}
	if last == nil {
		last = errors.New("window expired")
	}
	return last
}

// awaitRevert waits out the timer and then checks, from a fresh session,
// whether the device actually reverted.
//
// It assumes nothing. An rpcd restart inside the window destroys the timer, so
// "we did not confirm" does not imply "it reverted" — that is the Unknown
// outcome, and it is the only alarming one.
func (e *Engine) awaitRevert(ctx context.Context, applier *ubus.Client, plan Plan, timeout time.Duration, reason string) Result {
	res := Result{Started: e.now(), Reason: reason}

	select {
	case <-ctx.Done():
		res.Outcome = Unknown
		res.Stranded = true
		res.Reason = reason + " (and the wait was cancelled before verification)"
		res.Finished = e.now()
		return res
	case <-time.After(timeout + e.RevertGrace):
	}

	verify, err := applier.FreshSession(ctx)
	if err != nil {
		res.Outcome = Unknown
		res.Stranded = true
		res.Reason = fmt.Sprintf("%s; could not open a session to verify the revert: %v", reason, err)
		res.Finished = e.now()
		return res
	}
	// Close, NOT Destroy. While any rollback is armed anywhere on the device —
	// including one belonging to a second controller or a LuCI apply — rpcd
	// hands every new login the APPLYING session's token, and we cannot tell
	// from here whose it is. Destroying it would revert a third party's change.
	// A lingering token costs 300s of idle timeout; the alternative costs
	// somebody else's config.
	defer verify.Close()

	stillThere, checked, checkErr := planStillApplied(ctx, verify, plan)
	res.Finished = e.now()
	switch {
	case checkErr == nil && !checked:
		res.Outcome = Unknown
		res.Stranded = true
		res.Reason = reason + "; the plan has nothing readable to verify " +
			"(deletes only), so the revert could not be confirmed"
	case checkErr != nil:
		res.Outcome = Unknown
		res.Stranded = true
		res.Reason = fmt.Sprintf("%s; could not verify: %v", reason, checkErr)
	case stillThere:
		res.Outcome = Unknown
		res.Stranded = true
		res.Reason = reason + "; the change is STILL PRESENT — the device did not " +
			"revert (rpcd likely restarted inside the window). Reverse it by " +
			"applying the previous model."
	default:
		res.Outcome = Reverted
		res.Reason = reason + "; the device reverted itself"
	}
	return res
}

// planStillApplied reports whether the plan's writes survive on the device.
// planStillApplied reports whether the plan's writes survive, and whether it
// was able to check anything at all. "Nothing to check" must not read as
// "reverted" — a plan of pure deletes leaves no value to look for, and
// answering Reverted there is a guess wearing a verdict's clothes.
func planStillApplied(ctx context.Context, verify *ubus.Client, plan Plan) (still, checked bool, err error) {
	for _, op := range plan.Ops {
		if op.Kind == OpDelete || len(op.Values) == 0 {
			continue
		}
		section := op.Section
		if section == "" {
			section = op.Name
		}
		if section == "" {
			continue
		}
		for k, want := range op.Values {
			var out struct {
				Value string `json:"value"`
			}
			callErr := verify.Call(ctx, "uci", "get", map[string]any{
				"config": op.Config, "section": section, "option": k}, &out)
			if callErr != nil {
				var se *ubus.StatusError
				if errors.As(callErr, &se) &&
					(se.Status == ubus.StatusNotFound || se.Status == ubus.StatusNoData) {
					checked = true
					continue // gone: consistent with a revert
				}
				return false, checked, callErr
			}
			checked = true
			if out.Value == want {
				return true, checked, nil
			}
		}
	}
	return false, checked, nil
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
