package protocol

import (
	"encoding/json"
	"strings"
	"time"
)

// CustomToolHint is appended to a downgraded freeform tool's description.
//
// Codex declares file-editing tools as {"type":"custom","format":{...}} and the
// chat endpoint has no such concept. Without this instruction the model wraps
// the payload in JSON or a code fence and the tool call becomes unusable.
const CustomToolHint = "This is a freeform tool. Put the COMPLETE raw payload into the single " +
	"'input' string parameter, verbatim. Do not wrap it in JSON, do not wrap it in markdown " +
	"code fences, do not add commentary."

// ResponsesToChat translates a Responses API body into chat completions.
func ResponsesToChat(payload map[string]any) map[string]any {
	messages := []any{}
	if instructions := asString(payload["instructions"]); strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	pendingReasoning := ""

	appendToLastAssistant := func(key string, value any) bool {
		if len(messages) == 0 {
			return false
		}
		last, ok := messages[len(messages)-1].(map[string]any)
		if !ok || asString(last["role"]) != "assistant" {
			return false
		}
		last[key] = value
		return true
	}

	switch inp := payload["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": inp})
	case []any:
		for _, raw := range inp {
			switch item := raw.(type) {
			case string:
				messages = append(messages, map[string]any{"role": "user", "content": item})
			case map[string]any:
				itype := asString(item["type"])
				switch itype {
				case "", "message":
					body := FlattenContent(item["content"])
					if body == nil || body == "" {
						continue
					}
					role := firstNonEmpty(asString(item["role"]), "user")
					if role == "developer" {
						role = "system"
					}
					// Merge adjacent assistant text so a function_call sequence is
					// not interrupted.
					if role == "assistant" && len(messages) > 0 {
						if last, ok := messages[len(messages)-1].(map[string]any); ok && asString(last["role"]) == "assistant" {
							prev := asString(last["content"])
							if prev != "" {
								last["content"] = prev + "\n" + asString(body)
							} else {
								last["content"] = body
							}
							if pendingReasoning != "" {
								if _, exists := last["reasoning_content"]; !exists {
									last["reasoning_content"] = pendingReasoning
									pendingReasoning = ""
								}
							}
							continue
						}
					}
					msg := map[string]any{"role": role, "content": body}
					if role == "assistant" && pendingReasoning != "" {
						msg["reasoning_content"] = pendingReasoning
						pendingReasoning = ""
					}
					messages = append(messages, msg)

				case "reasoning":
					rText := ""
					switch summ := item["summary"].(type) {
					case []any:
						var parts []string
						for _, s := range summ {
							if sm, ok := s.(map[string]any); ok {
								if t := asString(sm["text"]); t != "" {
									parts = append(parts, t)
								}
							}
						}
						rText = strings.Join(parts, "\n")
					case string:
						rText = summ
					}
					if rText == "" {
						switch c := item["content"].(type) {
						case string:
							rText = c
						case []any:
							if f := FlattenContent(c); f != nil {
								rText = asString(f)
							}
						}
					}
					if rText != "" && !appendToLastAssistant("reasoning_content", rText) {
						pendingReasoning = rText
					}

				case "function_call", "custom_tool_call":
					var args string
					if itype == "custom_tool_call" {
						rawInput := item["input"]
						switch v := rawInput.(type) {
						case map[string]any, []any:
							if blob, err := MarshalNoEscape(v); err == nil {
								args = string(blob)
							}
						case string:
							args = v
						default:
							args = asString(rawInput)
						}
						args = `{"input":` + jsonQuote(args) + "}"
					} else {
						args = firstNonEmpty(asString(item["arguments"]), "{}")
					}
					tc := map[string]any{
						"id":   firstNonEmpty(asString(item["call_id"]), asString(item["id"])),
						"type": "function",
						"function": map[string]any{
							"name":      asString(item["name"]),
							"arguments": args,
						},
					}
					if len(messages) > 0 {
						if last, ok := messages[len(messages)-1].(map[string]any); ok && asString(last["role"]) == "assistant" {
							existing, _ := last["tool_calls"].([]any)
							last["tool_calls"] = append(existing, tc)
							if pendingReasoning != "" {
								if _, exists := last["reasoning_content"]; !exists {
									last["reasoning_content"] = pendingReasoning
									pendingReasoning = ""
								}
							}
							continue
						}
					}
					msg := map[string]any{"role": "assistant", "content": "",
						"tool_calls": []any{tc}}
					if pendingReasoning != "" {
						msg["reasoning_content"] = pendingReasoning
						pendingReasoning = ""
					}
					messages = append(messages, msg)

				case "function_call_output", "custom_tool_call_output":
					rawOut := item["output"]
					var content string
					switch v := rawOut.(type) {
					case []any:
						if f := FlattenContent(v); f != nil {
							content = asString(f)
						}
					case map[string]any:
						if itype == "function_call_output" &&
							(firstNonEmpty(asString(v["type"]), "x") == "input_image" ||
								v["image_url"] != nil) {
							if f := FlattenContent([]any{v}); f != nil {
								content = asString(f)
							}
						} else {
							if blob, err := MarshalNoEscape(v); err == nil {
								content = string(blob)
							}
						}
					case string:
						content = v
					default:
						content = asString(rawOut)
					}
					messages = append(messages, map[string]any{
						"role":         "tool",
						"tool_call_id": asString(item["call_id"]),
						"content":      content,
					})
				}
				// Unknown item types are not dropped silently: dropping a tool
				// call would leave the upstream history inconsistent.
			}
		}
	}

	chat := map[string]any{
		"model":    payload["model"],
		"messages": messages,
	}
	for _, k := range []string{"temperature", "top_p", "seed"} {
		if v, ok := payload[k]; ok && v != nil {
			chat[k] = v
		}
	}
	if v, ok := payload["max_output_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	if effort := RequestedEffort(payload); effort != "" {
		chat["reasoning_effort"] = effort
	}

	rawTools, _ := payload["tools"].([]any)
	rawTools = append(rawTools, additionalToolsFromInput(payload["input"])...)
	if len(rawTools) > 0 {
		flat, nsMap := FlattenNamespaceTools(rawTools)
		if tools := ToolsForChat(flat); len(tools) > 0 {
			chat["tools"] = tools
		}
		if len(nsMap) > 0 {
			// Used on the way back to restore the namespace; never sent upstream.
			chat["_qd_tool_ns"] = nsMap
		}
	}
	if tc, ok := payload["tool_choice"]; ok && tc != nil {
		chat["tool_choice"] = tc
	}
	if v, ok := payload["parallel_tool_calls"]; ok && v != nil {
		chat["parallel_tool_calls"] = v
	} else {
		// Responses semantics default to allowing parallel calls; sending it
		// explicitly stops the upstream from serialising them.
		chat["parallel_tool_calls"] = true
	}
	if so, ok := payload["stream_options"].(map[string]any); ok {
		chat["stream_options"] = so
	}
	return chat
}

