package model

import (
	"fmt"
	"sort"
	"strings"
)

// DeviceFunction is one independently selected responsibility of a device.
//
// Role remains on the wire and in the database for older clients. Functions
// are the authority for new code: a gateway may route without publishing
// WLANs, while a combined OpenWrt router may route, publish WLANs and carry
// switched traffic at the same time.
type DeviceFunction string

const (
	FunctionGateway DeviceFunction = "gateway"
	FunctionAP      DeviceFunction = "ap"
	FunctionSwitch  DeviceFunction = "switch"
)

// DeviceFunctionChoices is the canonical storage and wire order.
var DeviceFunctionChoices = []DeviceFunction{
	FunctionGateway, FunctionAP, FunctionSwitch,
}

// DeviceFunctions is a canonical, duplicate-free set in
// DeviceFunctionChoices order.
type DeviceFunctions []DeviceFunction

// ParseDeviceFunctions validates a new functions array. A nil array means the
// caller is legacy and is expanded from role without changing old behaviour.
// A present-but-empty array is rejected: it is almost certainly a form bug and
// would otherwise adopt a device that the renderer is licensed to do nothing
// with.
func ParseDeviceFunctions(in []string, role Role) (DeviceFunctions, error) {
	if in == nil {
		return FunctionsForRole(role), nil
	}
	if len(in) == 0 {
		return nil, fmt.Errorf("at least one device function is required; valid functions are ap, gateway, switch")
	}
	seen := make(map[DeviceFunction]bool, len(in))
	for _, raw := range in {
		fn := DeviceFunction(strings.ToLower(strings.TrimSpace(raw)))
		switch fn {
		case FunctionGateway, FunctionAP, FunctionSwitch:
			seen[fn] = true
		default:
			return nil, fmt.Errorf("%q is not a device function; valid functions are ap, gateway, switch", raw)
		}
	}
	out := make(DeviceFunctions, 0, len(seen))
	for _, fn := range DeviceFunctionChoices {
		if seen[fn] {
			out = append(out, fn)
		}
	}
	return out, nil
}

// FunctionsForRole is the compatibility map for rows and clients created
// before functions were independently selectable. These combinations exactly
// reproduce what each legacy role rendered.
func FunctionsForRole(role Role) DeviceFunctions {
	switch role.or() {
	case RoleGateway:
		return DeviceFunctions{FunctionGateway, FunctionAP, FunctionSwitch}
	case RoleSwitch:
		return DeviceFunctions{FunctionSwitch}
	default:
		return DeviceFunctions{FunctionAP, FunctionSwitch}
	}
}

// DeviceFunctionsOf reads stored strings. Only nil is legacy and expands the
// bundled role. A present but invalid/empty set fails closed to an explicit
// empty set; callers that render treat that as an invalid inventory row rather
// than silently widening a gateway-only device into gateway+AP+switch.
func DeviceFunctionsOf(in []string, role string) DeviceFunctions {
	r := RoleOf(role)
	functions, err := ParseDeviceFunctions(in, r)
	if err != nil {
		return DeviceFunctions{}
	}
	return functions
}

func (f DeviceFunctions) Has(want DeviceFunction) bool {
	for _, got := range f {
		if got == want {
			return true
		}
	}
	return false
}

func (f DeviceFunctions) Routes() bool   { return f.Has(FunctionGateway) }
func (f DeviceFunctions) Wireless() bool { return f.Has(FunctionAP) }
func (f DeviceFunctions) Switches() bool { return f.Has(FunctionSwitch) }

// PrimaryRole keeps the legacy role deterministic. It conveys the broadest
// selected responsibility, but new code must not use it as a function set.
func (f DeviceFunctions) PrimaryRole() Role {
	switch {
	case f.Routes():
		return RoleGateway
	case f.Wireless():
		return RoleAP
	default:
		return RoleSwitch
	}
}

func (f DeviceFunctions) Strings() []string {
	out := make([]string, 0, len(f))
	for _, fn := range f {
		out = append(out, string(fn))
	}
	return out
}

func (f DeviceFunctions) Describe() string {
	parts := f.Strings()
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
