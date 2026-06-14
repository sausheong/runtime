-- Runs as the Postgres superuser on FIRST database init only (the pgvector
-- image executes everything in /docker-entrypoint-initdb.d/). The unprivileged
-- `runtime` role cannot CREATE EXTENSION, so this is the turnkey path that makes
-- semantic Memory work out of the box. A `docker compose down -v` wipes the
-- volume and re-runs this on the next `up`.
CREATE EXTENSION IF NOT EXISTS vector;
