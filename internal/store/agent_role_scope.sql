ALTER TABLE runtime_agent_tenant_roles
    ADD COLUMN IF NOT EXISTS agent_id TEXT NOT NULL DEFAULT '';

CREATE OR REPLACE FUNCTION runtime_agent_tenant()
RETURNS TEXT
LANGUAGE SQL
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT tenant_id
      FROM public.runtime_agent_tenant_roles
     WHERE role_name = session_user::TEXT
$$;
REVOKE ALL ON FUNCTION runtime_agent_tenant() FROM PUBLIC;

CREATE OR REPLACE FUNCTION runtime_agent_id()
RETURNS TEXT
LANGUAGE SQL
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT agent_id
      FROM public.runtime_agent_tenant_roles
     WHERE role_name = session_user::TEXT
$$;
REVOKE ALL ON FUNCTION runtime_agent_id() FROM PUBLIC;

CREATE OR REPLACE FUNCTION runtime_agent_can_access_session(target_session_id TEXT)
RETURNS BOOLEAN
LANGUAGE SQL
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT EXISTS (
        SELECT 1
          FROM public.sessions s
          JOIN public.runtime_agent_tenant_roles r
            ON (r.tenant_id = '*' OR r.tenant_id = s.tenant_id)
           AND (r.agent_id = '*' OR r.agent_id = s.agent_id)
         WHERE r.role_name = session_user::TEXT
           AND s.id = target_session_id
    )
$$;
REVOKE ALL ON FUNCTION runtime_agent_can_access_session(TEXT) FROM PUBLIC;

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS runtime_agent_tenant_sessions ON sessions;
CREATE POLICY runtime_agent_tenant_sessions ON sessions
    USING (
        (runtime_agent_tenant() = '*' OR tenant_id = runtime_agent_tenant())
        AND
        (runtime_agent_id() = '*' OR agent_id = runtime_agent_id())
    )
    WITH CHECK (
        (runtime_agent_tenant() = '*' OR tenant_id = runtime_agent_tenant())
        AND
        (runtime_agent_id() = '*' OR agent_id = runtime_agent_id())
    );

ALTER TABLE session_events ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS runtime_agent_tenant_events ON session_events;
CREATE POLICY runtime_agent_tenant_events ON session_events
    USING (runtime_agent_can_access_session(session_id))
    WITH CHECK (runtime_agent_can_access_session(session_id));

ALTER TABLE session_transcripts ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS runtime_agent_tenant_transcripts ON session_transcripts;
CREATE POLICY runtime_agent_tenant_transcripts ON session_transcripts
    USING (
        (runtime_agent_tenant() = '*' OR tenant = runtime_agent_tenant())
        AND runtime_agent_can_access_session(session_id)
    )
    WITH CHECK (
        (runtime_agent_tenant() = '*' OR tenant = runtime_agent_tenant())
        AND runtime_agent_can_access_session(session_id)
    );

ALTER TABLE online_eval_results ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS runtime_agent_tenant_online_results ON online_eval_results;
CREATE POLICY runtime_agent_tenant_online_results ON online_eval_results
    USING (
        (runtime_agent_tenant() = '*' OR tenant = runtime_agent_tenant())
        AND runtime_agent_can_access_session(session_id)
    )
    WITH CHECK (
        (runtime_agent_tenant() = '*' OR tenant = runtime_agent_tenant())
        AND runtime_agent_can_access_session(session_id)
    );
