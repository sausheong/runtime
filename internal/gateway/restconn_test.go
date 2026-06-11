package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sausheong/harness/tool"
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
