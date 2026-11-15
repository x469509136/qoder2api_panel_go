package guards

import (
	"encoding/json"
	"regexp"
	"strings"
)

const jsonFence = "```"

var trailingCommaRe = regexp.MustCompile(`,\s*([}\]])`)

// looseEchoRe matches the single-call echo shape with unescaped inner quotes:
//
//	[{"name":"Bash","arguments":"{"command":"ls"}"}]
var looseEchoRe = regexp.MustCompile(
	`^\[\s*\{\s*"name"\s*:\s*"([^"]{1,200})"\s*,\s*"arguments"\s*:\s*"(.*)"\s*\}\s*\]$`)

// StripJSONFence removes leading/trailing code-fence remnants.
//
// In streaming, the marker split can cut the opening fence into the prose, so
// the held buffer is "marker + array + newline + closing fence". Only the ends
// are touched; a fence in the middle is left alone.
func StripJSONFence(text string) string {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, jsonFence) {
		inner := t[len(jsonFence):]
		if len(inner) >= 4 && strings.EqualFold(inner[:4], "json") {
			inner = inner[4:]
		}
		t = strings.TrimLeft(inner, " \t\r\n")
	}
	if strings.HasSuffix(t, jsonFence) && len(t) > len(jsonFence) {
		t = strings.TrimRight(t[:len(t)-len(jsonFence)], " \t\r\n")
	}
	return t
}

// RepairJSONQuotes escapes bare inner double quotes.
//
// The model writes {"arguments": "{"filename":"a.png"}"} — the inner quotes are
// unescaped and the outer array is not valid JSON. Already-escaped quotes are
// preserved.
func RepairJSONQuotes(text string) string {
	const placeholder = "\x00"
	s := strings.ReplaceAll(text, `\"`, placeholder)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, placeholder, `\"`)
}

// EscapeUnescapedQuotes escapes only the inner literal quotes, tracking JSON
// string state.
//
// A quote inside a string closes it only when the next non-space character is
// one of , } ] : or the text ends; anything else means it is a literal quote the
// model forgot to escape.
func EscapeUnescapedQuotes(s string) string {
	var out strings.Builder
	inString := false
	i := 0
	for i < len(s) {
		c := s[i]
		if !inString {
			out.WriteByte(c)
			if c == '"' {
				inString = true
			}
			i++
			continue
		}
		if c == '\\' && i+1 < len(s) {
			out.WriteString(s[i : i+2])
			i += 2
			continue
		}
		if c == '"' {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\r' || s[j] == '\n') {
				j++
			}
			if j >= len(s) || strings.ContainsRune(",}]:", rune(s[j])) {
				out.WriteByte(c)
				inString = false
			} else {
				out.WriteString(`\"`)
			}
			i++
			continue
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

// RepairArgsJSON repairs a malformed arguments string.
//
// It tries, in order: strict, escaped inner quotes, trailing-comma removal, and
// the combinations. Returns the repaired string, or nil when nothing parses —
// the caller then treats it as a failure rather than inventing a call.
func RepairArgsJSON(raw string) *string {
	text := strings.TrimSpace(raw)
	if text == "" {
		empty := ""
		return &empty
	}
	cands := []string{text}
	if v := RepairJSONQuotes(text); v != text {
		cands = append(cands, v)
	}
	if v := EscapeUnescapedQuotes(text); v != text {
		cands = append(cands, v)
	}
	base := append([]string{}, cands...)
	for _, c := range base {
		cands = append(cands, trailingCommaRe.ReplaceAllString(c, "$1"))
	}
	for _, cand := range cands {
		var v any
		if json.Unmarshal([]byte(cand), &v) == nil {
			got := cand
			return &got
		}
	}
	return nil
}

// LoadsLenient parses tool argument text, trying every repair.
func LoadsLenient(raw string) (any, bool) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return map[string]any{}, true
	}
	noTrailing := trailingCommaRe.ReplaceAllString(t, "$1")
	escaped := EscapeUnescapedQuotes(t)
	cands := []string{t}
	if noTrailing != t {
		cands = append(cands, noTrailing)
	}
	if escaped != t {
		cands = append(cands, escaped)
		cands = append(cands, trailingCommaRe.ReplaceAllString(escaped, "$1"))
	}
	for _, cand := range cands {
		var v any
		if json.Unmarshal([]byte(cand), &v) == nil {
			return v, true
		}
	}
	return nil, false
}

// SalvageEchoArray reconstructs the loose single-call echo shape.
func SalvageEchoArray(rest string) []map[string]any {
	m := looseEchoRe.FindStringSubmatch(strings.TrimSpace(rest))
	if m == nil {
		return nil
	}
	name, rawArgs := m[1], m[2]
	for _, cand := range []string{rawArgs, RepairJSONQuotes(rawArgs)} {
		var v any
		if json.Unmarshal([]byte(cand), &v) == nil {
			return []map[string]any{{"name": name, "arguments": cand}}
		}
	}
	return nil
}

var toolNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:\-]*$`)

// MakeToolCall assembles a structured tool call; nil when the arguments are not
// valid JSON.
func MakeToolCall(name string, args any) map[string]any {
	clean := strings.Trim(strings.TrimSpace(name), "\"'`")
	if clean == "" || !toolNameRe.MatchString(clean) {
		return nil
	}
	var argStr string
	switch v := args.(type) {
	case map[string]any, []any:
		blob, err := marshalNoEscape(v)
		if err != nil {
			return nil
		}
		argStr = string(blob)
	case string:
		argStr = strings.TrimSpace(v)
		if argStr == "" {
			argStr = "{}"
		}
	case nil:
		argStr = "{}"
	default:
		blob, err := marshalNoEscape(v)
		if err != nil {
			return nil
		}
		argStr = string(blob)
	}
	var parsed any
	if json.Unmarshal([]byte(argStr), &parsed) != nil {
		return nil
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": clean, "arguments": argStr,
		},
	}
}

func marshalNoEscape(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(sb.String(), "\n")), nil
}
