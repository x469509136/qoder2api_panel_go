// Package protocol converts between the three client protocols and the single
// chat-completions shape the upstream understands.
//
// Layout:
//
//	build.go      chat request -> Qoder upstream body (baseprompt + flatten)
//	anthropic.go  Anthropic Messages <-> chat
//	responses.go  Responses API   <-> chat
//	chat.go       chat-completions inbound normalisation and aggregation
//	stream.go     SSE framing helpers
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolCall is one assistant tool invocation.
type ToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the function payload of a ToolCall.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ContentPart is one element of a multimodal message body.
type ContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *ImageURLPart `json:"image_url,omitempty"`
}

// ImageURLPart wraps the URL of an image part.
type ImageURLPart struct {
	URL string `json:"url"`
}

// Message is one chat message. Content is kept as a decoded value because
// clients send both a plain string and a parts array.
type Message struct {
	Role             string     `json:"role"`
	Content          any        `json:"content"`
	Name             string     `json:"name,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
}

// Usage is the upstream token accounting block.
type Usage struct {
	PromptTokens            int            `json:"prompt_tokens"`
	CompletionTokens        int            `json:"completion_tokens"`
	TotalTokens             int            `json:"total_tokens"`
	CachedTokens            int            `json:"cached_tokens,omitempty"`
	PromptCacheHitTokens    int            `json:"prompt_cache_hit_tokens,omitempty"`
	Credits                 float64        `json:"credits,omitempty"`
	Credit                  float64        `json:"credit,omitempty"`
	OriginalCredits         float64        `json:"original_credits,omitempty"`
	Billable                bool           `json:"billable,omitempty"`
	CompletionTokensDetails *TokensDetails `json:"completion_tokens_details,omitempty"`
	PromptTokensDetails     *TokensDetails `json:"prompt_tokens_details,omitempty"`
	Raw                     map[string]any `json:"-"`
}

// TokensDetails carries the nested cached/reasoning counters.
type TokensDetails struct {
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// DecodeUsage normalises the different upstream usage shapes.
func DecodeUsage(raw any) *Usage {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	u := &Usage{Raw: m}
	u.PromptTokens = int(asInt(m["prompt_tokens"]))
	u.CompletionTokens = int(asInt(m["completion_tokens"]))
	u.TotalTokens = int(asInt(m["total_tokens"]))
	u.CachedTokens = int(asInt(m["cached_tokens"]))
	u.PromptCacheHitTokens = int(asInt(m["prompt_cache_hit_tokens"]))
	// The upstream reports plural "credits"; the singular is only a fallback.
	u.Credits = asFloat(m["credits"])
	u.Credit = asFloat(m["credit"])
	if u.Credits == 0 {
		u.Credits = u.Credit
	}
	u.OriginalCredits = asFloat(m["original_credits"])
	u.Billable = asBool(m["billable"])
	if det, ok := m["completion_tokens_details"].(map[string]any); ok {
		u.CompletionTokensDetails = &TokensDetails{
			CachedTokens:    int(asInt(det["cached_tokens"])),
			ReasoningTokens: int(asInt(det["reasoning_tokens"])),
		}
	}
	if det, ok := m["prompt_tokens_details"].(map[string]any); ok {
		u.PromptTokensDetails = &TokensDetails{
			CachedTokens:    int(asInt(det["cached_tokens"])),
			ReasoningTokens: int(asInt(det["reasoning_tokens"])),
		}
	}
	return u
}

// EffectiveCached returns the cache-hit token count across all shapes the
// upstream uses. DeepSeek reports prompt_cache_hit_tokens, Qwen reports
// completion_tokens_details.cached_tokens, and neither reports the standard
// prompt_tokens_details.cached_tokens.
func (u *Usage) EffectiveCached() int {
	if u == nil {
		return 0
	}
	for _, v := range []int{u.CachedTokens, u.PromptCacheHitTokens,
		cachedOf(u.CompletionTokensDetails), cachedOf(u.PromptTokensDetails)} {
		if v > 0 {
			return v
		}
	}
	return 0
}

// CachedReported reports whether the upstream actually reported a cache field,
// which separates "no cache hit" from "the upstream did not say".
func (u *Usage) CachedReported() bool {
	if u == nil || u.Raw == nil {
		return false
	}
	if _, ok := u.Raw["cached_tokens"]; ok {
		return true
	}
	if _, ok := u.Raw["prompt_cache_hit_tokens"]; ok {
		return true
	}
	if det, ok := u.Raw["completion_tokens_details"].(map[string]any); ok {
		if _, ok := det["cached_tokens"]; ok {
			return true
		}
	}
	if det, ok := u.Raw["prompt_tokens_details"].(map[string]any); ok {
		if _, ok := det["cached_tokens"]; ok {
			return true
		}
	}
	return false
}

func cachedOf(d *TokensDetails) int {
	if d == nil {
		return 0
	}
	return d.CachedTokens
}

// ReasoningTokens returns the reasoning token count.
func (u *Usage) ReasoningTokens() int {
	if u == nil || u.CompletionTokensDetails == nil {
		return 0
	}
	return u.CompletionTokensDetails.ReasoningTokens
}

// NormalizeForClient copies the non-standard cache fields into the standard
// slot while keeping the original fields, so clients that already adapted to
// the upstream shape keep working.
func (u *Usage) NormalizeForClient() {
	if u == nil || u.Raw == nil {
		return
	}
	cached := u.EffectiveCached()
	if cached == 0 {
		return
	}
	if u.PromptTokensDetails == nil {
		u.PromptTokensDetails = &TokensDetails{}
		u.Raw["prompt_tokens_details"] = map[string]any{}
	}
	det := u.Raw["prompt_tokens_details"].(map[string]any)
	det["cached_tokens"] = cached
	u.PromptTokensDetails.CachedTokens = cached
}

// ---------------------------------------------------------------------------
// Model name normalisation / gates
// ---------------------------------------------------------------------------

// RequestedEffort extracts the client-supplied reasoning effort.
func RequestedEffort(payload map[string]any) string {
	for _, key := range []string{"reasoning_effort", "effort"} {
		if v, ok := payload[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		if v, ok := reasoning["effort"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// NormalizeToolChoice rewrites the tool_choice forms the upstream rejects.
func NormalizeToolChoice(payload map[string]any) {
	tc, ok := payload["tool_choice"]
	if !ok || tc == nil {
		return
	}
	switch v := tc.(type) {
	case string:
		if v != "auto" && v != "none" && v != "required" {
			payload["tool_choice"] = "auto"
		}
	case map[string]any:
		t, _ := v["type"].(string)
		switch t {
		case "function", "auto", "none", "required":
			// already valid
			return
		case "any":
			payload["tool_choice"] = "required"
		case "tool":
			if name, ok := v["name"].(string); ok && name != "" {
				payload["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			} else {
				payload["tool_choice"] = "auto"
			}
		default:
			payload["tool_choice"] = "auto"
		}
	}
}

// NormalizeTools nests every tool's schema under a `function` object.
//
// The upstream requires tools[].function and rejects the flat
// {"type":"function","name":...} shape that the Responses API uses with
// "'function' is a required property, expected an object - 'tools.N'".
func NormalizeTools(payload map[string]any) {
	tools, ok := payload["tools"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if _, hasFn := m["function"].(map[string]any); hasFn {
			out = append(out, m)
			continue
		}
		typ, _ := m["type"].(string)
		if typ != "" && typ != "function" {
			// Built-in/web tools are handled elsewhere (or dropped).
			out = append(out, m)
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if d, ok := m["description"]; ok {
			fn["description"] = d
		}
		if p, ok := m["parameters"]; ok {
			fn["parameters"] = p
		} else if p, ok := m["input_schema"]; ok {
			fn["parameters"] = p
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	payload["tools"] = out
}

// TranslateMaxCompletionTokens maps the newer parameter name onto the old one.
func TranslateMaxCompletionTokens(payload map[string]any) {
	if payload["max_tokens"] != nil {
		return
	}
	if v, ok := payload["max_completion_tokens"]; ok && v != nil {
		payload["max_tokens"] = v
	}
}

// NormalizeRoles maps role names the upstream rejects.
// Qoder only understands system/user/assistant; OpenAI's "developer" is
// equivalent to "system".
func NormalizeRoles(messages []Message) []Message {
	out := make([]Message, len(messages))
	for i, m := range messages {
		if m.Role == "developer" {
			m.Role = "system"
		}
		if m.Role == "" {
			m.Role = "user"
		}
		out[i] = m
	}
	return out
}

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

// MarshalNoEscape serialises without HTML escaping (upstream binds signatures
// and prompt prefixes to the exact bytes).
func MarshalNoEscape(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(sb.String(), "\n")), nil
}

func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", t), "0"), ".")
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", v)
	}
}

func asFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		var f float64
		fmt.Sscanf(t, "%g", &f)
		return f
	}
	return 0
}

func asInt(v any) int64 { return int64(asFloat(v)) }

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		low := strings.ToLower(strings.TrimSpace(t))
		return !(low == "" || low == "0" || low == "false" || low == "no" ||
			low == "off" || low == "none" || low == "null")
	case float64:
		return t != 0
	}
	return false
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
