package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// newUUID returns a random RFC-4122 v4-shaped identifier.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return newHex(32)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newHex returns n random hex characters.
func newHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n, 0)
	}
	return hex.EncodeToString(b)[:n]
}
