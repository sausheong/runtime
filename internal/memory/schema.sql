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
