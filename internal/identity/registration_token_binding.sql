ALTER TABLE registration_tokens
    ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE registration_tokens
    ADD COLUMN IF NOT EXISTS agent_generation TEXT NOT NULL DEFAULT '';

-- Version-1 rows cannot be assigned a tenant safely: agent IDs are mutable and
-- may already have been reused. Empty bindings are deliberately inactive and
-- require operators to mint replacement tokens after the upgrade.
ALTER TABLE registration_tokens ALTER COLUMN tenant_id DROP DEFAULT;
ALTER TABLE registration_tokens ALTER COLUMN agent_generation DROP DEFAULT;

CREATE INDEX IF NOT EXISTS registration_tokens_agent_binding_idx
    ON registration_tokens (agent_id, tenant_id, agent_generation);
