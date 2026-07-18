// Package policy provides deterministic Cedar authorization for gateway
// tool calls: a platform layer (operator .cedar file, immutable at runtime)
// plus an optional tenant layer (DB-backed, per-tenant compiled cache).
// Semantics are permit-by-default: an injected baseline permit allows any
// call no forbid matches, and Cedar's forbid-overrides rule makes a matched
// forbid in EITHER layer deny. Every failure path denies (fail-closed).
package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	cedar "github.com/cedar-policy/cedar-go"
	ctypes "github.com/cedar-policy/cedar-go/types"

	"github.com/sausheong/runtime/internal/identity"
)

// Request describes one gateway tool call to authorize.
type Request struct {
	Principal identity.Principal
	OK        bool   // false ⇒ open mode
	ToolName  string // federated "<server>__<tool>"
	Args      json.RawMessage
	Mode      string // "full" | "search"
}

// Decision is the outcome of Evaluate.
type Decision struct {
	Allow    bool
	PolicyID string // first matched forbid id on deny; "" on allow/error
	Err      error  // non-nil ⇒ Allow false (fail-closed evaluation error)
}

// TenantPolicies supplies compiled tenant-layer policy sets. Implemented by
// the Store in Task 3; nil ⇒ platform layer only (M1 mode).
type TenantPolicies interface {
	// PoliciesFor returns the tenant's policies as (id, cedar source) pairs
	// and a generation counter that moves on any mutation.
	PoliciesFor(ctx context.Context, tenant string) ([]NamedPolicy, uint64, error)
}

// NamedPolicy is one tenant-layer Cedar policy statement with a stable id.
type NamedPolicy struct {
	ID     string // "tenant/<name>"
	Source string // one Cedar policy statement
}

// Engine evaluates gateway tool calls. Construct with NewEngine; nil
// tenants ⇒ platform layer only.
type Engine struct {
	platform []namedCompiled // in file order; IDs platform/<n>
	tenants  TenantPolicies

	mu    sync.Mutex
	cache map[string]tenantCompiled // tenant -> compiled set + generation
}

type namedCompiled struct {
	id  string
	pol *cedar.Policy
}

type tenantCompiled struct {
	gen  uint64
	pols []namedCompiled
}

// baselinePermit implements permit-by-default. Compiled once at package
// init; parse of a constant cannot fail.
var baselinePermit = func() *cedar.Policy {
	var p cedar.Policy
	if err := p.UnmarshalCedar([]byte(`permit (principal, action, resource);`)); err != nil {
		panic("policy: baseline permit failed to parse: " + err.Error())
	}
	return &p
}()

// NewEngine compiles the platform layer. A parse error is returned (the
// caller — runtimed boot — must treat it as fatal: a broken guardrail file
// must never mean "no guardrails").
func NewEngine(platformSrc []byte, tenants TenantPolicies) (*Engine, error) {
	e := &Engine{tenants: tenants, cache: map[string]tenantCompiled{}}
	if len(platformSrc) > 0 {
		ps, err := cedar.NewPolicySetFromBytes("platform.cedar", platformSrc)
		if err != nil {
			return nil, fmt.Errorf("policy: platform file: %w", err)
		}
		e.platform = rekeyPlatform(ps)
	}
	return e, nil
}

// rekeyPlatform converts a parsed document PolicySet into ordered
// namedCompiled entries with ids platform/0..platform/n. cedar-go assigns
// document ids "policy0".."policyN" in source order; we walk that index.
func rekeyPlatform(ps *cedar.PolicySet) []namedCompiled {
	var out []namedCompiled
	for i := 0; ; i++ {
		p := ps.Get(cedar.PolicyID(fmt.Sprintf("policy%d", i)))
		if p == nil {
			break
		}
		out = append(out, namedCompiled{id: fmt.Sprintf("platform/%d", i), pol: p})
	}
	return out
}

