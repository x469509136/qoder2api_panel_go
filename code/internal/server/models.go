package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/catalog"
	"github.com/xzs/bridge_qoder/internal/upstream"
)

// modelsMu guards the in-memory catalog cache.
var modelsMu sync.Mutex

type modelsCacheEntry struct {
	at   float64
	rows []map[string]any
}

var modelsCache = map[string]modelsCacheEntry{}

const modelsTTL = 300.0

// fetchModels merges the live endpoint, the on-disk cache and the snapshot.
func (s *Server) fetchModels(realm string) []map[string]any {
	modelsMu.Lock()
	if hit, ok := modelsCache[realm]; ok && time.Since(time.Unix(int64(hit.at), 0)).Seconds() < modelsTTL {
		modelsMu.Unlock()
		return hit.rows
	}
	modelsMu.Unlock()

	live := s.readDynamicModels(realm)
	if len(live) == 0 {
		live = s.loadModelsDiskCache(realm)
	}
	var rows []map[string]any
	if len(live) > 0 {
		rows = live
	} else {
		rows = s.catalog.ModelsForRealm(realm)
	}
	// Register display names so a renamed model still resolves while the snapshot
	// has not caught up.
	var pairs [][2]string
	for _, row := range rows {
		name := asStringField(row, "display_name")
		key := asStringField(row, "key")
		if name != "" && key != "" {
			pairs = append(pairs, [2]string{name, key})
		}
	}
	s.catalog.RegisterLiveDisplayNames(pairs)

	modelsMu.Lock()
	modelsCache[realm] = modelsCacheEntry{at: float64(time.Now().Unix()), rows: rows}
	modelsMu.Unlock()
	return rows
}

// readDynamicModels fetches /algo/api/v2/model/list with a COSY signature.
//
// The signature covers the body, so the request must send the same encoded body
// it signed; a bare GET is rejected with 403.
func (s *Server) readDynamicModels(realm string) []map[string]any {
	acc := s.pool.RepresentativeMatching(realm)
	if acc == nil {
		return nil
	}
	signBody := signEncodeEmptyObject()
	cfg := acc.RealmConfig()
	for _, host := range accounts.GatewayCandidates(realm, cfg) {
		rawURL := host + upstream.ModelsPath
		payload := s.getJSONWithCOSY(acc, signBody, rawURL)
		if payload == nil {
			continue
		}
		rows := catalog.DialogModels(payload)
		out := make([]map[string]any, 0, len(rows))
		for _, m := range rows {
			key := asStringField(m, "key")
			if key == "" {
				continue
			}
			row := make(map[string]any, len(m)+3)
			for k, v := range m {
				row[k] = v
			}
			row["id"] = key
			if _, ok := row["name"]; !ok {
				row["name"] = firstNonEmptyStr(asStringField(m, "display_name"), key)
			}
			if _, ok := row["display_name"]; !ok {
				row["display_name"] = row["name"]
			}
			out = append(out, row)
		}
		if len(out) > 0 {
			s.saveModelsDiskCache(realm, out)
			s.log.Info("models", "model discovery ok: %d chat models from %s", len(out), realm)
			return out
		}
	}
	return nil
}

