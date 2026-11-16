package guards

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ToolCallOpen is the pseudo-XML tool call opening tag (issue #137).
const ToolCallOpen = "<tool_call"

var (
	callTagOpenAtRe = regexp.MustCompile(`(?i)<\s*tool_call\b([^>]*)>`)
	callTagCloseRe  = regexp.MustCompile(`(?i)<\s*/\s*tool_call\s*>`)
	callTagAttrRe   = regexp.MustCompile(`(?i)\b(?:name|tool|tool_name|function)\s*=\s*(?:"([^"]+)"|'([^']+)')`)
	// callTagLeadRe matches the leading "name + delimiter" of a tag body.
	// Go's RE2 has no lookahead, so the delimiter is captured instead of being
	// asserted and is trimmed by the caller.
	callTagLeadRe = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_.:\-]*)`)
)

// parseOneCallTag turns one tag body into a tool call.
//
//	Form A: {"name": "Bash", "arguments": {...}}
//	Form B: Bash {"command": "ls"} / Bash({...}) / bare tool name
//	Form C: <tool_call name="Bash">{...}</tool_call>
//	Form D: Bash, {"command":"ls"} / Bash: {...}
func parseOneCallTag(body, attrName string) map[string]any {
	b := StripJSONFence(strings.TrimSpace(body))
	if attrName != "" {
		b = strings.TrimLeft(b, "=:")
		b = strings.TrimSpace(b)
		if b == "" {
			return MakeToolCall(attrName, map[string]any{})
		}
		args, ok := LoadsLenient(b)
		if !ok {
			return nil
		}
		return MakeToolCall(attrName, args)
	}
	if b == "" {
		return nil
	}
	if strings.HasPrefix(b, "{") {
		obj, ok := LoadsLenient(b)
		if !ok {
			return nil
		}
		m, ok := obj.(map[string]any)
		if !ok {
			return nil
		}
		name, _ := m["name"].(string)
		if strings.TrimSpace(name) == "" {
			if v, ok := m["tool"].(string); ok {
				name = v
			}
		}
		if strings.TrimSpace(name) == "" {
			if v, ok := m["tool_name"].(string); ok {
				name = v
			}
		}
		if strings.TrimSpace(name) == "" {
			// A bare argument object with no tool name cannot be recovered.
			return nil
		}
		for _, key := range []string{"arguments", "input", "args", "parameters"} {
			if v, ok := m[key]; ok {
				return MakeToolCall(name, v)
			}
		}
		return MakeToolCall(name, map[string]any{})
	}
	m := callTagLeadRe.FindStringSubmatchIndex(b)
	if m == nil {
		return nil
	}
	// The name pattern includes ':' for namespaced tools (`ns:tool`), but in
	// `Read: {...}` the colon is a separator, so only a trailing colon is
	// stripped. The delimiter itself is not consumed by the pattern (RE2 has no
	// lookahead), which is why it is trimmed from the remainder here.
	name := strings.TrimRight(b[m[2]:m[3]], ":")
	rest := strings.TrimSpace(b[m[1]:])
	rest = strings.TrimLeft(rest, ",:=")
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")") {
		rest = strings.TrimSpace(rest[1 : len(rest)-1])
	}
	if rest == "" {
		return MakeToolCall(name, map[string]any{})
	}
	args, ok := LoadsLenient(rest)
	if !ok {
		return nil
	}
	return MakeToolCall(name, args)
}

// ParseToolCallTags recovers `<tool_call>Name {json}</tool_call>` invocations.
//
// Strict guards, so a reply that *explains* this format is not turned into a
// call the client cannot execute:
//  1. at least one tag block yields a tool name plus valid JSON arguments;
//  2. when the request declares tools, every recovered name must be among them;
//  3. without a gateway marker before the block, the whole message must be one
//     block (a tutorial has prose on both sides).
//
// Returns (calls, cleanText); cleanText is the text with the tag blocks and
// markers removed, or the original text when nothing was recovered.
func ParseToolCallTags(text string, allowedNames map[string]bool) ([]map[string]any, string) {
	t := text
	if t == "" || !strings.Contains(strings.ToLower(t), ToolCallOpen) {
		return nil, text
	}
	type span struct{ start, end int }
	var spans []span
	var calls []map[string]any
	pos := 0
	for {
		loc := callTagOpenAtRe.FindStringSubmatchIndex(t[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[0]
		attrEnd := pos + loc[1]
		attrs := t[loc[2]:loc[3]]
		attrName := ""
		if am := callTagAttrRe.FindStringSubmatch(attrs); am != nil {
			attrName = am[1]
			if attrName == "" {
				attrName = am[2]
			}
		}
		closeLoc := callTagCloseRe.FindStringIndex(t[attrEnd:])
		nextLoc := callTagOpenAtRe.FindStringSubmatchIndex(t[attrEnd:])
		var body string
		var end int
		switch {
		case nextLoc != nil && (closeLoc == nil || nextLoc[0] < closeLoc[0]):
			// Missing closing tag (truncated stream): the body ends at the next
			// opening tag.
			body = t[attrEnd : attrEnd+nextLoc[0]]
			end = attrEnd + nextLoc[0]
		case closeLoc != nil:
			body = t[attrEnd : attrEnd+closeLoc[0]]
			end = attrEnd + closeLoc[1]
		default:
			body = t[attrEnd:]
			end = len(t)
		}
		if call := parseOneCallTag(body, attrName); call != nil {
			calls = append(calls, call)
			spans = append(spans, span{start, end})
		}
		if end <= start {
			break
		}
		pos = end
	}
	if len(calls) == 0 {
		return nil, text
	}
	// Without declared tools, a `<tool_call>` is almost always a discussion of
	// the format (tutorial, question, pasted example) — do not convert it.
	if len(allowedNames) == 0 {
		return nil, text
	}
	for _, c := range calls {
		fn := c["function"].(map[string]any)
		if !allowedNames[fn["name"].(string)] {
			return nil, text
		}
	}
	hasMarker := markerRe.MatchString(t)
	lead := strings.TrimSpace(strings.Trim(strings.TrimSpace(markerRe.ReplaceAllString(t[:spans[0].start], "")), "`"))
	if !hasMarker && lead != "" {
		return nil, text
	}
	tailTxt := t[spans[len(spans)-1].end:]
	tailTxt = strings.TrimSpace(strings.Trim(strings.TrimSpace(markerRe.ReplaceAllString(tailTxt, "")), "`"))
	if tailTxt != "" && (leakStructRe.MatchString(tailTxt) || callTagOpenAtRe.MatchString(tailTxt)) {
		return nil, text
	}
	if !hasMarker && tailTxt != "" {
		return nil, text
	}

	var clean strings.Builder
	last := 0
	for _, sp := range spans {
		clean.WriteString(t[last:sp.start])
		last = sp.end
	}
	clean.WriteString(t[last:])
	out := markerRe.ReplaceAllString(clean.String(), "")
	out = angleMarkerRe.ReplaceAllString(out, "")
	return calls, strings.TrimSpace(out)
}

