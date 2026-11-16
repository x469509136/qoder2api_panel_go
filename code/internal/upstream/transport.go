package upstream

import (
	"errors"
	"net"
	"strings"
)

// isTransientTransport reports whether a transport error is worth an immediate
// same-account retry: TLS EOF, connection reset, timeouts and DNS hiccups are
// common against the qoder.sh edge.
func isTransientTransport(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"eof", "reset", "broken pipe", "connection refused", "connection aborted",
		"tls", "handshake", "no such host", "i/o timeout", "context deadline",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
