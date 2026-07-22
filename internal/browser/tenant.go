package browser

import "encoding/json"

// tenantKey and sessionKey are the reserved arguments the gateway injects for
// forward_tenant upstreams. browserd trusts them because it is a stdio child
// reachable only through the gateway.
const (
	tenantKey  = "__rt_tenant"
	sessionKey = "__rt_session"
)

// defaultTenant mirrors Identity M1's absent-tenant rule.
const defaultTenant = "default"

// popReserved extracts and removes the reserved gateway keys in one decode.
// tenantPresent reports whether __rt_tenant existed at all (any string value,
// including ""), preserving the fail-closed guard in NewServer. A present-but-
// empty tenant maps to "default" (the gateway's open mode injects ""); an
// ABSENT key means the call did not come through a forward_tenant gateway
// upstream — NewServer fails closed on that unless allowDirect is set. session
// is "" when absent (normal in tenant-scoped mode — no fail-closed on session).
func popReserved(raw json.RawMessage) (tenant string, tenantPresent bool, session string, rest json.RawMessage, err error) {
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", false, "", nil, err
		}
	}
	if m == nil { // raw was JSON null
		m = map[string]any{}
	}
	if v, ok := m[tenantKey].(string); ok {
		tenant = v
		tenantPresent = true
	}
	if v, ok := m[sessionKey].(string); ok {
		session = v
	}
	delete(m, tenantKey)
	delete(m, sessionKey)
	if tenant == "" {
		tenant = defaultTenant
	}
	rest, err = json.Marshal(m)
	return tenant, tenantPresent, session, rest, err
}

// popTenant is a thin wrapper over popReserved that drops the session, kept
// for callers (and tests) that only care about the tenant channel.
func popTenant(raw json.RawMessage) (tenant string, present bool, rest json.RawMessage, err error) {
	tenant, present, _, rest, err = popReserved(raw)
	return tenant, present, rest, err
}
