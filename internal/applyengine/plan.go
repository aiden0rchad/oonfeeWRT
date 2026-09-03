// Package applyengine drives a config change onto one device and decides,
// honestly, what happened to it.
//
// The state machine is APPLY → HEALTH → CONFIRM, in that order, and the
// ordering is the whole point: health runs while the device's rollback timer is
// still armed, so a failed change costs nothing — we simply decline to confirm
// and the device restores itself. Confirming first and reversing afterwards is
// the expensive path, and it is what the design originally specified.
package applyengine

import (
	"fmt"
	"time"
)

// DefaultTimeout is the rollback window requested from the device. It must be
// long enough for a service to come back and for health to run, and short
// enough that a stranded operator is not waiting long.
const DefaultTimeout = 90 * time.Second

// DefaultRevertGrace is how long past the rollback window to wait before
// checking whether the device actually reverted.
//
// Exported so that callers who bound an apply with their own deadline can
// derive a floor instead of guessing one. A budget shorter than
// DefaultTimeout+DefaultRevertGrace cannot see an apply through: the context
// expires while the device's own timer is still running, and every apply that
// needs the full window ends Unknown and Stranded — the engine's one alarming
// outcome — with the change still on the device.
const DefaultRevertGrace = 15 * time.Second

// MinApplyBudget is the shortest deadline an apply can be given and still reach
// a definite answer.
func MinApplyBudget() time.Duration { return DefaultTimeout + DefaultRevertGrace }

// Op is a staged UCI operation. Nothing here commits: staging is what makes
// uci.apply able to snapshot a pre-change state, and a manual commit first
// silently disarms the rollback.
type Op struct {
	Kind    OpKind
	Config  string
	Section string
	Type    string            // Add only
	Name    string            // Add only; named sections keep diffs readable
	Values  map[string]string // Add and Set
	// Lists are UCI list options. A list is NOT a string with spaces in it:
	// uci.set accepts the string form silently, stores it, and netifd then
	// ignores it — a failure with no error anywhere in the chain.
	Lists  map[string][]string
	Option string // Delete of a single option

	// Patch marks an option-only Set on a pre-existing section that remains
	// operator-owned. Ordinary Add/Set operations are stamped with OwnershipTag;
	// a Patch is deliberately not. It is invalid on Add, whole-section Delete,
	// or a Set containing the ownership marker.
	Patch bool
}

type OpKind string

const (
	OpAdd    OpKind = "add"
	OpSet    OpKind = "set"
	OpDelete OpKind = "delete"
)

// OwnershipTag is written onto every section we create. Anything without it was
// written by a human in LuCI or over SSH and is read for display, never
// rewritten. Verified on hardware: the option survives commit, apply and a
// service reload, and fw4/netifd ignore the unknown key.
const OwnershipTag = "oonfeewrt"

// Plan is one atomic change to one device.
//
// uci.apply is a single all-or-nothing transaction across every staged config,
// so a Plan may span configs freely — but it cannot be split into independently
// gated applies. The ordering of Ops is a staging order, not a sequence of
// commits.
type Plan struct {
	Ops []Op

	// Timeout is the rollback window. Zero uses DefaultTimeout.
	Timeout time.Duration

	// AcknowledgeTraversal must be set when the change touches the network path
	// the controller manages this device through. Without it, PREFLIGHT refuses:
	// severing your own management path is a decision, not an accident.
	AcknowledgeTraversal bool
}

func (p Plan) timeout() time.Duration {
	if p.Timeout <= 0 {
		return DefaultTimeout
	}
	return p.Timeout
}

// Configs lists the distinct UCI configs the plan touches, in first-seen order.
func (p Plan) Configs() []string {
	seen := map[string]bool{}
	var out []string
	for _, op := range p.Ops {
		if !seen[op.Config] {
			seen[op.Config] = true
			out = append(out, op.Config)
		}
	}
	return out
}

// Outcome is deliberately three-valued. Collapsing it to success/failure is
// what makes a controller lie: "we could not confirm" and "the change is gone"
// are different facts, and so is "we could not confirm and it is still there".
type Outcome string

const (
	// Applied: health passed and confirm landed. The change is permanent.
	Applied Outcome = "applied"

	// Reverted: we declined to confirm, or could not, and a fresh session
	// verified the device restored itself. This is the safety net working —
	// present it as a failed change, not as a broken controller.
	Reverted Outcome = "reverted"

	// Unknown: confirm did not land AND the change is still present on a fresh
	// read. Reachable whenever rpcd restarts inside the window, which destroys
	// both the session that would confirm and the timer that would revert.
	// Never render this as Applied.
	Unknown Outcome = "unknown"
)

// Result is what the engine learned, not what it hoped.
type Result struct {
	Outcome Outcome
	Reason  string

	// HealthErr is why we declined to confirm, when we did.
	HealthErr error

	// Stranded is set with Unknown: the change is live and unconfirmed, and the
	// only way back is to apply the previous model.
	Stranded bool

	Started  time.Time
	Finished time.Time
}

func (r Result) String() string {
	if r.Reason == "" {
		return string(r.Outcome)
	}
	return fmt.Sprintf("%s: %s", r.Outcome, r.Reason)
}

// DirtyError reports foreign uncommitted edits found by PREFLIGHT.
//
// uci.changes cannot see these: rpcd scopes staged deltas to a per-session
// savedir while LuCI and the CLI use /tmp/.uci, so the documented "abort if
// uci.changes is dirty" gate was structurally blind. We list /tmp/.uci instead.
type DirtyError struct {
	Configs []string
}

func (e *DirtyError) Error() string {
	return fmt.Sprintf("device has unsaved changes in %v — "+
		"someone is mid-edit in LuCI or over SSH", e.Configs)
}
