package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/logx"
	"github.com/xzs/bridge_qoder/internal/sign"
)

// fakeUpstream stands in for the Qoder gateway: it verifies the request is a
// signed, encoded COSY call and answers with an envelope stream.
type fakeUpstream struct {
	t        *testing.T
	inner    []string // inner chunk JSON strings
	requests int
	lastBody map[string]any
	status   int
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests++
	body, _ := io.ReadAll(r.Body)

	// The live model-list endpoint is a separate GET with no x-model-key. Answer
	// it with an empty catalog so the snapshot stays authoritative in tests.
	if strings.Contains(r.URL.Path, "/model/list") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
		return
	}

	// Every header the official client sends must be present.
	for _, h := range []string{"authorization", "cosy-key", "cosy-date", "cosy-user",
		"cosy-machineid", "cosy-version"} {
		if r.Header.Get(h) == "" {
			f.t.Errorf("missing header %q", h)
		}
	}
	if !strings.HasPrefix(r.Header.Get("authorization"), "Bearer COSY.") {
		f.t.Errorf("bad authorization: %q", r.Header.Get("authorization"))
	}
	// The body must be Qoder-encoded; decode it and check the shape.
	decoded, err := sign.QoderDecode(string(body))
	if err != nil {
		f.t.Fatalf("body is not Qoder-encoded: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		f.t.Fatalf("decoded body is not JSON: %v (%.200s)", err, decoded)
	}
	f.lastBody = payload
	for _, key := range []string{"request_id", "chat_record_id", "session_id",
		"model_config", "chat_context", "messages", "business", "parameters"} {
		if _, ok := payload[key]; !ok {
			f.t.Errorf("body missing %q", key)
		}
	}

	if f.status != 0 && f.status != 200 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"code":"10605","isQueued":true,"retryAfterSeconds":1}`))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, inner := range f.inner {
		envelope := map[string]any{
			"headers":         map[string]any{},
			"body":            inner,
			"statusCodeValue": 200,
		}
		blob, _ := json.Marshal(envelope)
		fmt.Fprintf(w, "data: %s\n\n", blob)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprintf(w, "data: %s\n\n", `{"body":"[DONE]"}`)
}

func chunk(content, reasoning string, finish any) string {
	delta := map[string]any{}
	if content != "" {
		delta["content"] = content
	}
	if reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	obj := map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1700000000,
		"model": "Qwen3.8-Max",
		"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": finish,
		}},
	}
	blob, _ := json.Marshal(obj)
	return string(blob)
}

func chunkWithTool(index int, id, name, args string) string {
	obj := map[string]any{
		"id": "chatcmpl-test", "model": "Qwen3.8-Max",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": index, "id": id,
				"function": map[string]any{"name": name, "arguments": args},
			}}},
			"finish_reason": nil,
		}},
	}
	blob, _ := json.Marshal(obj)
	return string(blob)
}

func usageChunk(prompt, completion, cached int) string {
	obj := map[string]any{
		"id": "chatcmpl-test", "model": "Qwen3.8-Max",
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens": prompt, "completion_tokens": completion,
			"total_tokens": prompt + completion, "credits": 1.5,
			"prompt_tokens_details": map[string]any{"cached_tokens": cached},
		},
	}
	blob, _ := json.Marshal(obj)
	return string(blob)
}

// newTestServer builds a server with one fake account and a fake upstream.
func newTestServer(t *testing.T, up *fakeUpstream) (*Server, *httptest.Server, string) {
	t.Helper()
	upstreamSrv := httptest.NewServer(up)
	t.Cleanup(upstreamSrv.Close)

	dir := t.TempDir()
	accountsDir := filepath.Join(dir, "accounts")
	usageDir := filepath.Join(dir, "usage")
	if err := os.MkdirAll(accountsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Point the CN realm at the fake upstream. The signature covers only the
	// path, so overriding the host is safe.
	origGateway := accounts.RealmConfigs[accounts.RealmCN].Gateway
	cfgCN := accounts.RealmConfigs[accounts.RealmCN]
	cfgCN.Gateway = upstreamSrv.URL
	accounts.RealmConfigs[accounts.RealmCN] = cfgCN
	t.Cleanup(func() { accounts.RealmConfigs[accounts.RealmCN] = cfgCN })
	_ = origGateway

	accJSON := map[string]any{
		"uid":          "test-uid-0001",
		"nickname":     "tester",
		"domain":       "qoder.com.cn",
		"realm":        "cn",
		"platform":     "CLI",
		"accessToken":  "dt-test-token",
		"refreshToken": "drt-test-refresh",
		"expiresAt":    time.Now().Add(24 * time.Hour).Unix(),
		"source":       "oauth",
		"enabled":      true,
		"userType":     "personal_professional_trial",
	}
	blob, _ := json.Marshal(accJSON)
	if err := os.WriteFile(filepath.Join(accountsDir, "test-uid-0001.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Host: "127.0.0.1", Port: 0, AccountsDir: accountsDir, UsageDir: usageDir,
		SystemPrompt: DefaultSystemPrompt, DefaultRealm: "cn",
		Version: "test", MaxPayloadBytes: 50 << 20,
	}
	logger := logx.Discard()
	srv, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return srv, httpSrv, dir
}

func postJSON(t *testing.T, url string, body map[string]any) *http.Response {
	t.Helper()
	blob, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func TestChatNonStreamingEndToEnd(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{
		chunk("Hello", "", nil),
		chunk(", world!", "", nil),
		chunk("", "", "stop"),
		usageChunk(120, 8, 100),
	}}
	srv, httpSrv, _ := newTestServer(t, up)

	resp := postJSON(t, httpSrv.URL+"/v1/chat/completions", map[string]any{
		"model":    "Qwen3.8-Max",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON: %s", raw)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices: %s", raw)
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if got := msg["content"]; got != "Hello, world!" {
		t.Fatalf("content = %v", got)
	}
	// The upstream body must carry the resolved key and the conversation ids.
	if up.lastBody == nil {
		t.Fatal("upstream saw no body")
	}
	mc, _ := up.lastBody["model_config"].(map[string]any)
	if mc["key"] != "qmodel_38max" {
		t.Fatalf("model_config.key = %v", mc["key"])
	}
	if mc["display_name"] != "Qwen3.8-Max" {
		t.Fatalf("display_name = %v", mc["display_name"])
	}
	biz, _ := up.lastBody["business"].(map[string]any)
	if biz["model_key"] != "qmodel_38max" {
		t.Fatalf("business.model_key = %v (billing attribution)", biz["model_key"])
	}
	if _, ok := up.lastBody["tools"].([]any); !ok {
		t.Fatal("tools must always be present (empty when the client sent none)")
	}
	// Usage was recorded.
	sum := srv.usage.SummarySnapshot()
	if sum.Requests != 1 || sum.PromptTokens != 120 || sum.CompletionTokens != 8 {
		t.Fatalf("usage summary = %+v", sum)
	}
}

func TestChatStreamingEndToEnd(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{
		chunk("Hel", "", nil),
		chunk("lo", "", nil),
		chunk("", "", "stop"),
	}}
	_, httpSrv, _ := newTestServer(t, up)

	resp := postJSON(t, httpSrv.URL+"/v1/chat/completions", map[string]any{
		"model":    "Qwen3.8-Max",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	var content strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	sawDone := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "data: [DONE]" {
			sawDone = true
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj) != nil {
			continue
		}
		choices, _ := obj["choices"].([]any)
		for _, c := range choices {
			ch, _ := c.(map[string]any)
			delta, _ := ch["delta"].(map[string]any)
			if s, ok := delta["content"].(string); ok {
				content.WriteString(s)
			}
		}
	}
	if !sawDone {
		t.Fatal("stream did not terminate with data: [DONE]")
	}
	if content.String() != "Hello" {
		t.Fatalf("streamed content = %q", content.String())
	}
}

func TestChatRecoversLeakedToolCall(t *testing.T) {
	// The model echoes the marker as prose; the gateway must return a real call.
	leak := "[assistant 请求调用工具]\n" +
		`[{"name":"Bash","arguments":"{\"command\":\"ls\"}"}]`
	up := &fakeUpstream{t: t, inner: []string{
		chunk("", "", nil),
		chunk(leak, "", nil),
		chunk("", "", "stop"),
	}}
	_, httpSrv, _ := newTestServer(t, up)

	resp := postJSON(t, httpSrv.URL+"/v1/chat/completions", map[string]any{
		"model":    "Qwen3.8-Max",
		"messages": []any{map[string]any{"role": "user", "content": "list files"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":       "Bash",
				"parameters": map[string]any{"type": "object"},
			},
		}},
	})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices: %s", raw)
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("recovered %d tool calls, want 1; raw=%s", len(calls), raw)
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "Bash" {
		t.Fatalf("name = %v", fn["name"])
	}
	if content := asStringAny(msg["content"]); strings.Contains(content, "请求调用工具") {
		t.Fatalf("marker leaked into content: %q", content)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
}

func TestAnthropicEndToEnd(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{
		chunk("Hi ", "", nil),
		chunk("there", "", nil),
		chunk("", "", "stop"),
		usageChunk(50, 4, 40),
	}}
	_, httpSrv, _ := newTestServer(t, up)

	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages",
		strings.NewReader(`{"model":"Qwen3.8-Max","max_tokens":64,`+
			`"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "anything")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	if out["type"] != "message" {
		t.Fatalf("type = %v", out["type"])
	}
	content, _ := out["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content: %s", raw)
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Hi there" {
		t.Fatalf("content block = %v", block)
	}
	usage, _ := out["usage"].(map[string]any)
	// input_tokens + cache_read must equal prompt_tokens.
	sumIn := asFloatAny(usage["input_tokens"]) + asFloatAny(usage["cache_read_input_tokens"])
	if sumIn != 50 {
		t.Fatalf("token conservation broken: input=%v cache=%v prompt=50",
			usage["input_tokens"], usage["cache_read_input_tokens"])
	}
}

