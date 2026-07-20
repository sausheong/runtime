CREATE TABLE IF NOT EXISTS eval_policies (
  tenant      TEXT NOT NULL,
  agent_id    TEXT NOT NULL,
  sample_rate INT NOT NULL DEFAULT 0,
  criteria    JSONB NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, agent_id)
);
