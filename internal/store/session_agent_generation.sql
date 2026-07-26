ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS agent_generation TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS sessions_agent_generation_idx
    ON sessions (tenant_id, agent_id, agent_generation, created_at DESC);
