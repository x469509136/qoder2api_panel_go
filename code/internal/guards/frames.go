package guards

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xzs/bridge_qoder/internal/protocol"
)

// Frame is one SSE payload destined for the client.
//
// Raw carries the original bytes so an untouched frame is forwarded byte for
// byte; Payload is the decoded object when one was produced.
type Frame struct {
	Raw     []byte
	Payload map[string]any
	// Comment is an SSE comment line (": ping") relayed verbatim.
	Comment bool
	// Done marks the "[DONE]" sentinel.
	Done bool
}

// ParseFrame decodes one upstream line into a Frame.
func ParseFrame(raw []byte) Frame {
	text := string(raw)
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Frame{Raw: raw}
	}
	if strings.HasPrefix(trimmed, ":") {
		return Frame{Raw: raw, Comment: true}
	}
	for strings.HasPrefix(trimmed, "data:") {
		trimmed = strings.TrimSpace(trimmed[5:])
	}
	if trimmed == "" {
		return Frame{Raw: raw}
	}
	if trimmed == "[DONE]" {
		return Frame{Raw: raw, Done: true}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return Frame{Raw: raw}
	}
	return Frame{Raw: raw, Payload: payload}
}

// EncodeFrame serialises a payload as a data line.
func EncodeFrame(payload map[string]any) Frame {
	blob, _ := protocol.MarshalNoEscape(payload)
	raw := append([]byte("data: "), blob...)
	raw = append(raw, '\n', '\n')
	return Frame{Raw: raw, Payload: payload}
}

// Meta remembers the upstream stream's identity fields for synthesized frames.
type Meta struct {
	ID      string
	Model   string
	Created int64
}

func metaOf(payload map[string]any, meta *Meta) {
	if payload == nil {
		return
	}
	if v := asString(payload["id"]); v != "" {
		meta.ID = v
	}
	if v := asString(payload["model"]); v != "" {
		meta.Model = v
	}
	if v, ok := payload["created"]; ok && v != nil {
		meta.Created = asInt(v)
	}
}

func chunkFrame(meta Meta, delta map[string]any, finish any) Frame {
	id := meta.ID
	if id == "" {
		id = "chatcmpl-qoder"
	}
	created := meta.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	return EncodeFrame(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created,
		"model": meta.Model,
		"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": finish,
		}},
	})
}

// carryDelta copies every non-content increment of a delta.
//
// Synthesising a content-only frame would drop a tool_calls start (with its
// function.name) that arrived in the same frame; downstream clients then report
// "tool call delta is missing id or name".
func carryDelta(delta map[string]any) map[string]any {
	out := map[string]any{}
	if delta == nil {
		return out
	}
	if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
		out["tool_calls"] = tcs
	}
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		out["reasoning_content"] = rc
	}
	return out
}

func contentChunk(meta Meta, piece string, delta map[string]any, finish any) Frame {
	body := map[string]any{"content": piece}
	for k, v := range carryDelta(delta) {
		body[k] = v
	}
	return chunkFrame(meta, body, finish)
}

// frameWithoutContent copies a frame with delta.content removed.
func frameWithoutContent(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		out[k] = v
	}
	choices, _ := payload["choices"].([]any)
	newChoices := make([]any, 0, len(choices))
	for _, c := range choices {
		ch, ok := c.(map[string]any)
		if !ok {
			newChoices = append(newChoices, c)
			continue
		}
		ch2 := make(map[string]any, len(ch))
		for k, v := range ch {
			ch2[k] = v
		}
		if delta, ok := ch["delta"].(map[string]any); ok {
			d2 := make(map[string]any, len(delta))
			for k, v := range delta {
				d2[k] = v
			}
			delete(d2, "content")
			ch2["delta"] = d2
		}
		newChoices = append(newChoices, ch2)
	}
	out["choices"] = newChoices
	return out
}

// withContent clones a frame with a different content delta.
func withContent(payload map[string]any, content string) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		out[k] = v
	}
	choices, _ := payload["choices"].([]any)
	newChoices := make([]any, 0, len(choices))
	for _, c := range choices {
		ch, ok := c.(map[string]any)
		if !ok {
			newChoices = append(newChoices, c)
			continue
		}
		ch2 := make(map[string]any, len(ch))
		for k, v := range ch {
			ch2[k] = v
		}
		delta, _ := ch["delta"].(map[string]any)
		d2 := map[string]any{"content": content}
		for k, v := range carryDelta(delta) {
			if k == "content" {
				continue
			}
			d2[k] = v
		}
		ch2["delta"] = d2
		newChoices = append(newChoices, ch2)
	}
	out["choices"] = newChoices
	return out
}

