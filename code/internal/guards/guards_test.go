package guards

import (
	"strings"
	"testing"
)

func textFrames(pieces ...string) []Frame {
	return textFramesWithFinish("stop", pieces...)
}

func textFramesWithFinish(finish string, pieces ...string) []Frame {
	frames := make([]Frame, 0, len(pieces)+1)
	for _, p := range pieces {
		frames = append(frames, EncodeFrame(map[string]any{
			"id": "chatcmpl-1", "model": "Qwen3.8-Max", "created": 1700000000,
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": p},
			}},
		}))
	}
	frames = append(frames, EncodeFrame(map[string]any{
		"id": "chatcmpl-1", "model": "Qwen3.8-Max", "created": 1700000000,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": finish,
		}},
	}))
	return frames
}

// joinedContent concatenates every content delta in a frame slice.
func joinedContent(frames []Frame) string {
	var b strings.Builder
	for _, f := range frames {
		if f.Payload == nil {
			continue
		}
		choices, _ := f.Payload["choices"].([]any)
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if s, ok := delta["content"].(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

func toolCallsIn(frames []Frame) []map[string]any {
	var out []map[string]any
	for _, f := range frames {
		if f.Payload == nil {
			continue
		}
		choices, _ := f.Payload["choices"].([]any)
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					if m, ok := tc.(map[string]any); ok {
						out = append(out, m)
					}
				}
			}
		}
	}
	return out
}