func (s *Server) getJSONWithCOSY(acc *accounts.Account, body, rawURL string) map[string]any {
	sess, err := s.sessions.Get(acc)
	if err != nil {
		return nil
	}
	headers := sess.Headers(body, rawURL, "", false, "application/json")
	req := accounts.Request{
		Method: http.MethodGet, URL: rawURL, Headers: headers,
		Body: []byte(body), Timeout: 15 * time.Second,
	}
	data, err := accounts.DefaultClient.Do(req)
	if err != nil {
		s.log.Warn("models", "model discovery failed on %s: %v", rawURL, err)
		return nil
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func (s *Server) modelsDiskPath(realm string) string {
	return s.cfg.AccountsDir + "/models-cache-" + realm + ".json"
}

func (s *Server) saveModelsDiskCache(realm string, rows []map[string]any) {
	blob, err := json.Marshal(rows)
	if err != nil {
		return
	}
	_ = writeFileAtomic(s.modelsDiskPath(realm), blob)
}

func (s *Server) loadModelsDiskCache(realm string) []map[string]any {
	raw, err := readFile(s.modelsDiskPath(realm))
	if err != nil {
		return nil
	}
	var doc struct {
		At   float64          `json:"at"`
		Rows []map[string]any `json:"rows"`
	}
	if json.Unmarshal(raw, &doc) == nil && len(doc.Rows) > 0 {
		if doc.At == 0 || time.Since(time.Unix(int64(doc.At), 0)) < 7*24*time.Hour {
			return doc.Rows
		}
	}
	var rows []map[string]any
	if json.Unmarshal(raw, &rows) == nil && len(rows) > 0 {
		return rows
	}
	return nil
}

// resolveModelKey maps a client model name onto an upstream key for a realm.
func (s *Server) resolveModelKey(model, realm string) string {
	if realm == "" {
		realm = s.currentRealm()
	}
	key := s.catalog.ResolveUpstreamKey(model, realm)
	if s.catalog.IsKnownUpstreamKey(key, realm) {
		return key
	}
	// The snapshot may lag behind a rename (Qwen3.7-Max became
	// Qwen4-Max-Preview). The live catalog knows the new display name, but the
	// registry only learns it when the model list is fetched — and a request can
	// arrive first. Fetching here costs a cached call (5 min TTL) and lets an
	// otherwise-unresolvable name still route to its real upstream key instead of
	// being passed through and silently billed as "auto".
	s.fetchModels(realm)
	if again := s.catalog.ResolveUpstreamKey(model, realm); s.catalog.IsKnownUpstreamKey(again, realm) {
		return again
	}
	return key
}

// displayNameFor returns the official display name of an upstream key.
func (s *Server) displayNameFor(modelKey, realm string) string {
	for _, raw := range s.fetchModels(realm) {
		if asStringField(raw, "key") == modelKey {
			if name := asStringField(raw, "display_name"); name != "" {
				return name
			}
			return modelKey
		}
	}
	return modelKey
}

// catalogMetaFor returns the decoded catalog entry for a key.
func (s *Server) catalogMetaFor(modelKey, realm string) catalog.Model {
	for _, raw := range s.fetchModels(realm) {
		if asStringField(raw, "key") == modelKey {
			return catalog.ModelInfo(raw)
		}
	}
	return catalog.Model{Key: modelKey}
}

// ---------------------------------------------------------------------------
// /v1/models
// ---------------------------------------------------------------------------

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	realm := s.requestRealm(r)
	merge := r.URL.Query().Get("merge_realms") == "1"
	type row struct {
		key, realm string
		meta       map[string]any
	}
	var rows []row
	if merge {
		cur := realm
		if cur == "" {
			cur = s.currentRealm()
		}
		seen := map[string]bool{}
		for _, rlm := range []string{cur, otherRealm(cur)} {
			for _, m := range s.fetchModels(rlm) {
				key := asStringField(m, "key")
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
				rows = append(rows, row{key, rlm, m})
			}
		}
	} else {
		if realm == "" {
			realm = s.currentRealm()
		}
		for _, m := range s.fetchModels(realm) {
			rows = append(rows, row{asStringField(m, "key"), realm, m})
		}
	}
	out := make([]map[string]any, 0, len(rows))
	for _, item := range rows {
		out = append(out, s.modelEntry(item.key, item.meta, item.realm))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "data": out,
		"catalog_source": "live-or-snapshot", "realm": realm,
	})
}