func TestResponsesEndToEnd(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{
		chunk("answer", "", nil),
		chunk("", "", "stop"),
		usageChunk(20, 2, 0),
	}}
	_, httpSrv, _ := newTestServer(t, up)

	resp := postJSON(t, httpSrv.URL+"/v1/responses", map[string]any{
		"model":        "Qwen3.8-Max",
		"instructions": "be brief",
		"input":        []any{map[string]any{"type": "message", "role": "user", "content": "q"}},
	})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	if out["object"] != "response" {
		t.Fatalf("object = %v", out["object"])
	}
	if out["output_text"] != "answer" {
		t.Fatalf("output_text = %v", out["output_text"])
	}
	if up.lastBody == nil {
		t.Fatal("no upstream body")
	}
	msgs, _ := up.lastBody["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("instructions were not folded into a system message: %v", msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(asStringAny(first["content"]), "be brief") {
		t.Fatalf("first message = %v", first)
	}
}

func TestAnthropicStreamingEndToEnd(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{
		chunk("", "thinking...", nil),
		chunk("text", "", nil),
		chunkWithTool(0, "call_1", "Bash", `{"command":"ls"}`),
		chunk("", "", "tool_calls"),
	}}
	_, httpSrv, _ := newTestServer(t, up)

	resp := postJSON(t, httpSrv.URL+"/v1/messages", map[string]any{
		"model": "Qwen3.8-Max", "max_tokens": 64, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{
			"name": "Bash", "input_schema": map[string]any{"type": "object"},
		}},
	})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	for _, needle := range []string{
		"event: message_start", "event: content_block_start",
		"event: content_block_delta", "event: content_block_stop",
		"event: message_delta", "event: message_stop",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("stream missing %q\n%s", needle, text)
		}
	}
	if !strings.Contains(text, `"type":"tool_use"`) && !strings.Contains(text, `"type": "tool_use"`) {
		t.Fatalf("tool_use block missing from stream:\n%s", text)
	}
	// input_json_delta carries the arguments incrementally.
	if !strings.Contains(text, "input_json_delta") {
		t.Fatalf("input_json_delta missing:\n%s", text)
	}
}

