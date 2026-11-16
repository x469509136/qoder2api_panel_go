package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFlattenStructuredToolHistory(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "list files"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{
			ID: "call_1", Type: "function",
			Function: FunctionCall{Name: "Bash", Arguments: `{"command":"ls"}`},
		}}},
		{Role: "tool", ToolCallID: "call_1", Name: "Bash", Content: "a.txt"},
	}
	out := FlattenMessages(msgs, FlattenOptions{Structured: true, ToolResultKeep: 2000,
		ToolResultScope: "tail", ToolResultKeepTail: 1})
	if out.System != "you are helpful" {
		t.Fatalf("system = %q", out.System)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(out.Messages))
	}
	assistant := out.Messages[1]
	calls, _ := assistant["tool_calls"].([]map[string]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", assistant["tool_calls"])
	}
	if calls[0]["id"] != "call_1" {
		t.Fatalf("tool call id lost: %v", calls[0])
	}
	tool := out.Messages[2]
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool message = %v", tool)
	}
	// content must be a string, never nil: nil makes the upstream drop the
	// paired assistant message.
	if _, ok := tool["content"].(string); !ok {
		t.Fatalf("tool content is not a string: %T", tool["content"])
	}
}

func TestFlattenTextualToolHistory(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "c1", Function: FunctionCall{Name: "Bash", Arguments: `{"command":"ls"}`},
		}}},
		{Role: "tool", Name: "Bash", Content: "out"},
	}
	out := FlattenMessages(msgs, FlattenOptions{Structured: false,
		ToolResultKeep: 2000, ToolResultScope: "tail", ToolResultKeepTail: 1})
	blob, _ := json.Marshal(out.Messages)
	text := string(blob)
	if !strings.Contains(text, LeakMarker) {
		t.Fatalf("textual history missing the marker: %s", text)
	}
	if !strings.Contains(text, ToolResultMarker) {
		t.Fatalf("textual history missing the tool-result marker: %s", text)
	}
	for _, m := range out.Messages {
		if m["role"] == "tool" {
			t.Fatal("textual mode must not emit role:tool")
		}
	}
}

func TestToolResultTruncationIsByteStable(t *testing.T) {
	long := strings.Repeat("x", 5000)
	msgs := []Message{
		{Role: "user", Content: "start"},
		{Role: "tool", Name: "Bash", Content: long},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "next"},
		{Role: "tool", Name: "Bash", Content: "short"},
	}
	opts := FlattenOptions{ToolResultKeep: 100, ToolResultScope: "tail", ToolResultKeepTail: 1}
	first := FlattenMessages(msgs, opts)
	// Appending more turns must not change the earlier bytes.
	grown := append(append([]Message{}, msgs...), Message{Role: "user", Content: "more"})
	second := FlattenMessages(grown, opts)
	for i := range first.Messages {
		a, _ := json.Marshal(first.Messages[i])
		b, _ := json.Marshal(second.Messages[i])
		if string(a) != string(b) {
			t.Fatalf("message %d changed after appending a turn:\n%s\n%s", i, a, b)
		}
	}
	// The truncation marker must contain no character counts (byte stability).
	found := false
	for _, m := range first.Messages {
		if s, ok := m["content"].(string); ok && strings.Contains(s, "已截短") {
			found = true
			if strings.ContainsAny(s, "0123456789") && !strings.Contains(s, "x") {
				t.Fatalf("truncation marker contains a count: %q", s)
			}
		}
	}
	if !found {
		t.Fatal("the old tool result was not truncated")
	}
}

func TestAttachMaxTokensTranslate(t *testing.T) {
	payload := map[string]any{"max_completion_tokens": 256}
	TranslateMaxCompletionTokens(payload)
	if payload["max_tokens"] != 256 {
		t.Fatalf("max_tokens = %v", payload["max_tokens"])
	}
}

func TestNormalizeToolsNestsFunction(t *testing.T) {
	payload := map[string]any{"tools": []any{
		map[string]any{"type": "function", "name": "Bash",
			"parameters": map[string]any{"type": "object"}},
	}}
	NormalizeTools(payload)
	tools, _ := payload["tools"].([]any)
	first, _ := tools[0].(map[string]any)
	fn, ok := first["function"].(map[string]any)
	if !ok || fn["name"] != "Bash" {
		t.Fatalf("tool was not nested: %v", first)
	}
}

func TestMessagesToChatSystemInline(t *testing.T) {
	t.Setenv("QD_ANTHROPIC_MSG_SYSTEM", "inline")
	payload := map[string]any{
		"system": "top level",
		"messages": []any{
			map[string]any{"role": "system", "content": "dynamic per turn"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	chat := MessagesToChat(payload, nil, "")
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3 (system + inlined system + user): %v", len(msgs), msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "top level" {
		t.Fatalf("leading system = %v", first)
	}
	second, _ := msgs[1].(map[string]any)
	if second["role"] != "user" || second["content"] != "dynamic per turn" {
		t.Fatalf("inlined system should be a user message: %v", second)
	}
}

func TestMessagesToChatToolResultOrdering(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "tu_1", "name": "Bash",
				"input": map[string]any{"command": "ls"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": "ok"},
		}},
	}}
	chat := MessagesToChat(payload, nil, "")
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %v", len(msgs), msgs)
	}
	// The assistant call comes first, then the tool result that answers it —
	// exactly the pairing the upstream requires.
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "assistant" {
		t.Fatalf("first message = %v", first)
	}
	second, _ := msgs[1].(map[string]any)
	if second["role"] != "tool" || second["tool_call_id"] != "tu_1" {
		t.Fatalf("second message = %v", second)
	}
}

