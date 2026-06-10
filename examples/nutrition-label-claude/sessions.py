"""Runtime session_id → Claude SDK session_id map.

The SDK owns conversation state (JSONL transcripts under CLAUDE_CONFIG_DIR);
the platform owns the runtime session id. This one-table map ties them so a
turn can resume= the SDK session belonging to its runtime session. Lives in
the same SQLite file as the contract store (RUNTIME_SHIM_DB) — separate
table, no schema interference; the same co-location the OpenAI adapter used
for its SQLiteSession.
"""
from __future__ import annotations

import sqlite3


class SessionMap:
    def __init__(self, db_path: str):
        self._db = db_path
        with self._conn() as c:
            c.execute(
                "CREATE TABLE IF NOT EXISTS sdk_sessions ("
                "runtime_id TEXT PRIMARY KEY, sdk_id TEXT NOT NULL)"
            )

    def _conn(self) -> sqlite3.Connection:
        return sqlite3.connect(self._db)

    def lookup(self, runtime_id: str) -> str | None:
        with self._conn() as c:
            row = c.execute(
                "SELECT sdk_id FROM sdk_sessions WHERE runtime_id = ?", (runtime_id,)
            ).fetchone()
        return row[0] if row else None

    def store(self, runtime_id: str, sdk_id: str) -> None:
        with self._conn() as c:
            c.execute(
                "INSERT INTO sdk_sessions (runtime_id, sdk_id) VALUES (?, ?) "
                "ON CONFLICT(runtime_id) DO UPDATE SET sdk_id = excluded.sdk_id",
                (runtime_id, sdk_id),
            )
