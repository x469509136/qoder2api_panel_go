package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/tasks"
)

// handleAccounts dispatches the account management surface.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodGet {
		switch {
		case path == "/accounts":
			s.listAccounts(w, r)
			return
		case path == "/accounts/export":
			// Refuse before building the document. A caller that asked for
			// secrets=1 and got a token-free file would reasonably conclude
			// the export is broken; say why instead (same rule as
			// /settings/reveal).
			if r.URL.Query().Get("secrets") == "1" && s.settings.PanelPasswordIsDefault() {
				s.writeError(w, http.StatusForbidden,
					"change the default panel password before exporting tokens",
					"invalid_request_error")
				return
			}
			s.writeJSON(w, http.StatusOK, s.exportAccounts(r))
			return
		case path == "/accounts/credits/usage":
			s.handleCreditsUsage(w, r)
			return
		case path == "/accounts/credits/grants":
			s.handleCreditsGrants(w, r)
			return
		case path == "/accounts/activity":
			s.handleActivity(w, r)
			return
		case path == "/accounts/credits":
			s.handleCreditsRefresh(w, r)
			return
		case path == "/accounts/login/poll":
			s.writeJSON(w, http.StatusOK, s.pool.PollLogin(r.URL.Query().Get("state")))
			return
		}
		s.writeError(w, http.StatusNotFound, "unknown accounts path: "+path, "invalid_request_error")
		return
	}

	payload, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	switch path {
	case "/accounts/login/start":
		realm := firstNonEmptyStr(normalizeRealm(asStringAny(payload["realm"])), s.currentRealm())
		result, err := s.pool.StartLogin(realm, asStringAny(payload["platform"]), nil)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		s.writeJSON(w, http.StatusOK, result)
	case "/accounts/login/cancel":
		okCancel := accounts.CancelLogin(asStringAny(payload["state"]))
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": okCancel})
	case "/accounts/import/pat", "/accounts/import-pat":
		s.importPAT(w, r, payload)
	case "/accounts/import/desktop":
		s.importDesktop(w, r, payload)
	case "/accounts/refresh":
		s.refreshAccounts(w, r, payload)
	case "/accounts/test":
		s.testAccount(w, r, payload)
	case "/accounts/set":
		s.setAccount(w, r, payload)
	case "/accounts/set-all":
		s.setAllAccounts(w, r, payload)
	case "/accounts/delete":
		s.deleteAccount(w, r, payload)
	case "/accounts/note":
		s.setAccountNote(w, r, payload)
	case "/accounts/checkin":
		s.checkinAccounts(w, r, payload)
	case "/accounts/checkin-when-disabled":
		s.setCheckinWhenDisabled(w, r, payload)
	case "/accounts/import":
		s.importAccounts(w, r)
	case "/accounts/credits", "/accounts/credits/fetch":
		s.handleCreditsRefresh(w, r)
	default:
		s.writeError(w, http.StatusNotFound, "unknown accounts path: "+path, "invalid_request_error")
	}
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