func TestMarkerEchoIsRecovered(t *testing.T) {
	allowed := map[string]bool{"Bash": true}
	frames := textFramesWithFinish("stop",
		Marker+"\n",
		`[{"name":"Bash","arguments":"{\"command\":\"ls\"}"}]`,
	)
	out, err := Process(frames, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := toolCallsIn(out)
	if len(calls) != 1 {
		t.Fatalf("recovered %d calls, want 1; content=%q", len(calls), joinedContent(out))
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "Bash" {
		t.Fatalf("name = %v", fn["name"])
	}
	if got := joinedContent(out); got != "" {
		t.Fatalf("marker leaked into content: %q", got)
	}
	// finish_reason must become tool_calls.
	found := false
	for _, f := range out {
		if f.Payload == nil {
			continue
		}
		choices, _ := f.Payload["choices"].([]any)
		for _, c := range choices {
			if ch, ok := c.(map[string]any); ok && ch["finish_reason"] == "tool_calls" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("finish_reason was not rewritten to tool_calls")
	}
}

func TestProsePassesThroughVerbatim(t *testing.T) {
	prose := "Here is how you would use the tool, step by step: first read the file."
	out, err := Process(textFrames(prose), map[string]bool{"Bash": true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := joinedContent(out); got != prose {
		t.Fatalf("prose changed:\n got %q\nwant %q", got, prose)
	}
}

func TestProseDiscussingMarkerPassesThrough(t *testing.T) {
	// A reply that explains the marker must survive byte for byte.
	prose := "关于 " + Marker + " 这个标记，它会被模型复述，所以我们做了回读。"
	out, err := Process(textFrames(prose), map[string]bool{"Bash": true})
	if err != nil {
		t.Fatalf("unexpected error: %v (discussing the marker must not fail the turn)", err)
	}
	if got := joinedContent(out); got != prose {
		t.Fatalf("prose changed:\n got %q\nwant %q", got, prose)
	}
}

func TestProseThenMarkerSameFrame(t *testing.T) {
	allowed := map[string]bool{"Bash": true}
	piece := "let me run that\n\n" + Marker + "\n" +
		`[{"name":"Bash","arguments":"{\"command\":\"pwd\"}"}]`
	out, err := Process(textFramesWithFinish("stop", piece), allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls := toolCallsIn(out); len(calls) != 1 {
		t.Fatalf("recovered %d calls, want 1", len(calls))
	}
	if got := joinedContent(out); !strings.Contains(got, "let me run that") {
		t.Fatalf("leading prose lost: %q", got)
	}
	if strings.Contains(joinedContent(out), Marker) {
		t.Fatalf("marker leaked: %q", joinedContent(out))
	}
}

func TestDSMLEchoRecovered(t *testing.T) {
	allowed := map[string]bool{"Bash": true}
	dsml := `<|DSML|function_calls>` +
		`<|DSML|invoke name="Bash">` +
		`<|DSML|parameter name="command">ls -la</|DSML|parameter>` +
		`</|DSML|invoke></|DSML|function_calls>`
	out, err := Process(textFramesWithFinish("stop", "running now\n"+dsml), allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := toolCallsIn(out)
	if len(calls) != 1 {
		t.Fatalf("recovered %d calls, want 1; content=%q", len(calls), joinedContent(out))
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "Bash" {
		t.Fatalf("name = %v", fn["name"])
	}
	if !strings.Contains(fn["arguments"].(string), "ls -la") {
		t.Fatalf("arguments lost the parameter: %v", fn["arguments"])
	}
}

func TestPseudoXMLToolTagRecovered(t *testing.T) {
	allowed := map[string]bool{"Bash": true}
	body := Marker + "\n<tool_call>Bash {\"command\":\"ls -la\"}</tool_call>"
	out, err := Process(textFramesWithFinish("stop", body), allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := toolCallsIn(out)
	if len(calls) != 1 {
		t.Fatalf("recovered %d calls, want 1; content=%q", len(calls), joinedContent(out))
	}
}

func TestUnescapeRepairRecoversArgs(t *testing.T) {
	allowed := map[string]bool{"Bash": true}
	// The outer array is valid, but the inner arguments have an unescaped quote
	// pair ("hi"). Repairing the escaping must recover the call rather than fail
	// the turn.
	body := Marker + "\n" +
		`[{"name":"Bash","arguments":"{\"command\":\"echo \"hi\" && ls\"}"}]`
	out, err := Process(textFramesWithFinish("stop", body), allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := toolCallsIn(out)
	if len(calls) != 1 {
		t.Fatalf("recovered %d calls, want 1; content=%q", len(calls), joinedContent(out))
	}
	fn := calls[0]["function"].(map[string]any)
	if !strings.Contains(fn["arguments"].(string), "hi") {
		t.Fatalf("arguments lost content: %v", fn["arguments"])
	}
}

// TestUnrepairableLooseEchoFailsClosed covers the loose single-call shape whose
// arguments are damaged beyond repair (the closing brace is missing).
func TestUnrepairableLooseEchoFailsClosed(t *testing.T) {
	allowed := map[string]bool{"Bash": true}
	body := Marker + "\n" + `[{"name":"Bash","arguments":"{"command":"echo "hi" && ls"}"}]`
	if _, err := Process(textFramesWithFinish("stop", body), allowed); err == nil {
		t.Fatal("expected the turn to fail closed")
	}
}

func TestInvalidArgsFailsClosed(t *testing.T) {
	allowed := map[string]bool{"Bash": true}
	// Well-formed outer array, arguments that cannot be repaired.
	body := Marker + "\n" + `[{"name":"Bash","arguments":"{\"command\": <<<>>>"}]`
	_, err := Process(textFramesWithFinish("stop", body), allowed)
	if err == nil {
		t.Fatal("expected a protocol-level failure, got a normal finish")
	}
	if err.Type != ErrInvalidArgs {
		t.Fatalf("error type = %v, want ErrInvalidArgs", err.Type)
	}
	if strings.Contains(err.Detail, "command") {
		t.Fatalf("error detail leaked the payload: %q", err.Detail)
	}
}

func TestMarkerOnlyFailsClosed(t *testing.T) {
	allowed := map[string]bool{"Bash": true}
	out, err := Process(textFramesWithFinish("stop", Marker), allowed)
	if err == nil {
		t.Fatalf("expected a failure for a bare marker; content=%q", joinedContent(out))
	}
	if got := joinedContent(out); strings.Contains(got, Marker) {
		t.Fatalf("bare marker leaked as content: %q", got)
	}
}

func TestUndeclaredToolNamesPassThrough(t *testing.T) {
	// The model repeats a tool the request never declared: recoverable in no way,
	// so the turn must not be punished.
	allowed := map[string]bool{"Bash": true}
	body := Marker + "\n" + `[{"name":"NotDeclared","arguments":"{}"}]`
	out, err := Process(textFramesWithFinish("stop", body), allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := joinedContent(out); !strings.Contains(got, "NotDeclared") {
		t.Fatalf("expected pass-through, got %q", got)
	}
}

func TestFillToolCallIDsIsStablePerIndex(t *testing.T) {
	frames := []Frame{
		EncodeFrame(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "function": map[string]any{"name": "Bash", "arguments": ""}},
			}},
		}}}),
		EncodeFrame(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "function": map[string]any{"arguments": "{\"a\":"}},
			}},
		}}}),
	}
	out := FillToolCallIDs(frames)
	calls := toolCallsIn(out)
	if len(calls) != 2 {
		t.Fatalf("got %d calls", len(calls))
	}
	id0, _ := calls[0]["id"].(string)
	id1, _ := calls[1]["id"].(string)
	if id0 == "" || id1 == "" {
		t.Fatalf("ids not filled: %q %q", id0, id1)
	}
	if id0 != id1 {
		t.Fatalf("id changed across frames of one call: %q vs %q", id0, id1)
	}
}

