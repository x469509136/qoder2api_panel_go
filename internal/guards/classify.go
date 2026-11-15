package guards

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
)

// Echo classification. Each function answers one narrow question so the guards
// stay readable and independently testable.

var (
	leakStructRe = regexp.MustCompile(
		`name\s*=\s*["']` + // DSML invoke/parameter attribute
			`|<tool_call` +
			`|"(?:arguments|name)"\s*:` +
			`|\{"`)

	leakTagRe = regexp.MustCompile(`<\s*/?\s*[A-Za-z_][\w:.-]*\s*/?\s*>`)

	dsmlTokenRe = regexp.MustCompile(`<[^<>]{0,4}DSML|DSML[^<>]{0,4}>|｜DSML｜`)

	fenceRe = regexp.MustCompile("(?s)```.*?(?:```|$)|`[^`\\n]{0,200}`")

	jsonNameRe = regexp.MustCompile(`"name"\s*:\s*"([^"]{1,120})"`)
	attrNameRe = regexp.MustCompile(`name\s*=\s*["']([^"']{1,120})["']`)
)

// EchoNamesDeclared reports whether every tool name in the echo is among the
// declared tools.
//
// Failing closed on "the request declared tools" alone is too broad: a model
// repeating an *undeclared* tool was never recoverable, and punishing that
// would break a batch of ordinary turns.
func EchoNamesDeclared(text string, allowed map[string]bool) bool {
	if len(allowed) == 0 {
		return false
	}
	names := jsonNameRe.FindAllStringSubmatch(text, -1)
	if len(names) == 0 {
		return false
	}
	for _, n := range names {
		if !allowed[n[1]] {
			return false
		}
	}
	return true
}

// EchoNamesAllUndeclared reports whether every name in the echo is unknown.
func EchoNamesAllUndeclared(text string, allowed map[string]bool) bool {
	if len(allowed) == 0 {
		return false
	}
	var names []string
	for _, m := range jsonNameRe.FindAllStringSubmatch(text, -1) {
		names = append(names, m[1])
	}
	for _, m := range attrNameRe.FindAllStringSubmatch(text, -1) {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		return false
	}
	for _, n := range names {
		if allowed[n] {
			return false
		}
	}
	return true
}

// MarkerOnlyEcho reports whether the text carries nothing but the marker.
func MarkerOnlyEcho(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	t := NormalizeLeakMarker(strings.TrimSpace(text))
	rest := t
	for strings.HasPrefix(rest, Marker) {
		rest = strings.TrimLeft(rest[len(Marker):], " \t\r\n")
	}
	rest = strings.TrimSpace(StripDSMLCloseTail(rest))
	return rest == ""
}

// LeakResiduePresent reports whether the text is protocol residue that failed to
// be cleaned — not prose that merely mentions the marker.
//
// The check must cut both ways: an explanation of this very bug must pass
// through untouched, or the answering turn breaks itself and the client retries
// forever.
func LeakResiduePresent(text string) bool {
	if text == "" || !(strings.Contains(text, Marker) || strings.Contains(text, MarkerAlt)) {
		return false
	}
	if strings.Contains(text, "DSML") && strings.Contains(text, "<") {
		return true
	}
	if strings.Contains(text, "</") {
		return true
	}
	if leakStructRe.MatchString(text) {
		return true
	}
	return strings.Contains(text, ToolCallOpen) || strings.Contains(text, `{"`)
}

// DSMLResiduePresent reports DSML protocol tokens — it does not require the leak
// marker, because a DSML-only fragment with markers=0 has been observed.
func DSMLResiduePresent(text string) bool {
	if text == "" || !strings.Contains(text, "DSML") {
		return false
	}
	if dsmlTokenRe.MatchString(text) {
		return true
	}
	return leakStructRe.MatchString(text) || leakTagRe.MatchString(text)
}

// OutsideFences strips fenced code and inline code before residue checks.
//
// In the non-streaming path the whole text is available, and a model explaining
// this very protocol *inside a code block* is normal content.
func OutsideFences(text string) string { return fenceRe.ReplaceAllString(text, "") }

