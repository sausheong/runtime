import os
from runtime_contract.store import Store
from runtime_contract.events import ContractEvent


def test_create_append_replay(tmp_path):
    db = str(tmp_path / "shim.db")
    s = Store(db)
    sid = s.create_session()
    assert sid
    seq1 = s.append_event(sid, ContractEvent(type="text", text="a"))
    seq2 = s.append_event(sid, ContractEvent(type="done"))
    assert seq2 == seq1 + 1
    all_ev = s.events_since(sid, 0)
    assert [e.type for _, e in all_ev] == ["text", "done"]
    tail = s.events_since(sid, seq1)
    assert [e.type for _, e in tail] == ["done"]


def test_persistence_across_reopen(tmp_path):
    db = str(tmp_path / "shim.db")
    s = Store(db)
    sid = s.create_session()
    s.append_event(sid, ContractEvent(type="text", text="hi"))
    s.set_status(sid, "completed")
    s.set_turn_count(sid, 1)
    s2 = Store(db)
    row = s2.get_session(sid)
    assert row["status"] == "completed"
    assert row["turn_count"] == 1
    assert [e.type for _, e in s2.events_since(sid, 0)] == ["text"]
    assert any(r["id"] == sid for r in s2.list_sessions())


def test_tokens_default_zero(tmp_path):
    s = Store(str(tmp_path / "shim.db"))
    sid = s.create_session()
    row = s.get_session(sid)
    assert row["tokens_total"] == 0
    assert all("tokens_total" in r for r in s.list_sessions())


def test_add_tokens_accumulates(tmp_path):
    db = str(tmp_path / "shim.db")
    s = Store(db)
    sid = s.create_session()
    s.add_tokens(sid, 300)   # turn 1
    s.add_tokens(sid, 651)   # turn 2 accumulates, not overwrites
    assert s.get_session(sid)["tokens_total"] == 951
    # non-positive is a no-op (a turn that reported nothing never perturbs total)
    s.add_tokens(sid, 0)
    s.add_tokens(sid, -5)
    assert s.get_session(sid)["tokens_total"] == 951
    # survives reopen (durable) and shows in list_sessions
    s2 = Store(db)
    assert s2.get_session(sid)["tokens_total"] == 951
    assert next(r for r in s2.list_sessions() if r["id"] == sid)["tokens_total"] == 951


def test_tokens_migration_on_legacy_db(tmp_path):
    """A DB created before the tokens_total column gains it on reopen (migration),
    defaulting existing rows to 0."""
    import sqlite3
    db = str(tmp_path / "legacy.db")
    raw = sqlite3.connect(db)
    raw.execute(
        "CREATE TABLE sessions (id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'running', "
        "turn_count INTEGER NOT NULL DEFAULT 0, completed_at TEXT, duration_ms INTEGER)"
    )
    raw.execute("INSERT INTO sessions (id, status) VALUES ('ses-old', 'completed')")
    raw.commit()
    raw.close()
    s = Store(db)  # runs the ALTER-TABLE migration
    assert s.get_session("ses-old")["tokens_total"] == 0
    s.add_tokens("ses-old", 42)
    assert s.get_session("ses-old")["tokens_total"] == 42