// setFinishReason rewrites a frame's finish reason.
func setFinishReason(payload map[string]any, reason string) map[string]any {
	out := deepCopy(payload)
	choices, _ := out["choices"].([]any)
	for _, c := range choices {
		if ch, ok := c.(map[string]any); ok && ch["finish_reason"] != nil {
			ch["finish_reason"] = reason
		}
	}
	return out
}

func deepCopy(payload map[string]any) map[string]any {
	blob, err := protocol.MarshalNoEscape(payload)
	if err != nil {
		return payload
	}
	var out map[string]any
	if err := json.Unmarshal(blob, &out); err != nil {
		return payload
	}
	return out
}

// choiceDelta extracts the first choice's delta and finish reason.
func choiceDelta(payload map[string]any) (delta map[string]any, finish any, hasChoices bool) {
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, nil, false
	}
	ch, ok := choices[0].(map[string]any)
	if !ok {
		return nil, nil, true
	}
	delta, _ = ch["delta"].(map[string]any)
	return delta, ch["finish_reason"], true
}

// ---------------------------------------------------------------------------
// Stages: each one buffers independently and resolves at end of stream, which
// keeps every recovery path testable on its own.
// ---------------------------------------------------------------------------

// Staged runs a buffering recovery stage.
type stagedConfig struct {
	// enter reports whether a piece should start buffering.
	enter func(emitted, piece string) bool
	// tailHold returns the trailing bytes that might still become a tag.
	tailHold func(piece string) string
	// resolve turns the buffer into frames, or reports that it was not a call.
	resolve func(buf string, allowed map[string]bool, meta Meta) (frames []Frame, recovered, ok bool)
	// onUnresolved is called when resolve declines; return false to drop the
	// buffer instead of emitting it.
	onUnresolved func(buf string, allowed map[string]bool) ([]Frame, bool)
}

// runStage applies one buffering stage over a frame slice.
func runStage(frames []Frame, allowed map[string]bool, cfg stagedConfig) ([]Frame, *ToolCallError) {
	var out []Frame
	var buf, tail string
	var finish *struct {
		payload map[string]any
		fin     any
	}
	var tailUsage []Frame
	var emitted strings.Builder
	recovered := false
	meta := Meta{}

	flushFinish := func() {
		if finish == nil {
			return
		}
		payload := finish.payload
		if recovered {
			payload = setFinishReason(payload, "tool_calls")
		}
		out = append(out, EncodeFrame(payload))
		finish = nil
	}

	for _, f := range frames {
		if f.Payload == nil || f.Comment || f.Done {
			out = append(out, f)
			continue
		}
		metaOf(f.Payload, &meta)
		delta, fin, hasChoices := choiceDelta(f.Payload)
		if !hasChoices {
			if _, isUsage := f.Payload["usage"].(map[string]any); isUsage {
				if buf != "" || tail != "" {
					tailUsage = append(tailUsage, f)
					continue
				}
				if finish != nil {
					// Release the held closing frame before the usage footer to keep
					// the standard [finish] -> [usage] order.
					flushFinish()
				}
			}
			out = append(out, f)
			continue
		}
		piece, ok := delta["content"].(string)
		if !ok || piece == "" {
			if fin != nil {
				finish = &struct {
					payload map[string]any
					fin     any
				}{f.Payload, fin}
			} else {
				out = append(out, f)
			}
			continue
		}
		piece = tail + piece
		tail = ""
		if buf == "" {
			hold := cfg.tailHold(piece)
			body := piece
			if hold != "" {
				body = piece[:len(piece)-len(hold)]
			}
			if cfg.enter(emitted.String(), body) {
				buf = body
				tail = hold
				continue
			}
			if body != "" {
				emitted.WriteString(body)
				out = append(out, EncodeFrame(withContent(f.Payload, body)))
			}
			tail = hold
			continue
		}
		buf += piece
	}

	if buf != "" {
		resolved, _, ok := cfg.resolve(buf, allowed, meta)
		if ok {
			recovered = true
			out = append(out, resolved...)
		} else if cfg.onUnresolved != nil {
			extra, emit := cfg.onUnresolved(buf, allowed)
			if !emit {
				return out, nil
			}
			out = append(out, extra...)
		} else {
			out = append(out, chunkFrame(meta, map[string]any{"content": buf}, nil))
		}
	}
	if tail != "" {
		if ResidueTextOnly(tail) {
			droppedFragments.Add(1)
		} else {
			out = append(out, chunkFrame(meta, map[string]any{"content": tail}, nil))
		}
	}
	if finish != nil {
		flushFinish()
	} else if recovered {
		out = append(out, chunkFrame(meta, map[string]any{}, "tool_calls"))
	}
	out = append(out, tailUsage...)
	return out, nil
}

