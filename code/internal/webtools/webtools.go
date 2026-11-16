// Package webtools executes web_search / web_fetch locally.
//
// Codex and similar clients declare built-in tools the Qoder upstream will not
// run. With this feature on, the gateway rewrites those declarations into
// same-named functions, executes them locally when the model calls them, and
// feeds the results back for another round (at most three).
//
// This is the only place where the *model* drives an outbound request, so the
// SSRF boundary is strict: http/https only, no credentials in the URL, the
// resolved address must be globally routable, redirects are re-checked hop by
// hop, and the response has both a byte and a character cap.
package webtools

import (
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Tool names the gateway can execute.
const (
	WebSearchName = "web_search"
	WebFetchName  = "web_fetch"
)

// Declaration types that map onto the local tool.
var (
	SearchDeclTypes = []string{"web_search", "web_search_preview", "web_search_preview_2025_03_11"}
	FetchDeclTypes  = []string{"web_fetch"}
)

const (
	MaxResults    = 5
	MaxFetchChars = 20000
	MaxBodyBytes  = 4 * 1024 * 1024
	HTTPTimeout   = 15 * time.Second
	CacheTTL      = 600 * time.Second
	BlockCooldown = 600 * time.Second
	ErrorCooldown = 60 * time.Second
	MinInterval   = 500 * time.Millisecond
	MaxRedirects  = 5
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// Engines are tried in order.
var Engines = []string{"duckduckgo", "bing", "wikipedia"}

var (
	mu             sync.Mutex
	cache          = map[string]cacheEntry{}
	engineStateMap = map[string]engineState{}
	lastAt         time.Time
)

type cacheEntry struct {
	at    time.Time
	value any
}

type engineState struct {
	until   time.Time
	blocked bool
}

// Enabled reports the QD_WEB_TOOLS switch (an env value wins over the panel).
func Enabled(panelValue bool) bool {
	raw := strings.ToLower(strings.TrimSpace(getenv("QD_WEB_TOOLS")))
	switch raw {
	case "on", "1", "true", "yes":
		return true
	case "off", "0", "false", "no":
		return false
	}
	return panelValue
}

// Rounds is how many tool round-trips a single turn may consume.
func Rounds() int {
	var v int
	if _, err := fmt.Sscanf(strings.TrimSpace(getenv("QD_WEB_TOOLS_ROUNDS")), "%d", &v); err != nil || v <= 0 {
		return 3
	}
	return v
}

// Throttle keeps a minimum gap between engine requests so one provider does not
// see a burst.
func Throttle() {
	mu.Lock()
	wait := MinInterval - time.Since(lastAt)
	mu.Unlock()
	if wait > 0 {
		if wait > 3*time.Second {
			wait = 3 * time.Second
		}
		time.Sleep(wait)
	}
	mu.Lock()
	lastAt = time.Now()
	mu.Unlock()
}

func cacheGet(key string) (any, bool) {
	mu.Lock()
	defer mu.Unlock()
	hit, ok := cache[key]
	if !ok {
		return nil, false
	}
	if time.Since(hit.at) > CacheTTL {
		delete(cache, key)
		return nil, false
	}
	return hit.value, true
}

func cachePut(key string, value any) {
	mu.Lock()
	defer mu.Unlock()
	if len(cache) > 256 {
		cache = map[string]cacheEntry{}
	}
	cache[key] = cacheEntry{at: time.Now(), value: value}
}

func engineOK(name string) bool {
	mu.Lock()
	defer mu.Unlock()
	st, ok := engineStateMap[name]
	return !ok || time.Now().After(st.until)
}

func penaliseEngine(name string, blocked bool) {
	mu.Lock()
	defer mu.Unlock()
	cooldown := ErrorCooldown
	if blocked {
		cooldown = BlockCooldown
	}
	engineStateMap[name] = engineState{until: time.Now().Add(cooldown), blocked: blocked}
}

// ---------------------------------------------------------------------------
// SSRF guard
// ---------------------------------------------------------------------------

// ipIsPublic reports whether an address is globally routable unicast.
func ipIsPublic(raw string) bool {
	ip := net.ParseIP(strings.Trim(strings.TrimSpace(raw), "[]"))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	// 100.64/10 carrier-grade NAT and 198.18/15 benchmarking are not "public" for
	// this purpose: a local proxy may hijack them via fake-IP DNS.
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return false
		}
		if ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19) {
			return false
		}
		if ip4[0] == 169 && ip4[1] == 254 {
			return false
		}
		if ip4[0] == 0 {
			return false
		}
	}
	return true
}

// CheckURL validates a target URL's shape and resolves its address.
func CheckURL(raw string, allowLocal bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http/https URLs are allowed")
	}
	if u.User != nil {
		return nil, fmt.Errorf("URLs with embedded credentials are not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("URL has no host")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("dns lookup failed: %w", err)
	}
	for _, ip := range ips {
		if allowLocal || ipIsPublic(ip.String()) {
			return u, nil
		}
	}
	return nil, fmt.Errorf("target resolves only to private or loopback addresses")
}

