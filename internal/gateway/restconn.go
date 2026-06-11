package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
)

const (
	restRequestTimeout = 30 * time.Second
	restMaxResponse    = 1 << 20 // 1 MiB
	restMaxRedirects   = 3
)

// newRestClient builds the per-upstream HTTP client: bounded timeout and a
// same-host-only redirect policy (a compromised upstream must not bounce the
// gateway's credentials to another host).
func newRestClient() *http.Client {
	return &http.Client{
		Timeout: restRequestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= restMaxRedirects {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("cross-host redirect refused (%s -> %s)", via[0].URL.Host, req.URL.Host)
			}
			return nil
		},
	}
}

// restConn is the connected form of an OpenAPI upstream. Stateless: tools
// carry their own client; Ping probes the API base URL.
type restConn struct {
	baseURL string
	client  *http.Client
	tools   []tool.Tool
}

func (c *restConn) Tools() []tool.Tool { return c.tools }

// Ping: HEAD base_url, GET fallback on 405. ANY HTTP response = alive (REST
// APIs have no standard health endpoint; a 404 still proves reachability).
// Only transport errors mark the upstream down.
func (c *restConn) Ping(ctx context.Context) error {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL, nil)
		if err != nil {
			return err
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			return nil
		}
	}
	return nil // both answered (405 twice) — an answer is an answer; alive
}

func (c *restConn) Close() error { return nil }

// dialOpenAPI is the production dialer branch for openapi: upstreams. Fetches
// the spec (file or URL, with the configured headers), generates the tools,
// and returns a stateless conn. A fetch/parse failure is a dial error — the
// supervision loop retries with backoff, re-fetching each time (drift
// handling for free).
func dialOpenAPI(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
	var specBytes []byte
	var err error
	if strings.HasPrefix(s.OpenAPI, "http://") || strings.HasPrefix(s.OpenAPI, "https://") {
		specBytes, err = fetchSpec(ctx, s.OpenAPI, s.Headers)
	} else {
		specBytes, err = os.ReadFile(s.OpenAPI)
	}
	if err != nil {
		return nil, fmt.Errorf("openapi spec %s: %w", s.OpenAPI, err)
	}
	client := newRestClient()
	tools, resolvedBase, err := generateTools(s.Name, specBytes, s.BaseURL, s.Operations, client)
	if err != nil {
		return nil, err
	}
	if resolvedBase == "" {
		return nil, fmt.Errorf("openapi: no base_url resolvable for %s", s.Name)
	}
	ht := make([]tool.Tool, len(tools))
	for i := range tools {
		tools[i].staticHeaders = s.Headers
		ht[i] = tools[i]
	}
	return &restConn{baseURL: resolvedBase, client: client, tools: ht}, nil
}

// fetchSpec GETs a spec URL with a bounded timeout and the upstream's
// configured headers (spec endpoints behind the same auth work). It uses
// newRestClient, NOT http.DefaultClient: the fetch carries the upstream's
// credentials, and the default client follows cross-host redirects (Go only
// strips the six standard auth headers, and even Authorization still flows
// to subdomains) — the same exact-same-host policy as API calls applies.
func fetchSpec(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := newRestClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spec fetch: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB spec cap
}