// recoveredCallFrames renders structured calls, prefixing any leftover prose.
func recoveredCallFrames(meta Meta, calls []map[string]any, clean string) []Frame {
	var out []Frame
	if clean != "" {
		if !ResidueTextOnly(clean) {
			out = append(out, chunkFrame(meta, map[string]any{"content": clean}, nil))
		} else {
			droppedFragments.Add(1)
		}
	}
	for i, call := range calls {
		fn, _ := call["function"].(map[string]any)
		id := asString(call["id"])
		if id == "" {
			id = protocol.NewRequestIDPrefix("call_")
		}
		out = append(out, chunkFrame(meta, map[string]any{
			"tool_calls": []any{map[string]any{
				"index": i, "id": id,
				"type":     firstNonEmptyStr(asString(call["type"]), "function"),
				"function": fn,
			}},
		}, nil))
	}
	return out
}

// RecoverMarkerEcho recovers "marker + JSON array" echoes (issue #8).
//
// This is the stage that owns the hold/fail-closed semantics documented in the
// package comment.
func RecoverMarkerEcho(frames []Frame, allowed map[string]bool) ([]Frame, *ToolCallError) {
	var out []Frame
	var hold string
	holding := false
	holdFrames := 0
	var finished *struct {
		payload map[string]any
		fin     any
	}
	var tailUsage []Frame
	recovered := false
	meta := Meta{}

	for _, f := range frames {
		if f.Payload == nil || f.Comment || f.Done {
			out = append(out, f)
			continue
		}
		metaOf(f.Payload, &meta)
		delta, fin, hasChoices := choiceDelta(f.Payload)
		if !hasChoices {
			if holding {
				if _, isUsage := f.Payload["usage"].(map[string]any); isUsage {
					// A usage footer arriving mid-hold carries no content and does
					// not affect the candidate; keep it for after the closing frame.
					tailUsage = append(tailUsage, f)
					continue
				}
			}
			out = append(out, f)
			continue
		}
		rawPiece, hasContent := delta["content"]
		piece, pieceIsString := rawPiece.(string)
		if hasContent && !pieceIsString {
			// A non-string content is rare; pass it through untouched.
			if holding {
				out = append(out, contentChunk(meta, hold, nil, nil))
				hold = ""
				holding = false
				holdFrames = 0
			}
			out = append(out, f)
			continue
		}

		if holding {
			hold += piece
			if piece != "" {
				holdFrames = 0
			} else {
				holdFrames++
			}
			if stripped := StripDSMLCloseTail(hold); stripped != hold && StillLeakCandidate(stripped) {
				hold = stripped
			}
			if EchoHoldCandidate(hold) {
				if len(hold) > HoldMaxChars || holdFrames > HoldMaxFrames {
					// Past the bound: fail open, but a bare marker shell must still
					// fail closed instead of being emitted as prose.
					if len(allowed) > 0 && MarkerOnlyEcho(hold) {
						return out, &ToolCallError{Type: ErrEmptyEcho,
							Kind: "marker_only_released", Detail: DetailEmptyEcho,
							Chars: len(hold)}
					}
					out = append(out, contentChunk(meta, hold, nil, fin))
					hold = ""
					holding = false
					holdFrames = 0
					if fin != nil {
						finished = &struct {
							payload map[string]any
							fin     any
						}{frameWithoutContent(f.Payload), fin}
					}
					continue
				}
				if fin != nil {
					finished = &struct {
						payload map[string]any
						fin     any
					}{frameWithoutContent(f.Payload), fin}
				} else if hasToolOrReasoningDelta(delta) {
					out = append(out, EncodeFrame(frameWithoutContent(f.Payload)))
				}
				continue
			}
			// The candidate is disproved.
			if EchoWithBadArgs(hold, allowed) {
				return out, &ToolCallError{Type: ErrInvalidArgs,
					Kind: "invalid_arguments", Detail: DetailInvalidArgs, Chars: len(hold)}
			}
			// Prose followed by a marker inside one large hold: release the prose
			// and keep only the marker onwards for the candidate check.
			if pos, _ := LastMarkerSpan(hold); pos > 0 {
				tailPart := hold[pos:]
				if StillLeakCandidate(tailPart) {
					prose := hold[:pos]
					if strings.TrimSpace(prose) != "" {
						out = append(out, contentChunk(meta, prose, nil, fin))
					}
					hold = tailPart
					if fin != nil {
						finished = &struct {
							payload map[string]any
							fin     any
						}{frameWithoutContent(f.Payload), fin}
					}
					continue
				}
			}
			if LeakGarbageOnly(hold) {
				droppedFragments.Add(1)
			} else if len(allowed) > 0 && LeakResiduePresent(hold) &&
				!EchoNamesAllUndeclared(hold, allowed) {
				// A DSML fragment that the DSML stage can still recover is left for
				// it; only genuinely unrecoverable residue fails here.
				if dsml, _ := protocol.ParseDSMLToolCalls(hold); len(dsml) > 0 {
					out = append(out, contentChunk(meta, hold, nil, fin))
				} else if tags, _ := ParseToolCallTags(hold, allowed); len(tags) > 0 {
					out = append(out, contentChunk(meta, hold, nil, fin))
				} else {
					return out, &ToolCallError{Type: ErrEmptyEcho,
						Kind: "unrecovered_falsified", Detail: DetailEmptyEcho,
						Chars: len(hold)}
				}
			} else {
				out = append(out, contentChunk(meta, hold, nil, fin))
			}
			hold = ""
			holding = false
			holdFrames = 0
			if fin != nil {
				finished = &struct {
					payload map[string]any
					fin     any
				}{frameWithoutContent(f.Payload), fin}
			}
			continue
		}

		if fin != nil && piece == "" {
			finished = &struct {
				payload map[string]any
				fin     any
			}{f.Payload, fin}
			continue
		}
		if piece != "" {
			emit, held := FindMarkerSplit(piece, true)
			if held != "" {
				if emit != "" {
					out = append(out, contentChunk(meta, emit, nil, fin))
				}
				hold = held
				holding = true
				holdFrames = 1
				if fin != nil {
					finished = &struct {
						payload map[string]any
						fin     any
					}{frameWithoutContent(f.Payload), fin}
				}
				continue
			}
			if LeakPrefixHold(piece) || ToolEchoPrefixHold(piece) {
				hold = piece
				holding = true
				holdFrames = 1
				if fin != nil {
					finished = &struct {
						payload map[string]any
						fin     any
					}{frameWithoutContent(f.Payload), fin}
				}
				continue
			}
			out = append(out, contentChunk(meta, piece, delta, fin))
			continue
		}
		// A usage footer after the closing frame: keep the standard order.
		if finished != nil && !holding {
			if _, isUsage := f.Payload["usage"].(map[string]any); isUsage {
				out = append(out, EncodeFrame(finished.payload))
				finished = nil
			}
		}
		out = append(out, f)
	}

	if holding {
		calls, clean := ParseLeakedToolCalls(hold, allowed)
		if len(calls) == 0 && strings.Contains(strings.ToLower(hold), ToolCallOpen) {
			calls, clean = ParseToolCallTags(hold, allowed)
			if len(calls) > 0 {
				tagRecovered.Add(1)
			}
		}
		switch {
		case len(calls) > 0:
			recovered = true
			leakRecovered.Add(1)
			out = append(out, recoveredCallFrames(meta, calls, clean)...)
		case EchoWithBadArgs(hold, allowed):
			// Checked before the truncation branch: a bad-args block has a complete
			// outer array, so the truncation predicate would swallow it and report
			// a normal stop.
			return out, &ToolCallError{Type: ErrInvalidArgs,
				Kind: "invalid_arguments", Detail: DetailInvalidArgs, Chars: len(hold)}
		case LeakedPartialDroppable(hold, allowed):
			kind := "partial_array"
			if MarkerOnlyEcho(hold) {
				kind = "marker_only"
			}
			return out, &ToolCallError{Type: ErrEmptyEcho, Kind: kind,
				Detail: DetailEmptyEcho, Chars: len(hold)}
		case ToolEchoDroppable(hold, allowed):
			droppedFragments.Add(1)
		case MarkerOnlyEcho(hold):
			// A lone marker carries no information and must never reach the user
			// (without declared tools we cannot prove it is an echo).
			droppedFragments.Add(1)
		default:
			if LeakGarbageOnly(hold) {
				droppedFragments.Add(1)
			} else if len(allowed) > 0 &&
				(LeakResiduePresent(hold) || DSMLResiduePresent(hold)) &&
				!EchoNamesAllUndeclared(hold, allowed) {
				if dsml, _ := protocol.ParseDSMLToolCalls(hold); len(dsml) > 0 {
					out = append(out, contentChunk(meta, hold, nil, nil))
				} else {
					return out, &ToolCallError{Type: ErrEmptyEcho,
						Kind: "unrecovered_leak", Detail: DetailEmptyEcho, Chars: len(hold)}
				}
			} else {
				out = append(out, contentChunk(meta, hold, nil, nil))
			}
		}
	}

	if finished != nil {
		payload := finished.payload
		if recovered {
			payload = setFinishReason(payload, "tool_calls")
		}
		out = append(out, EncodeFrame(payload))
	} else if recovered {
		out = append(out, chunkFrame(meta, map[string]any{}, "tool_calls"))
	}
	out = append(out, tailUsage...)
	return out, nil
}

