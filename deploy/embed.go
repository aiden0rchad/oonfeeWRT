// Package deploy embeds the ACL variant the controller installs on a device.
// Each device receives exactly one ACL file: managed devices receive the
// configuration-capable policy, while monitor-only devices receive a policy
// with the same observation scope and no write grants. Adoption writes the
// chosen content and un-adoption removes the one file.
//
// Review this file like code. It is the blast radius (IMPLEMENTATION §10).
package deploy

import (
	_ "embed"
)

// ACL is the rpcd access-control file installed at
// /usr/share/rpcd/acl.d/oonfeewrt.json.
//
//go:embed acl/oonfeewrt.json
var ACL []byte

// MonitorACL is the read-only rpcd access-control file installed for a
// monitor-only device at the same path as ACL.
//
//go:embed acl/oonfeewrt-monitor.json
var MonitorACL []byte
