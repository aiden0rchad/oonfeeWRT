package adoption

import (
	"crypto/rand"
	"encoding/base64"
)

// passwordBytes is the entropy behind the controller credential. 24 bytes is
// 192 bits — far beyond anything that matters against a LAN-local rpcd, and the
// credential is machine-generated and machine-stored, so length costs nothing.
const passwordBytes = 24

// randomPassword returns a URL-safe random password.
//
// Base64url avoids '$' and ':' entirely, which matters because the value is
// written into /etc/config/rpcd where ':' separates fields in adjacent formats
// and '$' introduces a crypt prefix. A password that cannot be confused for
// either is one less way to write a broken config.
func randomPassword() (string, error) {
	buf := make([]byte, passwordBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
