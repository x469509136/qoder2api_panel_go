package qoder

import "time"

// UTC8 is the reward schedule's timezone. Daily credits reset at 10:00 in it,
// and rendering them in the host's zone would show a time eight hours off in a
// UTC container.
var UTC8 = time.FixedZone("UTC+8", 8*3600)

// CheckinWindowHourUTC8 is the fixed reset hour for daily credits.
const CheckinWindowHourUTC8 = 10

// LegacyExpiryNever is CN's "never expires" placeholder epoch.
const LegacyExpiryNever = 253402214400

// Credential is the minimum a caller must supply to talk to Qoder.
//
// It is a value, not a session object: the package never mutates it, so a host
// can keep owning persistence, locking and rotation policy.
type Credential struct {
	// UID identifies the account. Optional for one-off calls; required when a
	// caller wants the returned data attributed to an account.
	UID string
	// Realm selects the endpoint table ("cn" or "intl").
	Realm string
	// AccessToken is the Bearer value.
	AccessToken string
	// RefreshToken is drt- (device) or jrt- (job).
	RefreshToken string
	// PersonalToken is a pt- PAT, the last-resort exchange credential.
	PersonalToken string
	// VPCOverride selects an enterprise VPC instance. nil means "inherit the
	// deployment default", "" means "force public CN".
	VPCOverride *string
	// MachineIdentitySource is "native" once a real machine identity has been
	// sampled. Check-in and campaign endpoints require it; sending a derived
	// one makes the daily claim campaign disappear.
	MachineIdentitySource string
	// ExpiresAt is the access token's expiry (unix seconds, 0 = unknown).
	ExpiresAt int64
}

// IsExpired reports whether the access token is past its expiry (with skew).
func (c Credential) IsExpired(now time.Time, skew time.Duration) bool {
	if c.ExpiresAt == 0 {
		return false
	}
	return now.Add(skew).Unix() >= c.ExpiresAt
}

// Tokens is the result of a refresh: the new credential material a host should
// persist. The previous refresh token is carried over when the upstream omits
// a replacement.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	// Kind records which flow produced it: "device" or "job".
	Kind string
}

// Credits is the normalised quota snapshot.
//
// This is the structure a host reads to answer "how much is left and when does
// it expire": Remain/RemainEffective/Size plus ExpiresAt and Composition.
type Credits struct {
	Remain          int              `json:"remain"`
	RemainEffective int              `json:"remain_effective"`
	Used            int              `json:"used"`
	Size            int              `json:"size"`
	Exceeded        bool             `json:"exceeded"`
	UsagePct        *float64         `json:"usage_pct,omitempty"`
	ExpiresAt       int64            `json:"expires_at,omitempty"`
	Shared          *SharedQuota     `json:"shared,omitempty"`
	Packages        []map[string]any `json:"packages,omitempty"`
	Composition     []Composition    `json:"composition,omitempty"`
	Source          string           `json:"source,omitempty"`
	UpdatedAt       int64            `json:"updated_at"`
	UpdatedISO      string           `json:"updated_iso"`
}

// SharedQuota is the organisation-level pool.
type SharedQuota struct {
	Remain    int    `json:"remain"`
	Used      int    `json:"used"`
	Size      int    `json:"size"`
	Available bool   `json:"available"`
	Unlimited bool   `json:"unlimited,omitempty"`
	Source    string `json:"source"`
}

// Composition is one quota container (plan / add-on / SOTA / shared).
type Composition struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Remain    int    `json:"remain"`
	Used      int    `json:"used"`
	Size      int    `json:"size"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// EffectiveRemain returns the balance used by guardrails and summaries.
func (c *Credits) EffectiveRemain() int {
	if c == nil {
		return 0
	}
	return c.RemainEffective
}

// ExpiresAtTime renders the expiry as a time (zero when unset).
func (c *Credits) ExpiresAtTime() time.Time {
	if c == nil || c.ExpiresAt == 0 {
		return time.Time{}
	}
	return time.Unix(c.ExpiresAt, 0).In(UTC8)
}

// ExpiresInDays reports whole days until expiry (-1 when unset). Callers use
// this to warn about near-expiring credits without re-deriving the epoch.
func (c *Credits) ExpiresInDays(now time.Time) int {
	if c == nil || c.ExpiresAt == 0 {
		return -1
	}
	d := time.Unix(c.ExpiresAt, 0).Sub(now)
	if d < 0 {
		return 0
	}
	return int(d.Hours() / 24)
}

// LedgerIsUnknown distinguishes "unknown balance" from "zero balance".
//
// A revoked credential gets a degenerate quota body: all zeros (or missing
// keys) plus isQuotaExceeded=true and CN's never-expires placeholder. Treating
// that as zero would trip a host's guardrails and take the account out.
func LedgerIsUnknown(c *Credits) bool {
	if c == nil {
		return false
	}
	if c.RemainEffective != 0 || c.Size != 0 {
		return false
	}
	if !c.Exceeded {
		return false
	}
	return c.ExpiresAt >= LegacyExpiryNever
}

// CheckinWindow is the next daily reset window.
type CheckinWindow struct {
	Epoch int64  `json:"epoch"`
	Note  string `json:"note"`
}

// NextCheckinWindow returns the next "10:00 UTC+8" boundary and a human note.
//
// At or after 10:00 the window rolls to tomorrow, so callers never receive an
// instant that has already passed. The note is always rendered in UTC+8.
func NextCheckinWindow(now time.Time) CheckinWindow {
	local := now.In(UTC8)
	today := time.Date(local.Year(), local.Month(), local.Day(),
		CheckinWindowHourUTC8, 0, 0, 0, UTC8)
	target := today
	if !local.Before(today) {
		target = today.AddDate(0, 0, 1)
	}
	return CheckinWindow{Epoch: target.Unix(), Note: target.Format("01-02 15:04") + "（UTC+8）"}
}
