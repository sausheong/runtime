CREATE TABLE IF NOT EXISTS eval_sets (
  tenant     TEXT NOT NULL,
  name       TEXT NOT NULL,
  cases      JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, name)
);

CREATE TABLE IF NOT EXISTS eval_runs (
  run_id      TEXT PRIMARY KEY,
  tenant      TEXT NOT NULL,
  set_name    TEXT NOT NULL,
  agent_id    TEXT NOT NULL,
  status      TEXT NOT NULL,
  total       INT NOT NULL DEFAULT 0,
  passed      INT NOT NULL DEFAULT 0,
  failed      INT NOT NULL DEFAULT 0,
  score       DOUBLE PRECISION NOT NULL DEFAULT 0,
  error       TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS eval_results (
  run_id     TEXT NOT NULL REFERENCES eval_runs(run_id) ON DELETE CASCADE,
  case_index INT NOT NULL,
  input      TEXT NOT NULL,
  output     TEXT NOT NULL,
  scorer     TEXT NOT NULL,
  passed     BOOLEAN NOT NULL,
  detail     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, case_index)
);

CREATE INDEX IF NOT EXISTS eval_runs_tenant_idx ON eval_runs (tenant, created_at DESC);
