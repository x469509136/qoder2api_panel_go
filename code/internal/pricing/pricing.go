// Package pricing estimates what a request's tokens would cost on a public
// reference market.
//
// This is a reference figure only. It has nothing to do with the upstream's
// credits and must never be used for settlement. Models that are not in the
// table are not priced at all — the panel shows an em dash rather than a guess.
package pricing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Price is (input uncached, input cached, output) in USD per 1M tokens.
type Price [3]float64

// DefaultPrices covers the current official model keys.
var DefaultPrices = map[string]Price{
	"dfmodel":       {0.28, 0.028, 0.42},
	"dmodel":        {0.55, 0.14, 2.19},
	"gmodel":        {0.60, 0.15, 2.20},
	"gfmodel":       {0.06, 0.015, 0.40},
	"kmodel":        {0.60, 0.15, 2.50},
	"kmodel_latest": {0.60, 0.15, 2.50},
	"qmodel":        {0.40, 0.10, 1.20},
	"qmodel_latest": {1.20, 0.30, 6.00},
	"qmodel_38max":  {1.20, 0.30, 6.00},
	"qfmodel":       {0.05, 0.012, 0.20},
	"mmodel":        {0.30, 0.075, 1.20},
	"cmodel":        {3.00, 0.75, 15.00},
}

// Aliases maps display names onto the pricing keys.
var Aliases = map[string]string{
	"deepseek":          "dfmodel",
	"deepseek-flash":    "dfmodel",
	"deepseek-v4-flash": "dfmodel",
	"deepseek-v4-pro":   "dmodel",
	"glm":               "gmodel",
	"glm-5.3":           "gmodel",
	"glm-5.3-flash":     "gfmodel",
	"kimi":              "kmodel",
	"kimi-k2.8-preview": "kmodel",
	"kimi-k3":           "kmodel_latest",
	"qwen3.7-plus":      "qmodel",
	"qwen3.7-max":       "qmodel_latest",
	"qwen3.8-max":       "qmodel_38max",
	"qwen3.8-flash":     "qfmodel",
	"minimax-m3":        "mmodel",
	"cantus":            "cmodel",
}

// LoadOverrides reads usage/pricing-overrides.json.
func LoadOverrides(usageDir string) map[string]Price {
	out := map[string]Price{}
	raw, err := os.ReadFile(filepath.Join(usageDir, "pricing-overrides.json"))
	if err != nil {
		return out
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return out
	}
	for key, val := range doc {
		name := normalize(key)
		switch v := val.(type) {
		case map[string]any:
			out[name] = Price{num(v["in"]), num(v["cached"]), num(v["out"])}
		case []any:
			if len(v) == 3 {
				out[name] = Price{num(v[0]), num(v[1]), num(v[2])}
			}
		}
	}
	return out
}

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		var f float64
		if _, err := fmtSscanf(t, "%g", &f); err == nil {
			return f
		}
	}
	return 0
}

func normalize(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// PriceFor returns the reference price for a model, or ok=false when unknown.
func PriceFor(name string, overrides map[string]Price) (Price, bool) {
	key := normalize(name)
	if key == "" {
		return Price{}, false
	}
	if p, ok := overrides[key]; ok {
		return p, true
	}
	if p, ok := DefaultPrices[key]; ok {
		return p, true
	}
	if target, ok := Aliases[key]; ok {
		if p, ok := DefaultPrices[target]; ok {
			return p, true
		}
	}
	return Price{}, false
}

// CostOf computes the reference cost in USD.
func CostOf(name string, prompt, cached, completion int, overrides map[string]Price) (float64, bool) {
	p, ok := PriceFor(name, overrides)
	if !ok {
		return 0, false
	}
	if cached > prompt {
		cached = prompt
	}
	fresh := prompt - cached
	cost := (float64(fresh)*p[0] + float64(cached)*p[1] + float64(completion)*p[2]) / 1e6
	return cost, true
}

// Table renders the whole price table plus the unpriced model list.
func Table(usageDir string, knownModels []string) map[string]any {
	overrides := LoadOverrides(usageDir)
	rows := map[string]any{}
	unpriced := []string{}
	for _, name := range knownModels {
		if p, ok := PriceFor(name, overrides); ok {
			rows[name] = map[string]any{"in": p[0], "cached": p[1], "out": p[2]}
			continue
		}
		unpriced = append(unpriced, name)
	}
	return map[string]any{
		"unit": "USD per 1M tokens", "prices": rows,
		"unpriced": unpriced, "overrides_loaded": len(overrides),
		"note": "reference only — not a settlement figure",
	}
}