// dsmlTailHold returns the trailing bytes that might still become a DSML tag.
func dsmlTailHold(piece string) string {
	i := strings.LastIndex(piece, "<")
	if i < 0 || len(piece)-i > 24 {
		return ""
	}
	tail := piece[i:]
	if strings.Contains(tail, ">") {
		return ""
	}
	if len(tail) <= 2 || strings.Contains(strings.ToUpper(tail), "DS") ||
		strings.HasSuffix(strings.ToUpper(tail), "D") ||
		strings.Contains(strings.ToUpper(tail), "DSML") {
		return tail
	}
	return ""
}

// toolTagTailHold returns the trailing bytes that might still become a
// `<tool_call` opening tag.
func toolTagTailHold(piece string) string {
	i := strings.LastIndex(piece, "<")
	if i < 0 || len(piece)-i > len(ToolCallOpen)+8 {
		return ""
	}
	tail := piece[i:]
	if strings.Contains(tail, ">") {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(ToolCallOpen), strings.ToLower(tail)) {
		return tail
	}
	return ""
}

// RecoverDSML recovers DeepSeek's native DSML tool calls (issue #51).
//
// The chat and Anthropic paths previously had no DSML recovery at all, so a
// model emitting native DSML leaked the whole protocol into the content.
func RecoverDSML(frames []Frame, allowed map[string]bool) ([]Frame, *ToolCallError) {
	out, err := runStage(frames, allowed, stagedConfig{
		enter: func(emitted, body string) bool {
			return strings.Contains(body, "DSML")
		},
		tailHold: dsmlTailHold,
		resolve: func(buf string, allowed map[string]bool, meta Meta) ([]Frame, bool, bool) {
			parsed, clean := protocol.ParseDSMLToolCalls(buf)
			if len(parsed) == 0 {
				// The same recovery chain also handles "closing tag + array" and
				// "marker + junk + array".
				if calls, lclean := ParseLeakedToolCalls(buf, allowed); len(calls) > 0 {
					return recoveredCallFrames(meta, calls, lclean), true, true
				}
				return nil, false, false
			}
			dsmlRecovered.Add(1)
			calls := make([]map[string]any, 0, len(parsed))
			for _, c := range parsed {
				calls = append(calls, map[string]any{
					"id": c.ID, "type": "function",
					"function": map[string]any{"name": c.Name, "arguments": c.Arguments},
				})
			}
			return recoveredCallFrames(meta, calls, clean), true, true
		},
		onUnresolved: func(buf string, allowed map[string]bool) ([]Frame, bool) {
			if LeakGarbageOnly(buf) {
				droppedFragments.Add(1)
				return nil, false
			}
			if len(allowed) > 0 &&
				(LeakResiduePresent(buf) || DSMLResiduePresent(buf)) &&
				!EchoNamesAllUndeclared(buf, allowed) {
				// Unresolvable DSML residue must not leak as content.
				return nil, false
			}
			return nil, true
		},
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

// RecoverToolTags recovers `<tool_call>Name {json}</tool_call>` echoes
// (issue #137).
func RecoverToolTags(frames []Frame, allowed map[string]bool) ([]Frame, *ToolCallError) {
	if len(allowed) == 0 {
		// Without declared tools, a `<tool_call>` is almost always a discussion
		// of the format (tutorial, question, pasted example).
		return frames, nil
	}
	return runStage(frames, allowed, stagedConfig{
		enter: func(emitted, body string) bool {
			if !strings.Contains(strings.ToLower(body), ToolCallOpen) {
				return false
			}
			// A tag inside an unclosed code fence is documentation, not a call.
			// Counting per frame would miss a fence split across frames, so the
			// whole emitted prefix is counted.
			return strings.Count(emitted+body, "```")%2 == 0
		},
		tailHold: toolTagTailHold,
		resolve: func(buf string, allowed map[string]bool, meta Meta) ([]Frame, bool, bool) {
			calls, clean := ParseToolCallTags(buf, allowed)
			if len(calls) == 0 {
				return nil, false, false
			}
			tagRecovered.Add(1)
			return recoveredCallFrames(meta, calls, clean), true, true
		},
		// Fail-open: an unresolvable tag (tutorial, bad args, undeclared name) is
		// returned as ordinary content.
		onUnresolved: func(buf string, allowed map[string]bool) ([]Frame, bool) {
			return nil, true
		},
	})
}

// ---------------------------------------------------------------------------
// Guard: tool_call index normalisation
// ---------------------------------------------------------------------------

type toolSlot struct {
	index int
	id    string
}

type indexState struct {
	ids    map[string]*toolSlot
	active map[int]*toolSlot
	slots  map[int]*toolSlot
}

// NormalizeToolCallIndices rewrites upstream index churn into 0,1,2...
//
// The upstream sometimes alternates 0,1,0,1 for parallel calls or sends
// everything as index 0. A stable running index is what clients expect.
func NormalizeToolCallIndices(frames []Frame) []Frame {
	states := map[int]*indexState{}
	out := make([]Frame, 0, len(frames))
	for _, f := range frames {
		if f.Payload == nil || f.Comment || f.Done {
			out = append(out, f)
			continue
		}
		choices, ok := f.Payload["choices"].([]any)
		if !ok {
			out = append(out, f)
			continue
		}
		changed := false
		for choicePos, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			calls, ok := delta["tool_calls"].([]any)
			if !ok {
				continue
			}
			ci := choicePos
			if v, ok := choice["index"]; ok && v != nil {
				if n, isInt := asIntOK(v); isInt {
					ci = n
				}
			}
			st, ok := states[ci]
			if !ok {
				st = &indexState{ids: map[string]*toolSlot{},
					active: map[int]*toolSlot{}, slots: map[int]*toolSlot{}}
				states[ci] = st
			}
			for callPos, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				wireIndex := callPos
				if v, ok := call["index"]; ok && v != nil {
					if n, isInt := asIntOK(v); isInt && n >= 0 {
						wireIndex = n
					}
				}
				callID := strings.TrimSpace(asString(call["id"]))
				var slot *toolSlot
				if callID != "" {
					slot = st.ids[callID]
				}
				if slot == nil {
					slot = st.active[wireIndex]
					if slot != nil && callID != "" && slot.id != "" && slot.id != callID {
						slot = nil
					}
				}
				if slot == nil {
					normalized := wireIndex
					if _, taken := st.slots[normalized]; taken {
						maxIdx := -1
						for k := range st.slots {
							if k > maxIdx {
								maxIdx = k
							}
						}
						normalized = maxIdx + 1
					}
					slot = &toolSlot{index: normalized, id: callID}
					st.slots[normalized] = slot
				}
				if callID != "" {
					slot.id = callID
					st.ids[callID] = slot
				}
				st.active[wireIndex] = slot
				if cur, ok := asIntOK(call["index"]); !ok || cur != slot.index {
					call["index"] = slot.index
					changed = true
				}
			}
		}
		if changed {
			out = append(out, EncodeFrame(f.Payload))
		} else {
			out = append(out, f)
		}
	}
	return out
}

func asIntOK(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if t == float64(int(t)) {
			return int(t), true
		}
	case int:
		return t, true
	case int64:
		return int(t), true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Guard: fill tool_call ids
// ---------------------------------------------------------------------------

// FillToolCallIDs guarantees every tool_call delta carries a non-empty id.
//
// A client that persists id="" corrupts its whole session: the loader replays
// from sequence zero and one empty id invalidates the replay. All three tiers
// pass it through; only the gateway's exit can stop it.
func FillToolCallIDs(frames []Frame) []Frame {
	ids := map[string]string{}
	named := map[string]bool{}
	out := make([]Frame, 0, len(frames))
	for _, f := range frames {
		if f.Payload == nil || f.Comment || f.Done {
			out = append(out, f)
			continue
		}
		choices, ok := f.Payload["choices"].([]any)
		if !ok {
			out = append(out, f)
			continue
		}
		touched := false
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			calls, ok := delta["tool_calls"].([]any)
			if !ok {
				continue
			}
			for pos, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				key := toolCallKey(call, pos, len(calls))
				cur := strings.TrimSpace(asString(call["id"]))
				fn, _ := call["function"].(map[string]any)
				name := ""
				if fn != nil {
					name = strings.TrimSpace(asString(fn["name"]))
				}
				if cur != "" {
					// An upstream id always wins: overwrite so later frames
					// converge on the real value instead of a minted one.
					ids[key] = cur
					if name != "" {
						named[key] = true
					}
					continue
				}
				// A second name under the same key means a second call (serial
				// calls with no index both land at position 0) and needs a new id.
				if name != "" && named[key] {
					ids[key] = protocol.NewRequestIDPrefix("call_")
				}
				newID := ids[key]
				if newID == "" {
					newID = protocol.NewRequestIDPrefix("call_")
					ids[key] = newID
				}
				if name != "" {
					named[key] = true
				}
				call["id"] = newID
				touched = true
			}
		}
		if touched {
			out = append(out, EncodeFrame(f.Payload))
		} else {
			out = append(out, f)
		}
	}
	return out
}