// ResidueTextOnly reports whether post-recovery leftover text still carries
// protocol residue and should be dropped.
func ResidueTextOnly(text string) bool {
	if text == "" || LeakGarbageOnly(text) {
		return false
	}
	return LeakResiduePresent(text) || DSMLResiduePresent(text)
}

// LeakGarbageOnly reports text that is only the marker plus empty shell tags,
// with no recoverable structure (issue #49 form 3).
//
// The predicate must be narrow: prose mentioning the marker has no HTML-style
// tags and must pass through byte-for-byte.
func LeakGarbageOnly(text string) bool {
	if text == "" || !(strings.Contains(text, Marker) || strings.Contains(text, MarkerAlt)) {
		return false
	}
	if leakStructRe.MatchString(text) {
		return false
	}
	return leakTagRe.MatchString(text)
}

// LeakStartFencePrefixOK reports whether text before a marker is only fence.
func LeakStartFencePrefixOK(text string) bool {
	t := strings.TrimSpace(text)
	for {
		if t == "" {
			return true
		}
		if strings.HasPrefix(t, "```") {
			t = t[3:]
			if len(t) >= 4 && strings.EqualFold(t[:4], "json") {
				t = t[4:]
			}
			t = strings.TrimLeft(t, " \t\r\n")
			continue
		}
		return false
	}
}

// ---------------------------------------------------------------------------
// Streaming hold classification
// ---------------------------------------------------------------------------

// StillLeakCandidate reports whether text is still a plausible echo prefix.
func StillLeakCandidate(text string) bool {
	if text == "" {
		return false
	}
	trimmed := strings.TrimSpace(text)
	if MarkerPrefixOK(trimmed) || MarkerStartsAt(trimmed) {
		return true
	}
	if markerRe.MatchString(trimmed) {
		// After the marker, only a JSON array (or fence/DSML tail) may follow.
		pos, end := LastMarkerSpan(trimmed)
		rest := strings.TrimSpace(trimmed[end:])
		rest = strings.TrimSpace(StripDSMLCloseTail(rest))
		if rest == "" {
			return true
		}
		if strings.HasPrefix(rest, "[") || strings.HasPrefix(rest, "{") ||
			strings.HasPrefix(rest, "```") || strings.HasPrefix(rest, "<") {
			return true
		}
		_ = pos
		return false
	}
	return false
}

// EchoHoldCandidate reports whether the held buffer should stay held.
func EchoHoldCandidate(text string) bool {
	if text == "" {
		return false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	if MarkerPrefixOK(trimmed) || MarkerStartsAt(trimmed) {
		return true
	}
	if markerRe.MatchString(trimmed) {
		pos, _ := LastMarkerSpan(trimmed)
		prefix := trimmed[:pos]
		if strings.TrimSpace(markerRe.ReplaceAllString(prefix, "")) != "" {
			// Prose before the marker: keep holding only if it is fence debris.
			if !LeakStartFencePrefixOK(markerRe.ReplaceAllString(prefix, "")) {
				return false
			}
		}
		rest := strings.TrimSpace(trimmed[markerEnd(trimmed):])
		rest = strings.TrimSpace(StripDSMLCloseTail(rest))
		if rest == "" {
			return true
		}
		if strings.HasPrefix(rest, "```") {
			rest = StripJSONFence(rest)
			if rest == "" {
				return true
			}
		}
		if !strings.HasPrefix(rest, "[") {
			// A bare "{" is allowed: the array's opening bracket is sometimes lost.
			if strings.HasPrefix(rest, "{") {
				return true
			}
			return false
		}
		return jsonArrayPrefixOK(rest) || SalvageEchoArray(rest) != nil || isOpenJSONArray(rest)
	}
	// Nothing marker-like yet: a leading fence may precede the marker.
	if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "`") {
		return true
	}
	return false
}

func markerEnd(text string) int {
	_, end := LastMarkerSpan(text)
	return end
}

