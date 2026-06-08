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
