package sandbox

import "encoding/json"

// tenantKey is the reserved argument the gateway injects for
// forward_tenant upstreams. sandboxd trusts it because it is a stdio child
// reachable only through the gateway.
const tenantKey = "__rt_tenant"

// defaultTenant mirrors Identity M1's absent-tenant rule.
const defaultTenant = "default"

// popTenant extracts and removes the reserved tenant key from raw JSON tool
// arguments, returning the remaining arguments for normal decoding. present
// reports whether the key existed at all (any string value, including "").
// A present-but-empty tenant maps to "default" (the gateway's open mode
// injects ""); an ABSENT key means the call did not come through a
// forward_tenant gateway upstream — NewServer fails closed on that unless
// allowDirect is set.
func popTenant(raw json.RawMessage) (tenant string, present bool, rest json.RawMessage, err error) {
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", false, nil, err
		}
	}
	if m == nil { // raw was JSON null
		m = map[string]any{}
	}
	if v, ok := m[tenantKey].(string); ok {
		tenant = v
		present = true
	}
	delete(m, tenantKey)
	if tenant == "" {
		tenant = defaultTenant
	}
	rest, err = json.Marshal(m)
	return tenant, present, rest, err
}
