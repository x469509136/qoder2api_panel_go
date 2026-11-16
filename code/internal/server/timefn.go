package server

import (
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/usage"
)

func nowFloat() float64 { return float64(time.Now().Unix()) }

func timeNowFn() time.Time { return time.Now() }

func simpleUsageRow(payload map[string]any, acc *accounts.Account, start time.Time) usage.Row {
	wall := int(time.Since(start).Milliseconds())
	row := usage.Row{
		Model:  firstNonEmptyStr(asStringAny(payload["model"]), "auto"),
		Stream: false, ElapsedMS: &wall,
	}
	if acc != nil {
		row.Account = acc.UID
		row.Realm = acc.Realm
	}
	return row
}