func jsonQuote(s string) string {
	blob, err := MarshalNoEscape(s)
	if err != nil {
		return `""`
	}
	return string(blob)
}

// additionalToolsFromInput collects tools declared inside input[].
func additionalToolsFromInput(input any) []any {
	list, ok := input.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if tools, ok := m["additional_tools"].([]any); ok {
			out = append(out, tools...)
		}
	}
	return out
}

// IsNamespaceTool reports whether a tool declaration is a namespace container.
func IsNamespaceTool(tool map[string]any) bool {
	return asString(tool["type"]) == "namespace"
}

// FlattenNamespaceTools expands {"type":"namespace","tools":[...]} containers.
// The returned map is name -> namespace for the way back.
func FlattenNamespaceTools(tools []any) ([]any, map[string]string) {
	nsMap := map[string]string{}
	var out []any
	var walk func(items []any, ns string, depth int)
	walk = func(items []any, ns string, depth int) {
		if depth > 6 {
			return
		}
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if IsNamespaceTool(m) {
				childNS := firstNonEmpty(asString(m["name"]), ns)
				if children, ok := m["tools"].([]any); ok {
					walk(children, childNS, depth+1)
				}
				continue
			}
			name := asString(m["name"])
			if name == "" {
				if fn, ok := m["function"].(map[string]any); ok {
					name = asString(fn["name"])
				}
			}
			if ns != "" && name != "" {
				nsMap[name] = ns
			}
			out = append(out, m)
		}
	}
	walk(tools, "", 0)
	return out, nsMap
}

// ToolsForChat normalises Responses tool declarations into chat function tools.
// Custom freeform tools degrade to a single `input` string parameter.
func ToolsForChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, item := range tools {
		m, ok := item.(map[string]any)
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
		typ := asString(m["type"])
		switch typ {
		case "function", "":
			out = append(out, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": asString(m["description"]),
					"parameters": firstNonNil(m["parameters"],
						map[string]any{"type": "object", "properties": map[string]any{}}),
				},
			})
		case "custom":
			desc := asString(m["description"])
			out = append(out, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": strings.TrimSpace(desc + " " + CustomToolHint),
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"input": map[string]any{
								"type":        "string",
								"description": "The complete raw payload, verbatim.",
							},
						},
						"required": []any{"input"},
					},
				},
			})
		default:
			// Built-in tools (web_search etc.) are handled by the local web-tools
			// layer or dropped with a log by the caller.
		}
	}
	return out
}

func firstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// CustomToolNames returns the names declared as freeform custom tools.
func CustomToolNames(tools []any) map[string]bool {
	out := map[string]bool{}
	for _, item := range tools {
		if m, ok := item.(map[string]any); ok && asString(m["type"]) == "custom" {
			if name := asString(m["name"]); name != "" {
				out[name] = true
			}
		}
	}
	return out
}

