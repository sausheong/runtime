-- A wildcard mapping is installed only when the database owner explicitly
-- binds a role to tenant '*'. Production startup never does this; the
-- integration harness uses it to share one disposable login across concurrent
-- Runtime processes without tenant-rebind races.
CREATE OR REPLACE FUNCTION runtime_agent_can_access_session(target_session_id TEXT)
RETURNS BOOLEAN
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    allowed BOOLEAN;
BEGIN
    EXECUTE $query$
        SELECT EXISTS (
            SELECT 1
              FROM public.sessions s
              JOIN public.runtime_agent_tenant_roles r
                ON r.tenant_id = s.tenant_id OR r.tenant_id = '*'
             WHERE r.role_name = session_user::TEXT
               AND s.id = $1
        )
    $query$ INTO allowed USING target_session_id;
    RETURN allowed;
END
$$;

DO $$
BEGIN
    -- Integration tests may deliberately drop the reconciled core tables while
    -- retaining the migration ledger. The normal post-ledger schema reconcile
    -- executes this migration again after recreating them.
    IF to_regclass('public.sessions') IS NOT NULL THEN
        DROP POLICY IF EXISTS runtime_agent_tenant_sessions ON sessions;
        CREATE POLICY runtime_agent_tenant_sessions ON sessions
            USING (runtime_agent_tenant() = '*' OR tenant_id = runtime_agent_tenant())
            WITH CHECK (runtime_agent_tenant() = '*' OR tenant_id = runtime_agent_tenant());
    END IF;
    IF to_regclass('public.session_transcripts') IS NOT NULL THEN
        DROP POLICY IF EXISTS runtime_agent_tenant_transcripts ON session_transcripts;
        CREATE POLICY runtime_agent_tenant_transcripts ON session_transcripts
            USING ((runtime_agent_tenant() = '*' OR tenant = runtime_agent_tenant()) AND runtime_agent_can_access_session(session_id))
            WITH CHECK ((runtime_agent_tenant() = '*' OR tenant = runtime_agent_tenant()) AND runtime_agent_can_access_session(session_id));
    END IF;
    IF to_regclass('public.online_eval_results') IS NOT NULL THEN
        DROP POLICY IF EXISTS runtime_agent_tenant_online_results ON online_eval_results;
        CREATE POLICY runtime_agent_tenant_online_results ON online_eval_results
            USING ((runtime_agent_tenant() = '*' OR tenant = runtime_agent_tenant()) AND runtime_agent_can_access_session(session_id))
            WITH CHECK ((runtime_agent_tenant() = '*' OR tenant = runtime_agent_tenant()) AND runtime_agent_can_access_session(session_id));
    END IF;
END
$$;
