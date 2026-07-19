package policy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
)

func principal(tenant, subject string) identity.Principal {
	return identity.Principal{TenantID: tenant, Subject: subject, Role: identity.RoleOperator}
}

func call(toolName string, args string) Request {
	return Request{Principal: principal("acme", "svc-a"), OK: true,
		ToolName: toolName, Args: json.RawMessage(args), Mode: "full"}
}

func TestPermitByDefault(t *testing.T) {
	e, err := NewEngine(nil, nil) // no platform policies, no tenant layer
	if err != nil {
		t.Fatal(err)
	}
	d := e.Evaluate(context.Background(), call("sandbox__run_code", `{"code":"print(1)"}`))
	if !d.Allow || d.Err != nil {
		t.Errorf("want allow with no policies, got %+v", d)
	}
}

func TestPlatformForbidOnArgs(t *testing.T) {
	src := []byte(`forbid (principal, action == Gateway::Action::"call_tool", resource)
when { resource.server == "sandbox" && context.input has code && context.input.code like "*rm -rf*" };`)
	e, err := NewEngine(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := e.Evaluate(context.Background(), call("sandbox__run_code", `{"code":"rm -rf /tmp/x"}`))
	if d.Allow {
		t.Error("matching forbid must deny")
	}
	if d.PolicyID != "platform/0" {
		t.Errorf("PolicyID = %q, want platform/0", d.PolicyID)
	}
	d = e.Evaluate(context.Background(), call("sandbox__run_code", `{"code":"print(1)"}`))
	if !d.Allow {
		t.Errorf("non-matching call must pass: %+v", d)
	}
	// other servers unaffected
	d = e.Evaluate(context.Background(), call("browser__navigate", `{"url":"http://x"}`))
	if !d.Allow {
		t.Errorf("other server must pass: %+v", d)
	}
}

func TestPrincipalAttributes(t *testing.T) {
	src := []byte(`forbid (principal, action, resource)
when { principal.subject != "svc-ops" && resource.server == "browser" };`)
	e, err := NewEngine(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := e.Evaluate(context.Background(), Request{Principal: principal("acme", "svc-a"), OK: true,
		ToolName: "browser__navigate", Args: nil, Mode: "full"}); d.Allow {
		t.Error("non-ops subject must be denied browser tools")
	}
	if d := e.Evaluate(context.Background(), Request{Principal: principal("acme", "svc-ops"), OK: true,
		ToolName: "browser__navigate", Args: nil, Mode: "full"}); !d.Allow {
		t.Errorf("ops subject must pass: %+v", d)
	}
}

func TestOpenModePrincipal(t *testing.T) {
	// Platform policies apply in open mode via the synthetic principal.
	src := []byte(`forbid (principal == Runtime::Key::"open/anonymous", action, resource)
when { resource.server == "sandbox" };`)
	e, err := NewEngine(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := e.Evaluate(context.Background(), Request{OK: false, ToolName: "sandbox__run_code",
		Args: json.RawMessage(`{}`), Mode: "full"})
	if d.Allow {
		t.Error("open-mode call must match the open/anonymous principal")
	}
}

func TestNilAndNonObjectArgs(t *testing.T) {
	// input must be {} for nil, empty, `null`, and non-object args — and a
	// `context.input has code` policy must simply not match (not error).
	src := []byte(`forbid (principal, action, resource)
when { context.input has code };`)
	e, err := NewEngine(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{"", "null", "[1,2]", `"str"`} {
		d := e.Evaluate(context.Background(), call("sandbox__run_code", args))
		if !d.Allow || d.Err != nil {
			t.Errorf("args %q: want allow (input coerced to {}), got %+v", args, d)
		}
	}
}

func TestMalformedArgsJSONFailsClosed(t *testing.T) {
	e, err := NewEngine(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := e.Evaluate(context.Background(), call("sandbox__run_code", `{bad json`))
	if d.Allow || d.Err == nil {
		t.Errorf("malformed args must fail closed with Err, got %+v", d)
	}
}

func TestPlatformParseErrorFailsConstruction(t *testing.T) {
	if _, err := NewEngine([]byte(`this is not cedar`), nil); err == nil {
		t.Error("platform parse error must fail NewEngine")
	}
}

func TestSuperuserSubjectToPlatformLayer(t *testing.T) {
	src := []byte(`forbid (principal, action, resource) when { resource.server == "sandbox" };`)
	e, err := NewEngine(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	su := identity.Principal{Subject: "root", Superuser: true}
	d := e.Evaluate(context.Background(), Request{Principal: su, OK: true,
		ToolName: "sandbox__run_code", Args: json.RawMessage(`{}`), Mode: "full"})
	if d.Allow {
		t.Error("superuser must NOT bypass platform forbids")
	}
}

func TestMultiForbidDeterministicPolicyID(t *testing.T) {
	// Two forbids that BOTH match: the reported PolicyID must be the
	// smallest id every time, not map-iteration luck.
	src := []byte(`forbid (principal, action, resource) when { resource.server == "sandbox" };
forbid (principal, action, resource) when { context.input has code };`)
	e, err := NewEngine(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		d := e.Evaluate(context.Background(), call("sandbox__run_code", `{"code":"x"}`))
		if d.Allow {
			t.Fatal("must deny")
		}
		if d.PolicyID != "platform/0" {
			t.Fatalf("run %d: PolicyID = %q, want stable platform/0", i, d.PolicyID)
		}
	}
}
