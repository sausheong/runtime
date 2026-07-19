package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/rheader"
)

// This is the M2a acceptance test: it drives a real HTTP tools/call through the
// gateway's HTTP() wrapper (where the re-verify+tenant-bind hook lives) and
// asserts what landed at the tool-dispatch point (gate #5) via
// CallerAssertionFrom. Because M2a lands nothing user-visible, this end-to-end
// proof of the gateway landing — including the fail-closed cases — IS its
// acceptance. Hermetic: no Postgres — the re-verify uses a fake OIDCVerifier +
// fake UserTenantSource, and a fake tool observes the landed ctx.

// fakeAssertionVerifier is a fake identity.OIDCVerifier: "good.jwt"→alice,
// "bob.jwt"→bob, everything else → error (fail-closed source).
type fakeAssertionVerifier struct{}

func (fakeAssertionVerifier) Verify(_ context.Context, raw string) (string, error) {
	switch raw {
	case "good.jwt":
		return "alice", nil
	case "bob.jwt":
		return "bob", nil
	default:
		return "", errors.New("bad")
	}
}

// fakeUserTenantSource is a fake gateway.UserTenantSource: alice→tenant acme,
// bob→tenant other (mismatch case), anything else → no rows (0-row no-match).
type fakeUserTenantSource struct{}

func (fakeUserTenantSource) UsersBySubject(_ context.Context, sub string) ([]identity.UserRow, error) {
	switch sub {
	case "alice":
		return []identity.UserRow{{TenantID: "acme", Subject: "alice"}}, nil
	case "bob":
		return []identity.UserRow{{TenantID: "other", Subject: "bob"}}, nil
	default:
		return nil, nil
	}
}

// landed records what CallerAssertionFrom(ctx) saw at dispatch.
type landed struct {
	subject string
	jwt     string
	ok      bool
}

// observerTool is a tool.Tool whose Execute records gateway.CallerAssertionFrom
// of the ctx the SDK propagated to the dispatch point (gate #5).
type observerTool struct {
	name string
	mu   sync.Mutex
	seen landed
}

func (o *observerTool) Name() string                           { return o.name }
func (o *observerTool) Description() string                    { return "observer " + o.name }
func (o *observerTool) Parameters() json.RawMessage            { return json.RawMessage(`{"type":"object"}`) }
func (o *observerTool) IsConcurrencySafe(json.RawMessage) bool { return false }
func (o *observerTool) Execute(ctx context.Context, _ json.RawMessage) (tool.ToolResult, error) {
	sub, jwt, ok := CallerAssertionFrom(ctx)
	o.mu.Lock()
	o.seen = landed{subject: sub, jwt: jwt, ok: ok}
	o.mu.Unlock()
	return tool.ToolResult{Output: "ok"}, nil
}

func (o *observerTool) captured() landed {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seen
}

// fixedAssertionRoundTripper sets X-Runtime-Assertion to a fixed value (mirrors
// Task 5's assertionRoundTripper, but with a test-controlled fixed value so each
// case can drive an exact inbound header). An empty value sets no header — the
// "no header" case.
type fixedAssertionRoundTripper struct {
	base  http.RoundTripper
	value string
}

func (t fixedAssertionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.value != "" {
		r = r.Clone(r.Context())
		r.Header.Set(rheader.Assertion, t.value)
	}
	return base.RoundTrip(r)
}

// dialGatewayWithAssertion connects an SDK client to h.HTTP() with the agent
// principal p injected, and an HTTP client that stamps X-Runtime-Assertion:
// headerVal (empty ⇒ no header) on every outbound request — including the
// tools/call. This exercises the real HTTP() wrapper end-to-end.
func dialGatewayWithAssertion(t *testing.T, h *Handler, p *identity.Principal, headerVal string) *sdk.ClientSession {
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
		HTTPClient: &http.Client{Transport: fixedAssertionRoundTripper{value: headerVal}},
	}
	sess, err := cli.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// startObserverGateway wires a single open upstream "obs" serving obs__probe
// (the observer tool) and returns the handler + the observer.
func startObserverGateway(t *testing.T) (*Handler, *observerTool) {
	t.Helper()
	obs := &observerTool{name: "mcp__obs__probe"}
	conns := map[string]*fakeConn{"obs": {tools: []tool.Tool{obs}}}
	m := startManager(t, []config.GatewayServer{{Name: "obs", Command: "x"}}, conns)
	return NewHandler(m), obs
}

