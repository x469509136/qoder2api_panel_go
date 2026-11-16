package protocol

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Anthropic conversion rules, per the bridge design:
//
//  1. translate only, never decide request shape (that is a single decision
//     made by the caller and passed down);
//  2. do not re-implement the echo/read-back guards here — the input contract
//     is "chat chunks already cleaned by the guards package";
//  3. never synthesise an Anthropic thinking signature.

// StopReasonMap maps chat finish reasons onto Anthropic stop reasons.
var StopReasonMap = map[string]string{
	"stop":          "end_turn",
	"length":        "max_tokens",
	"tool_calls":    "tool_use",
	"function_call": "tool_use",
}

// BudgetSteps are the thinking budget breakpoints from the design doc.
var BudgetSteps = []struct {
	Limit int
	Name  string
}{
	{1024, "minimal"},
	{4096, "low"},
	{16384, "medium"},
	{32768, "high"},
	{0, "max"},
}

// MsgSystemMode resolves QD_ANTHROPIC_MSG_SYSTEM.
//
//	inline (default): demote an in-messages system message to a user message in
//	                  place — no information is lost and the leading system
//	                  prefix stays byte-identical between turns;
//	merge           : fold it into the leading system message (legacy). Dynamic
//	                  per-turn content then pollutes the prefix and the cache
//	                  hit rate collapses from ~92% to ~15%;
//	drop            : discard it (not recommended).
func MsgSystemMode() string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("QD_ANTHROPIC_MSG_SYSTEM")))
	switch raw {
	case "merge", "top", "legacy":
		return "merge"
	case "drop", "off", "none":
		return "drop"
	}
	return "inline"
}

// ThinkingMode resolves the Anthropic thinking-block policy.
//
// "empty" emits thinking blocks with an empty signature. That is only safe when
// no downstream verifies signatures: an upstream relay that forwards chunks is
// fine, a direct Anthropic API is not. "raw" (forged signatures) is never
// offered.
func ThinkingMode(override string) string {
	for _, cand := range []string{override} {
		v := strings.ToLower(strings.TrimSpace(cand))
		if v == "empty" || v == "off" {
			return v
		}
	}
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("QD_ANTHROPIC_THINKING")))
	if raw == "empty" {
		return raw
	}
	return "off"
}

// TextOfBlocks joins the text blocks of an Anthropic content array.
func TextOfBlocks(blocks any) string {
	if s, ok := blocks.(string); ok {
		return s
	}
	list, ok := blocks.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, b := range list {
		switch v := b.(type) {
		case string:
			parts = append(parts, v)
		case map[string]any:
			typ, _ := v["type"].(string)
			if (typ == "" || typ == "text") && v["text"] != nil {
				if t, ok := v["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
	}
	return strings.Join(nonEmpty(parts), "\n")
}

func nonEmpty(items []string) []string {
	out := items[:0]
	for _, s := range items {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ImageToDataURI converts an Anthropic image block into a chat-usable URL.
func ImageToDataURI(block map[string]any) string {
	src, ok := block["source"].(map[string]any)
	if !ok {
		return ""
	}
	switch asString(src["type"]) {
	case "base64":
		mediaType := firstNonEmpty(asString(src["media_type"]), "image/png")
		data := asString(src["data"])
		if data == "" {
			return ""
		}
		return "data:" + mediaType + ";base64," + data
	case "url":
		return asString(src["url"])
	}
	return ""
}

// ToolResultText flattens an Anthropic tool_result block's content.
//
// Non-text blocks degrade to a placeholder rather than being dropped: silently
// losing an image from a tool result would misrepresent what the tool returned.
func ToolResultText(block map[string]any) string {
	c := block["content"]
	if s, ok := c.(string); ok {
		return s
	}
	list, ok := c.([]any)
	if !ok {
		if c == nil {
			return ""
		}
		return asString(c)
	}
	var parts []string
	for _, b := range list {
		switch v := b.(type) {
		case string:
			parts = append(parts, v)
		case map[string]any:
			typ, _ := v["type"].(string)
			if typ == "" || typ == "text" {
				parts = append(parts, asString(v["text"]))
			} else {
				parts = append(parts, fmt.Sprintf("[%s omitted]", firstNonEmpty(typ, "block")))
			}
		}
	}
	return strings.Join(nonEmpty(parts), "\n")
}

// ToolChoiceToChat maps the four Anthropic tool_choice forms onto chat.
func ToolChoiceToChat(tc any) any {
	switch v := tc.(type) {
	case string:
		switch v {
		case "any":
			return "required"
		case "none":
			return "none"
		}
		return "auto"
	case map[string]any:
		switch asString(v["type"]) {
		case "any":
			return "required"
		case "none":
			return "none"
		case "tool":
			if name := asString(v["name"]); name != "" {
				return map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
		case "auto":
			return "auto"
		}
	}
	return "auto"
}

// SystemPromptOf merges the three system sources into one leading block.
func SystemPromptOf(payload map[string]any) string {
	var parts []string
	switch sysv := payload["system"].(type) {
	case string:
		if strings.TrimSpace(sysv) != "" {
			parts = append(parts, sysv)
		}
	case []any:
		if t := TextOfBlocks(sysv); strings.TrimSpace(t) != "" {
			parts = append(parts, t)
		}
	}
	if MsgSystemMode() == "merge" {
		if msgs, ok := payload["messages"].([]any); ok {
			for _, m := range msgs {
				mm, ok := m.(map[string]any)
				if !ok || asString(mm["role"]) != "system" {
					continue
				}
				if t := TextOfBlocks(mm["content"]); strings.TrimSpace(t) != "" {
					parts = append(parts, t)
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// CountMsgSystem counts in-messages system entries (for the usage fingerprint).
func CountMsgSystem(payload map[string]any) int {
	n := 0
	if msgs, ok := payload["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && asString(mm["role"]) == "system" {
				n++
			}
		}
	}
	return n
}

// SystemSource labels where the system prompt came from (issue #55).
func SystemSource(payload map[string]any) string {
	hasTop := strings.TrimSpace(asString(payload["system"])) != ""
	nMsg := CountMsgSystem(payload)
	mode := MsgSystemMode()
	switch {
	case mode == "merge" && nMsg > 0:
		if hasTop {
			return "top+msg"
		}
		return "msg"
	case mode == "inline" && nMsg > 0:
		if hasTop {
			return "top+inline"
		}
		return "inline"
	}
	if hasTop {
		return "top"
	}
	return "none"
}

// ToolsToChat converts Anthropic tool declarations into chat function tools.
// Already-chat-shaped entries pass through unchanged.
func ToolsToChat(tools any) []any {
	list, ok := tools.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(list))
	for _, t := range list {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if _, hasFn := m["function"].(map[string]any); hasFn {
			out = append(out, m)
			continue
		}
		name := strings.TrimSpace(asString(m["name"]))
		if name == "" {
			continue
		}
		schema, ok := m["input_schema"].(map[string]any)
		if !ok {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": asString(m["description"]),
				"parameters":  schema,
			},
		})
	}
	return out
}

// EffortFromAnthropic maps thinking.budget_tokens / output_config.effort onto a
// reasoning effort tier.
//
// The mapping is monotonic and clamped to what the model supports. Clamping only
// downward would silently downgrade "medium"/"high" on dfmodel (low/high/max).
func EffortFromAnthropic(payload map[string]any, allowed []string, defaultEffort string) string {
	th, _ := payload["thinking"].(map[string]any)
	oc, _ := payload["output_config"].(map[string]any)
	ocEffort := ""
	if oc != nil {
		cand := strings.ToLower(strings.TrimSpace(asString(oc["effort"])))
		if _, ok := effortRank[cand]; ok {
			ocEffort = cand
		}
	}
	if th == nil {
		if ocEffort == "" {
			return ""
		}
		th = map[string]any{"type": "enabled", "effort": ocEffort}
	}
	if strings.ToLower(asString(th["type"])) == "disabled" {
		return ""
	}
	target := ""
	if e := strings.ToLower(strings.TrimSpace(asString(th["effort"]))); e != "" {
		if _, ok := effortRank[e]; ok {
			target = e
		}
	}
	if target == "" {
		// Claude Code sends thinking={"type":"adaptive"} plus
		// output_config={"effort":"low|medium|high|max"} for third-party model
		// names. adaptive has neither effort nor budget, so skipping the
		// output_config branch maps all four tiers to "no effort sent".
		target = ocEffort
	}
	if target == "" {
		budget, ok := th["budget_tokens"]
		if !ok || budget == nil {
			return ""
		}
		b := asFloat(budget)
		for _, step := range BudgetSteps {
			if step.Limit == 0 || b <= float64(step.Limit) {
				target = step.Name
				break
			}
		}
	}
	if target == "" {
		return ""
	}
	if len(allowed) == 0 {
		return ""
	}
	filtered := make([]string, 0, len(allowed))
	for _, a := range allowed {
		v := strings.ToLower(strings.TrimSpace(a))
		if _, ok := effortRank[v]; ok {
			filtered = append(filtered, v)
		}
	}
	if len(filtered) == 0 {
		return ""
	}
	if contains(filtered, target) {
		return target
	}
	ref := defaultEffort
	if _, ok := effortRank[ref]; !ok {
		ref = target
	}
	want := effortRank[target]
	refRank := effortRank[ref]
	best := filtered[0]
	bestScore := -1
	for _, cand := range filtered {
		d1 := effortRank[cand] - want
		if d1 < 0 {
			d1 = -d1
		}
		d2 := effortRank[cand] - refRank
		if d2 < 0 {
			d2 = -d2
		}
		score := d1*1000 + d2
		if bestScore < 0 || score < bestScore {
			bestScore = score
			best = cand
		}
	}
	return best
}

var effortRank = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// ParseToolArguments decodes an arguments string, unwrapping one extra level of
// object/array encoding left over from an older client.
func ParseToolArguments(args string) (any, error) {
	var inp any
	if err := json.Unmarshal([]byte(args), &inp); err != nil {
		return nil, err
	}
	if s, ok := inp.(string); ok {
		var decoded any
		if err := json.Unmarshal([]byte(s), &decoded); err == nil {
			switch decoded.(type) {
			case map[string]any, []any:
				return decoded, nil
			}
		}
	}
	return inp, nil
}

// MessagesToChat converts an Anthropic /v1/messages body into chat.
func MessagesToChat(payload map[string]any, allowedEfforts []string, defaultEffort string) map[string]any {
	messages := []any{}
	msgs, _ := payload["messages"].([]any)
	mode := MsgSystemMode()
	for _, item := range msgs {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := firstNonEmpty(asString(m["role"]), "user")
		if role == "system" {
			if mode == "merge" || mode == "drop" {
				continue
			}
			// inline (default): demote in place so the dynamic content does not
			// get hoisted to the front and change the cached prefix every turn.
			if txt := strings.TrimSpace(TextOfBlocks(m["content"])); txt != "" {
				messages = append(messages, map[string]any{"role": "user", "content": txt})
			}
			continue
		}
		var blocks []any
		switch c := m["content"].(type) {
		case string:
			blocks = []any{map[string]any{"type": "text", "text": c}}
		case []any:
			blocks = c
		case map[string]any:
			// A bare single block (image/tool_use) must not be flattened to "".
			blocks = []any{c}
		}

		var texts, images []string
		var toolCalls []map[string]any
		type toolResult struct{ id, text string }
		var results []toolResult

		for _, b := range blocks {
			switch v := b.(type) {
			case string:
				texts = append(texts, v)
			case map[string]any:
				switch asString(v["type"]) {
				case "", "text":
					if t, ok := v["text"].(string); ok {
						texts = append(texts, t)
					}
				case "image":
					if uri := ImageToDataURI(v); uri != "" {
						images = append(images, uri)
					}
				case "tool_use":
					args := v["input"]
					if s, isStr := args.(string); isStr {
						// Repair histories that double-encoded the object.
						if decoded, err := ParseToolArguments(s); err == nil {
							switch decoded.(type) {
							case map[string]any, []any:
								args = decoded
							}
						}
					}
					argsStr := ""
					if s, isStr := args.(string); isStr {
						argsStr = s
					} else {
						if args == nil {
							args = map[string]any{}
						}
						blob, err := MarshalNoEscape(args)
						if err == nil {
							argsStr = string(blob)
						} else {
							argsStr = "{}"
						}
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":   asString(v["id"]),
						"type": "function",
						"function": map[string]any{
							"name":      asString(v["name"]),
							"arguments": argsStr,
						},
					})
				case "tool_result":
					results = append(results, toolResult{
						id:   asString(v["tool_use_id"]),
						text: ToolResultText(v),
					})
				case "thinking":
					// Inbound thinking blocks are always dropped.
				}
			}
		}

		// Order: tool results first (they pair with the preceding tool_use),
		// then the assistant call, then the user text.
		for _, r := range results {
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": r.id, "content": r.text,
			})
		}
		text := strings.Join(texts, "")
		switch {
		case len(toolCalls) > 0:
			calls := make([]any, 0, len(toolCalls))
			for _, c := range toolCalls {
				calls = append(calls, c)
			}
			messages = append(messages, map[string]any{
				"role": "assistant", "content": text, "tool_calls": calls,
			})
		case role == "assistant" && (text != "" || len(images) > 0):
			messages = append(messages, map[string]any{"role": "assistant", "content": text})
		case len(images) > 0:
			parts := []any{}
			if text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, u := range images {
				parts = append(parts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": u},
				})
			}
			r := role
			if r != "user" && r != "assistant" {
				r = "user"
			}
			messages = append(messages, map[string]any{"role": r, "content": parts})
		case text != "":
			r := role
			if r != "user" && r != "assistant" {
				r = "user"
			}
			messages = append(messages, map[string]any{"role": r, "content": text})
		}
		// else: the message carried only tool results (already emitted) or was
		// empty — do not append a ghost turn with content:"".
	}

	chat := map[string]any{"messages": messages}
	if sysText := SystemPromptOf(payload); sysText != "" {
		chat["messages"] = append([]any{map[string]any{"role": "system", "content": sysText}}, messages...)
	}
	if model := asString(payload["model"]); model != "" {
		chat["model"] = model
	}
	if v, ok := payload["max_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := payload[k]; ok && v != nil {
			chat[k] = v
		}
	}
	if stops, ok := payload["stop_sequences"].([]any); ok && len(stops) > 0 {
		chat["stop"] = stops
	}
	if tools := ToolsToChat(payload["tools"]); len(tools) > 0 {
		chat["tools"] = tools
	}
	if tc, ok := payload["tool_choice"]; ok && tc != nil {
		chat["tool_choice"] = ToolChoiceToChat(tc)
	}
	if effort := EffortFromAnthropic(payload, allowedEfforts, defaultEffort); effort != "" {
		chat["reasoning_effort"] = effort
	}
	if meta, ok := payload["metadata"].(map[string]any); ok {
		if uid := asString(meta["user_id"]); uid != "" {
			chat["metadata"] = map[string]any{"user_id": uid}
		}
	}
	return chat
}

// ValidateAnthropicRequest enforces the fields Anthropic requires.
// max_tokens is mandatory here, unlike chat.
func ValidateAnthropicRequest(payload map[string]any) (bool, string) {
	if payload == nil {
		return false, "request body must be a JSON object"
	}
	msgs, _ := payload["messages"].([]any)
	if len(msgs) == 0 {
		return false, "messages is required"
	}
	mt, ok := payload["max_tokens"]
	if !ok || mt == nil {
		return false, "max_tokens is required"
	}
	if asInt(mt) <= 0 {
		return false, "max_tokens must be positive"
	}
	return true, ""
}

// ChatUsageToAnthropic converts chat usage into Anthropic usage.
//
// Canonical form: Anthropic's input_tokens excludes cache reads, and
// input_tokens + cache_read_input_tokens == prompt_tokens. cache_creation is
// always 0 because the upstream has no such concept and nothing is invented.
func ChatUsageToAnthropic(u *Usage) map[string]any {
	prompt := 0
	completion := 0
	cached := 0
	reasoning := 0
	if u != nil {
		prompt = u.PromptTokens
		completion = u.CompletionTokens
		cached = u.EffectiveCached()
		reasoning = u.ReasoningTokens()
	}
	if cached > prompt {
		cached = prompt
	}
	if cached < 0 {
		cached = 0
	}
	return map[string]any{
		"input_tokens":                prompt - cached,
		"cache_read_input_tokens":     cached,
		"cache_creation_input_tokens": 0,
		"output_tokens":               completion,
		"output_tokens_details":       map[string]any{"reasoning_tokens": reasoning},
	}
}

// StopReason maps a chat finish reason.
func StopReason(finish string) string {
	if v, ok := StopReasonMap[firstNonEmpty(finish, "stop")]; ok {
		return v
	}
	return "end_turn"
}

// NewAnthropicID builds an Anthropic-shaped object id.
func NewAnthropicID(prefix string) string {
	return prefix + NewRequestID()[:24]
}

// EstimateAnthropicTokens is the local count_tokens estimate (CJK-aware, no
// network call).
func EstimateAnthropicTokens(payload map[string]any) map[string]any {
	chunks := []string{SystemPromptOf(payload)}
	if msgs, ok := payload["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := mm["content"].(string); ok {
				chunks = append(chunks, s)
			} else {
				chunks = append(chunks, TextOfBlocks(mm["content"]))
			}
		}
	}
	if tools, ok := payload["tools"]; ok && tools != nil {
		if blob, err := MarshalNoEscape(tools); err == nil {
			chunks = append(chunks, string(blob))
		}
	}
	text := strings.Join(nonEmpty(chunks), "\n")
	cjk, other := 0, 0
	for _, r := range text {
		if r > 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	tokens := cjk + (other+3)/4
	if tokens < 1 {
		tokens = 1
	}
	return map[string]any{"input_tokens": tokens}
}

// ChatToMessages folds a chat.completion into an Anthropic message.
func ChatToMessages(chatObj map[string]any, model string, thinkingMode string) map[string]any {
	choice := firstChoice(chatObj)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		msg = map[string]any{}
	}
	content := []any{}
	text := asString(msg["content"])
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	if ThinkingMode(thinkingMode) == "empty" {
		if rc := asString(msg["reasoning_content"]); rc != "" {
			content = append(content, map[string]any{
				"type": "thinking", "thinking": rc, "signature": "",
			})
		}
	}
	for _, tc := range toolCallsOf(msg) {
		name := asString(tc["function"].(map[string]any)["name"])
		if name == "" {
			// An empty name makes the whole turn malformed downstream; better to
			// drop this one call than to emit a bad block.
			continue
		}
		args := asString(tc["function"].(map[string]any)["arguments"])
		var input any = map[string]any{}
		if strings.TrimSpace(args) != "" {
			if parsed, err := ParseToolArguments(args); err == nil {
				input = parsed
			}
		}
		if _, ok := input.(map[string]any); !ok {
			input = map[string]any{"value": input}
		}
		id := asString(tc["id"])
		if id == "" {
			id = NewAnthropicID("toolu_")
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": input,
		})
	}
	return map[string]any{
		"id":            NewAnthropicID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         firstNonEmpty(model, asString(chatObj["model"])),
		"content":       content,
		"stop_reason":   StopReason(asString(choice["finish_reason"])),
		"stop_sequence": nil,
		"usage":         ChatUsageToAnthropic(DecodeUsage(chatObj["usage"])),
	}
}

func firstChoice(chatObj map[string]any) map[string]any {
	choices, _ := chatObj["choices"].([]any)
	if len(choices) == 0 {
		return map[string]any{}
	}
	m, _ := choices[0].(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func toolCallsOf(msg map[string]any) []map[string]any {
	list, _ := msg["tool_calls"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := m["function"].(map[string]any); !ok {
			m = map[string]any{"function": map[string]any{}}
		}
		out = append(out, m)
	}
	return out
}

// AnthropicErrorFrame builds the terminal error event of an Anthropic stream.
func AnthropicErrorFrame(message, kind string) []byte {
	if kind == "" {
		kind = "api_error"
	}
	blob, _ := MarshalNoEscape(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": kind, "message": message},
	})
	return []byte("event: error\ndata: " + string(blob) + "\n\n")
}
