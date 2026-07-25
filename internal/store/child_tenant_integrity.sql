CREATE OR REPLACE FUNCTION runtime_enforce_session_child_tenant()
RETURNS TRIGGER
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    parent_tenant TEXT;
BEGIN
    SELECT tenant_id INTO parent_tenant
      FROM public.sessions
     WHERE id = NEW.session_id;
    IF parent_tenant IS NULL THEN
        RAISE EXCEPTION 'parent session % does not exist', NEW.session_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF NEW.tenant IS DISTINCT FROM parent_tenant THEN
        RAISE EXCEPTION 'child tenant % does not match parent session tenant %',
            NEW.tenant, parent_tenant
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION runtime_enforce_session_child_tenant() FROM PUBLIC;

DROP TRIGGER IF EXISTS runtime_session_transcript_tenant ON session_transcripts;
CREATE TRIGGER runtime_session_transcript_tenant
BEFORE INSERT OR UPDATE OF session_id, tenant ON session_transcripts
FOR EACH ROW EXECUTE FUNCTION runtime_enforce_session_child_tenant();

DROP TRIGGER IF EXISTS runtime_online_eval_tenant ON online_eval_results;
CREATE TRIGGER runtime_online_eval_tenant
BEFORE INSERT OR UPDATE OF session_id, tenant ON online_eval_results
FOR EACH ROW EXECUTE FUNCTION runtime_enforce_session_child_tenant();

DROP POLICY IF EXISTS runtime_agent_tenant_transcripts ON session_transcripts;
CREATE POLICY runtime_agent_tenant_transcripts ON session_transcripts
    USING (tenant = runtime_agent_tenant() AND runtime_agent_can_access_session(session_id))
    WITH CHECK (tenant = runtime_agent_tenant() AND runtime_agent_can_access_session(session_id));

DROP POLICY IF EXISTS runtime_agent_tenant_online_results ON online_eval_results;
CREATE POLICY runtime_agent_tenant_online_results ON online_eval_results
    USING (tenant = runtime_agent_tenant() AND runtime_agent_can_access_session(session_id))
    WITH CHECK (tenant = runtime_agent_tenant() AND runtime_agent_can_access_session(session_id));
