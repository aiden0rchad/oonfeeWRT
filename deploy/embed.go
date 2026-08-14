// Package deploy embeds the files the controller installs on a device.
//
// There is exactly one, and that is the point: `deploy/acl/oonfeewrt.json` is
// the entire device-side footprint. Adoption writes it and un-adoption removes
// it, and nothing else the controller does leaves anything behind. Keeping it
// embedded rather than read from disk means a running binary cannot be pointed
// at a different ACL than the one it was built and tested with.
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