// TestMessagesToChatToolResultBeforeText checks the intra-message order: a user
// message carrying both a tool_result and text emits the result first.
func TestMessagesToChatToolResultBeforeText(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_9", "content": "res"},
			map[string]any{"type": "text", "text": "and now continue"},
		}},
	}}
	chat := MessagesToChat(payload, nil, "")
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "tool" {
		t.Fatalf("tool result must come first: %v", msgs[0])
	}
	if msgs[1].(map[string]any)["role"] != "user" {
		t.Fatalf("the text block follows: %v", msgs[1])
	}
}

func TestEffortFromAnthropicAdaptive(t *testing.T) {
	// Claude Code sends type=adaptive plus output_config.effort for third-party
	// models; the mapping must still apply.
	payload := map[string]any{
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "high"},
	}
	got := EffortFromAnthropic(payload, []string{"low", "medium", "high", "max"}, "medium")
	if got != "high" {
		t.Fatalf("effort = %q, want high", got)
	}
	// A tier the model does not offer clamps to the nearest supported one.
	got = EffortFromAnthropic(map[string]any{
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 16000},
	}, []string{"low", "high", "max"}, "high")
	if got != "high" {
		t.Fatalf("16k budget clamped to %q, want high", got)
	}
	// Disabled thinking sends nothing.
	if got := EffortFromAnthropic(map[string]any{
		"thinking": map[string]any{"type": "disabled"},
	}, []string{"low"}, "low"); got != "" {
		t.Fatalf("disabled thinking produced %q", got)
	}
}

func TestChatUsageToAnthropicConservation(t *testing.T) {
	u := &Usage{PromptTokens: 1000, CompletionTokens: 50, TotalTokens: 1050,
		CachedTokens: 800}
	out := ChatUsageToAnthropic(u)
	in := out["input_tokens"].(int)
	cached := out["cache_read_input_tokens"].(int)
	if in+cached != 1000 {
		t.Fatalf("input(%d) + cache(%d) != prompt(1000)", in, cached)
	}
	if out["cache_creation_input_tokens"].(int) != 0 {
		t.Fatal("cache_creation must be 0 (the upstream has no such concept)")
	}
	if out["output_tokens"].(int) != 50 {
		t.Fatalf("output_tokens = %v", out["output_tokens"])
	}
}

func TestResponsesToChatFunctionCall(t *testing.T) {
	payload := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "run it"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "Bash",
				"arguments": `{"command":"ls"}`},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "done"},
		},
	}
	chat := ResponsesToChat(payload)
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages: %v", len(msgs), msgs)
	}
	assistant, _ := msgs[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", assistant["tool_calls"])
	}
	tool, _ := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "c1" {
		t.Fatalf("tool message = %v", tool)
	}
}

func TestCustomToolDowngradeAndUnwrap(t *testing.T) {
	tools := []any{
		map[string]any{"type": "custom", "name": "apply_patch",
			"description": "edit files"},
	}
	names := CustomToolNames(tools)
	if !names["apply_patch"] {
		t.Fatal("custom tool not detected")
	}
	converted := ToolsForChat(tools)
	fn, _ := converted[0].(map[string]any)["function"].(map[string]any)
	if !strings.Contains(fn["description"].(string), "freeform tool") {
		t.Fatalf("the downgrade hint is missing: %v", fn["description"])
	}
	params, _ := fn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	if _, ok := props["input"]; !ok {
		t.Fatalf("the custom tool must expose a single input parameter: %v", params)
	}
	// The way back inflates the raw payload.
	if got := UnwrapCustomInput(`{"input":"*** Begin Patch"}`); got != "*** Begin Patch" {
		t.Fatalf("unwrapped = %q", got)
	}
}

func TestParseDSMLToolCalls(t *testing.T) {
	text := "running\n<|DSML|function_calls><|DSML|invoke name=\"Bash\">" +
		"<|DSML|parameter name=\"command\">ls -la</|DSML|parameter>" +
		"</|DSML|invoke></|DSML|function_calls>"
	calls, clean := ParseDSMLToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("parsed %d calls: %v", len(calls), calls)
	}
	if calls[0].Name != "Bash" {
		t.Fatalf("name = %q", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, "ls -la") {
		t.Fatalf("arguments = %q", calls[0].Arguments)
	}
	if clean != "running" {
		t.Fatalf("clean text = %q", clean)
	}
}

func TestEstimateAnthropicTokensCJK(t *testing.T) {
	out := EstimateAnthropicTokens(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "你好世界"}},
	})
	n := out["input_tokens"].(int)
	// Four CJK characters cost at least four tokens.
	if n < 4 {
		t.Fatalf("estimate = %d for four CJK characters", n)
	}
}

func TestValidateAnthropicRequest(t *testing.T) {
	if ok, _ := ValidateAnthropicRequest(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}); ok {
		t.Fatal("missing max_tokens must be rejected")
	}
	if ok, _ := ValidateAnthropicRequest(map[string]any{
		"max_tokens": 10,
	}); ok {
		t.Fatal("missing messages must be rejected")
	}
	if ok, _ := ValidateAnthropicRequest(map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 10,
	}); !ok {
		t.Fatal("a complete request must pass")
	}
}

func TestImagePreflightStable(t *testing.T) {
	// A non-PNG payload must pass through untouched.
	png := "data:image/png;base64," + strings.Repeat("A", 4000)
	msgs := []map[string]any{{
		"role": "user",
		"content": []any{map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": png},
		}},
	}}
	out, stats := PrepareInlineImages(msgs)
	if stats.Images != 1 {
		t.Fatalf("images = %d", stats.Images)
	}
	got, _ := out[0]["content"].([]any)[0].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if got != png {
		t.Fatal("an undecodable image must be passed through byte for byte")
	}
}
