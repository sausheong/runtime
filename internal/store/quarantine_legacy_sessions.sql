-- Rows predating the first recorded core migration had no durable tenant
-- owner. Assigning those rows to "default" would make them visible to a tenant
-- merely because it currently owns the same agent ID. Quarantine them instead;
-- an operator may explicitly attribute reviewed rows after upgrade.
DO $$
BEGIN
  IF to_regclass('public.sessions') IS NOT NULL THEN
    UPDATE sessions AS s
       SET tenant_id = '__legacy_unowned__'
      FROM runtime_schema_migrations AS m
     WHERE m.component = 'core'
       AND m.version = 1
       AND s.tenant_id = 'default'
       AND s.created_at < m.applied_at;
  END IF;
END $$;
