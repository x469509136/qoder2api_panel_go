package upstream

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

func getenv(name string) string {
	return envLookup(name)
}

// envLookup is a seam for tests.
var envLookup = defaultEnvLookup

func marshalNoEscape(v any) (string, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// ---------------------------------------------------------------------------
// Inner (HTTP 200) error observability
// ---------------------------------------------------------------------------

// The upstream sometimes returns HTTP 200 with statusCodeValue=200 but puts an
// error inside the inner chunk. The old path was blind to this: the client saw
// an empty 200 body and debugging was very expensive. This section only counts
// and warns once per kind — it never changes behaviour.

var (
	innerMu     sync.Mutex
	innerTotal  int64
	innerKinds  = map[string]int64{}
	innerWarned = map[string]bool{}
)

// InnerErrorSnapshot returns the counter state for /usage/perf.
func InnerErrorSnapshot() map[string]any {
	innerMu.Lock()
	defer innerMu.Unlock()
	kinds := make(map[string]int64, len(innerKinds))
	for k, v := range innerKinds {
		kinds[k] = v
	}
	warned := make([]string, 0, len(innerWarned))
	for k := range innerWarned {
		warned = append(warned, k)
	}
	return map[string]any{"total": innerTotal, "kinds": kinds, "warned": warned}
}

// InnerErrorKind classifies an inner error ("" = not an error shape).
func InnerErrorKind(code, message string) string {
	low := strings.ToLower(strings.TrimSpace(code + " " + message))
	if low == "" {
		return ""
	}
	containsAny := func(markers ...string) bool {
		for _, m := range markers {
			if strings.Contains(low, m) {
				return true
			}
		}
		return false
	}
	switch {
	case containsAny("datainspectionfailed", "inappropriate content", "contentfilter",
		"sensitivecontent", "data_inspection"):
		return "content_policy"
	case containsAny("rate_limit", "throttl", "frequency limit", "10605", "too many"):
		return "rate_limit"
	case containsAny("invalid_request", "invalid_parameter", "provider_error",
		"not found", "must be a response", "unsupported"):
		return "invalid_request"
	case containsAny("unauthor", "forbidden", "permission", "expired", "token"):
		return "auth"
	}
	return "other"
}

// NoteInnerError counts an inner error and returns its kind.
func NoteInnerError(obj map[string]any, status int) string {
	if obj == nil {
		return ""
	}
	var code, msg string
	switch err := obj["error"].(type) {
	case map[string]any:
		code = asStr(err["code"])
		if code == "" {
			code = asStr(err["type"])
		}
		msg = asStr(err["message"])
	case string:
		msg = err
	default:
		return ""
	}
	kind := InnerErrorKind(code, msg)
	if kind == "" {
		return ""
	}
	innerMu.Lock()
	innerTotal++
	innerKinds[kind]++
	first := !innerWarned[kind]
	if first {
		innerWarned[kind] = true
	}
	total := innerTotal
	innerMu.Unlock()
	_ = first
	_ = total
	return kind
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------------------
// Queue parsing
// ---------------------------------------------------------------------------

var retryAfterRe = regexp.MustCompile(`retryAfterSeconds\D+(\d+)`)

// QueueWaitSeconds extracts the retry delay from an upstream queue body.
// Returns 0 when the body is not a queue signal.
func QueueWaitSeconds(detail string) int {
	if detail == "" {
		return 0
	}
	low := strings.ToLower(detail)
	if !strings.Contains(detail, "10605") && !strings.Contains(low, "isqueued") &&
		!strings.Contains(low, "queue") && !strings.Contains(detail, "RESOURCE_LIMIT") {
		return 0
	}
	if m := retryAfterRe.FindStringSubmatch(detail); m != nil {
		n := 0
		for _, r := range m[1] {
			n = n*10 + int(r-'0')
		}
		if n < 1 {
			return 1
		}
		return n
	}
	// Queued without a stated delay: 30 s is the conservative choice.
	return 30
}

// IsClientFaultMarkers lists detail fragments that will never succeed on retry.
var IsClientFaultMarkers = []string{
	"invalid_parameter_error",
	"invalid_request_error",
	"authentication_error",
	"permission_error",
	`"Range of `,
	"DataInspectionFailed",
	"inappropriate content",
	"input text data may contain",
	"ContentFilter",
	"SensitiveContent",
}

// RequestFaultMarkers are the subset where switching accounts cannot help.
var RequestFaultMarkers = []string{
	"invalid_parameter_error", "invalid_request_error", `"Range of `, "Range of ",
	"DataInspectionFailed", "inappropriate content",
	"input text data may contain", "ContentFilter", "SensitiveContent",
}

// IsRequestFault reports a deterministic request-level error.
func IsRequestFault(code int, detail string) bool {
	if code == 413 {
		return true
	}
	for _, m := range RequestFaultMarkers {
		if strings.Contains(detail, m) {
			return true
		}
	}
	return false
}

// IsTransientUpstream reports an upstream error worth a same-account retry.
func IsTransientUpstream(code int, detail string) bool {
	if code == 401 || code == 403 || code == 429 {
		return false
	}
	for _, m := range IsClientFaultMarkers {
		if strings.Contains(detail, m) {
			return false
		}
	}
	if code == 418 || code >= 500 {
		return true
	}
	if code >= 400 && code < 500 && strings.Contains(detail, "provider_error") {
		return true
	}
	return false
}

// ImageConversionTimeoutRe matches the provider's image-conversion timeout.
var ImageConversionTimeoutRe = regexp.MustCompile(`image[ _-]+conversion[ _-]+(?:timeout|timed[ _-]+out)`)

// IsImageConversionTimeout distinguishes the provider's image stage timeout from
// a worker first-token timeout: the former deserves one bounded retry.
func IsImageConversionTimeout(detail string) bool {
	return ImageConversionTimeoutRe.MatchString(strings.ToLower(detail))
}

// IsFirstTokenTimeout reports an upstream worker that never produced a first
// token. Retrying the same account almost always times out again, so the request
// should move to another account instead.
func IsFirstTokenTimeout(detail string) bool {
	low := strings.ToLower(detail)
	return strings.Contains(low, "first token timeout") ||
		strings.Contains(low, "first_token_timeout") ||
		strings.Contains(low, "upstream timeout")
}

// IsTransientTransport reports a transport-level error worth a same-account
// retry (TLS EOF, connection reset, timeout).
func IsTransientTransport(err error) bool {
	return isTransientTransport(err)
}

// RetryAfterValue exposes the queue delay so the HTTP layer can set Retry-After.
func (e *QueuedError) RetryAfterValue() int { return e.RetryAfter }