func TestNormalizeToolCallIndices(t *testing.T) {
	// Upstream alternates 0,1,0,1 for two parallel calls.
	frames := []Frame{
		EncodeFrame(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "a", "function": map[string]any{"name": "A"}},
				map[string]any{"index": 1, "id": "b", "function": map[string]any{"name": "B"}},
			}},
		}}}),
		EncodeFrame(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "function": map[string]any{"arguments": "{}"}},
				map[string]any{"index": 1, "function": map[string]any{"arguments": "{}"}},
			}},
		}}}),
	}
	out := NormalizeToolCallIndices(frames)
	calls := toolCallsIn(out)
	if len(calls) != 4 {
		t.Fatalf("got %d calls", len(calls))
	}
	if calls[0]["index"] != 0 || calls[1]["index"] != 1 {
		t.Fatalf("indices not normalised: %v %v", calls[0]["index"], calls[1]["index"])
	}
	if calls[2]["index"] != 0 || calls[3]["index"] != 1 {
		t.Fatalf("indices drifted on the second frame: %v %v",
			calls[2]["index"], calls[3]["index"])
	}
}

func TestGuardNamelessToolCalls(t *testing.T) {
	frames := []Frame{
		EncodeFrame(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "x",
					"function": map[string]any{"arguments": "{\"a\":1}"}},
			}},
		}}}),
	}
	out := GuardNamelessToolCalls(frames)
	if calls := toolCallsIn(out); len(calls) != 0 {
		t.Fatalf("nameless call was not dropped: %v", calls)
	}
}

func TestTruncatedArgumentsFail(t *testing.T) {
	frames := []Frame{
		EncodeFrame(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "x",
					"function": map[string]any{"name": "Bash", "arguments": "{\"command\": \"ls"}},
			}},
		}}}),
	}
	if _, err := GuardTruncatedToolArgs(frames); err == nil {
		t.Fatal("expected a truncated-arguments failure")
	}
}

func TestJsonFixers(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":"b"}`, `{"a":"b"}`},
		{`{"a":1,}`, `{"a":1,}`},
	}
	for _, c := range cases {
		if fixed := RepairArgsJSON(c.in); fixed == nil {
			t.Fatalf("RepairArgsJSON(%q) = nil", c.in)
		}
	}
	if fixed := RepairArgsJSON(`{"a": }`); fixed != nil {
		t.Fatalf("expected nil for unrepairable input, got %q", *fixed)
	}
	// Trailing comma removal.
	if fixed := RepairArgsJSON(`{"a":1,}`); fixed == nil || !strings.Contains(*fixed, "1") {
		t.Fatalf("trailing comma not repaired")
	}
}

func TestEscapeUnescapedQuotes(t *testing.T) {
	in := `{"command":"echo "hi" && ls"}`
	out := EscapeUnescapedQuotes(in)
	if !strings.Contains(out, `\"hi\"`) {
		t.Fatalf("inner quotes not escaped: %s", out)
	}
	if !IsValidJSONObject(out) {
		t.Fatalf("result is not a valid JSON object: %s", out)
	}
}

func TestToolEchoModeEnv(t *testing.T) {
	t.Setenv("QD_TOOL_ECHO_MODE", "lenient")
	// The mode is resolved once per process; verify the setter path instead.
	SetToolEchoMode("lenient")
	if ToolEchoMode() != ToolEchoLenient {
		t.Fatalf("mode = %q", ToolEchoMode())
	}
	SetToolEchoMode("strict")
	if ToolEchoMode() != ToolEchoStrict {
		t.Fatalf("mode = %q", ToolEchoMode())
	}
}
