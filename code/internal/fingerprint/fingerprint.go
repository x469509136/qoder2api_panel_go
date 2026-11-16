// Package fingerprint derives stable per-account device identities.
//
// The gateway runs one long-lived process that impersonates dozens of devices.
// Generating an identity once per process would make every account share the
// same value, which associates them with a single machine — the opposite of
// what "one account, one device" isolation is meant to achieve. Every value
// here is therefore derived from the account UID with a salt, so it is
// deterministic per account and independent across accounts.
package fingerprint

import (
	"crypto/md5"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
)

// legacyMode keeps the pre-1.6 "32 hex chars, no dashes" device id. Account
// pools that were already running should set this to avoid looking like a
// brand-new device to the upstream.
var (
	legacyOnce sync.Once
	legacy     bool
)

// SetLegacy forces the legacy device-id shape. Call once during startup.
func SetLegacy(on bool) { legacyOnce.Do(func() { legacy = on }) }

func legacyEnabled() bool {
	legacyOnce.Do(func() {
		raw := strings.ToLower(strings.TrimSpace(os.Getenv("QD_DEVICE_ID_LEGACY")))
		legacy = raw == "1" || raw == "on" || raw == "true" || raw == "yes" ||
			raw == "enable" || raw == "enabled"
	})
	return legacy
}

// DeriveID returns a UUID-shaped identifier derived from uid and salt.
//
// Output looks like a1b2c3d4-e5f6-4a78-a9b0-c1d2e3f4a5b6 — version nibble
// fixed at 4 and variant nibble at a, which is exactly the shape of a real
// client's machineId.
func DeriveID(uid, salt string) string {
	if uid == "" {
		uid = "anonymous"
	}
	sum := md5.Sum([]byte(salt + ":" + uid))
	h := hex.EncodeToString(sum[:])
	if legacyEnabled() {
		return h
	}
	b := []byte(h)
	b[12] = '4'
	b[16] = 'a'
	h = string(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// DeriveMachineID is the value sent as cosy-machineid.
func DeriveMachineID(uid string) string { return DeriveID(uid, "machine") }

// DeriveSessionID is the value used for behaviour-event session ids.
func DeriveSessionID(uid string) string { return DeriveID(uid, "session") }

// DeriveMachineType returns an 18-char dashless machine type.
func DeriveMachineType(uid string) string {
	s := strings.ReplaceAll(DeriveID(uid, "machinetype"), "-", "")
	if len(s) > 18 {
		s = s[:18]
	}
	return s
}

// DeriveMachineToken returns a base64url-looking machine token.
func DeriveMachineToken(uid string) string {
	sum := sha512.Sum512([]byte("machinetoken:" + uid))
	s := base64.RawURLEncoding.EncodeToString(sum[:])
	if len(s) > 43 {
		s = s[:43]
	}
	return s
}

// DeriveHostname returns a stable per-account hostname (DESKTOP-1A2B3C4).
// The official infer-sse request carries it; the same account always presents
// the same hostname while different accounts differ.
func DeriveHostname(uid string) string {
	if uid == "" {
		uid = "anonymous"
	}
	sum := md5.Sum([]byte("hostname:" + uid))
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	return "DESKTOP-" + h[:7]
}

// GenerateRequestID returns a random 32-hex request id.
//
// The uid parameter is accepted for call-site compatibility only: a real
// client's request id is independent of the account, and mixing a uid-derived
// prefix in would be a stable fingerprint that is trivial to cluster on.
func GenerateRequestID(uid string) string {
	return randomHex(32)
}

// MachineOS resolves the cosy-machineos header value.
//
// Defaults keep Windows at x86_64_win32 and everything else at x86_64_linux so
// existing accounts do not drift; QD_COSY_MACHINE_OS=auto detects from the
// running platform, and any other literal value is used verbatim.
func MachineOS() string {
	raw := strings.TrimSpace(os.Getenv("QD_COSY_MACHINE_OS"))
	if strings.EqualFold(raw, "auto") {
		return detectMachineOS()
	}
	if raw != "" {
		return raw
	}
	if runtime.GOOS == "windows" {
		return "x86_64_win32"
	}
	return "x86_64_linux"
}

func detectMachineOS() string {
	arch := "x86_64"
	if m := strings.ToLower(runtime.GOARCH); m == "arm64" || m == "aarch64" {
		arch = "aarch64"
	}
	plat := "linux"
	switch runtime.GOOS {
	case "windows":
		plat = "win32"
	case "darwin":
		plat = "darwin"
	}
	return arch + "_" + plat
}

// DesktopFingerprint is the payload the official desktop reports in behaviour
// events. Kept for parity with the original implementation.
func DesktopFingerprint(uid, nickname, osName string) map[string]any {
	if osName == "" {
		osName = "win32"
	}
	now := nowMillis()
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       uid,
		"username":     nickname,
		"userNickname": nickname,
		"product":      "SaaS",
		"releaseDate":  int64(1789036585355),
		"commit":       "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":      "Qoder",
		"ideType":      "Qoder",
		"ideVersion":   "1.1.64",
		"machineId":    DeriveMachineID(uid),
		"sessionId":    DeriveSessionID(uid),
		"extName":      "qoder-desktop",
		"extVersion":   "1.1.64",
		"os":           osName,
		"arch":         "x64",
		"osVersion":    "10.0.26220",
		"cpuCores":     20,
		"memorySize":   24,
		"timestamp":    now,
		"presentAt":    now,
	}
}