// callProbe drives a real tools/call for obs__probe and returns what the tool
// observed at dispatch.
func callProbe(t *testing.T, sess *sdk.ClientSession, obs *observerTool) landed {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "obs__probe", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res.Content)
	}
	return obs.captured()
}

func TestOBOCallerAssertionE2E(t *testing.T) {
	acme := &identity.Principal{TenantID: "acme", Role: identity.RoleOperator}

	// 1. Lands on verify+match: agent tenant acme; header good.jwt→alice→acme;
	//    Assertion+Users wired ⇒ dispatch sees ok=true, alice, good.jwt.
	t.Run("lands_on_verify_and_match", func(t *testing.T) {
		h, obs := startObserverGateway(t)
		h.Assertion = fakeAssertionVerifier{}
		h.Users = fakeUserTenantSource{}
		sess := dialGatewayWithAssertion(t, h, acme, "good.jwt")
		got := callProbe(t, sess, obs)
		if !got.ok || got.subject != "alice" || got.jwt != "good.jwt" {
			t.Fatalf("want ok=true subject=alice jwt=good.jwt, got %+v", got)
		}
	})

	// 2. Absent when no header: wired, but no X-Runtime-Assertion ⇒ ok=false.
	t.Run("absent_when_no_header", func(t *testing.T) {
		h, obs := startObserverGateway(t)
		h.Assertion = fakeAssertionVerifier{}
		h.Users = fakeUserTenantSource{}
		sess := dialGatewayWithAssertion(t, h, acme, "") // no header
		got := callProbe(t, sess, obs)
		if got.ok {
			t.Fatalf("no header must not land an assertion, got %+v", got)
		}
	})

	// 3. Fail-closed on invalid JWT: verifier errors on bad.jwt ⇒ ok=false.
	t.Run("fail_closed_invalid_jwt", func(t *testing.T) {
		h, obs := startObserverGateway(t)
		h.Assertion = fakeAssertionVerifier{}
		h.Users = fakeUserTenantSource{}
		sess := dialGatewayWithAssertion(t, h, acme, "bad.jwt")
		got := callProbe(t, sess, obs)
		if got.ok {
			t.Fatalf("invalid JWT must fail closed, got %+v", got)
		}
	})

	// 4. Fail-closed on tenant mismatch: bob.jwt→bob→tenant other; agent tenant
	//    acme ⇒ ok=false.
	t.Run("fail_closed_tenant_mismatch", func(t *testing.T) {
		h, obs := startObserverGateway(t)
		h.Assertion = fakeAssertionVerifier{}
		h.Users = fakeUserTenantSource{}
		sess := dialGatewayWithAssertion(t, h, acme, "bob.jwt")
		got := callProbe(t, sess, obs)
		if got.ok {
			t.Fatalf("tenant mismatch must fail closed, got %+v", got)
		}
	})

	// 5. Inert when unwired: Assertion nil (M2a off) ⇒ even with good.jwt,
	//    ok=false — today's behavior. This is also the structural no-at-rest
	//    anchor: with the hook off the JWT never enters the gateway ctx at all,
	//    so it cannot reach any persistence path. (See no-at-rest note below.)
	t.Run("inert_when_unwired", func(t *testing.T) {
		h, obs := startObserverGateway(t)
		// h.Assertion intentionally left nil (h.Users irrelevant).
		sess := dialGatewayWithAssertion(t, h, acme, "good.jwt")
		got := callProbe(t, sess, obs)
		if got.ok {
			t.Fatalf("unwired handler must be inert (M2a off), got %+v", got)
		}
	})
}

// No-at-rest (Global Constraint): the caller JWT is a bearer secret that must
// never be persisted. M2a keeps it out of persistence STRUCTURALLY, verified at
// two layers:
//
//   - Agentd layer (Task 4): the JWT rides ctx only via identity.WithAssertion;
//     it is NOT a field of the checkpointed turnInput and is bridged through an
//     ephemeral per-session sync.Map that is deleted on session-workflow exit —
//     so DBOS/session-store checkpoints never contain it. (turnInput has no
//     assertion field: it structurally cannot carry the JWT.)
//   - Gateway layer (this test, Task 6): the re-verified assertion lands only on
//     the request ctx (WithCallerAssertion) at dispatch — request-scoped memory,
//     never written anywhere durable.
//
// A hermetic gateway-level test cannot inspect DB rows, but assertion #5
// (inert_when_unwired) proves the fail-closed default: with the hook off the
// JWT is dropped at the gateway boundary and never enters the landed ctx, let
// alone any store. Combined with the structural turnInput guarantee (Task 4),
// there is no code path that persists the caller JWT.