func toolCallKey(tc map[string]any, pos, n int) string {
	if idx, ok := asIntOK(tc["index"]); ok && idx >= 0 {
		return fmt.Sprintf("i%d", idx)
	}
	if n > 1 {
		return fmt.Sprintf("p%d", pos)
	}
	return "i0"
}

// ---------------------------------------------------------------------------
// Guard: nameless tool calls
// ---------------------------------------------------------------------------

// GuardNamelessToolCalls drops tool blocks whose name never arrives.
//
// A start frame already emitted without a name cannot be repaired, so the block
// is dropped and the finish reason degrades — better than the client waiting for
// a tool result that will never come.
func GuardNamelessToolCalls(frames []Frame) []Frame {
	type slot struct{ hasName bool }
	slots := map[string]*slot{}
	var out []Frame
	for _, f := range frames {
		if f.Payload == nil || f.Comment || f.Done {
			out = append(out, f)
			continue
		}
		choices, ok := f.Payload["choices"].([]any)
		if !ok {
			out = append(out, f)
			continue
		}
		dropped := false
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			calls, ok := delta["tool_calls"].([]any)
			if !ok {
				continue
			}
			kept := make([]any, 0, len(calls))
			for pos, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				key := toolCallKey(call, pos, len(calls))
				fn, _ := call["function"].(map[string]any)
				name := ""
				if fn != nil {
					name = strings.TrimSpace(asString(fn["name"]))
				}
				s, ok := slots[key]
				if !ok {
					s = &slot{}
					slots[key] = s
				}
				if name != "" {
					s.hasName = true
				}
				if !s.hasName {
					dropped = true
					continue
				}
				kept = append(kept, call)
			}
			if dropped {
				if len(kept) == 0 {
					delete(delta, "tool_calls")
				} else {
					delta["tool_calls"] = kept
				}
			}
		}
		if dropped {
			out = append(out, EncodeFrame(f.Payload))
		} else {
			out = append(out, f)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Guard: truncated tool arguments
// ---------------------------------------------------------------------------

// GuardTruncatedToolArgs fails a turn whose tool arguments never parse.
//
// Emitting a call with truncated JSON would have the client execute garbage or
// discard the turn silently; neither is acceptable.
func GuardTruncatedToolArgs(frames []Frame) ([]Frame, *ToolCallError) {
	buffers := map[string]*strings.Builder{}
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
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			calls, ok := delta["tool_calls"].([]any)
			if !ok {
				continue
			}
			for pos, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := call["function"].(map[string]any)
				if fn == nil {
					continue
				}
				args := asString(fn["arguments"])
				if args == "" {
					continue
				}
				key := toolCallKey(call, pos, len(calls))
				b, ok := buffers[key]
				if !ok {
					b = &strings.Builder{}
					buffers[key] = b
				}
				b.WriteString(args)
			}
		}
	}
	for _, b := range buffers {
		raw := b.String()
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var v any
		if json.Unmarshal([]byte(raw), &v) != nil {
			if fixed := RepairArgsJSON(raw); fixed != nil {
				continue
			}
			return frames, &ToolCallError{Type: ErrInvalidArgs,
				Kind: "truncated_arguments", Detail: DetailInvalidArgs, Chars: len(raw)}
		}
	}
	return frames, nil
}

