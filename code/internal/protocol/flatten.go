package protocol

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

// Markers embedded in the textual tool-history form.
const (
	// LeakMarker prefixes a serialized tool_calls array in assistant content.
	// Long sessions sometimes make the model echo this scaffold as plain text;
	// the guards in the guards package read it back.
	LeakMarker = "[assistant 请求调用工具]"
	// LeakMarkerAlt is the angle-bracket variant the same upstream converter
	// sometimes emits.
	LeakMarkerAlt = "<assistant 请求调用工具>"
	// ToolResultMarker prefixes a flattened tool result.
	ToolResultMarker = "[工具结果"
)

// FlattenOptions tunes history flattening.
type FlattenOptions struct {
	// KeepReasoning keeps the assistant reasoning_content field. Only the
	// DeepSeek family needs it: multi-turn consistency is tied to it, and other
	// models reject the unknown field.
	KeepReasoning bool
	// Structured sends tool history as native tool_calls / role:tool.
	Structured bool
	// ToolResultKeep is how many chars of each end of an older tool result to
	// keep (0 disables truncation).
	ToolResultKeep int
	// ToolResultScope is "tail" (default), "all" or "turn".
	ToolResultScope string
	// ToolResultKeepTail is how many trailing tool results stay intact in
	// "tail" mode.
	ToolResultKeepTail int
}

// FlattenResult is the flattened conversation.
type FlattenResult struct {
	// System is the first system/developer message, or "".
	System string
	// Messages is the flattened list in upstream shape.
	Messages []map[string]any
	// Images collects every inline image URL seen (for the optional legacy
	// top-level fields).
	Images []string
}

