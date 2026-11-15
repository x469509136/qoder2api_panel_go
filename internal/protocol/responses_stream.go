package protocol

import (
	"sort"
	"strings"
	"time"
)

// ResponsesStreamState translates cleaned chat chunks into Responses SSE.
//
// The sequence number must be strictly monotonic across the whole response. It
// is restored from the previous state when the upstream is reopened, otherwise
// a client sees it go backwards.
type ResponsesStreamState struct {
	RespID  string
	MsgID   string
	RsID    string
	Created int64
	Seq     int64
	Model   string

	TextParts   []string
	ReasonParts []string
	Outputs     []map[string]any
	ReasonIndex int
	MsgIndex    int
	Finish      string
	Usage       *Usage

	toolCalls   map[int]*responsesTool
	textBuffer  string
	CustomNames map[string]bool
	Namespace   map[string]string
}

type responsesTool struct {
	outputIndex int
	id          string
	name        string
	arguments   string
	custom      bool
	itemID      string
}

// NewResponsesStreamState initialises the translator.
func NewResponsesStreamState(model string, customNames map[string]bool, nsMap map[string]string) *ResponsesStreamState {
	return &ResponsesStreamState{
		RespID:      NewRequestIDPrefix("resp_"),
		MsgID:       NewRequestIDPrefix("msg_"),
		RsID:        NewRequestIDPrefix("rs_"),
		Created:     time.Now().Unix(),
		Model:       model,
		ReasonIndex: -1,
		MsgIndex:    -1,
		Finish:      "stop",
		toolCalls:   map[int]*responsesTool{},
		CustomNames: customNames,
		Namespace:   nsMap,
	}
}

// SeqValue returns the current sequence number (for reopening).
func (s *ResponsesStreamState) SeqValue() int64 { return s.Seq }

// SetSeq restores the sequence counter after an upstream reopen.
func (s *ResponsesStreamState) SetSeq(v int64) {
	if v > s.Seq {
		s.Seq = v
	}
}

func (s *ResponsesStreamState) event(etype string, payload map[string]any) []byte {
	s.Seq++
	data := make(map[string]any, len(payload)+2)
	data["type"] = etype
	data["sequence_number"] = s.Seq
	for k, v := range payload {
		data[k] = v
	}
	blob, _ := MarshalNoEscape(data)
	return []byte("event: " + etype + "\ndata: " + string(blob) + "\n\n")
}

func (s *ResponsesStreamState) responseObj(status string) map[string]any {
	outputs := make([]any, 0, len(s.Outputs))
	for _, o := range s.Outputs {
		if o != nil {
			outputs = append(outputs, o)
		}
	}
	obj := map[string]any{
		"id": s.RespID, "object": "response", "created_at": s.Created,
		"status": status, "model": s.Model, "output": outputs,
		"output_text":         strings.Join(s.TextParts, ""),
		"parallel_tool_calls": true, "tool_choice": "auto",
		"tools": []any{}, "metadata": map[string]any{},
	}
	if u := ResponsesUsage(s.Usage); u != nil {
		obj["usage"] = u
	}
	return obj
}

// Start returns the leading created/in_progress events.
func (s *ResponsesStreamState) Start() [][]byte {
	return [][]byte{
		s.event("response.created", map[string]any{"response": s.responseObj("in_progress")}),
		s.event("response.in_progress", map[string]any{"response": s.responseObj("in_progress")}),
	}
}

func (s *ResponsesStreamState) reasonItem(status string) map[string]any {
	return map[string]any{
		"id": s.RsID, "type": "reasoning", "status": status,
		"summary": []any{map[string]any{
			"type": "summary_text", "text": strings.Join(s.ReasonParts, ""),
		}},
	}
}

func (s *ResponsesStreamState) msgItem(status string) map[string]any {
	item := map[string]any{
		"id": s.MsgID, "type": "message", "status": status,
		"role": "assistant", "content": []any{},
	}
	if len(s.TextParts) > 0 {
		item["content"] = []any{map[string]any{
			"type": "output_text", "text": strings.Join(s.TextParts, ""),
			"annotations": []any{},
		}}
	}
	return item
}

