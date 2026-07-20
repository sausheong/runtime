package controlplane

import (
	"context"
	"encoding/json"

	"github.com/sausheong/runtime/internal/eval"
)

// policyResolverAdapter implements PolicyResolver over an eval.PolicyStoreAPI:
// it looks up the per-agent online-eval policy and marshals it to the wire form
// injected as RUNTIME_EVAL_POLICY at spawn. Not found ⇒ ("", nil) (no policy);
// a store error propagates (the Registry read path is fail-open and treats it as
// no policy). Mirrors the SecretBroker adapter shape.
type policyResolverAdapter struct{ ps eval.PolicyStoreAPI }

// NewPolicyResolver wraps an eval.PolicyStoreAPI as a controlplane.PolicyResolver
// for reg.SetPolicyResolver. A nil ps yields a resolver that always reports no
// policy.
func NewPolicyResolver(ps eval.PolicyStoreAPI) PolicyResolver {
	return &policyResolverAdapter{ps: ps}
}

func (a *policyResolverAdapter) PolicyJSON(ctx context.Context, tenant, agentID string) (string, error) {
	if a == nil || a.ps == nil {
		return "", nil
	}
	p, ok, err := a.ps.GetPolicy(ctx, tenant, agentID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