// FlattenMessages turns a client conversation into the flat {role, content}
// sequence the upstream accepts.
//
// Truncation of older tool results is what keeps the prompt prefix stable:
// once a result has been truncated, every later turn serialises identical
// bytes. The "turn" scope breaks this by re-truncating all history every time
// the user sends a message, which drops the upstream cache hit rate from ~95%
// to ~8%.
func FlattenMessages(messages []Message, opts FlattenOptions) FlattenResult {
	if opts.ToolResultScope == "" {
		opts.ToolResultScope = "tail"
	}
	if opts.ToolResultKeep < 0 {
		opts.ToolResultKeep = 0
	}

	var out FlattenResult
	out.Messages = make([]map[string]any, 0, len(messages))

	protect := map[int]bool{}
	if opts.ToolResultKeep > 0 {
		switch opts.ToolResultScope {
		case "turn":
			start := currentTurnStart(messages)
			for i := start; i < len(messages); i++ {
				protect[i] = true
			}
		case "tail":
			var toolIdx []int
			for i, m := range messages {
				if m.Role == "tool" {
					toolIdx = append(toolIdx, i)
				}
			}
			keep := opts.ToolResultKeepTail
			if keep <= 0 {
				keep = 1
			}
			if keep > len(toolIdx) {
				keep = len(toolIdx)
			}
			for _, i := range toolIdx[len(toolIdx)-keep:] {
				protect[i] = true
			}
		}
	}

	for idx, m := range messages {
		text, imgs := flattenContentText(m.Content)
		out.Images = append(out.Images, imgs...)

		switch m.Role {
		case "system", "developer":
			if out.System == "" && text != "" {
				out.System = text
			}
			continue

		case "tool":
			body := text
			if opts.ToolResultKeep > 0 && !protect[idx] {
				body = shrinkToolResult(body, opts.ToolResultKeep)
			}
			if opts.Structured {
				// Keep the tool role and its pairing id. content is always an
				// empty string, never null: null makes the upstream converter
				// drop the paired assistant message and DeepSeek/Kimi then fail
				// with provider_error.
				out.Messages = append(out.Messages, map[string]any{
					"role":         "tool",
					"tool_call_id": m.ToolCallID,
					"content":      body,
				})
				continue
			}
			suffix := ""
			if m.Name != "" {
				suffix = " (" + m.Name + ")"
			}
			out.Messages = append(out.Messages, map[string]any{
				"role":    "user",
				"content": ToolResultMarker + suffix + "]\n" + body,
			})
			continue

		case "assistant":
			if len(m.ToolCalls) > 0 {
				if opts.Structured {
					calls := make([]map[string]any, 0, len(m.ToolCalls))
					for _, tc := range m.ToolCalls {
						typ := tc.Type
						if typ == "" {
							typ = "function"
						}
						calls = append(calls, map[string]any{
							"id":   tc.ID,
							"type": typ,
							"function": map[string]any{
								"name":      tc.Function.Name,
								"arguments": normalizeArguments(tc.Function.Arguments),
							},
						})
					}
					item := map[string]any{
						"role": "assistant", "content": text,
						"tool_calls": calls,
					}
					if opts.KeepReasoning && m.ReasoningContent != nil {
						item["reasoning_content"] = *m.ReasoningContent
					}
					out.Messages = append(out.Messages, item)
					continue
				}
				calls := make([]map[string]any, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					calls = append(calls, map[string]any{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
				}
				blob, _ := MarshalNoEscape(calls)
				text = text + "\n\n" + LeakMarker + "\n" + string(blob)
				item := map[string]any{"role": "assistant", "content": text}
				if opts.KeepReasoning && m.ReasoningContent != nil {
					item["reasoning_content"] = *m.ReasoningContent
				}
				out.Messages = append(out.Messages, item)
				continue
			}
			item := map[string]any{"role": "assistant", "content": text}
			if opts.KeepReasoning && m.ReasoningContent != nil {
				item["reasoning_content"] = *m.ReasoningContent
			}
			out.Messages = append(out.Messages, item)
			continue

		case "user":
			if len(imgs) > 0 {
				// The upstream only reads structured image_url parts inside the
				// message content; the top-level image_urls fields are ignored.
				parts := flattenContent(m.Content)
				if list, ok := parts.([]any); ok {
					out.Messages = append(out.Messages, map[string]any{"role": "user", "content": list})
				} else {
					out.Messages = append(out.Messages, map[string]any{"role": "user", "content": text})
				}
			} else {
				out.Messages = append(out.Messages, map[string]any{"role": "user", "content": text})
			}
			continue
		}

		// Unknown roles degrade to user rather than being silently dropped.
		if m.Role != "" {
			out.Messages = append(out.Messages, map[string]any{"role": "user", "content": text})
		}
	}
	return out
}

func normalizeArguments(args string) string {
	if args == "" {
		return ""
	}
	trimmed := strings.TrimSpace(args)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return args
	}
	// A bare string is coerced into valid JSON so the upstream never sees
	// malformed arguments from a client that was sloppy.
	blob, err := json.Marshal(args)
	if err != nil {
		return args
	}
	return string(blob)
}

// flattenContentText turns a content value into plain text plus image URLs.
func flattenContentText(content any) (string, []string) {
	if content == nil {
		return "", nil
	}
	if s, ok := content.(string); ok {
		return s, nil
	}
	if m, ok := content.(map[string]any); ok {
		content = []any{m}
	}
	list, ok := content.([]any)
	if !ok {
		return asString(content), nil
	}
	var texts []string
	var images []string
	for _, piece := range list {
		switch p := piece.(type) {
		case string:
			texts = append(texts, p)
		case map[string]any:
			typ, _ := p["type"].(string)
			switch typ {
			case "text", "input_text", "output_text", "summary_text":
				if t, ok := p["text"].(string); ok {
					texts = append(texts, t)
				}
			case "image_url", "input_image", "image":
				if u := imageURLOf(p); u != "" {
					images = append(images, u)
				}
			}
		}
	}
	return strings.Join(texts, "\n"), images
}

func imageURLOf(p map[string]any) string {
	if iu, ok := p["image_url"].(map[string]any); ok {
		if u, ok := iu["url"].(string); ok {
			return u
		}
	}
	if u, ok := p["image_url"].(string); ok {
		return u
	}
	if u, ok := p["url"].(string); ok {
		return u
	}
	if data, ok := p["data"].(string); ok && data != "" {
		mime := "image/png"
		if m, ok := p["mimeType"].(string); ok && m != "" {
			mime = m
		} else if m, ok := p["mime_type"].(string); ok && m != "" {
			mime = m
		}
		return "data:" + mime + ";base64," + data
	}
	return ""
}

// flattenContent keeps the structured parts form when images are present.
func flattenContent(content any) any {
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
			typ, _ := p["type"].(string)
			switch typ {
			case "text", "input_text", "output_text", "summary_text":
				t, _ := p["text"].(string)
				texts = append(texts, t)
				parts = append(parts, map[string]any{"type": "text", "text": t})
			case "image_url", "input_image", "image":
				if u := imageURLOf(p); u != "" {
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": u},
					})
					hasImage = true
				}
			}
		}
	}
	if hasImage {
		return parts
	}
	return strings.Join(texts, "\n")
}

// shrinkToolResult keeps the first and last `keep` characters.
//
// The omission marker is a fixed string with no counts, so the same result
// always produces the same bytes.
func shrinkToolResult(text string, keep int) string {
	if keep <= 0 {
		return text
	}
	r := []rune(text)
	if len(r) <= keep*2 {
		return text
	}
	return string(r[:keep]) + "\n\n... [已截短] ...\n\n" + string(r[len(r)-keep:])
}

