package accounts

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HomeDir returns the user's home directory ("" on failure).
func homeDir(sub string) string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if sub == "" {
		return h
	}
	return filepath.Join(h, sub)
}

func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(buf)[:n]
}

// asString coerces a decoded-JSON value into a string.
func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		return t.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

func asFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0
		}
		return f
	}
	return 0
}

func asInt(v any) int64 { return int64(asFloat(v)) }

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		low := strings.ToLower(strings.TrimSpace(t))
		return !(low == "" || low == "0" || low == "false" || low == "no" ||
			low == "off" || low == "none" || low == "null")
	case float64:
		return t != 0
	case nil:
		return false
	}
	return false
}

// truthyDefault reads a defensive boolean. value == nil takes the default,
// because "field absent" and "field is null" are not equivalent in quota
// semantics (an absent `available` means "not declared unavailable").
func truthyDefault(value any, def bool) bool {
	if value == nil {
		return def
	}
	return asBool(value)
}

// normalizeEpoch converts seconds/milliseconds/string to unix seconds.
func normalizeEpoch(value any) int64 {
	n := asFloat(value)
	if n > 1e11 {
		n /= 1000
	}
	return int64(n)
}

// MarshalNoEscape is Python-compatible JSON (no HTML escaping).
func MarshalNoEscape(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(sb.String(), "\n")), nil
}

// writeJSONAtomic writes v as indented JSON via a temp file + rename so a
// crash can never leave a half-written credential file.
func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + fmt.Sprintf(".%d.tmp", time.Now().UnixNano())
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// urlQueryEscapeStd delegates to net/url (kept separate so login.go can swap
// the '+' handling without importing net/url at file scope).
func urlQueryEscapeStd(s string) string { return url.QueryEscape(s) }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