// Feed consumes one chat chunk and returns the resulting Response events.
func (s *ResponsesStreamState) Feed(chunk map[string]any) [][]byte {
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
		if piece := asString(delta["reasoning_content"]); piece != "" {
			if s.ReasonIndex < 0 {
				s.ReasonIndex = len(s.Outputs)
				s.Outputs = append(s.Outputs, nil)
				out = append(out, s.event("response.output_item.added", map[string]any{
					"output_index": s.ReasonIndex,
					"item":         s.reasonItem("in_progress"),
				}))
				out = append(out, s.event("response.reasoning_summary_part.added", map[string]any{
					"item_id": s.RsID, "output_index": s.ReasonIndex,
					"summary_index": 0,
					"part":          map[string]any{"type": "summary_text", "text": ""},
				}))
			}
			s.ReasonParts = append(s.ReasonParts, piece)
			out = append(out, s.event("response.reasoning_summary_text.delta", map[string]any{
				"item_id": s.RsID, "output_index": s.ReasonIndex,
				"summary_index": 0, "delta": piece,
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
		if piece := asString(delta["content"]); piece != "" {
			out = append(out, s.feedText(piece)...)
		}
		if fr := asString(choice["finish_reason"]); fr != "" {
			s.Finish = fr
		}
	}
	return out
}

func (s *ResponsesStreamState) feedText(piece string) [][]byte {
	var out [][]byte
	if s.MsgIndex < 0 {
		if s.ReasonIndex >= 0 {
			full := strings.Join(s.ReasonParts, "")
			out = append(out, s.event("response.reasoning_summary_text.done", map[string]any{
				"item_id": s.RsID, "output_index": s.ReasonIndex,
				"summary_index": 0, "text": full,
			}))
			out = append(out, s.event("response.reasoning_summary_part.done", map[string]any{
				"item_id": s.RsID, "output_index": s.ReasonIndex,
				"summary_index": 0,
				"part":          map[string]any{"type": "summary_text", "text": full},
			}))
			s.Outputs[s.ReasonIndex] = s.reasonItem("completed")
			out = append(out, s.event("response.output_item.done", map[string]any{
				"output_index": s.ReasonIndex, "item": s.Outputs[s.ReasonIndex],
			}))
		}
		s.MsgIndex = len(s.Outputs)
		s.Outputs = append(s.Outputs, nil)
		out = append(out, s.event("response.output_item.added", map[string]any{
			"output_index": s.MsgIndex,
			"item": map[string]any{
				"id": s.MsgID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		}))
		out = append(out, s.event("response.content_part.added", map[string]any{
			"item_id": s.MsgID, "output_index": s.MsgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}))
	}
	s.TextParts = append(s.TextParts, piece)
	out = append(out, s.event("response.output_text.delta", map[string]any{
		"item_id": s.MsgID, "output_index": s.MsgIndex,
		"content_index": 0, "delta": piece,
	}))
	return out
}

func (s *ResponsesStreamState) feedToolCall(tc map[string]any) [][]byte {
	var out [][]byte
	idx := 0
	if v, ok := tc["index"]; ok && v != nil {
		idx = int(asInt(v))
	}
	fn, _ := tc["function"].(map[string]any)
	if fn == nil {
		fn = map[string]any{}
	}
	name := asString(fn["name"])
	args := asString(fn["arguments"])
	callID := asString(tc["id"])

	entry, ok := s.toolCalls[idx]
	if !ok {
		outIdx := len(s.Outputs)
		s.Outputs = append(s.Outputs, nil)
		if callID == "" {
			callID = NewRequestIDPrefix("call_")
		}
		isCustom := name != "" && s.CustomNames[name]
		entry = &responsesTool{
			outputIndex: outIdx, id: callID, name: name,
			arguments: args, custom: isCustom,
			itemID: NewRequestIDPrefix("fc_"),
		}
		if isCustom {
			entry.itemID = NewRequestIDPrefix("ctc_")
		}
		s.toolCalls[idx] = entry
		item := map[string]any{
			"id": entry.itemID, "status": "in_progress",
			"call_id": callID, "name": name,
		}
		if ns, ok := s.Namespace[name]; ok && ns != "" {
			// The namespace must be present on the added frame: clients match
			// executors by (name, namespace, kind).
			item["namespace"] = ns
		}
		if isCustom {
			item["type"] = "custom_tool_call"
			item["input"] = ""
		} else {
			item["type"] = "function_call"
			item["arguments"] = ""
		}
		out = append(out, s.event("response.output_item.added", map[string]any{
			"output_index": outIdx, "item": item,
		}))
		return out
	}
	if name != "" && entry.name == "" {
		entry.name = name
		if s.CustomNames[name] {
			entry.custom = true
		}
	}
	if args != "" {
		entry.arguments += args
		if entry.custom {
			out = append(out, s.event("response.custom_tool_call_input.delta", map[string]any{
				"output_index": entry.outputIndex, "item_id": entry.itemID,
				"call_id": entry.id, "delta": args,
			}))
		} else {
			out = append(out, s.event("response.function_call_arguments.delta", map[string]any{
				"output_index": entry.outputIndex, "call_id": entry.id, "delta": args,
			}))
		}
	}
	return out
}

// FinishEvents closes the response with the terminal event.
func (s *ResponsesStreamState) FinishEvents(failed bool, failMessage string) [][]byte {
	var out [][]byte
	if s.MsgIndex >= 0 {
		full := strings.Join(s.TextParts, "")
		out = append(out, s.event("response.output_text.done", map[string]any{
			"item_id": s.MsgID, "output_index": s.MsgIndex,
			"content_index": 0, "text": full,
		}))
		out = append(out, s.event("response.content_part.done", map[string]any{
			"item_id": s.MsgID, "output_index": s.MsgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
		}))
		s.Outputs[s.MsgIndex] = s.msgItem("completed")
		out = append(out, s.event("response.output_item.done", map[string]any{
			"output_index": s.MsgIndex, "item": s.Outputs[s.MsgIndex],
		}))
	}
	if s.ReasonIndex >= 0 && s.MsgIndex < 0 && s.Outputs[s.ReasonIndex] == nil {
		full := strings.Join(s.ReasonParts, "")
		out = append(out, s.event("response.reasoning_summary_text.done", map[string]any{
			"item_id": s.RsID, "output_index": s.ReasonIndex,
			"summary_index": 0, "text": full,
		}))
		out = append(out, s.event("response.reasoning_summary_part.done", map[string]any{
			"item_id": s.RsID, "output_index": s.ReasonIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": full},
		}))
		s.Outputs[s.ReasonIndex] = s.reasonItem("completed")
		out = append(out, s.event("response.output_item.done", map[string]any{
			"output_index": s.ReasonIndex, "item": s.Outputs[s.ReasonIndex],
		}))
	}
	indexes := make([]int, 0, len(s.toolCalls))
	for i := range s.toolCalls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		entry := s.toolCalls[i]
		item := map[string]any{
			"id": entry.itemID, "status": "completed",
			"call_id": entry.id, "name": entry.name,
		}
		if ns, ok := s.Namespace[entry.name]; ok && ns != "" {
			item["namespace"] = ns
		}
		if entry.custom {
			item["type"] = "custom_tool_call"
			item["input"] = UnwrapCustomInput(entry.arguments)
		} else {
			item["type"] = "function_call"
			item["arguments"] = firstNonEmpty(entry.arguments, "{}")
		}
		s.Outputs[entry.outputIndex] = item
		out = append(out, s.event("response.output_item.done", map[string]any{
			"output_index": entry.outputIndex, "item": item,
		}))
	}

	if failed {
		s.Outputs = compactOutputs(s.Outputs)
		resp := s.responseObj("failed")
		resp["error"] = map[string]any{
			"code": "upstream_incomplete_tool_call", "message": failMessage,
		}
		out = append(out, s.event("response.failed", map[string]any{"response": resp}))
		return out
	}

	status := "completed"
	if s.Finish == "length" {
		status = "incomplete"
	}
	s.Outputs = compactOutputs(s.Outputs)
	resp := s.responseObj(status)
	if s.Finish == "length" {
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		out = append(out, s.event("response.incomplete", map[string]any{"response": resp}))
		return out
	}
	out = append(out, s.event("response.completed", map[string]any{"response": resp}))
	return out
}

func compactOutputs(outputs []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(outputs))
	for _, o := range outputs {
		if o != nil {
			out = append(out, o)
		}
	}
	return out
}

// FailedFrame builds a standalone response.failed event.
func FailedFrame(respID, model, code, message string, seq int64) []byte {
	obj := map[string]any{
		"type": "response.failed", "sequence_number": seq + 1,
		"response": map[string]any{
			"id": respID, "object": "response", "status": "failed",
			"model": model, "output": []any{},
			"error": map[string]any{"code": code, "message": message},
		},
	}
	blob, _ := MarshalNoEscape(obj)
	return []byte("event: response.failed\ndata: " + string(blob) + "\n\n")
}
