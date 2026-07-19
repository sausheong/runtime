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
-- A subject MAY belong to multiple tenants (one row per (tenant_id, subject), the
-- table PK). The old global-unique index on subject is dropped here so existing
-- databases migrate on boot; the apply is idempotent.
DROP INDEX IF EXISTS identity_users_subject_uq;
CREATE TABLE IF NOT EXISTS secrets (
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    value_enc  BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, name)
);
-- P2.1: credential type discriminator. Existing rows read as 'static'.
ALTER TABLE secrets ADD COLUMN IF NOT EXISTS type TEXT NOT NULL DEFAULT 'static';
CREATE TABLE IF NOT EXISTS registration_tokens (
    token_id    TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    hash        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);
