package qoder

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// HTTPStatusError is a non-2xx upstream response.
//
// The body is carried verbatim so a host can inspect upstream error codes
// (10605 queueing, 112 quota, ALREADY_CLAIMED) without re-reading the wire.
type HTTPStatusError struct {
	Code   int
	Body   string
	Header http.Header
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Code, truncateText(e.Body, 200))
}

// IsNotFound reports whether the upstream answered 404/405/410, which is how
// "this endpoint does not exist in this realm" is signalled.
func (e *HTTPStatusError) IsNotFound() bool {
	if e == nil {
		return false
	}
	switch e.Code {
	case 404, 405, 410:
		return true
	}
	return false
}

// HTTP is the outbound transport seam.
//
// A host that already has a configured client (custom proxy, instrumentation,
// test doubles) can pass its own implementation; the default is
// NewHTTPClient.
type HTTP interface {
	Do(ctx context.Context, req Request) ([]byte, error)
	DoJSON(ctx context.Context, req Request) (map[string]any, error)
}

// Request describes one outbound call.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
	Timeout time.Duration
}

// HTTPClient is a plain net/http implementation of HTTP.
//
// The transport is rebuilt when the proxy environment changes so a proxy switch
// does not require a restart.
type HTTPClient struct {
	mu        sync.Mutex
	timeout   time.Duration
	transport *http.Transport
	proxyURL  string
	// insecureSkipVerify is test-only; production keeps verification on.
	insecureSkipVerify bool
	// tlsConfig lets a host supply a custom TLS configuration.
	tlsConfig *tls.Config
}

// NewHTTPClient builds a client with an explicit default timeout.
func NewHTTPClient(timeout time.Duration) *HTTPClient {
	return &HTTPClient{timeout: timeout}
}

// SetInsecureSkipVerify disables TLS verification. Tests only.
func (c *HTTPClient) SetInsecureSkipVerify(v bool) {
	c.mu.Lock()
	c.insecureSkipVerify = v
	c.mu.Unlock()
}

// SetTLSConfig installs a custom TLS configuration (proxy CA bundles, client
// certificates). Passing nil restores the default.
func (c *HTTPClient) SetTLSConfig(cfg *tls.Config) {
	c.mu.Lock()
	c.tlsConfig = cfg
	c.transport = nil // force a rebuild
	c.mu.Unlock()
}

// DefaultHTTPClient is a process-wide client with a 30s timeout, used when
// Options.HTTP is nil.
var DefaultHTTPClient = NewHTTPClient(30 * time.Second)

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

func (c *HTTPClient) transportFor() *http.Transport {
	c.mu.Lock()
	defer c.mu.Unlock()
	proxy := firstEnv("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy")
	if c.transport != nil && c.proxyURL == proxy {
		return c.transport
	}
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if proxy != "" {
		if pu, err := url.Parse(proxy); err == nil && pu.Scheme != "" {
			tr.Proxy = http.ProxyURL(pu)
			c.proxyURL = proxy
		}
	} else {
		c.proxyURL = ""
	}
	c.transport = tr
	return tr
}

// Do performs the request and returns the body, or *HTTPStatusError.
func (c *HTTPClient) Do(ctx context.Context, req Request) ([]byte, error) {
	c.mu.Lock()
	timeout := c.timeout
	insecure := c.insecureSkipVerify
	tlsCfg := c.tlsConfig
	c.mu.Unlock()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	tr := c.transportFor()
	if insecure || tlsCfg != nil {
		// Clone so per-call TLS settings cannot leak into the shared transport.
		clone := tr.Clone()
		if tlsCfg != nil {
			clone.TLSClientConfig = tlsCfg
		} else {
			clone.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only
		}
		tr = clone
	}
	hc := &http.Client{Transport: tr, Timeout: timeout}
	if req.Timeout > 0 {
		hc.Timeout = req.Timeout
	}

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, method, req.URL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range req.Headers {
		hr.Header.Set(k, v)
	}
	resp, err := hc.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &HTTPStatusError{Code: resp.StatusCode, Body: string(data), Header: resp.Header}
	}
	if readErr != nil {
		return nil, readErr
	}
	return data, nil
}

// DoJSON performs the request and decodes a JSON object response.
func (c *HTTPClient) DoJSON(ctx context.Context, req Request) (map[string]any, error) {
	data, err := c.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("upstream returned non-JSON: %w", err)
	}
	return out, nil
}

// IsTransientTransport reports whether a transport-level error is worth an
// immediate same-account retry (TLS EOF, connection reset, timeout).
func IsTransientTransport(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"eof", "reset", "broken pipe", "connection refused",
		"connection aborted", "tls", "handshake", "no such host", "i/o timeout"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func truncateText(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
