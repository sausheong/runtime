CREATE TABLE IF NOT EXISTS tenants (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS identity_users (
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    subject    TEXT NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('viewer','operator','admin')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, subject)
);
CREATE TABLE IF NOT EXISTS service_keys (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    key_hash   TEXT NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('viewer','operator','admin')),
    label      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
-- A subject must be globally unique in M1 (one tenant per identity), so OIDC
-- login can resolve subject -> (tenant, role) without ambiguity.
CREATE UNIQUE INDEX IF NOT EXISTS identity_users_subject_uq ON identity_users (subject);
