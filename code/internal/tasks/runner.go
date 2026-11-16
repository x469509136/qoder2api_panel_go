package tasks

import (
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/activity"
)

// Runner executes the scheduler's periodic work.
type Runner struct {
	Pool        *accounts.Pool
	AccountsDir string
	UsageDir    string
	Log         Logger
}

// NewRunner builds a runner.
func NewRunner(pool *accounts.Pool, accountsDir, usageDir string, log Logger) *Runner {
	return &Runner{Pool: pool, AccountsDir: accountsDir, UsageDir: usageDir, Log: log}
}

// RunCheckin checks in for the given targets.
func (r *Runner) RunCheckin(targets []*accounts.Account) *BatchResult {
	return RunBatchCheckin(targets, 1.0, 1.0, r.AccountsDir, r.UsageDir, r.Log)
}

// RunCampaigns claims every claimable campaign.
func (r *Runner) RunCampaigns(targets []*accounts.Account) *BatchResult {
	return RunBatchCampaigns(targets, ClaimGapDefault, 1.5, r.AccountsDir, r.UsageDir, r.Log)
}

// RunKeepalive refreshes tokens.
func (r *Runner) RunKeepalive(force bool) *KeepaliveResult {
	return RunKeepalive(r.Pool, force, 4*time.Hour, r.Log)
}

// RefreshCredits refreshes one account's balance snapshot.
func (r *Runner) RefreshCredits(acc *accounts.Account) {
	if acc == nil {
		return
	}
	acc.FetchCredits(r.AccountsDir)
	InvalidatePanelCache(acc.UID)
}

// RecordActivity writes one activity event.
func (r *Runner) RecordActivity(kind string, ok bool, acc *accounts.Account, detail string, amount int) {
	if acc == nil {
		return
	}
	activity.Record(r.UsageDir, kind, ok, acc.UID, acc.Nickname, acc.Realm, detail, amount)
}
