package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/route"
	"github.com/xzs/bridge_qoder/internal/upstream"
)

// retryAction tells the caller what to do with a failed attempt.
type retryAction int

const (
	// actionFail returns the error to the client.
	actionFail retryAction = iota
	// actionRetrySame retries the same account (transient fault or queue wait).
	actionRetrySame
	// actionNextAccount rotates to another account.
	actionNextAccount
)

// openUpstreamFor opens one signed upstream call, applying routing, realm
// detection, account selection and the retry policy.
func (s *Server) openUpstreamFor(r *http.Request, payload map[string]any,
	sessionKey string) (*upstream.Response, *accounts.Account, error) {
	model := firstNonEmptyStr(asStringAny(payload["model"]), "auto")
	entry := s.keyEntry(r)
	if entry != nil {
		payload["_qd_key_id"] = entry.ID
	}
	reqRealm := s.requestRealm(r)

	likes := s.pool.AccountLikes()
	routeLikes := make([]route.AccountLike, 0, len(likes))
	for _, item := range likes {
		routeLikes = append(routeLikes, item)
	}
	decision, err := s.route.Resolve(routeLikes, model, entry, reqRealm)
	if err != nil {
		return nil, nil, &upstream.StatusError{Code: 503,
			Detail: "routing configuration error: " + err.Error()}
	}
	allowed := decision.AllowedUIDs
	if allowed != nil && len(allowed) == 0 {
		return nil, nil, &upstream.StatusError{Code: 403,
			Detail: "API Key / routing account scope has no eligible account"}
	}

	realm := decision.Realm
	realmReason := ""
	if realm == "" {
		realm = s.detectModelRealm(model)
	}
	if owner := s.catalog.ExclusiveRealm(model); owner != "" && realm != owner {
		return nil, nil, &upstream.StatusError{Code: 400,
			Detail: fmt.Sprintf("model is only available in realm '%s'; refusing cross-realm fallback", owner)}
	}
	// Account-availability fallback runs only when the client named no exit.
	if decision.Realm == "" && reqRealm == "" {
		other := otherRealm(realm)
		if s.pool.CountReady(realm, model, allowed) == 0 &&
			s.pool.CountReady(other, model, allowed) > 0 &&
			s.catalog.ExclusiveRealm(model) == "" {
			realm = other
		}
		if realm != s.currentRealm() {
			realmReason = s.autoRealmReason(model, realm)
			s.log.Info("realm", "request routed to realm %s (default=%s, model=%q)",
				realm, s.currentRealm(), model)
		}
	}
	payload["_qd_realm"] = realm
	payload["_qd_realm_reason"] = realmReason
	modelKey := s.resolveModelKey(model, realm)

	total := s.pool.CountReady(realm, model, allowed)
	tried := map[string]bool{}
	var lastErr error
	var lastQueued *upstream.QueuedError
	queueRounds, queueWaited, waitRounds := 0, 0, 0
	maxRounds := envIntDef("QD_QUEUE_MAX_ROUNDS", 6)
	maxWait := envIntDef("QD_QUEUE_WAIT_MAX_SECS", 180)
	budget := envIntDef("QD_QUEUE_WAIT_BUDGET_SECS", 600)

	for attempt := 0; attempt < total+4; attempt++ {
		var acc *accounts.Account
		if len(decision.Tiers) > 0 || decision.Configured {
			if picked := s.route.Pick(decision, realm, model, route.AsCandidates(s.pool.Candidates()), sessionKey, tried); picked != nil {
				if c, ok := picked.(accounts.PoolCandidate); ok {
					acc = c.Account
				}
			}
		} else {
			acc = s.pool.PickForSession(realm, model, sessionKey, tried, allowed)
		}
		if acc == nil {
			// Accounts may all be briefly cooling down; waiting serves the request
			// instead of failing it. The attempt cap prevents a loop.
			if wait := s.shortCooldownWait(realm, model, tried, allowed); wait > 0 {
				if waitRounds++; waitRounds <= 2 {
					s.log.Warn("chat", "accounts in short error-cooldown for %q (%.1fs left) - waiting",
						model, wait)
					time.Sleep(time.Duration((wait + 0.25) * float64(time.Second)))
					continue
				}
			}
			break
		}
		if acc.Realm != realm && entry == nil {
			if sessionKey != "" {
				s.pool.Affinity.Unbind(sessionKey)
			}
			continue
		}
		tried[acc.UID] = true

		body, err := s.buildBody(payload, acc, modelKey, realm, sessionKey)
		if err != nil {
			return nil, nil, err
		}

		hosts := accounts.GatewayCandidates(acc.Realm, acc.RealmConfig())
		action := actionNextAccount
		for hostIdx := 0; hostIdx < len(hosts); hostIdx++ {
			rawURL := hosts[hostIdx] + upstream.ChatPath
			resp, postErr := s.upstream.Post(context.Background(), upstream.Request{
				Body: body, Account: acc, ModelKey: modelKey, RawURL: rawURL,
				Sessions: s.sessions, Timeout: 600 * time.Second,
			})
			if postErr == nil {
				s.sentEffort = sentEffort(body)
				if sessionKey != "" {
					s.pool.Affinity.Bind(sessionKey, acc.UID)
				}
				acc.NoteSuccess(model)
				return resp, acc, nil
			}
			// A transport failure on one host may be host-specific: try the
			// official fallback hosts before penalising the account.
			if upstream.IsTransientTransport(postErr) && hostIdx+1 < len(hosts) {
				s.log.Warn("chat", "transient transport error on %s model %q: %s - switching gateway host",
					shortUID(acc.UID), model, truncateStr(errText(postErr), 120))
				continue
			}
			action = s.classifyPostError(postErr, acc, model, sessionKey, total,
				&lastQueued, &queueRounds, maxRounds, &queueWaited, maxWait, budget, &lastErr)
			if action == actionRetrySame {
				// Retry from the primary host again with a freshly built body.
				break
			}
			break
		}
		if action == actionFail {
			if lastErr != nil {
				return nil, nil, lastErr
			}
			return nil, nil, &upstream.StatusError{Code: 502, Detail: "upstream request failed"}
		}
		if action == actionRetrySame {
			// Rebuild for the same account (session id may have changed).
			tried[acc.UID] = false
			continue
		}
	}

	if lastQueued != nil {
		return nil, nil, lastQueued
	}
	if lastErr != nil {
		return nil, nil, lastErr
	}
	return nil, nil, &upstream.StatusError{Code: 503,
		Detail: fmt.Sprintf("no usable account for realm '%s': all are disabled, cooling down, or expired", realm)}
}

