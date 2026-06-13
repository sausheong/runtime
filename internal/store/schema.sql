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
CREATE TABLE IF NOT EXISTS session_events (
    session_id TEXT NOT NULL REFERENCES sessions(id),
    seq        BIGINT NOT NULL,
    type       TEXT NOT NULL,
    payload    JSONB NOT NULL,
    ts         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, seq)
);
