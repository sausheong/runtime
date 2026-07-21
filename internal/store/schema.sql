CREATE TABLE IF NOT EXISTS agents (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    contract_version TEXT NOT NULL DEFAULT 'v1',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    workflow_id TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'created',
    turn_count  INT  NOT NULL DEFAULT 0,
    replica     INT  NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_active_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS replica INT NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS tokens_total BIGINT           NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS cost_usd     DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS failure_category TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS session_events (
    session_id TEXT NOT NULL REFERENCES sessions(id),
    seq        BIGINT NOT NULL,
    type       TEXT NOT NULL,
    payload    JSONB NOT NULL,
    ts         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, seq)
);

CREATE TABLE IF NOT EXISTS session_transcripts (
  session_id  TEXT NOT NULL REFERENCES sessions(id),
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
  session_id     TEXT NOT NULL,
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
