CREATE TABLE IF NOT EXISTS agents (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    contract_version TEXT NOT NULL DEFAULT 'v1',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL DEFAULT 'default',
    agent_id    TEXT NOT NULL,
    workflow_id TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'created',
    turn_count  INT  NOT NULL DEFAULT 0,
    replica     INT  NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_active_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS replica INT NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS tokens_total BIGINT           NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS cost_usd     DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS failure_category TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS sessions_tenant_agent_idx
    ON sessions (tenant_id, agent_id, created_at DESC);
CREATE TABLE IF NOT EXISTS session_events (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq        BIGINT NOT NULL,
    type       TEXT NOT NULL,
    payload    JSONB NOT NULL,
    ts         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, seq)
);
ALTER TABLE session_events ADD COLUMN IF NOT EXISTS event_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS session_events_key_idx
    ON session_events (session_id, event_key)
    WHERE event_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS session_transcripts (
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  turn_index  INT NOT NULL,
  tenant      TEXT NOT NULL DEFAULT '',
  actor_id    TEXT NOT NULL DEFAULT '',
  entries     JSONB NOT NULL,
  stop_reason TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, turn_index)
);
CREATE INDEX IF NOT EXISTS session_transcripts_tenant_idx ON session_transcripts (tenant, created_at DESC);

CREATE TABLE IF NOT EXISTS online_eval_results (
  session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  criterion_name TEXT NOT NULL,
  tenant         TEXT NOT NULL DEFAULT '',
  actor_id       TEXT NOT NULL DEFAULT '',
  scorer         TEXT NOT NULL,
  passed         BOOLEAN NOT NULL,
  detail         TEXT NOT NULL DEFAULT '',
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, criterion_name)
);
CREATE INDEX IF NOT EXISTS online_eval_results_tenant_idx ON online_eval_results (tenant, created_at DESC);

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conname = 'session_events_session_id_fkey'
       AND conrelid = 'session_events'::regclass
       AND confdeltype <> 'c'
  ) THEN
    ALTER TABLE session_events DROP CONSTRAINT session_events_session_id_fkey;
    ALTER TABLE session_events ADD CONSTRAINT session_events_session_id_fkey
      FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;
  END IF;
  IF EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conname = 'session_transcripts_session_id_fkey'
       AND conrelid = 'session_transcripts'::regclass
       AND confdeltype <> 'c'
  ) THEN
    ALTER TABLE session_transcripts DROP CONSTRAINT session_transcripts_session_id_fkey;
    ALTER TABLE session_transcripts ADD CONSTRAINT session_transcripts_session_id_fkey
      FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conname = 'online_eval_results_session_id_fkey'
       AND conrelid = 'online_eval_results'::regclass
  ) THEN
    ALTER TABLE online_eval_results ADD CONSTRAINT online_eval_results_session_id_fkey
      FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;
  END IF;
END $$;
