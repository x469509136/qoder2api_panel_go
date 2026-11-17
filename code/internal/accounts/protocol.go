package accounts

import "time"

// Account-side helpers the bridge needs. They live here rather than in
// bridge.go so bridge.go stays a pure adapter listing.

// dirOf returns the directory portion of a credential path.
func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}

// QuotaFetchResult is the outcome of one credits refresh (panel shape).
type QuotaFetchResult struct {
	OK      bool     `json:"ok"`
	HTTP    int      `json:"http,omitempty"`
	Error   string   `json:"error,omitempty"`
	Credits *Credits `json:"credits,omitempty"`
}

// storeCredits stores a fresh snapshot and updates the panel's delta field.
func (a *Account) storeCredits(creds *Credits) {
	a.mu.Lock()
	prevEff := 0
	hadPrev := false
	if a.Credits != nil {
		prevEff = a.Credits.RemainEffective
		hadPrev = true
	}
	a.Credits = creds
	// The panel shows the last credit delta; only update when the number really
	// changed so a +100 check-in is not overwritten by a later refresh.
	if hadPrev && prevEff != creds.RemainEffective {
		delta := creds.RemainEffective - prevEff
		a.LastCreditsDelta = &delta
		a.LastCreditsDeltaAt = float64(time.Now().Unix())
	}
	a.LastQuotaAnomaly = nil
	a.CreditsDegraded = nil
	a.mu.Unlock()
	// A balance refresh is the only path that lifts a 402 park.
	a.ReviveBalanceCooldown()
	if a.Path != "" {
		_ = a.Save(dirOf(a.Path))
	}
}

// SessionInvalidator is wired by the server to drop the COSY session cache
// entry after a token rotation (avoids importing sign from this package).
var SessionInvalidator func(uid string)
