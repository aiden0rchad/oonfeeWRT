package render

import "fmt"

// MobilityDomain derives the 802.11r mobility domain for a WLAN.
//
// This is the cross-device consistency a controller exists for. Every AP in the
// group must publish the SAME mobility domain or fast transition simply does
// not happen between them — and getting there by configuring each AP by hand is
// exactly the error-prone busywork the product replaces.
//
// Deriving it from (site UUID, WLAN id) means every AP computes the same value
// independently, with no coordination, no stored state, and no ordering
// dependency during a fan-out. It is also stable across re-renders, so an AP
// that rejoins later agrees with the ones already running.
//
// The site UUID is included so two sites that both happen to have "WLAN 3" do
// not collide where their coverage overlaps.
func MobilityDomain(siteUUID string, wlanID int) string {
	return fmt.Sprintf("%04x", crc16CCITT([]byte(fmt.Sprintf("%s:%d", siteUUID, wlanID))))
}

// crc16CCITT is the CCITT-FALSE variant (poly 0x1021, init 0xFFFF).
//
// The choice of checksum carries no security weight — a mobility domain is a
// public 2-byte identifier, not a secret. What matters is that it is stable,
// cheap, and identical on every AP.
func crc16CCITT(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
