package protocol

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ToolCallOpen is the pseudo-XML tool call opening tag.
const ToolCallOpen = "<tool_call>"

var (
	tagStart = `<[^>]*DSML[^>]*`

	// DSMLCallsRe matches the whole <|DSML|function_calls> block.
	DSMLCallsRe = regexp.MustCompile(`(?s)` + tagStart + `calls>(.*?)</[^>]*DSML[^>]*calls>`)
	// DSMLInvokeRe matches one invoke block.
	DSMLInvokeRe = regexp.MustCompile(`(?s)` + tagStart + `invoke\s+name=["']([^"']+)["']>(.*?)</[^>]*invoke>`)
	// DSMLParamRe matches one parameter block.
	DSMLParamRe = regexp.MustCompile(`(?s)` + tagStart + `parameter\s+name=["']([^"']+)["'][^>]*>(.*?)</[^>]*parameter>`)

	// LeakMarkerRe matches every brace variant of the leak marker.
	LeakMarkerRe = regexp.MustCompile(`[\[<(]*\s*assistant\s*请求调用工具\s*[\]>)]?`)
	// ToolTagRe matches <tool_call>Name {json}</tool_call>.
	ToolTagRe = regexp.MustCompile(`(?s)<tool_call>\s*([A-Za-z_][\w.\-]*)\s*(\{.*?\})?\s*</tool_call>`)
	// ToolTagOpenRe matches the bare opening tag variants.
	ToolTagOpenRe = regexp.MustCompile(`(?i)<tool_call[^>]*>|</tool_call>`)
)

// ParsedToolCall is one recovered tool invocation.
type ParsedToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ParseDSMLToolCalls recovers DeepSeek's native DSML tool calls from plain text.
//
// A missing <|DSML|calls> wrapper (or a truncated closing tag) is tolerated:
// the invoke blocks are then searched in the whole text.
func ParseDSMLToolCalls(text string) ([]ParsedToolCall, string) {
	if text == "" || !strings.Contains(text, "DSML") {
		return nil, text
	}
	match := DSMLCallsRe.FindStringSubmatchIndex(text)
	var callsBlock string
	if match == nil {
		if !DSMLInvokeRe.MatchString(text) {
			return nil, text
		}
		callsBlock = text
	} else {
		callsBlock = text[match[2]:match[3]]
	}

	var calls []ParsedToolCall
	for _, inv := range DSMLInvokeRe.FindAllStringSubmatch(callsBlock, -1) {
		params := map[string]any{}
		ordered := []string{}
		paramBlock := inv[2]
		for _, p := range DSMLParamRe.FindAllStringSubmatch(paramBlock, -1) {
			name, value := p[1], strings.TrimSpace(p[2])
			if _, exists := params[name]; !exists {
				ordered = append(ordered, name)
			}
			var decoded any
			if err := json.Unmarshal([]byte(value), &decoded); err == nil {
				params[name] = decoded
			} else {
				params[name] = value
			}
		}
		_ = ordered
		args, err := MarshalNoEscape(params)
		if err != nil {
			args = []byte("{}")
		}
		calls = append(calls, ParsedToolCall{
			ID: NewRequestIDPrefix("call_"), Name: inv[1], Arguments: string(args),
		})
	}

	var clean string
	if match != nil {
		clean = strings.TrimSpace(strings.TrimSpace(text[:match[0]]) + " " +
			strings.TrimSpace(text[match[1]:]))
	} else {
		// No calls wrapper: cut from the tag start preceding the first invoke.
		idx := DSMLInvokeRe.FindStringIndex(text)
		start := 0
		if idx != nil {
			start = strings.LastIndex(text[:idx[0]], "<")
			if start < 0 {
				start = idx[0]
			}
		}
		clean = strings.TrimSpace(text[:start])
	}
	return calls, clean
}

// ParseToolCallTags recovers pseudo-XML `<tool_call>Name {json}</tool_call>`
// invocations (issue #137).
func ParseToolCallTags(text string, allowedNames map[string]bool) ([]ParsedToolCall, string) {
	if text == "" || !strings.Contains(strings.ToLower(text), ToolCallOpen) {
		return nil, text
	}
	var calls []ParsedToolCall
	var spans [][2]int
	for _, m := range ToolTagRe.FindAllStringSubmatchIndex(text, -1) {
		name := text[m[2]:m[3]]
		if allowedNames != nil && len(allowedNames) > 0 && !allowedNames[name] {
			continue
		}
		var args string
		if m[4] >= 0 {
			raw := text[m[4]:m[5]]
			if normalized, ok := normalizeArgumentJSON(raw); ok {
				args = normalized
			} else {
				continue
			}
		} else {
			args = "{}"
		}
		calls = append(calls, ParsedToolCall{
			ID: NewRequestIDPrefix("call_"), Name: name, Arguments: args,
		})
		spans = append(spans, [2]int{m[0], m[1]})
	}
	if len(calls) == 0 {
		return nil, text
	}
	clean := removeSpans(text, spans)
	clean = ToolTagOpenRe.ReplaceAllString(clean, "")
	return calls, strings.TrimSpace(clean)
}

func removeSpans(text string, spans [][2]int) string {
	var b strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp[0] < pos {
			continue
		}
		b.WriteString(text[pos:sp[0]])
		pos = sp[1]
	}
	b.WriteString(text[pos:])
	return b.String()
}

// normalizeArgumentJSON validates and re-serialises an argument object.
func normalizeArgumentJSON(raw string) (string, bool) {
	var v any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	blob, err := MarshalNoEscape(m)
	if err != nil {
		return "", false
	}
	return string(blob), true
}

// FlattenContent flattens Responses-style content into text (or vision parts).
func FlattenContent(content any) any {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if m, ok := content.(map[string]any); ok {
		content = []any{m}
	}
	list, ok := content.([]any)
	if !ok {
		return asString(content)
	}
	var texts []string
	parts := make([]any, 0, len(list))
	hasImage := false
	for _, piece := range list {
		switch p := piece.(type) {
		case string:
			texts = append(texts, p)
			parts = append(parts, map[string]any{"type": "text", "text": p})
		case map[string]any:
			typ := asString(p["type"])
			switch typ {
			case "input_text", "output_text", "text", "summary_text":
				t := asString(p["text"])
				texts = append(texts, t)
				parts = append(parts, map[string]any{"type": "text", "text": t})
			case "input_image", "image_url", "image":
				if u := imageURLOf(p); u != "" {
					parts = append(parts, map[string]any{
						"type": "image_url", "image_url": map[string]any{"url": u},
					})
					hasImage = true
				}
			}
		}
	}
	if hasImage {
		return parts
	}
	return strings.Join(nonEmpty(texts), "\n")
}