var angleMarkerRe = regexp.MustCompile(`<\s*assistant\s*请求调用工具\s*>`)

// ParseAnyToolEcho tries every recovery strategy in the documented order.
// Returns (calls, cleanText, kind) where kind is "", "marker", "dsml" or "tag".
func ParseAnyToolEcho(text string, allowedNames map[string]bool) ([]map[string]any, string, string) {
	if calls, prefix := ParseLeakedToolCalls(text, allowedNames); calls != nil {
		leakRecovered.Add(1)
		return calls, prefix, "marker"
	}
	if calls, clean := protocolDSML(text); len(calls) > 0 {
		dsmlRecovered.Add(1)
		return calls, clean, "dsml"
	}
	if calls, clean := ParseToolCallTags(text, allowedNames); len(calls) > 0 {
		tagRecovered.Add(1)
		return calls, clean, "tag"
	}
	return nil, text, ""
}

// protocolDSML adapts protocol.ParseDSMLToolCalls into tool-call maps.
func protocolDSML(text string) ([]map[string]any, string) {
	parsed, clean := protocolParseDSML(text)
	if len(parsed) == 0 {
		return nil, text
	}
	out := make([]map[string]any, 0, len(parsed))
	for _, p := range parsed {
		out = append(out, map[string]any{
			"id": p.ID, "type": "function",
			"function": map[string]any{"name": p.Name, "arguments": p.Arguments},
		})
	}
	return out, clean
}

// ToolNamesFromPayload extracts the tool names declared in a request body.
func ToolNamesFromPayload(payload map[string]any) map[string]bool {
	out := map[string]bool{}
	tools, _ := payload["tools"].([]any)
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := m["name"].(string); ok && name != "" {
			out[name] = true
		}
		if fn, ok := m["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				out[name] = true
			}
		}
	}
	return out
}

// NormalizeJSONArgs re-serialises a decoded object as compact JSON.
func NormalizeJSONArgs(v any) string {
	blob, err := marshalNoEscape(v)
	if err != nil {
		return "{}"
	}
	return string(blob)
}

// IsValidJSONObject reports whether raw parses to a JSON object.
func IsValidJSONObject(raw string) bool {
	var v map[string]any
	return json.Unmarshal([]byte(raw), &v) == nil
}
