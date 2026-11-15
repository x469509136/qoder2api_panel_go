package fingerprint

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405")))
	}
	return hex.EncodeToString(buf)[:n]
}

func nowMillis() int64 { return time.Now().UnixMilli() }