// ---------------------------------------------------------------------------
// Guard: unstructured echo and empty tool turns
// ---------------------------------------------------------------------------

// namedToolEchoRe detects the "[Name 工具调用]" form the model writes as prose.
// The opening brace/array is captured rather than asserted: RE2 has no
// lookahead.
var namedToolEchoRe = regexpMustCompile(`(?m)^[ \t]*\[([A-Za-z_][A-Za-z0-9_.:\-]*)[ \t]+工具调用\]\s*[{\[]`)

// GuardUnstructuredToolEcho fails a turn where the model emitted only
// unrecoverable protocol scaffolding.
func GuardUnstructuredToolEcho(frames []Frame, allowed map[string]bool) ([]Frame, *ToolCallError) {
	if len(allowed) == 0 {
		return frames, nil
	}
	var text strings.Builder
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
			text.WriteString(asString(delta["content"]))
		}
	}
	body := OutsideFences(text.String())
	if strings.TrimSpace(body) == "" {
		return frames, nil
	}
	if !(LeakResiduePresent(body) || DSMLResiduePresent(body)) {
		return frames, nil
	}
	// A reply that merely names an undeclared tool is not a failed turn.
	if EchoNamesAllUndeclared(body, allowed) {
		return frames, nil
	}
	return frames, &ToolCallError{Type: ErrEmptyEcho,
		Kind: "unstructured_echo", Detail: DetailEmptyEcho, Chars: len(body)}
}

