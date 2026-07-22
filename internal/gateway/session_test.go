package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/rheader"
)

func TestInjectSessionStripsAndSets(t *testing.T) {
	// A caller-supplied __rt_session must be overwritten with the platform value.
	raw := json.RawMessage(`{"x":1,"__rt_session":"attacker"}`)
	out, err := injectSession(raw, "real-sess")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["__rt_session"] != "real-sess" {
		t.Fatalf("__rt_session = %v, want real-sess", m["__rt_session"])
	}
	if m["x"] != float64(1) {
		t.Fatalf("payload dropped: %v", m)
	}
}

func TestInjectSessionNullPayload(t *testing.T) {
	out, err := injectSession(json.RawMessage(`null`), "s")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["__rt_session"] != "s" {
		t.Fatalf("__rt_session = %v, want s", m["__rt_session"])
	}
}

func TestSessionForwardCarrier(t *testing.T) {
	if _, ok := SessionForwardFrom(context.Background()); ok {
		t.Fatal("bare ctx should have no session")
	}
	ctx := WithSessionForward(context.Background(), "abc")
	got, ok := SessionForwardFrom(ctx)
	if !ok || got != "abc" {
		t.Fatalf("SessionForwardFrom = %q,%v want abc,true", got, ok)
	}
}

// sessionRoundTripper stamps X-Runtime-Session on every outbound request
// (mirrors fixedAssertionRoundTripper), so the gate-order test can drive the
// real HTTP() header-landing path end-to-end. Empty value ⇒ no header.
type sessionRoundTripper struct {
	base  http.RoundTripper
	value string
}

func (t sessionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.value != "" {
		r = r.Clone(r.Context())
		r.Header.Set(rheader.Session, t.value)
	}
	return base.RoundTrip(r)
}

// dialGatewaySession connects an SDK client to h.HTTP() with principal p and an
// HTTP client that stamps X-Runtime-Session: sessionID on every outbound request
// (empty ⇒ no header). Exercises the real HTTP() wrapper + injection path.
func dialGatewaySession(t *testing.T, h *Handler, p *identity.Principal, sessionID string) *sdk.ClientSession {
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
	transport := &sdk.StreamableClientTransport{
		Endpoint:   srv.URL,
		HTTPClient: &http.Client{Transport: sessionRoundTripper{value: sessionID}},
	}
	sess, err := cli.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// sessionCaptureGateway wires a single forward_tenant upstream "sbx" serving
// sbx__run and returns the handler + the capture tool that records the exact
// args the upstream received (post-injection).
func sessionCaptureGateway(t *testing.T) (*Handler, *captureTool) {
	t.Helper()
	ct := &captureTool{name: "mcp__sbx__run"}
	conns := map[string]*fakeConn{"sbx": {tools: []tool.Tool{ct}}}
	m := startManager(t, []config.GatewayServer{
		{Name: "sbx", Command: "x", ForwardTenant: true},
	}, conns)
	return NewHandler(m), ct
}

// TestSessionInjectedAfterPolicyGate proves the ordering constraint: the Cedar
// policy engine (gate #3) evaluates the agent's RAW args — WITHOUT __rt_session —
// while the upstream receives args WITH the platform-injected __rt_session.
//
// The proof uses a policy that forbids ONLY when context.input has __rt_session
// as a recorder of what the policy saw:
//   - clean-args call + forwarded session ⇒ ALLOWED (policy saw no __rt_session,
//     so it did NOT fire) AND the upstream captured __rt_session=the forwarded id
//     (injection happened, strictly AFTER the gate);
//   - control call whose RAW args already contain __rt_session ⇒ DENIED, proving
//     the policy genuinely inspects that key — so the allow above is load-bearing,
//     not a policy that ignores the field.
func TestSessionInjectedAfterPolicyGate(t *testing.T) {
	const forbidSession = `forbid (principal, action == Gateway::Action::"call_tool", resource)
when { context.input has __rt_session };`

	// (a) Clean raw args + forwarded session: policy must NOT fire (allow),
	// upstream must receive the injected session.
	h, ct := sessionCaptureGateway(t)
	h.Metrics = obs.NewControlMetrics()
	h.Policy = mustEngine(t, forbidSession)
	sess := dialGatewaySession(t, h,
		&identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, "sess-42")
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "sbx__run", Arguments: json.RawMessage(`{"x":1}`),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("policy saw __rt_session in raw args (denied) — injection ran BEFORE the gate: %+v", res.Content)
	}
	var got map[string]any
	if err := json.Unmarshal(ct.captured(), &got); err != nil {
		t.Fatalf("captured args not JSON: %v (%s)", err, ct.captured())
	}
	if got["__rt_session"] != "sess-42" {
		t.Fatalf("upstream __rt_session = %v, want sess-42 (injection did not run)", got["__rt_session"])
	}
	if got["__rt_tenant"] != "acme" {
		t.Fatalf("upstream __rt_tenant = %v, want acme", got["__rt_tenant"])
	}

	// (b) Control: raw args already carry __rt_session ⇒ the policy DOES fire,
	// confirming it inspects the key (so the allow in (a) means the gate ran on
	// clean args, not that the policy is inert). Fresh handler: the SDK server is
	// per-view cached, and a prior denied call on the same session is fine, but a
	// clean fixture keeps the two cases independent.
	h2, _ := sessionCaptureGateway(t)
	h2.Metrics = obs.NewControlMetrics()
	h2.Policy = mustEngine(t, forbidSession)
	sess2 := dialGatewaySession(t, h2,
		&identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, "sess-42")
	res2, err := sess2.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "sbx__run", Arguments: json.RawMessage(`{"x":1,"__rt_session":"attacker"}`),
	})
	if err != nil {
		t.Fatalf("control call: %v", err)
	}
	if !res2.IsError {
		t.Fatal("control: policy must fire when raw args contain __rt_session (proves the policy inspects the key)")
	}
}