// pinnedTransport dials the validated address while keeping the original host
// for the request and the TLS handshake.
//
// The address is pinned at check time: resolving once, connecting later, is a
// DNS-rebinding hole.
type pinnedTransport struct {
	addr string
}

func (t *pinnedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		Proxy: nil, // never honour an environment proxy here: it would re-resolve
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, t.addr)
		},
		DisableKeepAlives: true,
	}
	return tr.RoundTrip(req)
}

// openPinned fetches one URL over a pinned connection, following redirects with
// a re-check at every hop.
func openPinned(raw string, maxBytes int64) ([]byte, string, error) {
	current := raw
	for hop := 0; hop <= MaxRedirects; hop++ {
		u, err := CheckURL(current, false)
		if err != nil {
			return nil, "", err
		}
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		ips, err := net.LookupIP(u.Hostname())
		if err != nil {
			return nil, "", fmt.Errorf("dns lookup failed: %w", err)
		}
		addr := ""
		for _, ip := range ips {
			if ipIsPublic(ip.String()) {
				addr = net.JoinHostPort(ip.String(), port)
				break
			}
		}
		if addr == "" {
			return nil, "", fmt.Errorf("target resolves only to private or loopback addresses")
		}
		req, err := http.NewRequest(http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
		client := &http.Client{
			Timeout:   HTTPTimeout,
			Transport: &pinnedTransport{addr: addr},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if loc == "" {
				return nil, "", fmt.Errorf("redirect without a Location header")
			}
			next, err := u.Parse(loc)
			if err != nil {
				return nil, "", err
			}
			current = next.String()
			continue
		}
		if resp.StatusCode >= 400 {
			_ = resp.Body.Close()
			return nil, "", fmt.Errorf("upstream HTTP %d", resp.StatusCode)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, "", readErr
		}
		return body, resp.Header.Get("Content-Type"), nil
	}
	return nil, "", fmt.Errorf("too many redirects")
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// Result is one search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

var (
	ddgResultRe  = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>.*?<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	bingResultRe = regexp.MustCompile(`(?s)<li class="b_algo".*?<h2><a href="([^"]+)".*?>(.*?)</a></h2>(.*?)</li>`)
	tagRe        = regexp.MustCompile(`(?s)<[^>]+>`)
	wsRe         = regexp.MustCompile(`\s+`)
)

func clean(text string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(html.UnescapeString(tagRe.ReplaceAllString(text, " ")), " "))
}

// Search runs one query through the engine chain.
func Search(query string, limit int) ([]Result, error) {
	if limit <= 0 || limit > MaxResults {
		limit = MaxResults
	}
	key := "search:" + strings.ToLower(strings.TrimSpace(query))
	if hit, ok := cacheGet(key); ok {
		return hit.([]Result), nil
	}
	var lastErr error
	for _, engine := range Engines {
		if !engineOK(engine) {
			continue
		}
		Throttle()
		var results []Result
		var err error
		switch engine {
		case "duckduckgo":
			results, err = searchDuckDuckGo(query, limit)
		case "bing":
			results, err = searchBing(query, limit)
		case "wikipedia":
			results, err = searchWikipedia(query, limit)
		}
		if err != nil {
			lastErr = err
			penaliseEngine(engine, strings.Contains(strings.ToLower(err.Error()), "429"))
			continue
		}
		if len(results) > 0 {
			cachePut(key, results)
			return results, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no search engine available right now")
	}
	return nil, lastErr
}

func searchDuckDuckGo(query string, limit int) ([]Result, error) {
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	body, _, err := openPinned(u, MaxBodyBytes)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, m := range ddgResultRe.FindAllStringSubmatch(string(body), limit) {
		out = append(out, Result{Title: clean(m[2]), URL: m[1], Snippet: clean(m[3])})
	}
	return out, nil
}

func searchBing(query string, limit int) ([]Result, error) {
	u := "https://www.bing.com/search?q=" + url.QueryEscape(query)
	body, _, err := openPinned(u, MaxBodyBytes)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, m := range bingResultRe.FindAllStringSubmatch(string(body), limit) {
		out = append(out, Result{Title: clean(m[2]), URL: m[1], Snippet: clean(m[3])})
	}
	return out, nil
}

func searchWikipedia(query string, limit int) ([]Result, error) {
	u := "https://en.wikipedia.org/w/api.php?action=opensearch&format=json&limit=" +
		fmt.Sprintf("%d", limit) + "&search=" + url.QueryEscape(query)
	body, _, err := openPinned(u, MaxBodyBytes)
	if err != nil {
		return nil, err
	}
	var payload []any
	if err := jsonUnmarshal(body, &payload); err != nil {
		return nil, err
	}
	if len(payload) < 4 {
		return nil, nil
	}
	titles, _ := payload[1].([]any)
	descs, _ := payload[2].([]any)
	links, _ := payload[3].([]any)
	var out []Result
	for i := range titles {
		title := asString(titles[i])
		link := asString(links[i])
		desc := ""
		if i < len(descs) {
			desc = asString(descs[i])
		}
		out = append(out, Result{Title: title, URL: link, Snippet: desc})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Fetch
// ---------------------------------------------------------------------------

// Fetch retrieves one page and returns cleaned text.
func Fetch(rawURL string) (string, error) {
	key := "fetch:" + rawURL
	if hit, ok := cacheGet(key); ok {
		return hit.(string), nil
	}
	body, contentType, err := openPinned(rawURL, MaxBodyBytes)
	if err != nil {
		return "", err
	}
	text := string(body)
	if strings.Contains(contentType, "html") {
		text = cleanHTML(text)
	}
	if len(text) > MaxFetchChars {
		text = text[:MaxFetchChars]
	}
	cachePut(key, text)
	return text, nil
}

func cleanHTML(raw string) string {
	raw = regexp.MustCompile(`(?is)<script.*?</script>`).ReplaceAllString(raw, " ")
	raw = regexp.MustCompile(`(?is)<style.*?</style>`).ReplaceAllString(raw, " ")
	raw = regexp.MustCompile(`(?is)<!--.*?-->`).ReplaceAllString(raw, " ")
	raw = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</li>|</h[1-6]>`).ReplaceAllString(raw, "\n")
	text := clean(raw)
	text = regexp.MustCompile(`\n{3,}`).ReplaceAllString(text, "\n\n")
	return text
}

// ---------------------------------------------------------------------------
// Tool declarations
// ---------------------------------------------------------------------------

// ToolDeclaration is the function declaration the gateway advertises.
func ToolDeclaration(name string) map[string]any {
	switch name {
	case WebSearchName:
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        WebSearchName,
				"description": "Search the web and return the top results (title, URL, snippet).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string",
							"description": "The search query."},
					},
					"required": []any{"query"},
				},
			},
		}
	case WebFetchName:
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        WebFetchName,
				"description": "Fetch one http/https page and return its readable text.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{"type": "string",
							"description": "The absolute http/https URL."},
					},
					"required": []any{"url"},
				},
			},
		}
	}
	return nil
}

// BuiltinNames returns the local tool names a tool list declares.
func BuiltinNames(tools []any) []string {
	var out []string
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		typ := asString(m["type"])
		name := asString(m["name"])
		if name == "" {
			if fn, ok := m["function"].(map[string]any); ok {
				name = asString(fn["name"])
			}
		}
		for _, st := range SearchDeclTypes {
			if typ == st || name == WebSearchName {
				out = appendUnique(out, WebSearchName)
			}
		}
		for _, ft := range FetchDeclTypes {
			if typ == ft || name == WebFetchName {
				out = appendUnique(out, WebFetchName)
			}
		}
	}
	return out
}

// RewriteTools replaces built-in declarations with local function declarations.
func RewriteTools(tools []any, names []string) []any {
	if len(names) == 0 {
		return tools
	}
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	var out []any
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			out = append(out, t)
			continue
		}
		typ := asString(m["type"])
		alreadyLocal := false
		for _, n := range []string{WebSearchName, WebFetchName} {
			if nameSet[n] && typ == "function" && asString(m["name"]) == n {
				alreadyLocal = true
			}
		}
		if alreadyLocal {
			continue
		}
		isBuiltin := false
		for _, st := range append(append([]string{}, SearchDeclTypes...), FetchDeclTypes...) {
			if typ == st {
				isBuiltin = true
			}
		}
		if isBuiltin {
			continue
		}
		out = append(out, t)
	}
	for _, n := range names {
		if decl := ToolDeclaration(n); decl != nil {
			out = append(out, decl)
		}
	}
	return out
}

// RunTool executes one local tool call and returns its textual result.
func RunTool(name, arguments string) string {
	var args map[string]any
	if err := jsonUnmarshal([]byte(arguments), &args); err != nil {
		return `{"error":"invalid arguments"}`
	}
	switch name {
	case WebSearchName:
		query := asString(args["query"])
		if strings.TrimSpace(query) == "" {
			return `{"error":"query is required"}`
		}
		results, err := Search(query, MaxResults)
		if err != nil {
			return jsonError(err.Error())
		}
		blob, _ := jsonMarshal(map[string]any{"results": results})
		return blob
	case WebFetchName:
		target := asString(args["url"])
		text, err := Fetch(target)
		if err != nil {
			return jsonError(err.Error())
		}
		blob, _ := jsonMarshal(map[string]any{"url": target, "text": text})
		return blob
	}
	return jsonError("unknown tool: " + name)
}

func jsonError(msg string) string {
	blob, _ := jsonMarshal(map[string]any{"error": msg})
	return blob
}

func appendUnique(list []string, v string) []string {
	for _, item := range list {
		if item == v {
			return list
		}
	}
	return append(list, v)
}
