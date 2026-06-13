CREATE TABLE IF NOT EXISTS gateway_upstreams (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    transport   TEXT NOT NULL CHECK (transport IN ('http','openapi')),
    url         TEXT NOT NULL DEFAULT '',
    openapi     TEXT NOT NULL DEFAULT '',
    base_url    TEXT NOT NULL DEFAULT '',
    operations  TEXT[] NOT NULL DEFAULT '{}',
    cred_secret TEXT NOT NULL DEFAULT '',
    cred_header TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);
