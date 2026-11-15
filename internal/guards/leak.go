package guards

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/xzs/bridge_qoder/internal/protocol"
)

// ToolResultClose is the closing marker the model often invents.
const ToolResultClose = "[工具结果结束]"

// ToolResultMarker is the opening marker the gateway writes.
const ToolResultMarker = protocol.ToolResultMarker

// HoldWindow is the cross-frame buffering window: the longest marker minus one.
//
// Only a genuine marker prefix inside the last few characters of a frame is
// held; the next frame disproving it releases the bytes immediately (fail-open).
const HoldWindow = 17 // len(LeakMarker) - 1

// HoldMaxChars bounds the hold buffer.
//
// A model writing a very long JSON or regex example can keep text "looking like
// a candidate", which would grow the buffer without limit. Past this size the
// buffer is released with a warning: missing one recovery is better than
// stalling the client forever.
const HoldMaxChars = 32768

// HoldMaxFrames bounds the number of non-growing frames spent holding.
const HoldMaxFrames = 200

// ParseLeakedToolCalls recovers "marker + JSON array" echoes.
//
// Guards (so a reply that merely discusses the marker is not mistaken for a
// call):
//  1. after trimming, the text is exactly marker + JSON array (an optional code
//     fence is tolerated);
//  2. the array is non-empty, every item's name is a non-empty string and its
//     arguments parse as JSON;
//  3. when the request declares tools, every name must be among them.
//
// Returns (calls, prefix): prefix is any prose that preceded the last marker
// (the caller re-emits it), and it is unchanged text when nothing was recovered.
func ParseLeakedToolCalls(text string, allowedNames map[string]bool) ([]map[string]any, string) {
	t := strings.TrimSpace(text)
	t = NormalizeLeakMarker(t)
	t = StripDSMLCloseTail(t)
	if strings.HasPrefix(t, jsonFence) && strings.HasSuffix(t, jsonFence) && len(t) > 6 {
		inner := t[3:]
		if len(inner) >= 4 && strings.EqualFold(inner[:4], "json") {
			inner = inner[4:]
		}
		t = strings.TrimSpace(inner[:len(inner)-3])
	}
	// A repeated closing tag may precede the array with no marker at all.
	t = strings.TrimSpace(leadingDSMLClosers.ReplaceAllString(t, ""))

	pos, end := LastMarkerSpan(t)
	if pos < 0 {
		if strings.HasPrefix(t, "[") {
			// No marker but the whole text is a {name, arguments} array: reuse the
			// same strict path.
			t = Marker + "\n" + t
			pos, end = 0, len(Marker)
		} else {
			return nil, text
		}
	}
	prefix := strings.TrimRight(markerRe.ReplaceAllString(t[:pos], ""), " \t\r\n")
	t = t[end:]
	for {
		trimmed := strings.TrimLeft(t, " \t\r\n")
		loc := markerRe.FindStringIndex(trimmed)
		if loc == nil || loc[0] != 0 {
			break
		}
		t = trimmed[loc[1]:]
	}
	t = strings.TrimLeft(t, " \t\r\n")
	t = StripDSMLCloseTail(t)
	if strings.HasSuffix(t, jsonFence) && len(t) > len(jsonFence) {
		t = StripDSMLCloseTail(strings.TrimRight(t[:len(t)-len(jsonFence)], " \t\r\n"))
	}
	rest := strings.TrimSpace(t)
	if !(strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]")) {
		// Form C: the marker is followed by junk and the array's opening bracket
		// is missing; wrap the first { to the last } back into an array.
		i := strings.Index(rest, "{")
		j := strings.LastIndex(rest, "}")
		if !strings.HasPrefix(rest, "[") && i >= 0 && i < j {
			cand := "[" + rest[i:j+1] + "]"
			var v any
			if json.Unmarshal([]byte(cand), &v) != nil {
				return nil, text
			}
			rest = cand
		} else {
			return nil, text
		}
	}

	var arr []any
	if err := json.Unmarshal([]byte(rest), &arr); err != nil {
		// Unescaped inner quotes make even the outer array invalid: salvage the
		// single-call shape once.
		salvaged := SalvageEchoArray(rest)
		if salvaged == nil {
			return nil, text
		}
		arr = make([]any, 0, len(salvaged))
		for _, s := range salvaged {
			arr = append(arr, s)
		}
	}
	if len(arr) == 0 {
		return nil, text
	}

	calls := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, text
		}
		name, ok := m["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, text
		}
		args := m["arguments"]
		var argStr string
		switch v := args.(type) {
		case map[string]any, []any:
			blob, err := marshalNoEscape(v)
			if err != nil {
				return nil, text
			}
			argStr = string(blob)
		case string:
			argStr = v
		case nil:
			return nil, text
		default:
			return nil, text
		}
		if strings.TrimSpace(argStr) != "" {
			var parsed any
			if json.Unmarshal([]byte(argStr), &parsed) != nil {
				fixed := RepairArgsJSON(argStr)
				if fixed == nil {
					return nil, text
				}
				argStr = *fixed
				NoteArgsRepaired()
			}
		}
		call := map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": strings.TrimSpace(name), "arguments": argStr,
			},
		}
		calls = append(calls, call)
	}
	if len(allowedNames) > 0 {
		for _, c := range calls {
			fn := c["function"].(map[string]any)
			if !allowedNames[fn["name"].(string)] {
				return nil, text
			}
		}
	}
	return calls, prefix
}

