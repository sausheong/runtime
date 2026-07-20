package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/obs"
)

// captureCredTool records the credential header landed on ctx by gate #5 so a
// test can observe whether the OBO block injected an on-behalf-of token.
type captureCredTool struct {
	name   string
	ran    bool
	gotHdr string
	gotVal string
}

func (c *captureCredTool) Name() string                           { return c.name }
func (c *captureCredTool) Description() string                    { return "cap " + c.name }
func (c *captureCredTool) Parameters() json.RawMessage            { return json.RawMessage(`{"type":"object"}`) }
func (c *captureCredTool) IsConcurrencySafe(json.RawMessage) bool { return false }
func (c *captureCredTool) Execute(ctx context.Context, _ json.RawMessage) (tool.ToolResult, error) {
	c.ran = true
	c.gotHdr, c.gotVal = CredentialHeaderFrom(ctx)
	return tool.ToolResult{Output: "ok"}, nil
}

// driveOBOGate invokes toolHandler directly (the smallest real Handler surface)
// with the given ctx, isolating gate #5's OBO branch from the HTTP/session
// machinery. builtFor is the mode-qualified view key the server was "built" for.
func driveOBOGate(t *testing.T, h *Handler, builtFor, credSecret, credHeader string, ct tool.Tool, ctx context.Context) *sdk.CallToolResult {
	t.Helper()
	handler := h.toolHandler(builtFor, ct, false, nil, credSecret, credHeader, modeFull)
	res, err := handler(ctx, &sdk.CallToolRequest{
		Params: &sdk.CallToolParamsRaw{Name: ct.Name(), Arguments: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatalf("toolHandler transport error: %v", err)
	}
	return res
}

// Happy path: an OBO cred + a landed caller assertion ⇒ gate #5 mints the
// on-behalf-of token and injects it as the credential header on the ctx the
// upstream tool sees.
func TestOBOGateInjectsCredentialWithAssertion(t *testing.T) {
	var hits int32
	ts := oboTokenServer(t, &hits)
	defer ts.Close()
	src := &fakeOBOSource{cfg: identity.OBOConfig{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"}}

	h := &Handler{OBO: NewOBOManager(context.Background(), src)}
	h.PrincipalFor = func(context.Context) (identity.Principal, bool) {
		return identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, true
	}

	capTool := &captureCredTool{name: "orders__do"}
	ctx := WithCallerAssertion(context.Background(), "alice", "alice.jwt")
	res := driveOBOGate(t, h, "t:acme|full", "orders_obo", "Authorization", capTool, ctx)

	if res.IsError {
		t.Fatalf("call must succeed with a landed assertion: %+v", res.Content)
	}
	if !capTool.ran {
		t.Fatal("upstream tool never ran")
	}
	if capTool.gotHdr != "Authorization" || capTool.gotVal != "Bearer obo-alice.jwt" {
		t.Fatalf("credential header = %q=%q, want Authorization=Bearer obo-alice.jwt",
			capTool.gotHdr, capTool.gotVal)
	}
}

// Fail-closed: an OBO cred with NO landed caller assertion ⇒ the call is
// rejected with "credential unavailable", CredentialError is recorded, and the
// upstream tool is NEVER reached (never dispatch uncredentialed).
func TestOBOGateFailsClosedWithoutAssertion(t *testing.T) {
	var hits int32
	ts := oboTokenServer(t, &hits)
	defer ts.Close()
	src := &fakeOBOSource{cfg: identity.OBOConfig{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"}}

	cm := obs.NewControlMetrics()
	h := &Handler{OBO: NewOBOManager(context.Background(), src), Metrics: cm}
	h.PrincipalFor = func(context.Context) (identity.Principal, bool) {
		return identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, true
	}

	capTool := &captureCredTool{name: "orders__do"}
	// No WithCallerAssertion on ctx ⇒ jwt=="" ⇒ OBOManager.Bearer fails closed.
	res := driveOBOGate(t, h, "t:acme|full", "orders_obo", "Authorization", capTool, context.Background())

	if !res.IsError {
		t.Fatal("call must be rejected without a caller assertion (fail closed)")
	}
	if tc, ok := res.Content[0].(*sdk.TextContent); !ok || !strings.HasPrefix(tc.Text, "credential unavailable: orders_obo") {
		t.Fatalf("wrong reject text: %+v", res.Content[0])
	}
	if capTool.ran {
		t.Fatal("upstream tool must NOT run when the OBO mint fails closed")
	}
	if hits != 0 {
		t.Fatalf("token endpoint hit %d times, want 0 (no assertion ⇒ never exchange)", hits)
	}
	body := scrapeControlRegistry(t, cm)
	want := `runtime_gateway_credential_errors_total{server="orders",tenant="acme"} 1`
	if !strings.Contains(body, want) {
		t.Fatalf("missing credential-error metric %q in scrape:\n%s", want, body)
	}
}

// Superuser is exempt: gate #5's OBO block is skipped entirely (no assertion
// needed, no credential injected), and the call proceeds.
func TestOBOGateSkippedForSuperuser(t *testing.T) {
	var hits int32
	ts := oboTokenServer(t, &hits)
	defer ts.Close()
	src := &fakeOBOSource{cfg: identity.OBOConfig{TokenURL: ts.URL, ClientID: "cid", ClientSecret: "sec"}}

	h := &Handler{OBO: NewOBOManager(context.Background(), src)}
	h.PrincipalFor = func(context.Context) (identity.Principal, bool) {
		return identity.Principal{Superuser: true, Role: identity.RoleAdmin}, true
	}

	capTool := &captureCredTool{name: "orders__do"}
	// Superuser view key is "*"; no assertion on ctx — the block must be skipped.
	res := driveOBOGate(t, h, "*|full", "orders_obo", "Authorization", capTool, context.Background())

	if res.IsError {
		t.Fatalf("superuser call must succeed (OBO block skipped): %+v", res.Content)
	}
	if !capTool.ran {
		t.Fatal("upstream tool never ran for superuser")
	}
	if capTool.gotVal != "" {
		t.Fatalf("superuser must not carry an OBO credential header, got %q", capTool.gotVal)
	}
	if hits != 0 {
		t.Fatalf("token endpoint hit %d times, want 0 (superuser exempt)", hits)
	}
}
