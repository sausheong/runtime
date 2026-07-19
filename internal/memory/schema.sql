CREATE TABLE IF NOT EXISTS memory_events (
    seq                 BIGSERIAL PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    op                  TEXT NOT NULL CHECK (op IN ('create','update','delete')),
    entry_id            TEXT NOT NULL,
    content             TEXT,
    tags                TEXT[],
    origin              TEXT NOT NULL DEFAULT '',
    supersedes          TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    original_created_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS memory_events_tenant_idx ON memory_events (tenant_id);
CREATE INDEX IF NOT EXISTS memory_events_supersedes_idx ON memory_events (tenant_id, supersedes);
CREATE INDEX IF NOT EXISTS memory_events_entry_idx ON memory_events (tenant_id, entry_id);

-- P2.2: strategy discriminator + per-session summary key. Additive; existing
-- rows read as kind='fact', session_id NULL.
ALTER TABLE memory_events ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'fact';
ALTER TABLE memory_events ADD COLUMN IF NOT EXISTS session_id TEXT;
CREATE INDEX IF NOT EXISTS memory_events_summary_idx
    ON memory_events (tenant_id, session_id) WHERE kind = 'summary';

-- P2.2 M2: actor namespacing. Additive; existing rows read as actor_id=''
-- (the tenant-wide bucket). A non-empty actor_id scopes rows to one caller.
ALTER TABLE memory_events ADD COLUMN IF NOT EXISTS actor_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS memory_events_actor_idx ON memory_events (tenant_id, actor_id);
