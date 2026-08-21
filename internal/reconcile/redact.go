package reconcile

import "strings"

const redactedUCIValue = "<redacted>"

// IsSensitiveOption reports UCI option names whose values are credentials or
// private key material. The list is deliberately exact: nearby identifiers
// such as encryption, key_index and public_key are useful diagnostic state and
// must remain visible.
func IsSensitiveOption(option string) bool {
	switch strings.ToLower(option) {
	case "key", "key1", "key2", "key3", "key4", "psk", "wpa_psk",
		"preshared_key", "sae_password", "r0kh", "r1kh",
		"password", "passphrase", "secret", "auth_secret", "acct_secret",
		"dae_secret", "radius_secret", "encryption_key", "private_key",
		"private_key_password", "private_key_passwd", "wps_pin":
		return true
	default:
		return false
	}
}

// RedactOptionValue is the shared output boundary for diagnostics that name a
// UCI option. Comparison must happen before this call; secrets remain useful
// for deciding whether state differs, but never for explaining that decision.
func RedactOptionValue(option, value string) string {
	if IsSensitiveOption(option) {
		return redactedUCIValue
	}
	return value
}

func newDrift(config, section, option, ours, theirs string) Drift {
	return Drift{
		Config: config, Section: section, Option: option,
		Ours:   RedactOptionValue(option, ours),
		Theirs: RedactOptionValue(option, theirs),
	}
}
