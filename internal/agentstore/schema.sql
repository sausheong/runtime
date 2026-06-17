-- Dynamically-managed remote agents: registered at runtime via the admin API /
-- console (not the file config), so the control plane can add, remove, enable,
-- and disable agents without a restart and have them persist across restarts.
-- Mirrors gateway_upstreams. enabled=false keeps the row but drops the agent
-- from routing/listing. auth_secret is an optional per-tenant secret NAME
-- (brokered at dial), matching the gateway upstream cred model; '' = no bearer.
CREATE TABLE IF NOT EXISTS managed_agents (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    model       TEXT NOT NULL DEFAULT '',
    url         TEXT NOT NULL,
    auth_secret TEXT NOT NULL DEFAULT '',
    enabled     BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