func TestQueueRetry(t *testing.T) {
	// The first answer is a queue signal; then the request succeeds.
	queued := &queueThenOK{t: t}
	srv := httptest.NewServer(queued)
	defer srv.Close()

	dir := t.TempDir()
	accountsDir := filepath.Join(dir, "accounts")
	_ = os.MkdirAll(accountsDir, 0o755)
	cfgCN := accounts.RealmConfigs[accounts.RealmCN]
	cfgCN.Gateway = srv.URL
	accounts.RealmConfigs[accounts.RealmCN] = cfgCN
	t.Cleanup(func() { accounts.RealmConfigs[accounts.RealmCN] = cfgCN })

	accJSON, _ := json.Marshal(map[string]any{
		"uid": "queue-uid", "realm": "cn", "domain": "qoder.com.cn",
		"accessToken": "dt-x", "expiresAt": time.Now().Add(time.Hour).Unix(),
		"enabled": true,
	})
	_ = os.WriteFile(filepath.Join(accountsDir, "queue-uid.json"), accJSON, 0o600)

	cfg := &Config{Host: "127.0.0.1", AccountsDir: accountsDir,
		UsageDir: filepath.Join(dir, "usage"), DefaultRealm: "cn",
		Version: "test", MaxPayloadBytes: 50 << 20, SystemPrompt: DefaultSystemPrompt}
	gw, err := New(cfg, logx.Discard())
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(gw.Handler())
	defer httpSrv.Close()

	resp := postJSON(t, httpSrv.URL+"/v1/chat/completions", map[string]any{
		"model":    "Qwen3.8-Max",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if queued.calls < 2 {
		t.Fatalf("expected a retry after the queue signal, got %d call(s)", queued.calls)
	}
}

// queueThenOK answers the first request with a queue body and the second with a
// normal stream.
type queueThenOK struct {
	t     *testing.T
	calls int
}

func (q *queueThenOK) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q.calls++
	_, _ = io.Copy(io.Discard, r.Body)
	if q.calls == 1 {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"10605","message":"queued","isQueued":true,"retryAfterSeconds":1}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, inner := range []string{chunk("ok", "", "stop")} {
		blob, _ := json.Marshal(map[string]any{"body": inner, "statusCodeValue": 200})
		fmt.Fprintf(w, "data: %s\n\n", blob)
	}
	fmt.Fprint(w, "data: {\"body\":\"[DONE]\"}\n\n")
}

func TestPayloadTooLargeIsLocal(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{chunk("x", "", "stop")}}
	srv, httpSrv, _ := newTestServer(t, up)
	srv.upstream.MaxBody = 1024 // 1 KiB

	huge := strings.Repeat("a", 4000)
	resp := postJSON(t, httpSrv.URL+"/v1/chat/completions", map[string]any{
		"model":    "Qwen3.8-Max",
		"messages": []any{map[string]any{"role": "user", "content": huge}},
	})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413: %s", resp.StatusCode, raw)
	}
	if up.requests != 0 {
		t.Fatalf("the oversized body reached the upstream %d time(s)", up.requests)
	}
}
