package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
)

// execTool finds a generated tool by short name and runs Execute.
func execTool(t *testing.T, srv *httptest.Server, static map[string]string, short string, input string) tool.ToolResult {
	t.Helper()
	tools, _, err := generateTools("orders", []byte(testSpec), srv.URL, nil, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	for i := range tools {
		if tools[i].Name() == "orders__"+short {
			tools[i].staticHeaders = static
			res, err := tools[i].Execute(context.Background(), json.RawMessage(input))
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			return res
		}
	}
	t.Fatalf("tool %s not found", short)
	return tool.ToolResult{}
}

// envelope decodes the JSON result the agent sees.
type envelope struct {
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	Body      json.RawMessage   `json:"body"`
	Truncated bool              `json:"truncated"`
}

func decodeEnv(t *testing.T, res tool.ToolResult) envelope {
	t.Helper()
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	var e envelope
	if err := json.Unmarshal([]byte(res.Output), &e); err != nil {
		t.Fatalf("bad envelope %q: %v", res.Output, err)
	}
	return e
}

func TestExecutePathQueryBody(t *testing.T) {
	var gotPath, gotQuery, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		if r.Body != nil {
			b := make([]byte, 1024)
			n, _ := r.Body.Read(b)
			gotBody = string(b[:n])
		}
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	e := decodeEnv(t, execTool(t, srv, nil, "getOrder", `{"id":"ord 42"}`))
	if gotPath != "/orders/ord%2042" && gotPath != "/orders/ord 42" {
		t.Fatalf("path interpolation: %s", gotPath)
	}
	if e.Status != 200 || !strings.Contains(string(e.Body), "true") {
		t.Fatalf("envelope: %+v", e)
	}

	decodeEnv(t, execTool(t, srv, nil, "listOrders", `{"limit":5,"status":"open"}`))
	if !strings.Contains(gotQuery, "limit=5") || !strings.Contains(gotQuery, "status=open") {
		t.Fatalf("query: %s", gotQuery)
	}

	decodeEnv(t, execTool(t, srv, nil, "createOrder", `{"body":{"item":"widget","qty":2}}`))
	if !strings.Contains(gotBody, `"widget"`) || gotCT != "application/json" {
		t.Fatalf("body=%s ct=%s", gotBody, gotCT)
	}
}

func TestExecuteValidationErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must not reach the server")
	}))
	defer srv.Close()
	cases := []struct{ name, tool, input, wantSub string }{
		{"missing required path", "getOrder", `{}`, "id"},
		{"missing required query", "listOrders", `{}`, "limit"},
		{"missing required body", "createOrder", `{}`, "body"},
		{"traversal dotdot", "getOrder", `{"id":".."}`, "path"},
		{"traversal slash", "getOrder", `{"id":"a/b"}`, "path"},
		{"traversal encoded", "getOrder", `{"id":"a%2Fb"}`, "path"},
		{"header override", "listOrders", `{"limit":1,"header_X-Trace":"t","header_Authorization":"evil"}`, "header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			static := map[string]string{"Authorization": "Bearer real"}
			tools, _, err := generateTools("orders", []byte(testSpec), srv.URL, nil, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			for i := range tools {
				if tools[i].Name() == "orders__"+tc.tool {
					tools[i].staticHeaders = static
					res, err := tools[i].Execute(context.Background(), json.RawMessage(tc.input))
					if err != nil {
						t.Fatalf("want tool error, got transport error %v", err)
					}
					if res.Error == "" || !strings.Contains(strings.ToLower(res.Error), tc.wantSub) {
						t.Fatalf("want error containing %q, got %q", tc.wantSub, res.Error)
					}
					return
				}
			}
			t.Fatal("tool not found")
		})
	}
}

func TestExecuteHeaderPrecedenceAndSpecHeaders(t *testing.T) {
	var gotAuth, gotTrace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotTrace = r.Header.Get("Authorization"), r.Header.Get("X-Trace")
		fmt.Fprint(w, "{}")
	}))
	defer srv.Close()
	decodeEnv(t, execTool(t, srv, map[string]string{"Authorization": "Bearer real"},
		"listOrders", `{"limit":1,"header_X-Trace":"trace-1"}`))
	if gotAuth != "Bearer real" || gotTrace != "trace-1" {
		t.Fatalf("auth=%q trace=%q", gotAuth, gotTrace)
	}
}

