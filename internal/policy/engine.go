// Package policy provides deterministic Cedar authorization for gateway
// tool calls: a platform layer (operator .cedar file, immutable at runtime)
// plus an optional tenant layer (DB-backed, per-tenant compiled cache).
// Semantics are permit-by-default: an injected baseline permit allows any
// call no forbid matches, and Cedar's forbid-overrides rule makes a matched
// forbid in EITHER layer deny. Every failure path denies (fail-closed).
package policy

import (
	"bytes"
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
		// diag.Reasons follows PolicySet map-iteration order, which is
		// nondeterministic. Pick the smallest id so the same denied call
		// always reports the same policy (stable audit trail across
		// replicas and re-runs).
		id := ""
		for _, r := range diag.Reasons {
			if id == "" || string(r.PolicyID) < id {
				id = string(r.PolicyID)
			}
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
	// The args are decoded with PLAIN encoding/json and mapped to Cedar values
	// by jsonToCedar. We deliberately do NOT use cedar-go's types.UnmarshalJSON
	// here: that reads Cedar's JSON *dialect*, in which a top-level object with
	// a "__entity"/"__extn" key is an entity/extension reference — every other
	// key is discarded. An agent could then append `"__entity":{...}` to make
	// `context.input` a bare entity ref, so `context.input has <k>` is false
	// and an argument-inspecting forbid silently misses while the upstream
	// still receives the real payload. jsonToCedar treats every key literally.
	inputVal := cedar.Value(cedar.NewRecord(cedar.RecordMap{}))
	if len(req.Args) > 0 {
		dec := json.NewDecoder(bytes.NewReader(req.Args))
		dec.UseNumber()
		var raw any
		if err := dec.Decode(&raw); err != nil {
			return cedar.Request{}, nil, fmt.Errorf("policy: malformed arguments JSON: %w", err)
		}
		// Only a JSON object becomes the input record; any other shape
		// (null/array/scalar) coerces to {} so `has`-style policies simply
		// don't match rather than erroring.
		if obj, ok := raw.(map[string]any); ok {
			v, err := jsonToCedar(obj)
			if err != nil {
				return cedar.Request{}, nil, fmt.Errorf("policy: args to cedar value: %w", err)
			}
			inputVal = v
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

// jsonToCedar converts a plain-JSON value (as produced by encoding/json with
// UseNumber) into a cedar.Value, mapping types literally:
//   - object → Record (every key kept verbatim; "__entity"/"__extn" are just
//     ordinary keys, never entity/extension references)
//   - array  → Set
//   - json.Number → Long when integral, else Decimal (so floats and large
//     numbers never fail closed under permit-by-default)
//   - string → String, bool → Boolean, null → empty String
//
// The number policy: Cedar has no float type, only Long and Decimal. An
// integral value that fits int64 becomes a Long (so `>` / `==` comparisons in
// policies work). Anything else (fractional, or beyond int64) becomes a
// Decimal when representable, else its String form — never an error, so a
// benign `temperature: 0.7` arg cannot break a call when no policy even
// inspects it.
func jsonToCedar(v any) (ctypes.Value, error) {
	switch t := v.(type) {
	case nil:
		return ctypes.String(""), nil
	case bool:
		return ctypes.Boolean(t), nil
	case string:
		return ctypes.String(t), nil
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return ctypes.Long(i), nil
		}
		if f, err := t.Float64(); err == nil {
			if d, derr := ctypes.NewDecimalFromFloat(f); derr == nil {
				return d, nil
			}
		}
		return ctypes.String(t.String()), nil
	case map[string]any:
		m := ctypes.RecordMap{}
		for k, val := range t {
			cv, err := jsonToCedar(val)
			if err != nil {
				return nil, err
			}
			m[ctypes.String(k)] = cv
		}
		return ctypes.NewRecord(m), nil
	case []any:
		vals := make([]ctypes.Value, 0, len(t))
		for _, el := range t {
			cv, err := jsonToCedar(el)
			if err != nil {
				return nil, err
			}
			vals = append(vals, cv)
		}
		return ctypes.NewSet(vals...), nil
	default:
		return nil, fmt.Errorf("unsupported JSON type %T", v)
	}
}