// GuardEmptyToolTurn fails a tool-declaring turn that produced neither text nor
// a call.
func GuardEmptyToolTurn(frames []Frame, allowed map[string]bool) ([]Frame, *ToolCallError) {
	if len(allowed) == 0 {
		return frames, nil
	}
	hasText, hasTool, hasReasoning := false, false, false
	for _, f := range frames {
		if f.Payload == nil {
			continue
		}
		choices, hasChoices := f.Payload["choices"].([]any)
		if !hasChoices {
			continue // a usage footer is not content
		}
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if s, ok := delta["content"].(string); ok && strings.TrimSpace(s) != "" {
				hasText = true
			}
			if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
				hasTool = true
			}
			if s, ok := delta["reasoning_content"].(string); ok && strings.TrimSpace(s) != "" {
				hasReasoning = true
			}
		}
	}
	switch {
	case hasTool, hasText:
		return frames, nil
	case hasReasoning:
		return frames, &ToolCallError{Type: ErrEmptyEcho,
			Kind: "reasoning_only", Detail: DetailEmptyEcho}
	}
	return frames, &ToolCallError{Type: ErrEmptyEcho,
		Kind: "empty_tool_turn", Detail: DetailEmptyEcho}
}

// ---------------------------------------------------------------------------
// Chain
// ---------------------------------------------------------------------------