// exportAccounts renders the portable JSON export document.
//
// Tokens are included only for an explicit ?secrets=1: the export is a
// migration/backup format, so the panel's export buttons ask for them, but a
// bare GET must not hand plaintext credentials to anyone holding a panel
// session (with the default password that used to mean the whole subnet).
//
// The default-password gate matters as much as the query flag. A fresh
// install accepts "admin" — a value printed in the README — so ?secrets=1
// behind that password is a published credential dump: the throttle only
// slows the guess down, it does not make the password secret. /settings/reveal
// already refuses in that state; the export must agree, otherwise the weaker
// of the two defines the real boundary.
// Layout matches build_export_document() in the Python gateway so the two
// versions can import each other's files.
func (s *Server) exportAccounts(r *http.Request) map[string]any {
	realm := normalizeRealm(r.URL.Query().Get("realm"))
	includeSecrets := r.URL.Query().Get("secrets") == "1"
	wanted := map[string]bool{}
	for _, raw := range r.URL.Query()["uid"] {
		for _, part := range strings.Split(raw, ",") {
			if uid := strings.TrimSpace(part); uid != "" {
				wanted[uid] = true
			}
		}
	}
	rows := make([]map[string]any, 0)
	for _, acc := range s.pool.Accounts() {
		if realm != "" && acc.Realm != realm {
			continue
		}
		if len(wanted) > 0 && !wanted[acc.UID] {
			continue
		}
		row := acc.ToMap()
		if !includeSecrets {
			delete(row, "accessToken")
			delete(row, "refreshToken")
			delete(row, "personalToken")
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"format":     "qoder-accounts",
		"version":    1,
		"exportedAt": time.Now().Format("2006-01-02 15:04:05"),
		"count":      len(rows),
		"accounts":   rows,
	}
}

// listAccounts returns the account table, sorted so usable accounts come first.
func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	realm := normalizeRealm(r.URL.Query().Get("realm"))
	group := r.URL.Query().Get("group")
	rows := s.pool.ListPublic(realm, group)
	sortAccounts(rows, r.URL.Query().Get("sort"))
	s.writeJSON(w, http.StatusOK, map[string]any{
		"accounts": rows, "total": len(rows), "realm": firstNonEmptyStr(realm, "all"),
	})
}

// sortAccounts orders the account table.
//
// The default puts usable accounts first (enabled, no cooldown, has a token),
// then by remaining credits descending: an operator looking at the table wants
// the accounts that can serve right now at the top.
func sortAccounts(rows []map[string]any, mode string) {
	switch mode {
	case "uid", "name", "realm", "credits", "added":
		// Explicit sort keys are still stable and deterministic.
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			switch mode {
			case "name":
				return asStringAny(a["nickname"]) < asStringAny(b["nickname"])
			case "realm":
				return asStringAny(a["realm"]) < asStringAny(b["realm"])
			case "credits":
				return intAny(a["credits_remain"]) > intAny(b["credits_remain"])
			case "added":
				return intAny(a["addedAt"]) < intAny(b["addedAt"])
			}
			return asStringAny(a["uid"]) < asStringAny(b["uid"])
		})
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ai, aj := usableRank(rows[i]), usableRank(rows[j])
		if ai != aj {
			return ai > aj
		}
		return intAny(rows[i]["credits_remain"]) > intAny(rows[j]["credits_remain"])
	})
}

func usableRank(row map[string]any) int {
	score := 0
	if row["enabled"] == true {
		score += 2
	}
	if asFloatAny(row["cooldown_left_s"]) <= 0 {
		score++
	}
	if intAny(row["credits_remain"]) > 0 {
		score++
	}
	return score
}

// ---------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------

func (s *Server) importPAT(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	pat := asStringAny(payload["pat"])
	realm := firstNonEmptyStr(normalizeRealm(asStringAny(payload["realm"])), s.currentRealm())
	acc, err := s.pool.ImportPAT(pat, realm, nil)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	s.applyPoolSettings()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Public()})
}

// importDesktop imports credentials already present on this machine.
//
// An empty body only scans; nothing is imported without an explicit choice, so a
// shared machine never silently adopts someone else's login.
func (s *Server) importDesktop(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	scanned := accounts.ScanDesktopCredentials()
	if path := asStringAny(payload["path"]); path == "" && payload["all"] != true {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "imported": 0, "scanned": scanned,
			"note": "扫描完成，未导入任何账号；传 {\"all\":true} 或指定 path 才会导入",
		})
		return
	}
	var targets []accounts.ScannedCredential
	for _, item := range scanned {
		if !item.Valid {
			continue
		}
		if payload["all"] == true || item.Path == asStringAny(payload["path"]) {
			targets = append(targets, item)
		}
	}
	imported := 0
	for _, item := range targets {
		// The scan hit carries a bare qoder.Credential; AccountFromScan adds
		// the pool-side state fields (AddedAt, Enabled, ...).
		if acc, err := accounts.AccountFromScan(&item); err == nil {
			if err := s.pool.Add(acc); err == nil {
				imported++
			}
		}
	}
	s.applyPoolSettings()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "imported": imported, "scanned": scanned,
	})
}

