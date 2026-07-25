#!/usr/bin/env bash
set -euo pipefail

: "${POSTGRES_AGENT_PASSWORD:?POSTGRES_AGENT_PASSWORD is required}"

psql --set=ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  --set=agent_password="$POSTGRES_AGENT_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE runtime_agent LOGIN PASSWORD %L', :'agent_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'runtime_agent')
\gexec
SQL
