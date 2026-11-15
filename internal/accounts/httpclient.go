package accounts

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// HTTPStatusError carries an upstream HTTP status plus the body that was read.
//
// The body can only be read once, so it is attached to the error: a handler
// that re-reads the response would get a truncated or empty body, which makes
// queue detection silently fall through to "credential rejected".
type HTTPStatusError struct {
	Code   int
	Body   string
	Header http.Header
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Code, truncate(e.Body, 200))
}

// Client is the shared outbound HTTP client.
//
// It is deliberately constructed per request from the environment so a proxy
// change does not need a restart, and so HEAD/GET/POST all share one policy.
type Client struct {
	mu        sync.Mutex
	timeout   time.Duration
	transport *http.Transport
	proxyURL  string
}

// DefaultClient is the process-wide outbound client.
var DefaultClient = &Client{timeout: 30 * time.Second}

// NewHTTPClient builds a client with an explicit timeout.
func NewHTTPClient(timeout time.Duration) *Client { return &Client{timeout: timeout} }

func (c *Client) transportFor() (*http.Transport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	proxy := firstEnv("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy")
	if c.transport != nil && c.proxyURL == proxy {
		return c.transport, nil
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
	return tr, nil
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// InsecureSkipVerify is set by tests only; production keeps verification on.
var InsecureSkipVerify bool

func (c *Client) client() (*http.Client, error) {
	tr, err := c.transportFor()
	if err != nil {
		return nil, err
	}
	if InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only
	}
	timeout := c.timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}

// Request describes one outbound call.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
	Timeout time.Duration
}

// Do performs the request and returns the body, or *HTTPStatusError.
func (c *Client) Do(req Request) ([]byte, error) {
	hc, err := c.client()
	if err != nil {
		return nil, err
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequest(method, req.URL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range req.Headers {
		hr.Header.Set(k, v)
	}
	if req.Timeout > 0 {
		hc.Timeout = req.Timeout
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
func (c *Client) DoJSON(req Request) (map[string]any, error) {
	data, err := c.Do(req)
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

// UpstreamClockSkew measures the local clock's offset from the upstream's Date
// header (no auth; HEAD only). A skew over ~2 minutes breaks COSY signatures.
func UpstreamClockSkew(timeout time.Duration) (*int64, error) {
	hc, err := DefaultClient.client()
	if err != nil {
		return nil, err
	}
	if timeout == 0 {
		timeout = 4 * time.Second
	}
	hc.Timeout = timeout
	req, err := http.NewRequest(http.MethodHead, "https://openapi.qoder.com.cn/", nil)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	dateHeader := resp.Header.Get("Date")
	if dateHeader == "" {
		return nil, errors.New("no Date header")
	}
	serverTime, err := http.ParseTime(dateHeader)
	if err != nil {
		return nil, err
	}
	rtt := time.Since(start) / 2
	skew := serverTime.Sub(start.Add(rtt)).Seconds()
	rounded := int64(skew)
	return &rounded, nil
}