// importAccounts upserts a JSON export; ?dry_run=1 previews the outcome.
func (s *Server) importAccounts(w http.ResponseWriter, r *http.Request) {
	body, err := s.readPayload(r, true)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	list, ok := body.([]any)
	if !ok {
		if obj, ok := body.(map[string]any); ok {
			if arr, ok := obj["accounts"].([]any); ok {
				list = arr
			}
		}
	}
	if list == nil {
		s.writeError(w, http.StatusBadRequest, "expected a JSON array of accounts", "invalid_request_error")
		return
	}
	rows := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	realm := normalizeRealm(r.URL.Query().Get("realm"))
	group := r.URL.Query().Get("group")
	overwrite := r.URL.Query().Get("overwrite") == "1"
	if r.URL.Query().Get("dry_run") == "1" {
		added, updated, skipped := 0, 0, 0
		for _, row := range rows {
			uid := asStringAny(row["uid"])
			if uid == "" || asStringAny(row["accessToken"]) == "" {
				skipped++
				continue
			}
			if s.pool.Get(uid) != nil {
				updated++
			} else {
				added++
			}
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"dry_run": true, "added": added, "updated": updated, "skipped": skipped,
		})
		return
	}
	added, updated, skipped, errs := s.pool.ImportRows(rows, realm, group, overwrite)
	s.applyPoolSettings()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": len(errs) == 0, "added": added, "updated": updated,
		"skipped": skipped, "errors": errs,
	})
}

// ---------------------------------------------------------------------------
// Maintenance
// ---------------------------------------------------------------------------

func (s *Server) refreshAccounts(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	targets := s.resolveAccountTargets(payload)
	refreshed, failed := 0, 0
	var errs []string
	for _, acc := range targets {
		if err := acc.Refresh(); err != nil {
			failed++
			errs = append(errs, shortUID(acc.UID)+": "+truncateStr(err.Error(), 120))
			continue
		}
		refreshed++
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": failed == 0, "refreshed": refreshed, "failed": failed, "errors": errs,
	})
}

// testAccount probes credentials with a free model.
//
// The panel test exists to prove the credential and channel work; a paid model
// would fail on a zero-credit account with a 403 that looks like a broken key.
func (s *Server) testAccount(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	targets := s.resolveAccountTargets(payload)
	if len(targets) == 0 {
		s.writeError(w, http.StatusBadRequest, "no account selected", "invalid_request_error")
		return
	}
	model := asStringAny(payload["model"])
	if model == "" {
		model = s.defaultTestModel(targets[0].Realm)
	}
	acc := targets[0]
	testPayload := map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"stream":     false,
		"max_tokens": 8,
	}
	body, err := s.buildBody(testPayload, acc, s.resolveModelKey(model, acc.Realm), acc.Realm, "")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	cfg := acc.RealmConfig()
	resp, postErr := s.upstream.Post(r.Context(), upstreamRequest(body, acc, s.sessions,
		cfg.Gateway+upstreamChatPath))
	if postErr != nil {
		msg, _ := friendlyUpstreamError(upstreamStatus(postErr), errText(postErr))
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "uid": acc.UID, "nickname": acc.Nickname,
			"model": model, "error": msg,
		})
		return
	}
	defer resp.Close()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": acc.UID, "nickname": acc.Nickname, "model": model,
	})
}

// defaultTestModel prefers a zero-price model for the connectivity probe.
func (s *Server) defaultTestModel(realm string) string {
	var freeTier, isFree string
	for _, raw := range s.fetchModels(realm) {
		info := catalogInfo(raw)
		name := firstNonEmptyStr(info.DisplayName, info.Key)
		if name == "" {
			continue
		}
		if info.PriceFactor == 0 {
			freeTier = name
			break
		}
		if info.IsFree && isFree == "" {
			isFree = name
		}
	}
	if freeTier != "" {
		return freeTier
	}
	if isFree != "" {
		return isFree
	}
	return "auto"
}

