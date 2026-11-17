package qoder

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

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

// asInt coerces a decoded-JSON value into an int64.
func asInt(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if n == 0 {
			f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
			if err == nil {
				return int64(f)
			}
		}
		return n
	case bool:
		if t {
			return 1
		}
		return 0
	}
	return 0
}

// asFloat coerces a decoded-JSON value into a float64.
func asFloat(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	case bool:
		if t {
			return 1
		}
		return 0
	}
	return 0
}

// asBool coerces a decoded-JSON value into a bool. Strings "false"/"0" are
// treated as false, because the upstream sometimes sends booleans as strings.
func asBool(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case int64:
		return t != 0
	case json.Number:
		n, _ := t.Int64()
		return n != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "true" || s == "1" || s == "yes"
	}
	return false
}

// truthyDefault returns def when v is nil, otherwise the coerced bool.
func truthyDefault(v any, def bool) bool {
	if v == nil {
		return def
	}
	return asBool(v)
}

// normalizeEpoch converts a numeric, numeric-string or RFC3339 timestamp into
// unix seconds. Values that already look like seconds are kept as-is.
func normalizeEpoch(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return normalizeEpoch(n)
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts.Unix()
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return normalizeEpoch(f)
		}
		return 0
	}
	n := asInt(v)
	if n == 0 {
		if f, ok := v.(float64); ok {
			n = int64(f)
		}
	}
	if n <= 0 {
		return 0
	}
	// Milliseconds are the common upstream mistake; anything past year 5000 in
	// seconds is really milliseconds.
	if n > 100000000000 {
		return n / 1000
	}
	return n
}

// urlQueryEscapeStd mirrors url.QueryEscape for the pieces this package builds.
func urlQueryEscapeStd(s string) string {
	return url.QueryEscape(s)
}

// truncate shortens a string for messages.
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
