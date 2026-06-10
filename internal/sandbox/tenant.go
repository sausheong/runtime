package sandbox

import "encoding/json"

// tenantKey is the reserved argument the gateway injects for
// forward_tenant upstreams. sandboxd trusts it because it is a stdio child
// reachable only through the gateway.
const tenantKey = "__rt_tenant"

// defaultTenant mirrors Identity M1's absent-tenant rule.
const defaultTenant = "default"

// popTenant extracts and removes the reserved tenant key from raw JSON tool
// arguments, returning the remaining arguments for normal decoding. An empty
// or absent tenant maps to "default".
func popTenant(raw json.RawMessage) (tenant string, rest json.RawMessage, err error) {
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", nil, err
		}
	}
	if m == nil { // raw was JSON null
		m = map[string]any{}
	}
	if v, ok := m[tenantKey].(string); ok {
		tenant = v
	}
	delete(m, tenantKey)
	if tenant == "" {
		tenant = defaultTenant
	}
	rest, err = json.Marshal(m)
	return tenant, rest, err
}
