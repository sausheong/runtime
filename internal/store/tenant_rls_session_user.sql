-- SECURITY DEFINER changes current_user to the function owner. Use
-- session_user so the policy remains bound to the restricted login that opened
-- the database connection.
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
            ON r.tenant_id = s.tenant_id
         WHERE r.role_name = session_user::TEXT
           AND s.id = target_session_id
    )
$$;
