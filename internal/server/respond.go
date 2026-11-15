package server

import (
	"net/http"
	"strings"
)

// writeUpstreamError renders an upstream failure with the friendly message.
func (s *Server) writeUpstreamError(w http.ResponseWriter, err error) {
	code := upstreamStatus(err)
	msg, etype := friendlyUpstreamError(code, upstreamDetail(err))
	if queued, ok := err.(queuedWithRetry); ok {
		if wait := queued.RetryAfterValue(); wait > 0 {
			w.Header().Set("Retry-After", itoa(wait))
		}
	}
	s.writeError(w, code, msg, etype)
}

// writeUpstreamFriendly renders a friendly upstream error (no retry header).
func (s *Server) writeUpstreamFriendly(w http.ResponseWriter, err error) {
	code := upstreamStatus(err)
	msg, etype := friendlyUpstreamError(code, upstreamDetail(err))
	s.writeError(w, code, msg, etype)
}

// queuedWithRetry is satisfied by upstream.QueuedError.
type queuedWithRetry interface {
	RetryAfterValue() int
}

// webToolsEnabled reports whether local web tools are on (env wins over panel).
func webtoolsEnabled(s *Server) bool {
	return webtoolsEnabledFor(s.settings.WebToolsEnabled())
}

// isPanelPath reports whether a path belongs to the management surface.
func isPanelPath(path string) bool {
	for _, prefix := range []string{
		"/admin", "/accounts", "/usage", "/v1/usage", "/tasks", "/scheduler",
		"/settings", "/logs", "/diag", "/update", "/agents",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