// Execute performs the HTTP request for one generated REST tool. HTTP 4xx/5xx
// are RESULTS (the agent reasons about them); tool errors are reserved for
// validation failures and transport problems. See spec §6.
func (r restTool) Execute(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	args := map[string]json.RawMessage{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return tool.ToolResult{Error: "invalid arguments: " + err.Error()}, nil
		}
	}
	for _, req := range r.requiredFields {
		if _, ok := args[req]; !ok {
			return tool.ToolResult{Error: "missing required field: " + req}, nil
		}
	}

	// Path interpolation with traversal guard.
	urlPath := r.specPath
	for name := range r.pathParams {
		raw, ok := args[name]
		if !ok {
			continue // required-check above already caught missing required
		}
		val := scalarString(raw)
		if strings.Contains(val, "/") || strings.Contains(val, "..") ||
			strings.Contains(strings.ToLower(val), "%2f") || strings.Contains(strings.ToLower(val), "%2e%2e") {
			return tool.ToolResult{Error: fmt.Sprintf("invalid path parameter %q: path separators and traversal sequences are not allowed", name)}, nil
		}
		urlPath = strings.ReplaceAll(urlPath, "{"+name+"}", url.PathEscape(val))
	}

	// Query: absent optionals skipped; arrays serialized comma-joined (the
	// OpenAPI form-style default).
	q := url.Values{}
	for name := range r.queryParams {
		raw, ok := args[name]
		if !ok {
			continue
		}
		var arr []json.RawMessage
		if len(raw) > 0 && raw[0] == '[' && json.Unmarshal(raw, &arr) == nil {
			vals := make([]string, len(arr))
			for i, a := range arr {
				vals[i] = scalarString(a)
			}
			q.Set(name, strings.Join(vals, ","))
		} else {
			q.Set(name, scalarString(raw))
		}
	}

	// Body.
	var bodyReader io.Reader
	if r.hasBody {
		if raw, ok := args["body"]; ok {
			bodyReader = bytes.NewReader(raw)
		}
	}

	fullURL := r.baseURL + urlPath
	if enc := q.Encode(); enc != "" {
		fullURL += "?" + enc
	}
	req, err := http.NewRequestWithContext(ctx, r.method, fullURL, bodyReader)
	if err != nil {
		return tool.ToolResult{Error: "request build: " + err.Error()}, nil
	}

	// Headers: config statics are inviolable (case-insensitive).
	for k, v := range r.staticHeaders {
		req.Header.Set(k, v)
	}
	for prop, wireName := range r.headerParams {
		raw, ok := args[prop]
		if !ok {
			continue
		}
		if _, clash := headerClash(r.staticHeaders, wireName); clash {
			return tool.ToolResult{Error: fmt.Sprintf("header %q is set by gateway configuration and cannot be overridden", wireName)}, nil
		}
		req.Header.Set(wireName, scalarString(raw))
	}
	// Reject ANY header_* arg targeting a configured header even if the spec
	// didn't declare it (agents can only send declared ones — this is belt
	// and braces against schema-validation gaps).
	for argName := range args {
		if strings.HasPrefix(argName, "header_") {
			if _, clash := headerClash(r.staticHeaders, strings.TrimPrefix(argName, "header_")); clash {
				return tool.ToolResult{Error: fmt.Sprintf("header %q is set by gateway configuration and cannot be overridden", strings.TrimPrefix(argName, "header_"))}, nil
			}
		}
	}
	if bodyReader != nil {
		// JSON default only — a configured static Content-Type wins.
		if _, clash := headerClash(r.staticHeaders, "Content-Type"); !clash {
			req.Header.Set("Content-Type", "application/json")
		}
	}

	client := r.client
	if client == nil {
		client = newRestClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		// Surface redirect-policy refusals as tool errors with the cause.
		return tool.ToolResult{Error: "request failed: " + err.Error()}, nil
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, restMaxResponse+1)
	bodyBytes, err := io.ReadAll(limited)
	if err != nil {
		return tool.ToolResult{Error: "response read: " + err.Error()}, nil
	}
	truncated := false
	if len(bodyBytes) > restMaxResponse {
		bodyBytes, truncated = bodyBytes[:restMaxResponse], true
	}

	envelope := map[string]any{
		"status":    resp.StatusCode,
		"headers":   map[string]string{"content-type": resp.Header.Get("Content-Type")},
		"truncated": truncated,
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "json") && !truncated && json.Valid(bodyBytes) {
		envelope["body"] = json.RawMessage(bodyBytes)
	} else {
		envelope["body"] = string(bodyBytes)
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return tool.ToolResult{Error: "envelope encode: " + err.Error()}, nil
	}
	return tool.ToolResult{Output: string(out)}, nil
}

// scalarString renders a JSON scalar as its string form without quotes.
func scalarString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// headerClash reports whether name collides (case-insensitively) with a
// configured static header.
func headerClash(static map[string]string, name string) (string, bool) {
	for k := range static {
		if strings.EqualFold(k, name) {
			return k, true
		}
	}
	return "", false
}
