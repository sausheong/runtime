from sessions import SessionMap


def test_lookup_absent_returns_none(tmp_path):
    m = SessionMap(str(tmp_path / "shim.db"))
    assert m.lookup("ses-1") is None


def test_store_then_lookup(tmp_path):
    m = SessionMap(str(tmp_path / "shim.db"))
    m.store("ses-1", "sdk-abc")
    assert m.lookup("ses-1") == "sdk-abc"


def test_store_upserts(tmp_path):
    m = SessionMap(str(tmp_path / "shim.db"))
    m.store("ses-1", "sdk-abc")
    m.store("ses-1", "sdk-def")
    assert m.lookup("ses-1") == "sdk-def"


def test_survives_reopen(tmp_path):
    p = str(tmp_path / "shim.db")
    SessionMap(p).store("ses-1", "sdk-abc")
    assert SessionMap(p).lookup("ses-1") == "sdk-abc"
