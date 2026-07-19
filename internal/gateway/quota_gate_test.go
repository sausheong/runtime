package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/quota"
)

// newTestHandlerWithTool builds a Handler over a single fake upstream serving
// one tool cataloged as `name` (e.g. "sbx__run"). It mirrors startCaptureGateway:
// the underlying fakeTool follows the adapter convention "mcp__<server>__<tool>",
// which the Manager strips to the cataloged "<server>__<tool>". Caller sets
// h.PrincipalFor (invokeTool does not touch it, unlike dialGateway).
func newTestHandlerWithTool(t *testing.T, name string) *Handler {
	t.Helper()
	server, _, _ := strings.Cut(name, "__")
	conns := map[string]*fakeConn{server: {tools: []tool.Tool{fakeTool{name: "mcp__" + name, out: "ok"}}}}
	m := startManager(t, []config.GatewayServer{{Name: server, Command: "x"}}, conns)
	return NewHandler(m)
}

// invokeTool drives one end-to-end tools/call against h over a fresh SDK
// session, honoring the h.PrincipalFor the test has already set. Returns the
// tool result (transport errors fail the test).
func invokeTool(t *testing.T, h *Handler, name string) *sdk.CallToolResult {
	t.Helper()
	srv := httptest.NewServer(h.HTTP())
	t.Cleanup(srv.Close)
	cli := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	sess, err := cli.Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: name, Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

// A handler whose quota limiter forbids acme→sbx after 1 call must return the
// quota-exceeded tool error on the 2nd call; superuser is exempt.
func TestQuotaGateRejects(t *testing.T) {
	ctx := context.Background()
	ms := quota.NewMemStore()
	_ = ms.Insert(ctx, quota.Rule{Tenant: "acme", Upstream: "sbx", RatePerMin: 1})
	lim := quota.NewLimiter(ms, 0, nil)

	// Build a handler with a fake single-tool upstream "sbx__run" and an acme
	// operator principal. (Reuse the package's existing test scaffolding for a
	// Handler with a fake upstream + PrincipalFor; see server_test.go helpers.)
	h := newTestHandlerWithTool(t, "sbx__run") // helper: see note below
	h.Quota = lim
	h.PrincipalFor = func(context.Context) (identity.Principal, bool) {
		return identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, true
	}

	call := func() *sdk.CallToolResult {
		return invokeTool(t, h, "sbx__run") // helper: see note below
	}
	if res := call(); res.IsError {
		t.Fatalf("first call must pass: %+v", res.Content)
	}
	res := call()
	if !res.IsError {
		t.Fatal("second call must be quota-rejected")
	}
	if tc, ok := res.Content[0].(*sdk.TextContent); !ok || !strings.HasPrefix(tc.Text, "quota exceeded: acme/sbx") {
		t.Fatalf("wrong reject text: %+v", res.Content[0])
	}

	// Superuser is exempt: never rejected.
	h.PrincipalFor = func(context.Context) (identity.Principal, bool) {
		return identity.Principal{TenantID: "acme", Superuser: true, Role: identity.RoleAdmin}, true
	}
	for i := 0; i < 5; i++ {
		if res := call(); res.IsError {
			t.Fatalf("superuser must be exempt (call %d): %+v", i, res.Content)
		}
	}
}