var leadingDSMLClosers = regexp.MustCompile(`^(?:\s*</[^>]*DSML[^>]*>)+`)

// EchoWithBadArgs reports whether text is a marker echo whose inner arguments
// are unparseable and unrepairable (issue #37).
//
// Every condition must hold, so ordinary prose, a discussion of the marker and a
// valid JSON example never match.
func EchoWithBadArgs(text string, allowedNames map[string]bool) bool {
	t := strings.TrimSpace(text)
	t = NormalizeLeakMarker(t)
	prev := "\x00"
	for prev != t {
		prev = t
		t = strings.TrimSpace(StripJSONFence(StripDSMLCloseTail(t)))
	}
	lastPos := strings.LastIndex(t, Marker)
	if lastPos < 0 {
		return false
	}
	t = t[lastPos:]
	for strings.HasPrefix(t, Marker) {
		t = strings.TrimLeft(t[len(Marker):], " \t\r\n")
	}
	t = StripDSMLCloseTail(t)
	if strings.HasSuffix(t, jsonFence) && len(t) > len(jsonFence) {
		t = StripDSMLCloseTail(strings.TrimRight(t[:len(t)-len(jsonFence)], " \t\r\n"))
	}
	rest := strings.TrimSpace(t)
	if !(strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]")) {
		return false
	}
	var arr []any
	if err := json.Unmarshal([]byte(rest), &arr); err != nil {
		return false
	}
	if len(arr) == 0 {
		return false
	}
	bad := 0
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return false
		}
		name, ok := m["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return false
		}
		if len(allowedNames) > 0 && !allowedNames[strings.TrimSpace(name)] {
			return false
		}
		args, hasArgs := m["arguments"]
		if !hasArgs {
			return false
		}
		switch v := args.(type) {
		case map[string]any, []any:
			continue
		case string:
			if strings.TrimSpace(v) == "" {
				continue
			}
			var parsed any
			if json.Unmarshal([]byte(v), &parsed) != nil {
				// A repairable argument is not a "bad argument": it goes through
				// the normal recovery path instead of failing the whole turn.
				if RepairArgsJSON(v) == nil {
					bad++
				}
			}
		default:
			return false
		}
	}
	return bad > 0
}

// ---------------------------------------------------------------------------
// Recovery statistics
// ---------------------------------------------------------------------------

// Recovery counters, exposed on /usage/perf.
var (
	argsRepaired     atomic.Int64
	leakRecovered    atomic.Int64
	dsmlRecovered    atomic.Int64
	tagRecovered     atomic.Int64
	droppedFragments atomic.Int64
	lenientTurns     atomic.Int64
)

// NoteArgsRepaired counts one repaired argument payload.
func NoteArgsRepaired() { argsRepaired.Add(1) }

// RecoverySnapshot returns the counter values.
func RecoverySnapshot() map[string]int64 {
	return map[string]int64{
		"args_repaired":     argsRepaired.Load(),
		"leak_recovered":    leakRecovered.Load(),
		"dsml_recovered":    dsmlRecovered.Load(),
		"tag_recovered":     tagRecovered.Load(),
		"dropped_fragments": droppedFragments.Load(),
		"lenient_turns":     lenientTurns.Load(),
	}
}