// UnwrapCustomInput extracts the raw payload from a downgraded custom tool call.
func UnwrapCustomInput(args string) string {
	if strings.TrimSpace(args) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err == nil {
		if v, ok := m["input"].(string); ok {
			return v
		}
	}
	return args
}

// ResponsesUsage converts chat usage into the Responses shape.
func ResponsesUsage(u *Usage) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"input_tokens": u.PromptTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens": u.EffectiveCached(),
		},
		"output_tokens": u.CompletionTokens,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": u.ReasoningTokens(),
		},
		"total_tokens": u.TotalTokens,
	}
}

// ChatToResponse folds a chat.completion into a Responses object.
func ChatToResponse(chatObj map[string]any, model string, customNames map[string]bool, nsMap map[string]string) map[string]any {
	choice := firstChoice(chatObj)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		msg = map[string]any{}
	}
	text := asString(msg["content"])
	reasoning := asString(msg["reasoning_content"])
	output := []any{}
	if reasoning != "" {
		output = append(output, map[string]any{
			"id": NewRequestIDPrefix("rs_"), "type": "reasoning", "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	tcs := toolCallsOf(msg)
	for _, tc := range tcs {
		fn, _ := tc["function"].(map[string]any)
		name := asString(fn["name"])
		args := asString(fn["arguments"])
		callID := firstNonEmpty(asString(tc["id"]), NewRequestIDPrefix("call_"))
		if customNames[name] {
			output = append(output, stampNamespace(map[string]any{
				"id": NewRequestIDPrefix("ctc_"), "type": "custom_tool_call",
				"status": "completed", "call_id": callID, "name": name,
				"input": UnwrapCustomInput(args),
			}, nsMap))
			continue
		}
		output = append(output, stampNamespace(map[string]any{
			"id": NewRequestIDPrefix("fc_"), "type": "function_call",
			"status": "completed", "call_id": callID, "name": name,
			"arguments": firstNonEmpty(args, "{}"),
		}, nsMap))
	}
	// Text-shaped tool call fallbacks: a client on the Responses protocol must
	// not receive DSML markers or pseudo-XML as plain text either.
	if len(tcs) == 0 {
		if calls, clean := ParseDSMLToolCalls(text); len(calls) > 0 {
			for _, c := range calls {
				output = append(output, map[string]any{
					"id": NewRequestIDPrefix("fc_"), "type": "function_call",
					"status":    "completed",
					"call_id":   firstNonEmpty(asString(c.ID), NewRequestIDPrefix("call_")),
					"name":      asString(c.Name),
					"arguments": firstNonEmpty(asString(c.Arguments), "{}"),
				})
			}
			text = clean
		}
	}
	if len(tcs) == 0 && strings.Contains(strings.ToLower(text), ToolCallOpen) {
		if calls, clean := ParseToolCallTags(text, nil); len(calls) > 0 {
			for _, c := range calls {
				output = append(output, map[string]any{
					"id": NewRequestIDPrefix("fc_"), "type": "function_call",
					"status":    "completed",
					"call_id":   firstNonEmpty(c.ID, NewRequestIDPrefix("call_")),
					"name":      c.Name,
					"arguments": firstNonEmpty(c.Arguments, "{}"),
				})
			}
			text = clean
		}
	}
	if text != "" || len(output) == 0 {
		content := []any{}
		if text != "" {
			content = append(content, map[string]any{
				"type": "output_text", "text": text, "annotations": []any{},
			})
		}
		output = append(output, map[string]any{
			"id": NewRequestIDPrefix("msg_"), "type": "message",
			"status": "completed", "role": "assistant", "content": content,
		})
	}
	finish := firstNonEmpty(asString(choice["finish_reason"]), "stop")
	status := "completed"
	if finish == "length" {
		status = "incomplete"
	}
	obj := map[string]any{
		"id": NewRequestIDPrefix("resp_"), "object": "response",
		"created_at": time.Now().Unix(), "status": status, "model": model,
		"output": output, "output_text": text,
		"parallel_tool_calls": true, "tool_choice": "auto",
		"tools": []any{}, "metadata": map[string]any{},
	}
	if u := ResponsesUsage(DecodeUsage(chatObj["usage"])); u != nil {
		obj["usage"] = u
	}
	if finish == "length" {
		obj["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return obj
}

func stampNamespace(item map[string]any, nsMap map[string]string) map[string]any {
	if len(nsMap) == 0 {
		return item
	}
	if ns, ok := nsMap[asString(item["name"])]; ok && ns != "" {
		item["namespace"] = ns
	}
	return item
}

// NewRequestIDPrefix builds an object id with a type prefix.
func NewRequestIDPrefix(prefix string) string {
	return prefix + NewRequestID()[:24]
}