// Process runs the full guard chain over one request's frames.
//
// The order is data-flow order and must not be changed: each stage consumes the
// previous stage's output. Every stage is a separate buffer so they never fight
// over the same bytes.
func Process(frames []Frame, allowed map[string]bool) ([]Frame, *ToolCallError) {
	frames = NormalizeToolCallIndices(frames)

	marker, err := RecoverMarkerEcho(frames, allowed)
	if err != nil {
		return marker, err
	}
	frames = marker

	frames, err = RecoverDSML(frames, allowed)
	if err != nil {
		return frames, err
	}
	frames, err = RecoverToolTags(frames, allowed)
	if err != nil {
		return frames, err
	}
	frames = FillToolCallIDs(frames)
	frames = GuardNamelessToolCalls(frames)

	frames, err = GuardTruncatedToolArgs(frames)
	if err != nil {
		return frames, err
	}
	frames, err = GuardUnstructuredToolEcho(frames, allowed)
	if err != nil {
		return frames, err
	}
	return GuardEmptyToolTurn(frames, allowed)
}

// ToolCallErrorType classifies a protocol-level failure.
type ToolCallErrorType int

const (
	// ErrNone means no error.
	ErrNone ToolCallErrorType = iota
	// ErrInvalidArgs means the echo is confirmed but its arguments are not
	// valid JSON (issue #37).
	ErrInvalidArgs
	// ErrEmptyEcho means the echo is truncated or a bare marker (issue #72).
	ErrEmptyEcho
)

// ToolCallError carries the fixed failure detail.
//
// The detail is a fixed string on purpose: it ends up in logs and in the client
// response, so no argument, path or content may leak through it.
type ToolCallError struct {
	Type   ToolCallErrorType
	Kind   string
	Detail string
	Chars  int
}

func (e *ToolCallError) Error() string { return e.Detail }

// Fixed detail strings (never interpolated with user data).
const (
	DetailInvalidArgs = "upstream tool-call echo contains invalid JSON arguments; no tools were emitted"
	DetailEmptyEcho   = "upstream returned only a truncated tool-call echo (no usable call); " +
		"the turn was failed on purpose so the client retries instead of silently treating the task as finished"
)

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func hasToolOrReasoningDelta(delta map[string]any) bool {
	if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
		return true
	}
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		return true
	}
	_, ok := delta["role"]
	return ok
}