// classifyPostError applies the retry/rotation policy to one failed attempt.
//
// The policy, in order:
//   - queue (10605): wait retryAfterSeconds and retry the same account within the
//     round and time budget;
//   - a learned output range: clamp and retry the same account;
//   - 429: cool the account and rotate (exponential backoff when several exist);
//   - 401/403: revoked credential — cool, and disable outright on a dead session;
//   - request faults: return as-is, without cooling or rotating, because rotating
//     would cool the whole pool for a request that fails everywhere;
//   - transient upstream/transport: short cooldown and rotate;
//   - other 4xx: cool and rotate.
func (s *Server) classifyPostError(postErr error, acc *accounts.Account, model, sessionKey string,
	total int, lastQueued **upstream.QueuedError, queueRounds *int, maxRounds int,
	queueWaited *int, maxWait, budget int, lastErr *error) retryAction {
	if he, ok := postErr.(*upstream.StatusError); ok {
		detail := he.Detail
		if wait := upstream.QueueWaitSeconds(detail); wait > 0 {
			if *queueRounds < maxRounds && wait <= maxWait && *queueWaited+wait <= budget {
				*queueRounds++
				*queueWaited += wait
				s.noteRisk("queue", detail)
				s.log.Warn("chat", "upstream queued on %s model %q - waiting %ds (round %d/%d)",
					shortUID(acc.UID), model, wait, *queueRounds, maxRounds)
				time.Sleep(time.Duration(wait+1) * time.Second)
				return actionRetrySame
			}
			queued := &upstream.QueuedError{RetryAfter: wait, Detail: detail, Waited: *queueWaited}
			*lastQueued = queued
			*lastErr = queued
			return actionFail
		}
		if s.learnFromRange(detail, model, acc.Realm) {
			return actionRetrySame
		}
		switch {
		case he.Code == 429:
			s.noteRisk("rate", detail)
			acc.NoteError("HTTP 429 (model throttled)", 60, total <= 1, model, nil)
			if total > 1 {
				acc.NoteSoftRate("HTTP 429 upstream")
			}
			if sessionKey != "" {
				s.pool.Affinity.Unbind(sessionKey)
			}
			*lastErr = postErr
			return actionNextAccount
		case he.Code == 401 || he.Code == 403:
			s.noteRisk("rejected", detail)
			if accounts.SessionDead(detail) {
				acc.Disable("session dead (TOKEN_EXPIRE): re-login required")
				_ = acc.Save(s.cfg.AccountsDir)
				s.log.Error("account", "account %s session dead (TOKEN_EXPIRE) - disabled", shortUID(acc.UID))
			}
			acc.NoteError(fmt.Sprintf("HTTP %d %s", he.Code, truncateStr(detail, 80)),
				60, total <= 1, "", nil)
			if sessionKey != "" {
				s.pool.Affinity.Unbind(sessionKey)
			}
			*lastErr = postErr
			return actionNextAccount
		case upstream.IsRequestFault(he.Code, detail):
			s.log.Warn("chat", "request-fault HTTP %d on %s model %q - no rotation, no cooldown",
				he.Code, shortUID(acc.UID), model)
			*lastErr = postErr
			return actionFail
		case upstream.IsTransientUpstream(he.Code, detail):
			acc.NoteError(fmt.Sprintf("HTTP %d upstream transient: %s", he.Code, truncateStr(detail, 80)),
				15, total <= 1, "", nil)
			*lastErr = postErr
			return actionNextAccount
		case he.Code >= 400 && he.Code < 500:
			acc.NoteError(fmt.Sprintf("HTTP %d: %s", he.Code, truncateStr(detail, 80)),
				60, total <= 1, "", nil)
			if sessionKey != "" {
				s.pool.Affinity.Unbind(sessionKey)
			}
			*lastErr = postErr
			return actionNextAccount
		}
		*lastErr = postErr
		return actionFail
	}
	if upstream.IsTransientTransport(postErr) {
		acc.NoteError("transport transient: "+truncateStr(errText(postErr), 100), 15, total <= 1, "", nil)
		*lastErr = postErr
		return actionNextAccount
	}
	acc.NoteError(truncateStr(errText(postErr), 120), 60, total <= 1, "", nil)
	*lastErr = postErr
	return actionNextAccount
}

// learnFromRange records an output cap learned from a 400 and reports whether it
// did so (in which case the same account should retry with the clamped body).
func (s *Server) learnFromRange(detail, model, realm string) bool {
	limit := parseMaxTokensRange(detail)
	if limit <= 0 {
		return false
	}
	modelKey := s.resolveModelKey(model, realm)
	s.learnMaxOutputLimit(limit, modelKey, realm)
	s.log.Warn("chat", "learned upstream max_tokens limit %d for %q; clamping future requests", limit, modelKey)
	return true
}
