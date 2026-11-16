package logx

import (
	"regexp"
	"strings"
)

// Redaction removes upstream credential material from anything that reaches a
// log sink (file, stderr, the /logs ring).
//
// Why this exists: an upstream error body or URL is often logged verbatim for
// diagnosis ("model discovery failed on <url>: <err>"). Those strings can carry
// a bearer token, a refresh token, or an access token in a query parameter, and
// /logs is served to any panel session, so a logged token becomes a readable
// token. Redaction only rewrites shapes it recognises: losing a little
// diagnostic detail is cheaper than leaking an account.
//
// The patterns mirror the credential shapes this gateway actually handles:
//
//	dt- / drt- / pt- / jrt-   Qoder access / refresh / personal / job token
//	Bearer ...                any bearer header value
//	"accessToken":"..."       persisted account document fragments
//	?token=... / api_key=...  query strings and settings fields
type redactor struct {
	patterns []*regexp.Regexp
}

var defaultRedactor = newRedactor()

func newRedactor() *redactor {
	return &redactor{patterns: []*regexp.Regexp{
		regexp.MustCompile("(?i)\\bbearer\\s+[A-Za-z0-9._~+/=-]{8,}"),
		regexp.MustCompile("\\b(dt|drt|pt|jrt)-[A-Za-z0-9._~+/=-]{6,}"),
		regexp.MustCompile("(?i)\\bcosy-(?:key|user|machineid)[\"']?\\s*[:=]\\s*[\"']?[A-Za-z0-9._~+/=-]{6,}"),
		regexp.MustCompile("(?i)\"(access_?token|refresh_?token|personal_?token|device_?token|id_?token)\"\\s*:\\s*\"[^\"]*\""),
		regexp.MustCompile("(?i)[?&](access_?token|refresh_?token|personal_?token|token|key|api_?key)=[^&\\s\"]+"),
		regexp.MustCompile("(?i)\\b(panel_session_secret|panel_password_hash|api_?key)[\"']?\\s*[:=]\\s*[\"']?[A-Za-z0-9+/=_-]{12,}"),
	}}
}

// redact scrubs credential material out of s.
func (r *redactor) redact(s string) string {
	if s == "" || r == nil {
		return s
	}
	out := s
	for _, re := range r.patterns {
		out = re.ReplaceAllStringFunc(out, maskMatch)
	}
	return out
}

// maskMatch keeps the field name and a short non-secret prefix so an operator
// can still tell two incidents apart.
func maskMatch(match string) string {
	sep := -1
	for i := 0; i < len(match); i++ {
		if match[i] == '=' || match[i] == ':' {
			sep = i
			break
		}
	}
	if sep < 0 || sep+1 >= len(match) {
		return "[redacted]"
	}
	key := strings.TrimSpace(match[:sep+1])
	value := strings.Trim(strings.TrimSpace(match[sep+1:]), "\"")
	if len(value) <= 4 {
		return key + " [redacted]"
	}
	return key + " " + value[:4] + "...[redacted]"
}

// Redact scrubs one string using the package redactor.
func Redact(s string) string { return defaultRedactor.redact(s) }