// isOpenJSONArray reports whether rest is a JSON array still being written.
func isOpenJSONArray(rest string) bool {
	if !strings.HasPrefix(rest, "[") {
		return false
	}
	depth := 0
	inStr := false
	escaped := false
	for _, r := range rest {
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			switch r {
			case '\\':
				escaped = true
			case '"':
				inStr = false
			}
			continue
		}
		switch r {
		case '"':
			inStr = true
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
	}
	return depth > 0
}

// jsonArrayPrefixOK reports whether text is a syntactically plausible prefix of
// a JSON array of {name, arguments} objects.
func jsonArrayPrefixOK(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || !strings.HasPrefix(t, "[") {
		return false
	}
	if strings.HasSuffix(t, "]") {
		var arr []any
		if json.Unmarshal([]byte(t), &arr) == nil {
			return true
		}
		return SalvageEchoArray(t) != nil
	}
	return isOpenJSONArray(t)
}

// LeakedPartialDroppable reports a truncated echo that cannot be recovered.
func LeakedPartialDroppable(text string, allowed map[string]bool) bool {
	if text == "" {
		return false
	}
	t := strings.TrimSpace(NormalizeLeakMarker(text))
	if !strings.Contains(t, Marker) {
		return false
	}
	if strings.TrimSpace(markerRe.ReplaceAllString(t, "")) == "" {
		return true
	}
	if !strings.Contains(t, "[") {
		return false
	}
	// A complete, valid array means the block was recoverable — not "partial".
	if rest := jsonArrayPart(t); rest != "" {
		var arr []any
		if json.Unmarshal([]byte(rest), &arr) == nil {
			return false
		}
	}
	return EchoNamesDeclared(t, allowed) || len(allowed) == 0
}

func jsonArrayPart(t string) string {
	pos, end := LastMarkerSpan(t)
	if pos < 0 {
		return ""
	}
	rest := strings.TrimSpace(t[end:])
	rest = strings.TrimSpace(StripDSMLCloseTail(rest))
	if strings.HasSuffix(rest, "```") {
		rest = strings.TrimSpace(rest[:len(rest)-3])
	}
	if strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]") {
		return rest
	}
	return ""
}

// ToolEchoDroppable reports a leaked tool-*result* echo (unrecoverable: it would
// need tool_call_id pairing).
func ToolEchoDroppable(text string, allowed map[string]bool) bool {
	if !strings.Contains(text, ToolResultMarker) {
		return false
	}
	if strings.Contains(text, ToolResultClose) {
		return true
	}
	// A truncated block: the opening marker was emitted, the closing one never was.
	return len(allowed) == 0 || EchoNamesDeclared(text, allowed)
}

// ---------------------------------------------------------------------------
// Tool echo mode (QD_TOOL_ECHO_MODE)
// ---------------------------------------------------------------------------

const (
	// ToolEchoStrict fails the turn so the client retries.
	ToolEchoStrict = "strict"
	// ToolEchoLenient finishes normally and keeps the prose already produced.
	ToolEchoLenient = "lenient"
)

var (
	echoModeOnce sync.Once
	echoMode     string
)

// ToolEchoMode resolves the settings/env tool-fragment policy.
func ToolEchoMode() string {
	echoModeOnce.Do(func() {
		raw := strings.ToLower(strings.TrimSpace(os.Getenv("QD_TOOL_ECHO_MODE")))
		if raw == ToolEchoLenient {
			echoMode = ToolEchoLenient
			return
		}
		echoMode = ToolEchoStrict
	})
	return echoMode
}

// SetToolEchoMode overrides the resolved policy (panel setting wins over env).
func SetToolEchoMode(mode string) {
	echoModeOnce.Do(func() {})
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ToolEchoLenient:
		echoMode = ToolEchoLenient
	case ToolEchoStrict:
		echoMode = ToolEchoStrict
	}
}

// NoteLenientTurn counts one turn finished under the lenient policy.
func NoteLenientTurn() { lenientTurns.Add(1) }
