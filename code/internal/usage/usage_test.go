package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRecordAndSummary(t *testing.T) {
	s := newStore(t)
	gen := 1000
	row := Row{Model: "Qwen3.8-Max", PromptTokens: 1000, CompletionTokens: 200,
		TotalTokens: 1200, CachedTokens: 800, CachedReported: true, Credit: 5.5,
		GenMS: &gen, Account: "uid-1", Realm: "cn", KeyID: "k1"}
	if err := s.Record(row); err != nil {
		t.Fatal(err)
	}
	// Row is passed by value, so read the computed rate back from the log.
	recorded := s.RecentRows(1, 1, "")
	if len(recorded) != 1 {
		t.Fatalf("got %d rows", len(recorded))
	}
	row = recorded[0]
	sum := s.SummarySnapshot()
	if sum.Requests != 1 || sum.PromptTokens != 1000 || sum.CompletionTokens != 200 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Credit != 5.5 {
		t.Fatalf("credit = %v", sum.Credit)
	}
	// tokens/sec is derived from gen_ms.
	if row.TokensPerSec == 0 {
		t.Fatal("tokens_per_sec was not computed")
	}
	// The log is append-only JSONL with one line per request.
	raw, err := os.ReadFile(s.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	if lines := countLines(raw); lines != 1 {
		t.Fatalf("log has %d lines", lines)
	}
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

func TestSplitCreditSums(t *testing.T) {
	out := SplitCredit(10000, 8000, 500)
	for _, k := range []string{"input", "cached", "output"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("missing %q in %v", k, out)
		}
	}
	// Cached tokens above the prompt are clamped, not double counted.
	out2 := SplitCredit(100, 500, 0)
	if out2["cached"] <= 0 {
		t.Fatalf("cached component = %v", out2["cached"])
	}
	if out2["input"] != 0 {
		t.Fatalf("input component must be zero when everything is cached: %v", out2["input"])
	}
}

func TestProviderOf(t *testing.T) {
	cases := map[string]string{
		"Qwen3.8-Max":     "Qwen",
		"GLM-5.3":         "智谱 GLM",
		"DeepSeek-V4-Pro": "DeepSeek",
		"Kimi-K3":         "Kimi",
		"MiniMax-M3":      "MiniMax",
		"something-new":   "其他",
	}
	for model, want := range cases {
		if got := ProviderOf(model); got != want {
			t.Fatalf("ProviderOf(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestStatsReportFullPrice(t *testing.T) {
	s := newStore(t)
	// A long prompt with no cache hit counts as a full-price request.
	_ = s.Record(Row{Model: "Qwen3.8-Max", PromptTokens: 120000, CompletionTokens: 100,
		TotalTokens: 120100, CachedTokens: 0, CachedReported: true, Credit: 100,
		Realm: "cn", At: float64(time.Now().Unix())})
	// A short prompt with no cache report must NOT count.
	_ = s.Record(Row{Model: "GLM-5.3", PromptTokens: 200000, CompletionTokens: 10,
		TotalTokens: 200010, CachedReported: false, Credit: 50,
		Realm: "cn", At: float64(time.Now().Unix())})
	report := s.StatsReport(Window{}, 7, 5)
	full, _ := report["full_price"].(map[string]any)
	if n, _ := full["requests"].(int); n != 1 {
		t.Fatalf("full-price requests = %v, want 1", full["requests"])
	}
	kpi, _ := report["kpi"].(map[string]any)
	if kpi["requests"] != 2 {
		t.Fatalf("kpi requests = %v", kpi["requests"])
	}
}

func TestPerfStatsPercentiles(t *testing.T) {
	s := newStore(t)
	for _, ttft := range []int{100, 200, 300, 400, 500} {
		v := ttft
		gen := 1000
		_ = s.Record(Row{Model: "m", PromptTokens: 10, CompletionTokens: 100,
			TotalTokens: 110, TTFTMS: &v, GenMS: &gen,
			At: float64(time.Now().Unix())})
	}
	perf := s.PerfStats(Window{}, 100)
	ttft, _ := perf["ttft"].(map[string]any)
	if ttft["n"] != 5 {
		t.Fatalf("ttft n = %v", ttft["n"])
	}
	if p50, _ := ttft["p50"].(float64); p50 != 300 {
		t.Fatalf("p50 = %v, want 300", ttft["p50"])
	}
}

func TestRecentRowsNewestFirst(t *testing.T) {
	s := newStore(t)
	base := float64(time.Now().Unix())
	for i := 0; i < 5; i++ {
		_ = s.Record(Row{Model: "m", PromptTokens: i, TotalTokens: i,
			At: base + float64(i), Realm: "cn"})
	}
	rows := s.RecentRows(3, 1, "")
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].PromptTokens <= rows[1].PromptTokens {
		t.Fatalf("rows are not newest-first: %v, %v", rows[0].PromptTokens, rows[1].PromptTokens)
	}
}

func TestDailyReportGrouping(t *testing.T) {
	s := newStore(t)
	now := float64(time.Now().Unix())
	_ = s.Record(Row{Model: "Qwen3.8-Max", TotalTokens: 100, Realm: "cn",
		Account: "a", KeyID: "k1", At: now})
	_ = s.Record(Row{Model: "GLM-5.3", TotalTokens: 50, Realm: "intl",
		Account: "b", KeyID: "k2", At: now})
	for _, group := range []string{"model", "realm", "key", "account"} {
		report := s.DailyReport(7, group, "")
		series, _ := report["series"].([]string)
		if len(series) != 2 {
			t.Fatalf("group %q produced %d series: %v", group, len(series), series)
		}
	}
}

func TestWindowFiltering(t *testing.T) {
	now := float64(time.Now().Unix())
	w := Window{Since: now - 100, Until: now}
	if !w.Contains(now - 50) {
		t.Fatal("a timestamp inside the window was excluded")
	}
	if w.Contains(now - 500) {
		t.Fatal("a timestamp before the window was included")
	}
	// An unbounded window accepts everything.
	if !(Window{}).Contains(0) || !(Window{}).Contains(now) {
		t.Fatal("an empty window must be unbounded")
	}
}

func TestRangeWindowCustom(t *testing.T) {
	w := RangeWindow("custom", "1000", "2000")
	if w.Since != 1000 || w.Until != 2000 {
		t.Fatalf("custom window = %+v", w)
	}
	today := RangeWindow("today", "", "")
	if today.Since <= 0 {
		t.Fatal("the today window must have a start")
	}
}

func TestCreditsCoefficientsOverride(t *testing.T) {
	t.Setenv("QD_CREDIT_COEF", "1,2,3")
	c := CreditsCoefficients()
	if c != [3]float64{1, 2, 3} {
		t.Fatalf("coefficients = %v", c)
	}
	t.Setenv("QD_CREDIT_COEF", "garbage")
	if c := CreditsCoefficients(); c[0] != 1.069 {
		t.Fatalf("an invalid override must fall back to the defaults, got %v", c)
	}
}

func TestSummaryPersists(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Record(Row{Model: "m", PromptTokens: 7, TotalTokens: 7})
	// A new store on the same directory must see the summary.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.SummarySnapshot().PromptTokens; got != 7 {
		t.Fatalf("reloaded prompt tokens = %d", got)
	}
	_ = filepath.Join
}
