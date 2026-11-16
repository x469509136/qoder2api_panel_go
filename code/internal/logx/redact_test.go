package logx

import (
	"strings"
	"testing"
)

// TestRedactCredentials pins that the log sinks cannot carry raw credentials.
//
// Every sample below is a shape this gateway has actually produced or could
// produce: an upstream error echoing a header, a model-list URL with a token
// query parameter, a persisted account document fragment, or the panel secret.
func TestRedactCredentials(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		wants []string // substrings that must survive
		hides []string // substrings that must not survive
	}{
		{
			name:  "bearer header",
			in:    "upstream said: Authorization: Bearer dt-abcdef1234567890 - rejected",
			wants: []string{"Authorization"},
			hides: []string{"dt-abcdef1234567890"},
		},
		{
			name:  "qoder access token",
			in:    "refresh failed for dt-9f8e7d6c5b4a3210",
			hides: []string{"9f8e7d6c5b4a3210"},
		},
		{
			name:  "refresh token",
			in:    "token family drt-1122334455667788 rotated",
			hides: []string{"1122334455667788"},
		},
		{
			name:  "personal token",
			in:    "import failed: pt-abcdefghijklmnop",
			hides: []string{"abcdefghijklmnop"},
		},
		{
			name:  "query string token",
			in:    "GET https://api.qoder.sh/x?token=abcdef123456&realm=cn failed",
			wants: []string{"realm=cn"},
			hides: []string{"abcdef123456"},
		},
		{
			name:  "json accessToken",
			in:    `{"uid":"u1","accessToken":"dt-secret-value-here"}`,
			wants: []string{"uid"},
			hides: []string{"dt-secret-value-here"},
		},
		{
			name:  "panel session secret",
			in:    `panel_session_secret = WWg4t6KR3gihjzuEdn6KKuTAFmbupwCU`,
			wants: []string{"panel_session_secret"},
			hides: []string{"WWg4t6KR3gihjzuEdn6KKuTAFmbupwCU"},
		},
		{
			name:  "api key",
			in:    `api_key=sk-live-0123456789abcdef`,
			hides: []string{"sk-live-0123456789abcdef"},
		},
		{
			name:  "cosy key",
			in:    `cosy-key: QWERTYasdfgh123456 sent`,
			hides: []string{"QWERTYasdfgh123456"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Redact(tc.in)
			for _, w := range tc.wants {
				if !contains(out, w) {
					t.Errorf("redaction dropped useful context %q from %q", w, out)
				}
			}
			for _, h := range tc.hides {
				if contains(out, h) {
					t.Errorf("redaction leaked %q in %q", h, out)
				}
			}
		})
	}
}

// TestRedactIsIdempotent guards against a redacted line becoming unredacted when
// a sink re-processes it.
func TestRedactIsIdempotent(t *testing.T) {
	in := "Authorization: Bearer dt-abcdef1234567890"
	once := Redact(in)
	twice := Redact(once)
	if once != twice {
		t.Fatalf("not idempotent: once=%q twice=%q", once, twice)
	}
}

// TestRedactLeavesOrdinaryTextAlone pins that this stays a targeted filter.
func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	in := "chat done: model=Qwen3.8-Flash acct=01a1df2a tokens=2635 (in=2603 out=32)"
	if out := Redact(in); out != in {
		t.Fatalf("ordinary log line was rewritten: in=%q out=%q", in, out)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
