package server

import (
	"encoding/json"
	"net/http"

	"github.com/xzs/bridge_qoder/internal/upstream"
	"github.com/xzs/bridge_qoder/internal/webtools"
)

// webtoolsEnabledFor resolves the env-over-panel switch.
func webtoolsEnabledFor(panel bool) bool { return webtools.Enabled(panel) }

// builtinWebToolNames returns the local tool names a request declares.
func (s *Server) builtinWebToolNames(payload map[string]any) []string {
	tools, _ := payload["tools"].([]any)
	return webtools.BuiltinNames(tools)
}

// handleWebTools runs a turn with the local tools, feeding results back to the
// model for at most webtools.Rounds() rounds.
func (s *Server) handleWebTools(w http.ResponseWriter, r *http.Request, payload map[string]any, names []string) {
	tools, _ := payload["tools"].([]any)
	payload["tools"] = webtools.RewriteTools(tools, names)
	payload["stream"] = false

	allowed := guardsToolNames(payload)
	start := timeNowFn()

	for round := 0; round < webtools.Rounds(); round++ {
		up, acc, err := s.openUpstreamFor(r, payload, extractSessionKey(r, payload))
		if err != nil {
			s.writeUpstreamError(w, err)
			return
		}
		holder := &upstream.Holder{}
		result, err := s.aggregateStream(r.Context(), up, payload, holder, allowed)
		up.Close()
		if err != nil {
			s.writeUpstreamFriendly(w, err)
			return
		}
		// No tool calls: the model answered directly.
		msg, _ := firstChoiceMessage(result.Object)
		calls, _ := msg["tool_calls"].([]any)
		if len(calls) == 0 {
			s.recordUsage(simpleUsageRow(payload, acc, start))
			s.writeJSON(w, http.StatusOK, result.Object)
			return
		}
		// Execute only the local tools; leave other calls for the client.
		executed := false
		appended := appendMessagesForWebTools(payload, msg)
		for _, c := range calls {
			call, ok := c.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			name := asStringAny(fn["name"])
			if name != webtools.WebSearchName && name != webtools.WebFetchName {
				continue
			}
			out := webtools.RunTool(name, asStringAny(fn["arguments"]))
			appended = append(appended, map[string]any{
				"role": "tool", "tool_call_id": asStringAny(call["id"]), "content": out,
			})
			executed = true
		}
		if !executed {
			s.recordUsage(simpleUsageRow(payload, acc, start))
			s.writeJSON(w, http.StatusOK, result.Object)
			return
		}
		payload["messages"] = appended
	}
	s.writeError(w, http.StatusBadGateway,
		"local web tools exceeded their round budget; please retry", "upstream_error")
}

// appendMessagesForWebTools extends the conversation with the assistant's call
// turn.
func appendMessagesForWebTools(payload map[string]any, msg map[string]any) []any {
	msgs, _ := payload["messages"].([]any)
	out := make([]any, 0, len(msgs)+2)
	out = append(out, msgs...)
	out = append(out, map[string]any{
		"role": "assistant", "content": asStringAny(msg["content"]),
		"tool_calls": msg["tool_calls"],
	})
	return out
}

func firstChoiceMessage(obj map[string]any) (map[string]any, bool) {
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return map[string]any{}, false
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return map[string]any{}, false
	}
	return msg, true
}

// marshalJSON is a small helper for the web-tools JSON payloads.
func marshalJSON(v any) string {
	blob, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(blob)
}

// guardsToolNames extracts declared tool names for the guard chain.
func guardsToolNames(payload map[string]any) map[string]bool {
	out := map[string]bool{}
	tools, _ := payload["tools"].([]any)
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := m["function"].(map[string]any); ok {
			if name := asStringAny(fn["name"]); name != "" {
				out[name] = true
			}
		}
		if name := asStringAny(m["name"]); name != "" {
			out[name] = true
		}
	}
	return out
}
