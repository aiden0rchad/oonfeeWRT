// Package ubus is the transport to a managed OpenWrt device: JSON-RPC over
// uhttpd's /ubus endpoint, as rpcd exposes it.
//
// Nearly every rule in this package traces to a behaviour measured on real
// hardware and recorded in docs/IMPLEMENTATION.md §14. Where a rule looks
// arbitrary, it is not — it is a device behaviour that bites silently.
package ubus

import (
	"errors"
	"fmt"
)

// Status is the ubus-level result code carried *inside* a successful JSON-RPC
// response, as the first element of the result array.
type Status int

const (
	StatusOK               Status = 0
	StatusInvalidCommand   Status = 1
	StatusInvalidArgument  Status = 2
	StatusMethodNotFound   Status = 3
	StatusNotFound         Status = 4
	StatusNoData           Status = 5
	StatusPermissionDenied Status = 6
	StatusTimeout          Status = 7
	StatusNotSupported     Status = 8
	StatusUnknownError     Status = 9
	StatusConnectionFailed Status = 10
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusInvalidCommand:
		return "INVALID_COMMAND"
	case StatusInvalidArgument:
		return "INVALID_ARGUMENT"
	case StatusMethodNotFound:
		return "METHOD_NOT_FOUND"
	case StatusNotFound:
		return "NOT_FOUND"
	case StatusNoData:
		return "NO_DATA"
	case StatusPermissionDenied:
		return "PERMISSION_DENIED"
	case StatusTimeout:
		return "TIMEOUT"
	case StatusNotSupported:
		return "NOT_SUPPORTED"
	case StatusUnknownError:
		return "UNKNOWN_ERROR"
	case StatusConnectionFailed:
		return "CONNECTION_FAILED"
	}
	return fmt.Sprintf("status(%d)", int(s))
}

// JSON-RPC error codes rpcd uses. Only -32002 is interesting to us; the rest
// are protocol faults that indicate a bug on our side.
const (
	rpcErrAccessDenied = -32002
	rpcErrParse        = -32700
	rpcErrRequest      = -32600
	rpcErrInternal     = -32603
)

// StatusError means rpcd *proxied* the call and the object refused the target:
// the session is valid, the method is granted, and this specific target (a uci
// config name, a file path) is not permitted.
//
// This is permanent. Re-authenticating cannot change it and retrying is pure
// latency. Measured: docs/IMPLEMENTATION.md §14.
type StatusError struct {
	Object string
	Method string
	Status Status
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("ubus %s.%s: %s", e.Object, e.Method, e.Status)
}

// Permanent reports that no amount of retrying will help.
func (e *StatusError) Permanent() bool { return true }

// DeniedError means rpcd refused to proxy the call at all (JSON-RPC -32002).
//
// It is deliberately ambiguous, because the device is: it covers both an
// invalid/expired session AND an object+method that is in no granted
// access-group. Exactly one re-login disambiguates. If the call still fails
// after a successful re-login, Retried is true and this is a permanent
// capability gap — the ACL never granted it — not an auth problem.
type DeniedError struct {
	Object  string
	Method  string
	Retried bool
}

func (e *DeniedError) Error() string {
	if e.Retried {
		return fmt.Sprintf("ubus %s.%s: access denied after re-login "+
			"(the ACL does not grant this method)", e.Object, e.Method)
	}
	return fmt.Sprintf("ubus %s.%s: access denied", e.Object, e.Method)
}

// Permanent is true only once a re-login has already been tried and failed.
func (e *DeniedError) Permanent() bool { return e.Retried }

// ProtocolError is a malformed exchange: our bug, or something between us and
// the device mangling the body. Never retried.
type ProtocolError struct {
	Code    int
	Message string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("ubus protocol error %d: %s", e.Code, e.Message)
}

// IsPermanent reports whether retrying err could ever succeed. Callers use it
// to decide between backoff and giving up with a clear message.
func IsPermanent(err error) bool {
	// errors.As, not a bare type assertion: these errors are routinely wrapped
	// with fmt.Errorf("%w") on the way up, and a bare assertion reports every
	// wrapped permanent failure as retryable — which turns a permanent ACL gap
	// into an infinite backoff loop against the device.
	var p interface{ Permanent() bool }
	if errors.As(err, &p) {
		return p.Permanent()
	}
	var pe *ProtocolError
	return errors.As(err, &pe)
}