// currentTurnStart finds the start of the current turn (after the last
// assistant message).
func currentTurnStart(messages []Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			return i + 1
		}
	}
	return 0
}

// IsDeepSeekModel reports whether the model belongs to the DeepSeek family,
// whose multi-turn consistency is bound to reasoning_content.
var DeepSeekModelKeys = []string{"dmodel", "dfmodel"}

// IsKimiModel reports whether the model belongs to the Kimi family.
var KimiModelKeys = []string{"kmodel", "kmodel_latest"}

func modelFamilyMatch(model, key string, keys []string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	m := strings.ToLower(strings.TrimSpace(model))
	for _, family := range keys {
		if k == family || strings.HasPrefix(m, family) {
			return true
		}
	}
	// Display names.
	switch {
	case contains(keys, "dmodel"):
		if strings.Contains(m, "deepseek") {
			return true
		}
	case contains(keys, "kmodel"):
		if strings.Contains(m, "kimi") {
			return true
		}
	}
	return false
}

// IsDeepSeek reports whether model/modelKey is a DeepSeek model.
func IsDeepSeek(model, modelKey string) bool {
	return modelFamilyMatch(model, modelKey, DeepSeekModelKeys)
}

// IsKimi reports whether model/modelKey is a Kimi model.
func IsKimi(model, modelKey string) bool {
	return modelFamilyMatch(model, modelKey, KimiModelKeys)
}

// EnvInt reads an integer environment variable with bounds.
func EnvInt(name string, def, lo, hi int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// EnvTruth reports whether an environment variable is a truthy switch.
func EnvTruth(name string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		return def
	}
	switch raw {
	case "1", "on", "true", "yes", "enable", "enabled":
		return true
	case "0", "off", "false", "no", "disable", "disabled":
		return false
	}
	return def
}

// ToolResultSettings reads the truncation knobs from the environment.
func ToolResultSettings() (keep int, scope string, tail int) {
	keep = EnvInt("QD_TOOL_RESULT_KEEP", 2000, 0, 1_000_000)
	scope = strings.ToLower(strings.TrimSpace(os.Getenv("QD_TOOL_RESULT_SCOPE")))
	if scope != "all" && scope != "turn" && scope != "tail" {
		scope = "tail"
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("QD_TOOL_RESULT_KEEP"))); v == "0" || v == "off" {
		keep = 0
	}
	tail = EnvInt("QD_TOOL_RESULT_KEEP_TAIL", 1, 0, 1000)
	return
}

// StructuredToolHistoryMode resolves the QD_STRUCTURED_TOOL_HISTORY setting.
func StructuredToolHistoryMode() string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("QD_STRUCTURED_TOOL_HISTORY")))
	switch raw {
	case "on", "off":
		return raw
	}
	return "auto"
}

// StructuredRealms returns the realms where "auto" enables structured history.
func StructuredRealms() []string {
	raw := strings.TrimSpace(os.Getenv("QD_STRUCTURED_TOOL_HISTORY_REALMS"))
	if raw == "" {
		return []string{"cn", "intl"}
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// StructuredWhitelist lists the model families that get structured tool
// history under "auto".
var StructuredWhitelist = []string{"qwen", "glm", "deepseek", "kimi",
	"qmodel", "qfmodel", "qmodel_38max", "qmodel_latest", "gmodel", "gfmodel",
	"gm51model", "dmodel", "dfmodel", "kmodel", "kmodel_latest"}

// StructuredToolHistoryEnabled decides the single time whether a request uses
// native tool_calls or the textual envelope.
//
// Deciding this in two places (bridge layer and builder) is what produces the
// "client received the envelope as text" symptom.
func StructuredToolHistoryEnabled(model, modelKey, realm string, messages []Message) bool {
	mode := StructuredToolHistoryMode()
	if mode == "on" {
		return toolIDsComplete(messages)
	}
	if mode == "off" {
		return false
	}
	if realm != "" && !contains(StructuredRealms(), realm) {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(model))
	key := strings.ToLower(strings.TrimSpace(modelKey))
	matched := false
	for _, w := range StructuredWhitelist {
		if strings.Contains(name, w) || key == w || strings.HasPrefix(key, w) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	// One incomplete tool_call_id forces the whole request back to text mode.
	return toolIDsComplete(messages)
}

func toolIDsComplete(messages []Message) bool {
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			if tc.ID == "" {
				return false
			}
		}
		if m.Role == "tool" && m.ToolCallID == "" {
			return false
		}
	}
	return true
}
