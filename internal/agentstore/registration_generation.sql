ALTER TABLE managed_agents
    ADD COLUMN IF NOT EXISTS registration_generation TEXT NOT NULL DEFAULT '';

-- Preserve one stable identity for pre-migration managed agents. This value is
-- an identity/version marker, not a credential.
UPDATE managed_agents
   SET registration_generation =
       'legacy:' || tenant_id || ':' || id || ':' || created_at::text
 WHERE registration_generation = '';

ALTER TABLE managed_agents ALTER COLUMN registration_generation DROP DEFAULT;