// modelEntry builds a rich /v1/models entry.
//
// Nothing is invented: the official catalog has no max output token field, so it
// only appears when a learned limit exists.
func (s *Server) modelEntry(mid string, meta map[string]any, realm string) map[string]any {
	info := catalog.ModelInfo(meta)
	displayName := firstNonEmptyStr(info.DisplayName, mid)
	item := map[string]any{
		"id": displayName, "object": "model", "created": time.Now().Unix(),
		"owned_by": "qoder", "upstream_key": mid, "name": displayName,
		"enabled": info.Enable == nil || *info.Enable,
	}
	if realm != "" {
		item["realm"] = realm
		if span := s.catalog.KeyRealmSpan(mid); len(span) == 1 {
			item["realm_exclusive"] = span[0]
		}
	}
	if desc := firstNonEmptyStr(asStringField(meta, "description"),
		catalog.OfficialDescription(mid)); desc != "" {
		item["description"] = desc
	}
	if local := catalog.OfficialLabel(mid); local != "" && local != displayName {
		item["name_local"] = local
	}
	aliases := []string{mid}
	if displayName != mid {
		aliases = append(aliases, fmt.Sprintf("%s (%s)", mid, displayName))
	}
	for alias, target := range catalog.Aliases {
		if target == mid && !containsStr(aliases, alias) {
			aliases = append(aliases, alias)
		}
	}
	item["aliases"] = aliases
	if !item["enabled"].(bool) {
		item["disabled_reason"] = catalog.OfficialDisabledReason
		if len(info.Strategies) > 0 {
			for _, st := range info.Strategies {
				if key := asStringField(st, "disabled_message_key"); key != "" {
					item["disabled_message_key"] = key
					break
				}
			}
			item["strategies"] = info.Strategies
		}
	}
	vision := info.IsVL
	tools := true
	thinks := info.IsReasoning || (info.ThinkingConfig != nil && info.ThinkingConfig.Enabled != nil)
	inputs := []string{"text"}
	if vision {
		inputs = append(inputs, "image")
	}
	caps := map[string]any{"vision": vision, "tool_calls": tools, "reasoning": thinks}
	item["capabilities"] = caps
	item["supports_vision"] = vision
	item["supports_images"] = vision
	item["supports_tool_calls"] = tools
	item["supports_reasoning"] = thinks
	item["vision"] = vision
	item["multimodal"] = vision
	item["abilities"] = map[string]any{"vision": vision, "functionCall": tools,
		"function_call": tools, "reasoning": thinks}
	item["input_modalities"] = inputs
	item["output_modalities"] = []string{"text"}
	item["modalities"] = map[string]any{"input": inputs, "output": []string{"text"}}
	item["architecture"] = map[string]any{
		"input_modalities": inputs, "output_modalities": []string{"text"},
		"modality": strings.Join(inputs, "+") + "->text",
	}

	if info.MaxInputTokens > 0 {
		item["context_length"] = info.MaxInputTokens
		item["max_input_tokens"] = info.MaxInputTokens
	}
	if len(info.ContextConfig) > 0 {
		var windows []int64
		var labels []string
		defaultLabel := ""
		for _, label := range catalog.SortedKeys(info.ContextConfig) {
			conf := info.ContextConfig[label]
			if conf.TokenCount == 0 {
				continue
			}
			labels = append(labels, label)
			windows = append(windows, conf.TokenCount)
			if conf.IsDefault {
				defaultLabel = label
			}
		}
		if defaultLabel == "" && len(labels) > 0 {
			defaultLabel = labels[0]
		}
		if len(windows) > 0 {
			item["context_windows"] = windows
			item["context_window_labels"] = labels
			item["context_window_default"] = defaultLabel
		}
	}
	if limit := s.maxOutputLimit(mid, realm); limit > 0 {
		item["max_output_tokens"] = limit
		item["max_completion_tokens"] = limit
	}
	if efforts := catalog.SupportedEfforts(info); len(efforts) > 0 {
		item["reasoning_efforts"] = efforts
		if def := catalog.DefaultEffort(info); def != "" {
			item["reasoning_default_effort"] = def
		}
	}
	if info.ThinkingConfig != nil {
		if _, ok := meta["thinking_config"].(map[string]any)["disabled"]; ok {
			item["reasoning_can_disable"] = true
		}
	}

	// Pricing: the valley factor is derived from the peak times the discount.
	if info.PriceFactor != 0 || meta["price_factor"] != nil {
		item["price_factor"] = info.PriceFactor
	}
	if info.OriginalPrice != 0 {
		item["original_price_factor"] = info.OriginalPrice
		item["price_factor_peak"] = info.OriginalPrice
	}
	if promo := info.Promotion; promo != nil {
		peak := promo.BeforePromotionPrice
		if peak == 0 {
			peak = info.OriginalPrice
		}
		valley := info.PriceFactor
		if peak > 0 && promo.DiscountFactor > 0 && promo.DiscountFactor <= 1 {
			valley = roundTo(math.Round(peak*promo.DiscountFactor*1e4)/1e4, 4)
		}
		item["price_factor_valley"] = valley
		item["off_peak"] = map[string]any{
			"active": promo.Active, "window_start": promo.WindowStart,
			"window_end": promo.WindowEnd, "timezone": promo.Timezone,
			"discount_factor": promo.DiscountFactor,
			"badge":           firstNonEmptyStr(promo.Badge["zh"], promo.Badge["en"]),
			"badge_en":        promo.Badge["en"],
			"description":     firstNonEmptyStr(promo.Description["zh"], promo.Description["en"]),
			"description_en":  promo.Description["en"],
			"link":            firstNonEmptyStr(promo.LinkURL["zh"], promo.LinkURL["en"]),
		}
		item["off_peak_window"] = promo.WindowStart + "-" + promo.WindowEnd
		item["promotion"] = meta["promotion"]
		item["off_peak_active_now"] = catalog.OffPeakActiveNow(
			promo.WindowStart, promo.WindowEnd, promo.Timezone, localMinutes())
	}
	item["is_free"] = info.IsFree
	if info.IsNew {
		item["is_new"] = true
	}
	if info.Icon != "" {
		item["icon"] = info.Icon
	}
	return item
}

func (s *Server) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	realm := s.requestRealm(r)
	if realm == "" {
		realm = s.currentRealm()
	}
	modelsMu.Lock()
	delete(modelsCache, realm)
	modelsMu.Unlock()
	rows := s.fetchModels(realm)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "realm": realm, "count": len(rows),
	})
}

// maxOutputLimit returns a learned output cap (0 = unknown).
//
// The official catalog has no such field; the gateway learns it from the first
// 400 "Range of max_tokens should be [1, N]" the upstream returns.
func (s *Server) maxOutputLimit(modelKey, realm string) int {
	limits := s.loadMaxOutLimits()
	key := modelKey + "|" + realm
	if v, ok := limits[key]; ok && time.Since(time.Unix(int64(v.At), 0)) < 7*24*time.Hour {
		return v.Limit
	}
	return 0
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func signEncodeEmptyObject() string { return signQoderEncode([]byte("{}")) }

func asStringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func otherRealm(realm string) string {
	if realm == accounts.RealmINTL {
		return accounts.RealmCN
	}
	return accounts.RealmINTL
}

func localMinutes() int {
	now := time.Now()
	return now.Hour()*60 + now.Minute()
}

func roundTo(v float64, digits int) float64 {
	pow := math.Pow(10, float64(digits))
	return math.Round(v*pow) / pow
}

func containsStr(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
