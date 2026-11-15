// Package guards recovers tool calls that the model echoed as plain text and
// applies the fail-closed guards around them.
//
// The problem: in long sessions a model sometimes copies the textual tool-call
// scaffold it sees in its own history and emits it as ordinary content. The
// client then renders that JSON as prose and the turn's tool call never runs.
//
// The contract for every frame pipeline in this package is:
//
//   - a run that can be recovered is emitted as structured tool_calls;
//   - prose is never dropped — buffered bytes are re-emitted verbatim once they
//     are known not to be a marker;
//   - anything that cannot be recovered ends the turn with an explicit error,
//     never with a silent "success";
//   - text that merely *discusses* the markers passes through untouched.
package guards

import (
	"regexp"
	"strings"

	"github.com/xzs/bridge_qoder/internal/protocol"
)

// Marker variants. The upstream converter emits both the square-bracket and the
// angle-bracket form, and the model reproduces missing or doubled brackets, so
// one regex covers every variant.
const (
	Marker    = protocol.LeakMarker
	MarkerAlt = protocol.LeakMarkerAlt
)

var (
	// markerRe matches every observed bracket variant of the marker.
	markerRe = regexp.MustCompile(`[\[<(]*\s*assistant\s*请求调用工具\s*[\]>)]?`)
	// markerOpenRe matches only the opening part (no closing bracket).
	markerOpenRe = regexp.MustCompile(`^[\[<(]*\s*assistant\s*请求调用工具`)
	// markerCore is the marker with all whitespace removed.
	markerCore = "assistant请求调用工具"
	// dsmlCloseRe matches a trailing DSML closing tag.
	dsmlCloseRe = regexp.MustCompile(`</[^>]*DSML[^>]*>\s*$`)
)

// MarkerKey strips whitespace so prefix checks ignore spacing variants.
func MarkerKey(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, " ", ""), "　", "")
}

// MarkerPrefixOK reports whether text is a (possibly partial) marker.
func MarkerPrefixOK(text string) bool {
	key := MarkerKey(text)
	if key == "" {
		return false
	}
	return strings.HasPrefix(markerCore, key) || strings.HasPrefix(key, markerCore)
}

// LastMarkerSpan returns the last marker's [start,end) span, or (-1,-1).
func LastMarkerSpan(text string) (int, int) {
	locs := markerRe.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return -1, -1
	}
	last := locs[len(locs)-1]
	return last[0], last[1]
}

// NormalizeLeakMarker rewrites angle-bracket and hybrid forms to the canonical
// square-bracket form so exactly one code path handles detection.
func NormalizeLeakMarker(text string) string {
	if text == "" || !strings.Contains(text, "assistant") {
		return text
	}
	m := markerRe.FindString(text)
	if m == "" {
		return text
	}
	return markerRe.ReplaceAllString(text, Marker)
}

// StripDSMLCloseTail removes trailing DSML closing tags.
func StripDSMLCloseTail(text string) string {
	for {
		trimmed := dsmlCloseRe.ReplaceAllString(text, "")
		if trimmed == text {
			return text
		}
		text = trimmed
	}
}

// DSMLTailHoldable reports whether the tail is a DSML closing tag, complete or
// cut at a frame boundary.
func DSMLTailHoldable(tail string) bool {
	s := strings.TrimSpace(tail)
	if s == "" {
		return false
	}
	if !strings.Contains(s, "<") {
		return false
	}
	stripped := strings.TrimSpace(dsmlCloseRe.ReplaceAllString(s, ""))
	if stripped == "" {
		return true
	}
	// Empty payload / parameter tags also appear as tails.
	for {
		before := stripped
		stripped = strings.TrimSpace(dsmlCloseRe.ReplaceAllString(stripped, ""))
		stripped = strings.TrimSpace(regexp.MustCompile(`<[^>]*DSML[^>]*>\s*$`).
			ReplaceAllString(stripped, ""))
		if stripped == before {
			break
		}
	}
	if strings.TrimSpace(stripped) == "" {
		return true
	}
	// A truncated tag: starts with "<", has no ">" yet, is short, and looks like
	// a DSML tag in progress.
	if strings.HasPrefix(strings.TrimSpace(stripped), "<") &&
		!strings.Contains(stripped, ">") && len(stripped) <= 80 {
		upper := strings.ToUpper(stripped)
		return strings.Contains(upper, "DSML") || strings.HasPrefix(upper, "</")
	}
	return false
}

// MarkerStartsAt reports whether text already begins with the marker.
func MarkerStartsAt(text string) bool {
	return markerOpenRe.MatchString(MarkerKeyPreservingLeading(text))
}

// MarkerKeyPreservingLeading keeps leading brackets so the anchored regex can
// see them.
func MarkerKeyPreservingLeading(text string) string {
	return strings.TrimLeft(text, "　")
}

// FindMarkerSplit splits text at the first marker occurrence.
// Returns (prose, hold) where hold is "" when no marker is present.
func FindMarkerSplit(text string, allowPrefix bool) (string, string) {
	if text == "" {
		return "", ""
	}
	if loc := markerRe.FindStringIndex(text); loc != nil && loc[0] >= 0 {
		return text[:loc[0]], text[loc[0]:]
	}
	if allowPrefix && MarkerPrefixOK(text) {
		return "", text
	}
	return "", ""
}

// LeakPrefixHold reports whether text could still become a marker.
func LeakPrefixHold(text string) bool {
	if text == "" {
		return false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	// Only bracket-led text can grow into a marker.
	first := trimmed[0]
	if first != '[' && first != '<' && first != '(' {
		return false
	}
	key := MarkerKey(trimmed)
	key = strings.TrimLeft(key, "[<(")
	if key == "" {
		// Only opening brackets so far.
		return len(trimmed) <= 3
	}
	return strings.HasPrefix(markerCore, key) || strings.HasPrefix(key, markerCore)
}

// ToolEchoPrefixHold reports whether text could still become a <tool_call> or a
// DSML tag.
func ToolEchoPrefixHold(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !strings.HasPrefix(trimmed, "<") {
		return false
	}
	upper := strings.ToUpper(trimmed)
	if strings.Contains(upper, "TOOL_CALL") || strings.Contains(upper, "DSML") {
		return true
	}
	// A truncated opening tag: "<too", "<|DS".
	if !strings.Contains(trimmed, ">") && len(trimmed) <= 24 {
		lower := strings.ToLower(trimmed)
		return strings.HasPrefix(lower, "<") &&
			(strings.HasPrefix(lower, "<t") || strings.HasPrefix(lower, "</") ||
				strings.HasPrefix(lower, "<|") || len(lower) <= 4)
	}
	return false
}
