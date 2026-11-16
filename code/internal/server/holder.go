package server

import (
	"github.com/xzs/bridge_qoder/internal/protocol"
	"github.com/xzs/bridge_qoder/internal/upstream"
)

// holderUsage decodes the usage block captured from the stream.
func holderUsage(h *upstream.Holder) *protocol.Usage {
	if h == nil {
		return nil
	}
	return protocol.DecodeUsage(h.Usage)
}

func holderPrompt(h *upstream.Holder) int {
	if u := holderUsage(h); u != nil {
		return u.PromptTokens
	}
	return 0
}

func holderCompletion(h *upstream.Holder) int {
	if u := holderUsage(h); u != nil {
		return u.CompletionTokens
	}
	return 0
}

func holderCached(h *upstream.Holder) int {
	if u := holderUsage(h); u != nil {
		return u.EffectiveCached()
	}
	return 0
}

func holderCachedReported(h *upstream.Holder) bool {
	if u := holderUsage(h); u != nil {
		return u.CachedReported()
	}
	return false
}

func holderTotal(h *upstream.Holder) int {
	if u := holderUsage(h); u != nil {
		return u.TotalTokens
	}
	return 0
}

func holderCredit(h *upstream.Holder) float64 {
	if u := holderUsage(h); u != nil {
		return u.Credits
	}
	return 0
}

func holderReasoning(h *upstream.Holder) int {
	if u := holderUsage(h); u != nil {
		return u.ReasoningTokens()
	}
	return 0
}
