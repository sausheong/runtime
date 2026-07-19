package gateway

import (
	"context"
	"net/http/httptest"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/policy"
)

// policyConns exposes one sandbox tool the policy tests target by server name.
func policyConns() map[string]*fakeConn {
	return map[string]*fakeConn{
		"sandbox": {tools: []tool.Tool{fakeTool{name: "mcp__sandbox__run_code", out: "ran"}}},
	}
}

func policyServers() []config.GatewayServer {
	return []config.GatewayServer{{Name: "sandbox", Command: "x"}}
}

const rmForbid = `forbid (principal, action == Gateway::Action::"call_tool", resource)
when { resource.server == "sandbox" && context.input has code && context.input.code like "*rm -rf*" };`

func mustEngine(t *testing.T, src string) *policy.Engine {
	t.Helper()
	e, err := policy.NewEngine([]byte(src), nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func TestPolicyNilEngineUnchanged(t *testing.T) {
	m := startManager(t, policyServers(), policyConns())
	h := NewHandler(m) // Policy nil
	sess := dialGateway(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator})
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "sandbox__run_code", Arguments: map[string]any{"code": "rm -rf /"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("nil engine must not gate; got isError %+v", res.Content)
	}
}

func TestPolicyDenyByArgument(t *testing.T) {
	m := startManager(t, policyServers(), policyConns())
	h := NewHandler(m)
	h.Metrics = obs.NewControlMetrics()
	h.Policy = mustEngine(t, rmForbid)
	sess := dialGateway(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator})

	// Matching argument ⇒ denied with the exact policy id message.
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "sandbox__run_code", Arguments: map[string]any{"code": "rm -rf /"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("matching call must be denied")
	}
	txt, _ := res.Content[0].(*sdk.TextContent)
	if txt == nil || txt.Text != "forbidden by policy: platform/0" {
		t.Fatalf("deny text = %+v, want 'forbidden by policy: platform/0'", res.Content[0])
	}

	// Non-matching argument ⇒ allowed.
	res, err = sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "sandbox__run_code", Arguments: map[string]any{"code": "print(1)"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("non-matching call must pass; got %+v", res.Content)
	}
	// Metric values (allow=1, deny=1) are asserted in the obs package, which
	// has access to the unexported CounterVec (TestPolicyDecision).
}

func TestPolicySearchToolsNotGated(t *testing.T) {
	m := startManager(t, policyServers(), policyConns())
	h := NewHandler(m)
	h.Index = searchIndexForGw() // enable search mode
	h.Metrics = obs.NewControlMetrics()
	// Deny everything.
	h.Policy = mustEngine(t, `forbid (principal, action, resource);`)

	sess := dialSearch(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator})
	// search_tools itself is a read and must NOT be policy-gated.
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "search_tools", Arguments: map[string]any{"query": "run code"},
	})
	if err != nil {
		t.Fatalf("search_tools call: %v", err)
	}
	if res.IsError {
		t.Fatalf("search_tools must not be policy-gated; got %+v", res.Content)
	}
	// A cataloged tool through the same session IS denied.
	res, err = sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "sandbox__run_code", Arguments: map[string]any{"code": "x"},
	})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	if !res.IsError {
		t.Fatal("cataloged tool under deny-all must be denied")
	}
}

// dialSearch connects a client in search mode (?mode=search), mirroring
// dialGateway but appending the mode query param to the endpoint.
func dialSearch(t *testing.T, h *Handler, p *identity.Principal) *sdk.ClientSession {
	t.Helper()
	h.PrincipalFor = func(_ context.Context) (identity.Principal, bool) {
		if p == nil {
			return identity.Principal{}, false
		}
		return *p, true
	}
	srv := httptest.NewServer(h.HTTP())
	t.Cleanup(srv.Close)
	cli := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	sess, err := cli.Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: srv.URL + "?mode=search"}, nil)
	if err != nil {
		t.Fatalf("connect search: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}