func TestExecute4xxIsResultNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no such order"}`, 404)
	}))
	defer srv.Close()
	e := decodeEnv(t, execTool(t, srv, nil, "getOrder", `{"id":"x"}`))
	if e.Status != 404 {
		t.Fatalf("status: %d", e.Status)
	}
}

func TestExecuteNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "plain text")
	}))
	defer srv.Close()
	e := decodeEnv(t, execTool(t, srv, nil, "getOrder", `{"id":"x"}`))
	var s string
	if err := json.Unmarshal(e.Body, &s); err != nil || s != "plain text" {
		t.Fatalf("non-JSON body: %s", e.Body)
	}
}

func TestExecuteTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 2<<20)) // 2 MiB of zeros
	}))
	defer srv.Close()
	e := decodeEnv(t, execTool(t, srv, nil, "getOrder", `{"id":"x"}`))
	if !e.Truncated {
		t.Fatal("truncated flag not set")
	}
}

func TestExecuteContextCancellation(t *testing.T) {
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	t.Cleanup(func() { close(gate) })

	tools, _, err := generateTools("orders", []byte(testSpec), srv.URL, nil, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	for i := range tools {
		if tools[i].Name() == "orders__getOrder" {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			res, err := tools[i].Execute(ctx, json.RawMessage(`{"id":"x"}`))
			if err != nil {
				t.Fatalf("want tool error, got transport error: %v", err)
			}
			if res.Error == "" || !strings.Contains(strings.ToLower(res.Error), "context") {
				t.Fatalf("want tool error containing %q, got %q", "context", res.Error)
			}
			return
		}
	}
	t.Fatal("tool getOrder not found")
}

func TestExecuteArrayQueryAndAbsentOptionals(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	defer srv.Close()

	// Array values serialize comma-joined (form style, explode=false).
	decodeEnv(t, execTool(t, srv, nil, "listOrders", `{"limit":1,"status":["open","closed"]}`))
	if !strings.Contains(gotQuery, "status=open%2Cclosed") {
		t.Fatalf("array query not comma-joined: %s", gotQuery)
	}

	// Absent optionals are omitted entirely.
	decodeEnv(t, execTool(t, srv, nil, "listOrders", `{"limit":1}`))
	if strings.Contains(gotQuery, "status=") {
		t.Fatalf("absent optional must be omitted: %s", gotQuery)
	}
}

func TestExecuteStaticContentTypeWins(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	defer srv.Close()

	decodeEnv(t, execTool(t, srv, map[string]string{"Content-Type": "application/vnd.api+json"},
		"createOrder", `{"body":{"item":"widget","qty":1}}`))
	if gotCT != "application/vnd.api+json" {
		t.Fatalf("static Content-Type clobbered: got %q", gotCT)
	}
}

// findRestTool returns the generated tool with the given short name, with the
// production client (newRestClient) injected so the redirect policy is the
// one under test, not httptest's policy-free client.
func findRestTool(t *testing.T, baseURL, short string) restTool {
	t.Helper()
	tools, _, err := generateTools("orders", []byte(testSpec), baseURL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range tools {
		if tools[i].Name() == "orders__"+short {
			tools[i].client = newRestClient()
			return tools[i]
		}
	}
	t.Fatalf("tool %s not found", short)
	return restTool{}
}

func TestExecuteRedirectPolicy(t *testing.T) {
	// Same-host redirect followed.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orders/x" {
			http.Redirect(w, r, "/orders/final", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"redirected":true}`)
	}))
	defer target.Close()
	rt := findRestTool(t, target.URL, "getOrder")
	res, err := rt.Execute(context.Background(), json.RawMessage(`{"id":"x"}`))
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	e := decodeEnv(t, res)
	if e.Status != 200 || !strings.Contains(string(e.Body), "redirected") {
		t.Fatalf("same-host redirect not followed: %+v", e)
	}

	// Cross-host redirect refused.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("cross-host target must not be reached")
	}))
	defer other.Close()
	bouncer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer bouncer.Close()
	rt = findRestTool(t, bouncer.URL, "getOrder")
	res, err = rt.Execute(context.Background(), json.RawMessage(`{"id":"x"}`))
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if res.Error == "" || !strings.Contains(res.Error, "redirect") {
		t.Fatalf("cross-host redirect not refused: %+v", res)
	}
}

// ---- dialOpenAPI / restConn (Task 4) ----

// testSpecToolCount is the number of tools testSpec generates with no
// operations filter (see TestGenerateAllOperations).
const testSpecToolCount = 5

// specAndAPIServer serves the given spec bytes at /openapi.yaml and acts as
// the API itself (200 JSON for everything else). Spec content is swappable
// under a mutex for drift tests.
type specAndAPIServer struct {
	mu   sync.Mutex
	spec []byte
	srv  *httptest.Server
}

func newSpecAndAPIServer(t *testing.T, spec string) *specAndAPIServer {
	t.Helper()
	s := &specAndAPIServer{spec: []byte(spec)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.yaml" {
			s.mu.Lock()
			b := s.spec
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/yaml")
			w.Write(b)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *specAndAPIServer) setSpec(spec string) {
	s.mu.Lock()
	s.spec = []byte(spec)
	s.mu.Unlock()
}

func TestDialOpenAPIFetchesAndGenerates(t *testing.T) {
	s := newSpecAndAPIServer(t, testSpec)
	conn, err := dialOpenAPI(context.Background(), config.GatewayServer{
		Name: "orders", OpenAPI: s.srv.URL + "/openapi.yaml", BaseURL: s.srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := len(conn.Tools()); got != testSpecToolCount {
		t.Fatalf("want %d tools, got %d", testSpecToolCount, got)
	}
	if err := conn.Ping(context.Background()); err != nil {
		t.Fatalf("ping against live API: %v", err)
	}
}

func TestDialOpenAPILocalFile(t *testing.T) {
	s := newSpecAndAPIServer(t, testSpec)
	specPath := filepath.Join(t.TempDir(), "orders.yaml")
	if err := os.WriteFile(specPath, []byte(testSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	conn, err := dialOpenAPI(context.Background(), config.GatewayServer{
		Name: "orders", OpenAPI: specPath, BaseURL: s.srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := len(conn.Tools()); got != testSpecToolCount {
		t.Fatalf("want %d tools, got %d", testSpecToolCount, got)
	}
}

func TestDialOpenAPIBadSpecIsDialError(t *testing.T) {
	s := newSpecAndAPIServer(t, "this is not an openapi document {{{")
	_, err := dialOpenAPI(context.Background(), config.GatewayServer{
		Name: "orders", OpenAPI: s.srv.URL + "/openapi.yaml", BaseURL: s.srv.URL,
	})
	if err == nil {
		t.Fatal("garbage spec must be a dial error")
	}
}

// Spec §8: an operations filter that matches nothing still connects, with an
// empty tool set — it is NOT a dial error (the upstream is healthy; the
// operator just filtered everything out).
func TestDialOpenAPIZeroToolsStillConnects(t *testing.T) {
	s := newSpecAndAPIServer(t, testSpec)
	conn, err := dialOpenAPI(context.Background(), config.GatewayServer{
		Name: "orders", OpenAPI: s.srv.URL + "/openapi.yaml", BaseURL: s.srv.URL,
		Operations: []string{"noSuchOperationId"},
	})
	if err != nil {
		t.Fatalf("zero tools must still connect: %v", err)
	}
	defer conn.Close()
	if got := len(conn.Tools()); got != 0 {
		t.Fatalf("want 0 tools, got %d", got)
	}
	if err := conn.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

// Any HTTP response — 200, 404, 500 — proves reachability and means alive.
// Only transport errors (connection refused) mark the upstream down.
func TestPingSemantics(t *testing.T) {
	for _, status := range []int{200, 404, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		c := &restConn{baseURL: srv.URL, client: srv.Client()}
		if err := c.Ping(context.Background()); err != nil {
			t.Fatalf("status %d must be alive, got %v", status, err)
		}
		srv.Close()
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	c := &restConn{baseURL: dead.URL, client: dead.Client()}
	dead.Close()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("closed server must be a ping error")
	}
}

func TestPingHEADFallsBackToGETOn405(t *testing.T) {
	var sawGET atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case http.MethodGet:
			sawGET.Store(true)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	c := &restConn{baseURL: srv.URL, client: srv.Client()}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("405-on-HEAD must fall back to GET and be alive: %v", err)
	}
	if !sawGET.Load() {
		t.Fatal("GET fallback was not attempted after HEAD 405")
	}
}

// oneOpSpec is the drift target: same API, only listOrders remains.
const oneOpSpec = `
openapi: 3.0.3
info: {title: Orders, version: "2.0"}
paths:
  /orders:
    get:
      operationId: listOrders
      responses: {"200": {description: ok}}
`

// Spec drift: a reconnect re-fetches the spec, so the tool set follows the
// upstream's current document — no gateway restart needed.
func TestManagerReconnectRefetchesSpec(t *testing.T) {
	s := newSpecAndAPIServer(t, testSpec)
	cfg := config.GatewayServer{
		Name: "orders", OpenAPI: s.srv.URL + "/openapi.yaml", BaseURL: s.srv.URL,
	}
	m := NewManager([]config.GatewayServer{cfg},
		WithDial(func(ctx context.Context, s config.GatewayServer) (upstreamConn, error) {
			return dialOpenAPI(ctx, s)
		}),
		WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	waitFor(t, 5*time.Second, func() bool { return len(m.AllTools()) == testSpecToolCount })

	s.setSpec(oneOpSpec)

	// Force a down/redial the same way the tool-execution path would: capture
	// the live conn and report it failed. The supervise loop redials, which
	// re-fetches the (now swapped) spec.
	u := m.ups[0]
	u.mu.Lock()
	observed := u.conn
	u.mu.Unlock()
	m.markDown(u, observed, errors.New("forced for drift test"))

	waitFor(t, 5*time.Second, func() bool { return len(m.AllTools()) == 1 })
	if got := m.AllTools()[0].Name(); got != "orders__listOrders" {
		t.Fatalf("post-drift tool: %q", got)
	}
}

// The spec fetch carries the upstream's configured credentials, so it must
// get the same exact-same-host redirect policy as API calls. A spec URL that
// 302s to another host is a dial error — the cross-host target (which would
// happily serve a valid spec) must never see the request.
func TestFetchSpecRefusesCrossHostRedirect(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("cross-host redirect target must not be reached")
		// Would serve a valid spec — if the redirect were followed, the dial
		// would SUCCEED and the headers would have leaked cross-host.
		w.Header().Set("Content-Type", "application/yaml")
		fmt.Fprint(w, testSpec)
	}))
	defer other.Close()

	bouncer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/openapi.yaml", http.StatusFound)
	}))
	defer bouncer.Close()

	_, err := dialOpenAPI(context.Background(), config.GatewayServer{
		Name: "orders", OpenAPI: bouncer.URL + "/openapi.yaml", BaseURL: bouncer.URL,
		Headers: map[string]string{"X-API-Key": "secret"},
	})
	if err == nil {
		t.Fatal("cross-host spec redirect must be a dial error")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("dial error must mention redirect, got: %v", err)
	}
}

// REST tool names are already "<server>__<tool>" (no mcp__ prefix), so
// renameTools must pass them through unchanged.
func TestRenameToolsPassesRESTNamesThrough(t *testing.T) {
	tools, _, err := generateTools("orders", []byte(testSpec), "http://up:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := make([]tool.Tool, len(tools))
	for i := range tools {
		in[i] = tools[i]
	}
	out := renameTools(in)
	if len(out) != len(in) {
		t.Fatalf("len changed: %d -> %d", len(in), len(out))
	}
	for i := range in {
		if out[i].Name() != in[i].Name() {
			t.Fatalf("name changed: %q -> %q", in[i].Name(), out[i].Name())
		}
	}
}

// Regression: a REST upstream legally named "mcp" generates names like
// "mcp__listOrders". renameTools must branch on the tool's TYPE, not the
// "mcp__" name pattern — a pattern match would strip the server prefix and
// break ForwardsTenant/first-"__"-cut resolution.
func TestRenameToolsRESTServerNamedMCP(t *testing.T) {
	tools, _, err := generateTools("mcp", []byte(testSpec), "http://up:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := make([]tool.Tool, len(tools))
	names := map[string]bool{}
	for i := range tools {
		in[i] = tools[i]
		names[tools[i].Name()] = true
	}
	if !names["mcp__listOrders"] {
		t.Fatalf("precondition: expected generated tool mcp__listOrders, got %v", names)
	}
	out := renameTools(in)
	got := map[string]bool{}
	for _, o := range out {
		got[o.Name()] = true
	}
	if !got["mcp__listOrders"] {
		t.Fatalf("renameTools mangled REST tool from server named mcp: got %v", got)
	}
	for i := range in {
		if out[i].Name() != in[i].Name() {
			t.Fatalf("name changed: %q -> %q", in[i].Name(), out[i].Name())
		}
	}
}