// Evaluate authorizes one tool call. Any error ⇒ Decision{Allow:false, Err}.
func (e *Engine) Evaluate(ctx context.Context, req Request) (d Decision) {
	defer func() {
		if r := recover(); r != nil {
			d = Decision{Allow: false, Err: fmt.Errorf("policy: evaluation panic: %v", r)}
		}
	}()

	// Assemble the PolicySet: baseline permit + platform + tenant layer.
	ps := cedar.NewPolicySet()
	ps.Add("baseline/permit", baselinePermit)
	for _, nc := range e.platform {
		ps.Add(cedar.PolicyID(nc.id), nc.pol)
	}
	tenant := ""
	if req.OK && !req.Principal.Superuser {
		tenant = req.Principal.TenantID
	}
	if e.tenants != nil && tenant != "" {
		pols, err := e.tenantSet(ctx, tenant)
		if err != nil {
			return Decision{Allow: false, Err: fmt.Errorf("policy: tenant %q load: %w", tenant, err)}
		}
		for _, nc := range pols {
			ps.Add(cedar.PolicyID(nc.id), nc.pol)
		}
	}

	creq, entities, err := cedarRequest(req)
	if err != nil {
		return Decision{Allow: false, Err: err}
	}
	decision, diag := cedar.Authorize(ps, entities, creq)
	// Policies that error during evaluation are skipped by cedar.Authorize;
	// treat any eval error as fail-closed rather than silently narrowing
	// the forbid set.
	if len(diag.Errors) > 0 {
		return Decision{Allow: false, Err: fmt.Errorf("policy: evaluation: %s", diag.Errors[0].Message)}
	}
	if decision == cedar.Deny {
		id := ""
		if len(diag.Reasons) > 0 {
			id = string(diag.Reasons[0].PolicyID)
		}
		return Decision{Allow: false, PolicyID: id}
	}
	return Decision{Allow: true}
}

// tenantSet returns the compiled tenant layer, recompiling when the store
// generation has moved (same cache idiom as the gateway Handler's server
// cache).
func (e *Engine) tenantSet(ctx context.Context, tenant string) ([]namedCompiled, error) {
	named, gen, err := e.tenants.PoliciesFor(ctx, tenant)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, hit := e.cache[tenant]; hit && c.gen == gen {
		return c.pols, nil
	}
	pols := make([]namedCompiled, 0, len(named))
	for _, np := range named {
		var p cedar.Policy
		if err := p.UnmarshalCedar([]byte(np.Source)); err != nil {
			// Store validates on write, so this indicates corruption:
			// fail closed for the whole tenant rather than skipping rules.
			return nil, fmt.Errorf("compile %s: %w", np.ID, err)
		}
		pols = append(pols, namedCompiled{id: np.ID, pol: &p})
	}
	e.cache[tenant] = tenantCompiled{gen: gen, pols: pols}
	return pols, nil
}

// cedarRequest maps a gateway call onto the Cedar model (see spec):
// principal Runtime::Key::"<tenant>/<subject>" {tenant,subject,role,superuser},
// action Gateway::Action::"call_tool",
// resource Gateway::Tool::"<full>" {server,tool},
// context {input: <args object>, mode}.
func cedarRequest(req Request) (cedar.Request, ctypes.EntityGetter, error) {
	tenant, subject, role, super := "", "anonymous", "operator", false
	if req.OK {
		tenant, subject = req.Principal.TenantID, req.Principal.Subject
		role, super = string(req.Principal.Role), req.Principal.Superuser
	}
	pid := tenant + "/" + subject
	if !req.OK {
		pid = "open/anonymous"
	}
	principalUID := cedar.NewEntityUID("Runtime::Key", cedar.String(pid))
	server, _, _ := strings.Cut(req.ToolName, "__")
	resourceUID := cedar.NewEntityUID("Gateway::Tool", cedar.String(req.ToolName))

	// context.input: the caller's raw argument OBJECT; anything else ⇒ {}.
	inputVal := cedar.Value(cedar.NewRecord(cedar.RecordMap{}))
	if len(req.Args) > 0 {
		var probe map[string]any
		if err := json.Unmarshal(req.Args, &probe); err == nil && probe != nil {
			var v cedar.Value
			if err := ctypes.UnmarshalJSON(req.Args, &v); err != nil {
				return cedar.Request{}, nil, fmt.Errorf("policy: args to cedar value: %w", err)
			}
			inputVal = v
		} else if err != nil && !isJSONNonObject(req.Args) {
			return cedar.Request{}, nil, fmt.Errorf("policy: malformed arguments JSON: %w", err)
		}
	}

	entities := cedar.EntityMap{
		principalUID: cedar.Entity{UID: principalUID, Attributes: cedar.NewRecord(cedar.RecordMap{
			"tenant": cedar.String(tenant), "subject": cedar.String(subject),
			"role": cedar.String(role), "superuser": cedar.Boolean(super),
		})},
		resourceUID: cedar.Entity{UID: resourceUID, Attributes: cedar.NewRecord(cedar.RecordMap{
			"server": cedar.String(server), "tool": cedar.String(req.ToolName),
		})},
	}
	return cedar.Request{
		Principal: principalUID,
		Action:    cedar.NewEntityUID("Gateway::Action", "call_tool"),
		Resource:  resourceUID,
		Context: cedar.NewRecord(cedar.RecordMap{
			"input": inputVal, "mode": cedar.String(req.Mode),
		}),
	}, entities, nil
}

// isJSONNonObject reports whether raw parses as VALID JSON that is not an
// object (null, array, string, number, bool) — those coerce to input:{}
// rather than failing closed.
func isJSONNonObject(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	_, isObj := v.(map[string]any)
	return !isObj
}
