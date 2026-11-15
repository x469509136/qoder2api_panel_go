package guards

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

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

func asInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// regexpMustCompile is a local shim so the guard file can declare patterns
// without importing regexp twice.
func regexpMustCompile(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }
