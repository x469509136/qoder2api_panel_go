package protocol

import (
	"encoding/json"
	"sort"
	"strings"
)

// AnthropicStreamState carries state across frames so the translator can buffer
// tool blocks until their name is known.
//
// Anthropic's protocol does not allow changing the name of a block that has
// already started, so a nameless first frame must be buffered. Emitting a start
// with an empty name makes the downstream client treat the whole turn as
// malformed (issue #116).
type AnthropicStreamState struct {
	MsgID         string
	NextIndex     int
	TextIndex     int
	ThinkingIndex int
	ToolIndex     map[int]int
	Pending       map[int]*pendingTool
	OpenBlocks    []int
	Finish        string
	ThinkingMode  string
	Model         string
	// Usage is the last usage chunk seen (cumulative).
	Usage *Usage
}

type pendingTool struct {
	ai         int
	id         string
	name       string
	args       []string
	argsObject bool
	argsKnown  bool
}

// NewAnthropicStreamState initialises the translator.
func NewAnthropicStreamState(model, thinkingMode string) *AnthropicStreamState {
	return &AnthropicStreamState{
		MsgID:         NewAnthropicID("msg_"),
		TextIndex:     -1,
		ThinkingIndex: -1,
		ToolIndex:     map[int]int{},
		Pending:       map[int]*pendingTool{},
		Finish:        "stop",
		ThinkingMode:  ThinkingMode(thinkingMode),
		Model:         model,
	}
}

// Start returns the leading message_start event.
func (s *AnthropicStreamState) Start() []byte {
	usage := ChatUsageToAnthropic(s.Usage)
	return anthropicEvent("message_start", map[string]any{
		"message": map[string]any{
			"id": s.MsgID, "type": "message", "role": "assistant",
			"model": s.Model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                usage["input_tokens"],
				"cache_read_input_tokens":     usage["cache_read_input_tokens"],
				"cache_creation_input_tokens": 0,
				"output_tokens":               0,
			},
		},
	})
}

// Feed consumes one chat chunk (already cleaned by the guards) and returns the
// Anthropic events it produces.
func (s *AnthropicStreamState) Feed(chunk map[string]any) [][]byte {
	var out [][]byte
	if u := DecodeUsage(chunk["usage"]); u != nil {
		s.Usage = u
	}
	choices, _ := chunk["choices"].([]any)
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if piece := asString(delta["reasoning_content"]); piece != "" && s.ThinkingMode == "empty" {
			if s.ThinkingIndex < 0 {
				s.ThinkingIndex = s.NextIndex
				s.NextIndex++
				s.OpenBlocks = append(s.OpenBlocks, s.ThinkingIndex)
				out = append(out, anthropicEvent("content_block_start", map[string]any{
					"index": s.ThinkingIndex,
					"content_block": map[string]any{
						"type": "thinking", "thinking": "", "signature": "",
					},
				}))
			}
			out = append(out, anthropicEvent("content_block_delta", map[string]any{
				"index": s.ThinkingIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": piece},
			}))
		}
		if txt, ok := delta["content"].(string); ok && txt != "" {
			if s.TextIndex < 0 {
				s.TextIndex = s.NextIndex
				s.NextIndex++
				s.OpenBlocks = append(s.OpenBlocks, s.TextIndex)
				out = append(out, anthropicEvent("content_block_start", map[string]any{
					"index":         s.TextIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				}))
			}
			out = append(out, anthropicEvent("content_block_delta", map[string]any{
				"index": s.TextIndex,
				"delta": map[string]any{"type": "text_delta", "text": txt},
			}))
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, t := range tcs {
				tc, ok := t.(map[string]any)
				if !ok {
					continue
				}
				out = append(out, s.feedToolCall(tc)...)
			}
		}
		if fr := asString(choice["finish_reason"]); fr != "" {
			s.Finish = fr
		}
	}
	return out
}

func (s *AnthropicStreamState) feedToolCall(tc map[string]any) [][]byte {
	var out [][]byte
	ci := -1
	if v, ok := tc["index"]; ok && v != nil {
		ci = int(asInt(v))
	}
	if ci < 0 {
		ci = len(s.ToolIndex)
	}
	fn, _ := tc["function"].(map[string]any)
	if fn == nil {
		fn = map[string]any{}
	}
	name := asString(fn["name"])
	args := asString(fn["arguments"])

	slot, ok := s.Pending[ci]
	if !ok {
		slot = &pendingTool{ai: s.NextIndex}
		s.NextIndex++
		s.ToolIndex[ci] = slot.ai
		s.Pending[ci] = slot
	}
	if slot.name == "" && name != "" {
		slot.name = name
	}
	if slot.id == "" {
		slot.id = asString(tc["id"])
	}
	if args != "" {
		slot.args = append(slot.args, args)
		if !slot.argsKnown && strings.TrimSpace(args) != "" {
			slot.argsKnown = true
			slot.argsObject = strings.HasPrefix(strings.TrimSpace(args), "{")
		}
	}
	if slot.name == "" {
		// Stay buffered: never emit a start with an empty name.
		return out
	}
	if !containsInt(s.OpenBlocks, slot.ai) {
		s.OpenBlocks = append(s.OpenBlocks, slot.ai)
		id := slot.id
		if id == "" {
			id = NewAnthropicID("toolu_")
		}
		out = append(out, anthropicEvent("content_block_start", map[string]any{
			"index": slot.ai,
			"content_block": map[string]any{
				"type": "tool_use", "id": id, "name": slot.name,
				"input": map[string]any{},
			},
		}))
	}
	if slot.argsObject && len(slot.args) > 0 {
		queued := slot.args
		slot.args = nil
		for _, piece := range queued {
			out = append(out, anthropicEvent("content_block_delta", map[string]any{
				"index": slot.ai,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": piece},
			}))
		}
	}
	return out
}

// Finish returns the closing events.
func (s *AnthropicStreamState) FinishEvents() [][]byte {
	var out [][]byte
	// Blocks that never received a name are dropped entirely, and a tool_calls
	// finish reason degrades to "stop" when nothing else was emitted — otherwise
	// the client waits forever for a tool result that will never come.
	dropped := false
	cis := make([]int, 0, len(s.Pending))
	for ci := range s.Pending {
		cis = append(cis, ci)
	}
	sort.Ints(cis)
	for _, ci := range cis {
		slot := s.Pending[ci]
		if containsInt(s.OpenBlocks, slot.ai) {
			if len(slot.args) > 0 {
				queued := slot.args
				if parsed, err := ParseToolArguments(strings.Join(queued, "")); err == nil {
					if _, ok := parsed.(map[string]any); !ok {
						parsed = map[string]any{"value": parsed}
					}
					if blob, err := MarshalNoEscape(parsed); err == nil {
						queued = []string{string(blob)}
					}
				}
				for _, piece := range queued {
					out = append(out, anthropicEvent("content_block_delta", map[string]any{
						"index": slot.ai,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": piece},
					}))
				}
			}
			continue
		}
		dropped = true
	}
	if dropped && s.Finish == "tool_calls" &&
		s.TextIndex < 0 && s.ThinkingIndex < 0 {
		s.Finish = "stop"
	}

	blocks := make([]int, len(s.OpenBlocks))
	copy(blocks, s.OpenBlocks)
	sort.Ints(blocks)
	for _, i := range blocks {
		out = append(out, anthropicEvent("content_block_stop", map[string]any{"index": i}))
	}

	final := ChatUsageToAnthropic(s.Usage)
	// Newer Anthropic semantics: message_delta.usage is the cumulative total.
	// Omitting the input side leaves clients' input token count at zero.
	out = append(out, anthropicEvent("message_delta", map[string]any{
		"delta": map[string]any{
			"stop_reason": StopReason(s.Finish), "stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":                final["input_tokens"],
			"cache_read_input_tokens":     final["cache_read_input_tokens"],
			"cache_creation_input_tokens": 0,
			"output_tokens":               final["output_tokens"],
			"output_tokens_details":       final["output_tokens_details"],
		},
	}))
	out = append(out, anthropicEvent("message_stop", map[string]any{}))
	return out
}

func anthropicEvent(etype string, obj map[string]any) []byte {
	body := make(map[string]any, len(obj)+1)
	for k, v := range obj {
		body[k] = v
	}
	body["type"] = etype
	blob, _ := MarshalNoEscape(body)
	return []byte("event: " + etype + "\ndata: " + string(blob) + "\n\n")
}

func containsInt(list []int, v int) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// ParseChatChunkLine decodes one "data: {...}" chunk line; returns nil for
// comments, [DONE] and non-JSON lines.
func ParseChatChunkLine(line string) map[string]any {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ":") {
		return nil
	}
	for strings.HasPrefix(trimmed, "data:") {
		trimmed = strings.TrimSpace(trimmed[5:])
	}
	if trimmed == "" || trimmed == "[DONE]" {
		return nil
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		return nil
	}
	return chunk
}
