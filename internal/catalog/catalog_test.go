package catalog

import "testing"

func TestResolveUpstreamKeyForms(t *testing.T) {
	r := Default()
	cases := map[string]string{
		"qmodel_38max":               "qmodel_38max",
		"Qwen3.8-Max":                "qmodel_38max",
		"qwen3.8-max":                "qmodel_38max",
		"qmodel_38max (Qwen3.8-Max)": "qmodel_38max",
		"Kimi-K3":                    "kmodel_latest",
		"kimi-k3.1-pro":              "kmodel_latest",
		"DeepSeek-Flash":             "dfmodel",
		"GLM-5.3":                    "gmodel",
		"auto":                       "auto",
		"":                           "auto",
	}
	for in, want := range cases {
		if got := r.ResolveUpstreamKey(in, "cn"); got != want {
			t.Fatalf("ResolveUpstreamKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveFallsBackToRawName(t *testing.T) {
	r := Default()
	// An unknown BYOK model must be passed through untouched, not invented.
	if got := r.ResolveUpstreamKey("my-custom-model", "cn"); got != "my-custom-model" {
		t.Fatalf("got %q", got)
	}
}

func TestLiveDisplayNamesWin(t *testing.T) {
	r := Default()
	// The official client renamed kmodel_latest; until the snapshot catches up an
	// unknown display name must still resolve.
	if got := r.ResolveUpstreamKey("Kimi-K3.2-Pro", "cn"); got != "Kimi-K3.2-Pro" {
		t.Fatalf("pre-registration resolve = %q", got)
	}
	r.RegisterLiveDisplayNames([][2]string{{"Kimi-K3.2-Pro", "kmodel_latest"}})
	if got := r.ResolveUpstreamKey("Kimi-K3.2-Pro", "cn"); got != "kmodel_latest" {
		t.Fatalf("post-registration resolve = %q", got)
	}
}

func TestExclusiveRealm(t *testing.T) {
	r := Default()
	cases := map[string]string{
		"q37fmodel":   "cn",
		"gm51model":   "cn",
		"ultimate":    "intl",
		"performance": "intl",
		"Qwen3.8-Max": "",
	}
	for in, want := range cases {
		if got := r.ExclusiveRealm(in); got != want {
			t.Fatalf("ExclusiveRealm(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAutoRealmOnlyWhenSingleRealm(t *testing.T) {
	r := Default()
	// q37fmodel exists only in cn.
	if realm, reason := r.AutoRealmForModel("q37fmodel", "intl"); realm != "cn" || reason != "model_missing" {
		t.Fatalf("got %q/%q", realm, reason)
	}
	// A model present in both realms stays put.
	if realm, _ := r.AutoRealmForModel("Qwen3.8-Flash", "intl"); realm != "" {
		t.Fatalf("a shared model must not switch realms, got %q", realm)
	}
}

func TestSupportedEffortsAndDefault(t *testing.T) {
	r := Default()
	var meta Model
	for _, raw := range r.ModelsForRealm("cn") {
		if raw["key"] == "qmodel_38max" {
			meta = ModelInfo(raw)
		}
	}
	efforts := SupportedEfforts(meta)
	if len(efforts) == 0 {
		t.Fatal("Qwen3.8-Max should publish effort tiers")
	}
	def := DefaultEffort(meta)
	if def == "" {
		t.Fatal("default effort missing")
	}
	// Ordering must be canonical, not map order.
	wantOrder := []string{"low", "medium", "high", "xhigh", "max"}
	idx := 0
	for _, e := range efforts {
		for idx < len(wantOrder) && wantOrder[idx] != e {
			idx++
		}
		if idx >= len(wantOrder) {
			t.Fatalf("efforts are out of order: %v", efforts)
		}
	}
}

func TestNormalizeEffortClampsBidirectionally(t *testing.T) {
	r := Default()
	var df Model
	for _, raw := range r.ModelsForRealm("cn") {
		if raw["key"] == "dfmodel" {
			df = ModelInfo(raw)
		}
	}
	allowed := SupportedEfforts(df)
	got, note := NormalizeEffort("medium", df)
	if got == "" {
		t.Fatalf("medium was dropped (allowed=%v)", allowed)
	}
	if !contains(allowed, got) {
		t.Fatalf("normalized %q is not in the supported set %v", got, allowed)
	}
	if note == "" {
		t.Fatal("a clamped effort must report a note")
	}
	// An unsupported tier on a model with no tiers is dropped, not guessed.
	empty := Model{}
	if got, _ := NormalizeEffort("high", empty); got != "" {
		t.Fatalf("a model without tiers produced %q", got)
	}
}

func TestOffPeakWindow(t *testing.T) {
	// 22:00-08:00 wraps midnight.
	if !OffPeakActiveNow("22:00", "08:00", "Asia/Shanghai", 23*60) {
		t.Fatal("23:00 must be inside the off-peak window")
	}
	if !OffPeakActiveNow("22:00", "08:00", "Asia/Shanghai", 7*60) {
		t.Fatal("07:00 must be inside the off-peak window")
	}
	if OffPeakActiveNow("22:00", "08:00", "Asia/Shanghai", 12*60) {
		t.Fatal("noon must be outside the off-peak window")
	}
	// A same-day window works too.
	if !OffPeakActiveNow("13:00", "15:00", "", 14*60) {
		t.Fatal("14:00 must be inside 13:00-15:00")
	}
}

func TestDialogModelsPrefersChat(t *testing.T) {
	payload := map[string]any{
		"chat": []any{map[string]any{"key": "a", "display_name": "A"}},
		"app": []any{
			map[string]any{"key": "a", "display_name": "A-app"},
			map[string]any{"key": "lmodel", "display_name": "QoDuck"},
		},
		"inline": []any{map[string]any{"key": "should-not-appear"}},
	}
	rows := DialogModels(payload)
	if len(rows) != 2 {
		t.Fatalf("got %d rows: %v", len(rows), rows)
	}
	if rows[0]["display_name"] != "A" {
		t.Fatalf("chat metadata must win: %v", rows[0])
	}
	if rows[1]["key"] != "lmodel" {
		t.Fatalf("app-only entries must be appended: %v", rows[1])
	}
}

func TestStripModelPrefix(t *testing.T) {
	cases := map[string]string{
		"Qwen3.8-Flash":         "Qwen3.8-Flash",
		"qoder:Qwen3.8-Flash":   "Qwen3.8-Flash",
		"QODER:Qwen3.8-Flash":   "Qwen3.8-Flash",
		"qoder/Qwen3.8-Flash":   "Qwen3.8-Flash",
		"qoder-":                "qoder-", // a bare prefix is not a prefix
	}
	for in, want := range cases {
		if got, _ := StripModelPrefix(in); got != want {
			t.Fatalf("StripModelPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPrefixedModelResolves is the regression guard for the silent-auto trap:
// an unprefixed-unknown name is forwarded verbatim and billed as auto, so a
// namespace prefix must be stripped before resolution.
func TestPrefixedModelResolves(t *testing.T) {
	r := Default()
	for _, name := range []string{"qoder:Qwen3.8-Flash", "qoder:GLM-5.3",
		"qoder:DeepSeek-V4-Pro", "qoder:qmodel_38max"} {
		got := r.ResolveUpstreamKey(name, "cn")
		if got == name {
			t.Fatalf("%q was not resolved (it would be forwarded as auto)", name)
		}
		if r.IsKnownUpstreamKey(got, "cn") || got == "auto" {
			continue
		}
		t.Fatalf("%q resolved to unknown key %q", name, got)
	}
}

// TestRenamedModelResolvesViaLiveCatalog documents the rename window: the
// official client renamed qmodel_latest from "Qwen3.7-Max" to
// "Qwen4-Max-Preview", and the bundled snapshot still carries the old name.
// Without the live catalog the new name resolves to itself and is forwarded
// upstream, where an unknown name is silently billed as "auto".
func TestRenamedModelResolvesViaLiveCatalog(t *testing.T) {
	r := Default()
	// The snapshot does not know the new name yet.
	if r.IsKnownUpstreamKey("Qwen4-Max-Preview", "cn") {
		t.Skip("snapshot already updated; this guard is for the rename window")
	}
	// Registering the live display name is what closes the window.
	r.RegisterLiveDisplayNames([][2]string{{"Qwen4-Max-Preview", "qmodel_latest"}})
	if got := r.ResolveUpstreamKey("qoder:Qwen4-Max-Preview", "cn"); got != "qmodel_latest" {
		t.Fatalf("prefixed renamed model resolved to %q, want qmodel_latest", got)
	}
	if got := r.ResolveUpstreamKey("Qwen4-Max-Preview", "cn"); got != "qmodel_latest" {
		t.Fatalf("renamed model resolved to %q, want qmodel_latest", got)
	}
}
