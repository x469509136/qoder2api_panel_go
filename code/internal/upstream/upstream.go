// Package upstream speaks the Qoder inference protocol: it signs a body, posts
// it to a gateway host, and unpacks the nested SSE envelope.
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/sign"
)

// ChatPath is the inference endpoint. The query string is part of the official
// request and must not be dropped.
const ChatPath = "/algo/api/v2/service/pro/sse/agent_chat_generation" +
	"?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

// ModelsPath is the live model list endpoint.
const ModelsPath = "/algo/api/v2/model/list?Encode=1"

// MaxBodyBytes caps the encoded outbound body (default 29 MiB).
//
// The upstream gateway rejects bodies near 30 MB with a connection reset, and a
// reset looks like a transient failure — retrying it would walk the whole
// account pool into the breaker.
const DefaultMaxBodyMiB = 29

// StatusError is an upstream HTTP failure with its (single-read) body attached.
type StatusError struct {
	Code   int
	Detail string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream HTTP %d: %s", e.Code, truncate(e.Detail, 200))
}

// QueuedError reports the upstream's model-level queue (10605).
type QueuedError struct {
	RetryAfter int
	Detail     string
	Waited     int
}

func (e *QueuedError) Error() string {
	return fmt.Sprintf("upstream queued: wait %ds (%s)", e.RetryAfter, truncate(e.Detail, 120))
}

// PayloadTooLargeError is a locally refused oversized body.
type PayloadTooLargeError struct {
	Size  int
	Limit int
}

func (e *PayloadTooLargeError) Error() string {
	return fmt.Sprintf("payload too large: %d bytes exceeds the %d byte upstream limit", e.Size, e.Limit)
}

// Request describes one inference call.
type Request struct {
	Body     map[string]any
	Account  *accounts.Account
	ModelKey string
	RawURL   string
	Timeout  time.Duration
	Sessions *sign.SessionCache
	MaxBody  int // bytes; 0 uses the default
}

// Response is an open streaming response.
type Response struct {
	Body       io.ReadCloser
	Account    *accounts.Account
	EncodedLen int
}

// Close releases the response body.
func (r *Response) Close() error {
	if r == nil || r.Body == nil {
		return nil
	}
	return r.Body.Close()
}

// Client posts signed bodies and reads envelope streams.
type Client struct {
	HTTP    *http.Client
	MaxBody int
}

// NewClient builds a client with the shared transport settings.
func NewClient() *Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		HTTP:    &http.Client{Transport: tr},
		MaxBody: int(float64(envFloat("QD_MAX_BODY_MB", DefaultMaxBodyMiB)) * 1024 * 1024),
	}
}

func envFloat(name string, def float64) float64 {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return def
	}
	var v float64
	if _, err := fmt.Sscanf(raw, "%g", &v); err != nil || v <= 0 {
		return def
	}
	return v
}

// Post signs and sends one inference request.
//
// The body is re-encoded per attempt because the session id and account-derived
// fields change between accounts.
func (c *Client) Post(ctx context.Context, req Request) (*Response, error) {
	plain, err := marshalNoEscape(req.Body)
	if err != nil {
		return nil, err
	}
	plainBytes := []byte(plain)
	limit := req.MaxBody
	if limit <= 0 {
		limit = c.MaxBody
	}
	if limit <= 0 {
		limit = DefaultMaxBodyMiB * 1024 * 1024
	}
	// Qoder's custom encoding preserves padded Base64 length, so the wire size
	// can be checked without allocating the encoded body.
	if size := 4 * ((len(plainBytes) + 2) / 3); size > limit {
		return nil, &PayloadTooLargeError{Size: size, Limit: limit}
	}
	encoded := sign.QoderEncode(plainBytes)
	if len(encoded) > limit {
		return nil, &PayloadTooLargeError{Size: len(encoded), Limit: limit}
	}

	sess, err := req.Sessions.Get(req.Account)
	if err != nil {
		return nil, err
	}
	rawURL := req.RawURL
	if rawURL == "" {
		cfg := req.Account.RealmConfig()
		rawURL = cfg.Gateway + ChatPath
	}
	headers := sess.Headers(encoded, rawURL, req.ModelKey, true, "text/event-stream")

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 600 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	httpReq, err := http.NewRequestWithContext(cctx, http.MethodPost, rawURL,
		strings.NewReader(encoded))
	if err != nil {
		cancel()
		return nil, err
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		cancel()
		return nil, &StatusError{Code: resp.StatusCode, Detail: string(body)}
	}
	return &Response{Body: &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel},
		Account: req.Account, EncodedLen: len(encoded)}, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// EnvelopeStatusError is a non-200 status hidden inside an HTTP 200 stream.
type EnvelopeStatusError struct {
	Status int
	Detail string
	// Emitted reports whether any frame had already been forwarded.
	Emitted bool
}

func (e *EnvelopeStatusError) Error() string {
	return fmt.Sprintf("upstream envelope status %d: %s", e.Status,
		truncate(e.Detail, 200))
}

// Stream reads the envelope stream, calling onData for each inner chunk.
//
// The envelope shape is:
//
//	data:{"headers":{...},"body":"<inner chunk JSON>","statusCodeValue":200}
//	data:{"body":"[DONE]"}
//	event:finish{...}          ← timing metadata, ignored
//
// A statusCodeValue other than 200 is raised as *EnvelopeStatusError, the body
// becomes the error detail, and the loop stops.
func Stream(ctx context.Context, body io.Reader, holder *Holder,
	onData func(inner map[string]any) error) error {
	reader := bufio.NewReaderSize(body, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := reader.ReadString('\n')
		if len(line) == 0 && err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		data := StripDataPrefix(line)
		if data == "" {
			if err != nil && errors.Is(err, io.EOF) {
				return nil
			}
			continue
		}
		var outer map[string]any
		if json.Unmarshal([]byte(data), &outer) != nil {
			if err != nil && errors.Is(err, io.EOF) {
				return nil
			}
			continue
		}
		status := outer["statusCodeValue"]
		statusInt := asIntStatus(status)
		bodyStr, bodyIsStr := outer["body"].(string)
		if statusInt != 0 && statusInt != 200 {
			detail := bodyStr
			if !bodyIsStr {
				blob, _ := marshalNoEscape(outer)
				detail = truncate(blob, 400)
			}
			return &EnvelopeStatusError{Status: statusInt, Detail: detail}
		}
		if bodyIsStr {
			if bodyStr == "[DONE]" {
				return nil
			}
			var inner map[string]any
			if json.Unmarshal([]byte(bodyStr), &inner) == nil {
				NoteInnerError(inner, statusInt)
				if holder != nil && inner["usage"] != nil && holder.Usage == nil {
					holder.Usage = inner["usage"]
				}
				if err := onData(inner); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// Holder carries per-request observations across the stream.
type Holder struct {
	mu    sync.Mutex
	Usage any
}

// SetUsage stores the usage block (first one wins).
func (h *Holder) SetUsage(u any) {
	h.mu.Lock()
	if h.Usage == nil {
		h.Usage = u
	}
	h.mu.Unlock()
}

// StripDataPrefix removes repeated "data:" prefixes and comment lines.
func StripDataPrefix(line string) string {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, ":") {
		return ""
	}
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(s[5:])
	}
	if s == "" || strings.HasPrefix(s, ":") {
		return ""
	}
	return s
}

func asIntStatus(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		var n int
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// JSONSortedCompact is exported for tests that need the exact signed bytes.
func JSONSortedCompact(m map[string]any) []byte { return sign.JSONSortedCompact(m) }

// BytesReader is a small helper for callers that only need a reader.
func BytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