func (s *Server) setAccount(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	uid := asStringAny(payload["uid"])
	acc := s.pool.Get(uid)
	if acc == nil {
		s.writeError(w, http.StatusNotFound, "account not found: "+uid, "invalid_request_error")
		return
	}
	if v, ok := payload["enabled"].(bool); ok {
		if err := s.pool.SetEnabled(uid, v); err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
	}
	if vpc, ok := payload["vpcEndpoint"]; ok {
		if err := s.pool.SetVPC(uid, vpc); err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
	}
	if group, ok := payload["group"].(string); ok {
		acc.SetGroup(group)
		_ = acc.Save(s.cfg.AccountsDir)
	}
	if note, ok := payload["note"].(string); ok {
		acc.SetNote(note)
		_ = acc.Save(s.cfg.AccountsDir)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Public()})
}

func (s *Server) setAllAccounts(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	enabled, _ := payload["enabled"].(bool)
	realm := normalizeRealm(asStringAny(payload["realm"]))
	n := s.pool.SetAllEnabled(enabled, realm)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": n})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	uid := asStringAny(payload["uid"])
	if !s.pool.Remove(uid) {
		s.writeError(w, http.StatusNotFound, "account not found: "+uid, "invalid_request_error")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) setAccountNote(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	uid := strings.TrimSpace(asStringAny(payload["uid"]))
	if uid == "" {
		s.writeError(w, http.StatusBadRequest, "uid is required", "invalid_request_error")
		return
	}
	acc := s.pool.Get(uid)
	if acc == nil {
		s.writeError(w, http.StatusNotFound, "account not found: "+uid, "invalid_request_error")
		return
	}
	acc.SetNote(strings.TrimSpace(asStringAny(payload["note"])))
	if err := acc.Save(s.cfg.AccountsDir); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) setCheckinWhenDisabled(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	targets := s.resolveAccountTargets(payload)
	on, _ := payload["enabled"].(bool)
	n := 0
	for _, acc := range targets {
		acc.SetCheckinWhenDisabled(on)
		if err := acc.Save(s.cfg.AccountsDir); err == nil {
			n++
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": n})
}

// checkinAccounts runs the daily check-in for one or all accounts.
func (s *Server) checkinAccounts(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	targets := s.resolveAccountTargets(payload)
	res := tasks.RunBatchCheckin(targets, 1.0, 1.0, s.cfg.AccountsDir, s.cfg.UsageDir,
		func(level, msg string) { s.log.Info("tasks", "%s", msg) })
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accounts": res.AccountsCount, "credit_added": res.CreditAdded,
		"failed": res.Failed, "logs": res.Logs,
	})
}

// resolveAccountTargets expands a selector into accounts.
//
// An empty selector means every account; a uid list narrows it; a realm narrows
// it further.
func (s *Server) resolveAccountTargets(payload map[string]any) []*accounts.Account {
	var uids map[string]bool
	if list, ok := payload["uids"].([]any); ok && len(list) > 0 {
		uids = map[string]bool{}
		for _, item := range list {
			uids[asStringAny(item)] = true
		}
	} else if uid := asStringAny(payload["uid"]); uid != "" {
		uids = map[string]bool{uid: true}
	}
	realm := normalizeRealm(asStringAny(payload["realm"]))
	out := []*accounts.Account{}
	for _, acc := range s.pool.Accounts() {
		if uids != nil && !uids[acc.UID] {
			continue
		}
		if realm != "" && acc.Realm != realm {
			continue
		}
		out = append(out, acc)
	}
	return out
}

func (s *Server) handleCreditsRefresh(w http.ResponseWriter, r *http.Request) {
	targets := s.resolveAccountTargets(map[string]any{
		"uid":   r.URL.Query().Get("uid"),
		"realm": r.URL.Query().Get("realm"),
	})
	rows := []map[string]any{}
	for _, acc := range targets {
		res := acc.FetchCredits(s.cfg.AccountsDir)
		row := map[string]any{"uid": acc.UID, "nickname": acc.Nickname,
			"realm": acc.Realm, "ok": res.OK}
		if res.Credits != nil {
			row["credits"] = res.Credits
		}
		if res.Error != "" {
			row["error"] = res.Error
		}
		rows = append(rows, row)
		tasks.InvalidatePanelCache(acc.UID)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"accounts": rows})
}
