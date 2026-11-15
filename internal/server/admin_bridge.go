package server

import (
	"context"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/catalog"
	"github.com/xzs/bridge_qoder/internal/sign"
	"github.com/xzs/bridge_qoder/internal/upstream"
)

const upstreamChatPath = upstream.ChatPath

// upstreamRequest builds a signed request descriptor.
func upstreamRequest(body map[string]any, acc *accounts.Account,
	sessions *sign.SessionCache, rawURL string) upstream.Request {
	return upstream.Request{
		Body: body, Account: acc, ModelKey: "", RawURL: rawURL,
		Sessions: sessions, Timeout: 120 * time.Second,
	}
}

// catalogInfo decodes one raw catalog row.
func catalogInfo(raw map[string]any) catalog.Model { return catalog.ModelInfo(raw) }

// upstreamHolderAlias exists so test handlers can compile against the holder
// type without importing the upstream package everywhere.
type upstreamHolderAlias struct{ Usage any }

// asFloatAny coerces a decoded JSON number.
func asFloatAny(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		var f float64
		if _, err := fmtSscanfFloat(t, &f); err == nil {
			return f
		}
	}
	return 0
}

func ctxBackground() context.Context { return context.Background() }
