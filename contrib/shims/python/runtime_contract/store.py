"""SQLite-backed session + event store — Level-1 durability (survives restart).

Single writer per session (the run task), so seq = MAX(seq)+1 is safe, mirroring
the Go store's documented assumption. Thread-safe across sessions via a lock +
check_same_thread=False (FastAPI may run the background run task off the main thread).
"""
from __future__ import annotations
import json
import os
import sqlite3
import threading
import uuid
from typing import Optional
from .events import ContractEvent


class Store:
    def __init__(self, path: str):
        self.path = path
        parent = os.path.dirname(os.path.abspath(path))
        os.makedirs(parent, exist_ok=True)
        self._lock = threading.Lock()
        self._db = sqlite3.connect(path, check_same_thread=False)
        self._db.execute("PRAGMA journal_mode=WAL")
        self._db.execute(
            "CREATE TABLE IF NOT EXISTS sessions ("
            "id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'running', "
            "turn_count INTEGER NOT NULL DEFAULT 0, "
            "completed_at TEXT, duration_ms INTEGER)"
        )
        # Migrate existing DBs that predate the completed_at/duration_ms columns.
        for col, defn in [("completed_at", "TEXT"), ("duration_ms", "INTEGER")]:
            try:
                self._db.execute(f"ALTER TABLE sessions ADD COLUMN {col} {defn}")
                self._db.commit()
            except sqlite3.OperationalError:
                pass  # column already exists
        self._db.execute(
            "CREATE TABLE IF NOT EXISTS events ("
            "session_id TEXT NOT NULL, seq INTEGER NOT NULL, payload TEXT NOT NULL, "
            "PRIMARY KEY (session_id, seq))"
        )
        self._db.commit()

    def create_session(self) -> str:
        sid = "ses-" + uuid.uuid4().hex
        with self._lock:
            self._db.execute("INSERT INTO sessions (id) VALUES (?)", (sid,))
            self._db.commit()
        return sid

    def append_event(self, sid: str, ev: ContractEvent) -> int:
        with self._lock:
            cur = self._db.execute(
                "SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id=?", (sid,)
            )
            seq = int(cur.fetchone()[0]) + 1
            self._db.execute(
                "INSERT INTO events (session_id, seq, payload) VALUES (?,?,?)",
                (sid, seq, ev.to_json()),
            )
            self._db.commit()
        return seq

    def events_since(self, sid: str, after_seq: int) -> list[tuple[int, ContractEvent]]:
        with self._lock:
            rows = self._db.execute(
                "SELECT seq, payload FROM events WHERE session_id=? AND seq>? ORDER BY seq",
                (sid, after_seq),
            ).fetchall()
        out = []
        for seq, payload in rows:
            d = json.loads(payload)
            out.append((seq, ContractEvent(type=d["type"], text=d.get("text", ""), error=d.get("error", ""))))
        return out

    def get_session(self, sid: str) -> Optional[dict]:
        with self._lock:
            row = self._db.execute(
                "SELECT id, status, turn_count, completed_at, duration_ms FROM sessions WHERE id=?", (sid,)
            ).fetchone()
        if not row:
            return None
        return {"id": row[0], "status": row[1], "turn_count": row[2],
                "completed_at": row[3], "duration_ms": row[4]}

    def list_sessions(self) -> list[dict]:
        with self._lock:
            rows = self._db.execute(
                "SELECT id, status, turn_count, completed_at, duration_ms FROM sessions ORDER BY rowid"
            ).fetchall()
        return [{"id": r[0], "status": r[1], "turn_count": r[2],
                 "completed_at": r[3], "duration_ms": r[4]} for r in rows]

    def set_status(self, sid: str, status: str) -> None:
        with self._lock:
            self._db.execute("UPDATE sessions SET status=? WHERE id=?", (status, sid))
            self._db.commit()

    def set_completed(self, sid: str, status: str, completed_at: str, duration_ms: int) -> None:
        with self._lock:
            self._db.execute(
                "UPDATE sessions SET status=?, completed_at=?, duration_ms=? WHERE id=?",
                (status, completed_at, duration_ms, sid),
            )
            self._db.commit()

    def set_turn_count(self, sid: str, n: int) -> None:
        with self._lock:
            self._db.execute("UPDATE sessions SET turn_count=? WHERE id=?", (n, sid))
            self._db.commit()
