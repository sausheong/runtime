# Polyglot Shim (OpenAI Agents SDK) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Host the OpenAI Agents SDK agent (`../agents_sdk/openai-demo`) under `runtimed` as a first-class supervised agent, via a generalized spawn path (Go) + a reusable Python contract-server library and OpenAI adapter (the shim). Level-1 durability (conversation + event-log resume across restarts).

**Architecture:** Two work-streams. (A) Go: add optional `command:`/`workdir:` to a config agent entry, threaded through `registry` → `AgentProcess` → `SpawnFunc`, so the supervisor can launch an arbitrary process instead of the `agentd` binary (backward compatible). (B) Python: `contrib/shims/python/` — a framework-agnostic `runtime_contract` library (6 contract endpoints, SSE framing with `?since=N` replay, SQLite session+event store) with an `AgentAdapter` seam, plus `adapters/openai_agents.py` driving `Runner.run_streamed` with `SQLiteSession`.

**Tech Stack:** Go 1.25 (harness via `replace ../harness`), Python 3.12 + uv + FastAPI + uvicorn + openai-agents, SQLite.

**Spec:** `docs/superpowers/specs/2026-06-08-polyglot-shim-openai-design.md`

**Conventions (read before starting):**
- The `go` CLI is ground truth; ignore IDE/LSP diagnostics (the `replace ../harness` setup confuses gopls). Verify with `go build ./...` / `go test ./...`.
- Commit with `git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com'`.
- Branch is already `feat/polyglot-shim-openai`.
- `go test ./...` MUST stay Python-free and green. Python tests live under `contrib/` and run via `uv run pytest`, never `go test`.
- Integration tests (`//go:build integration`) need Postgres.app at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`; run `go test -tags integration ./test/ -count=1 -timeout 300s`.

---

## Work-stream A — Generalized spawn path (Go)

### Task 1: Add `command`/`workdir` to config

**Files:**
- Modify: `internal/config/config.go` (AgentConfig struct, ~lines 11-18)
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

In `internal/config/config_test.go` add (the file already imports `os`, `path/filepath`, `testing` and has a `writeTmp` helper):
```go
func TestLoadCommandWorkdir(t *testing.T) {
	p := writeTmp(t, `
agents:
  - id: openai
    name: OpenAI SDK Agent
    model: openai/gpt-5.4
    listen_addr: 127.0.0.1:8301
    workdir: /tmp/shim
    command: ["uv", "run", "python", "main.py"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Agents[0]
	if a.WorkDir != "/tmp/shim" {
		t.Errorf("workdir = %q, want /tmp/shim", a.WorkDir)
	}
	if len(a.Command) != 4 || a.Command[0] != "uv" || a.Command[3] != "main.py" {
		t.Errorf("command = %v, want [uv run python main.py]", a.Command)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoadCommandWorkdir -v`
Expected: FAIL (unknown fields WorkDir/Command).

- [ ] **Step 3: Add the fields**

In `internal/config/config.go`, extend `AgentConfig`:
```go
type AgentConfig struct {
	ID         string   `yaml:"id"`
	Name       string   `yaml:"name"`
	Model      string   `yaml:"model"`
	ListenAddr string   `yaml:"listen_addr"`
	Kind       string   `yaml:"kind"`    // optional; "" ⇒ testagent. Resolved by agentd's kind registry.
	Command    []string `yaml:"command"` // optional; when set, the supervisor execs this instead of the agentd binary (polyglot/foreign agents). argv form.
	WorkDir    string   `yaml:"workdir"` // optional working directory for Command (e.g. a Python shim project root).
}
```
No `Validate` change: `command`/`workdir` are optional and free-form. (`kind` and `command` are independent; a `command` agent typically sets neither.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestLoadCommandWorkdir -v`
Expected: PASS. Also `go test ./internal/config/` (whole package) stays green.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(config): optional command/workdir for foreign-process agents"
```

---

### Task 2: SpawnFunc execs an arbitrary command

**Files:**
- Modify: `controlplane/proxy.go` (AgentProcess struct + SpawnFunc, lines 12-40)
- Modify: `controlplane/registry.go` (NewRegistry, line ~26)
- Test: `controlplane/proxy_test.go` (new or existing)

- [ ] **Step 1: Write the failing test**

Create/extend `controlplane/proxy_test.go`. The test proves a `Command`-based AgentProcess spawns the given process (not agentd), applies `WorkDir`, and inherits env. Use a portable fake command: `sh -c` that writes its cwd + an env var to a file, then sleeps briefly.
```go
package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSpawnFuncCommand(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	ap := AgentProcess{
		AgentID: "x",
		Addr:    "127.0.0.1:0",
		Command: []string{"sh", "-c", "pwd > " + out + "; printf '%s' \"$RUNTIME_AGENT_ID\" >> " + out + "; sleep 0.3"},
		WorkDir: dir,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wait := ap.SpawnFunc()(ctx)
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
		t.Fatal("command did not exit in time")
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	got := string(b)
	// cwd line should be the WorkDir (resolve symlinks: /tmp may be /private/tmp on macOS)
	wantDir, _ := filepath.EvalSymlinks(dir)
	if !strings.Contains(got, dir) && !strings.Contains(got, wantDir) {
		t.Errorf("cwd not applied: out=%q want contains %q", got, dir)
	}
	if !strings.Contains(got, "x") {
		t.Errorf("RUNTIME_AGENT_ID not in env: out=%q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./controlplane/ -run TestSpawnFuncCommand -v`
Expected: FAIL (Command/WorkDir fields don't exist yet).

- [ ] **Step 3: Add fields + branch in SpawnFunc**

In `controlplane/proxy.go`, extend `AgentProcess`:
```go
type AgentProcess struct {
	AgentID string
	Addr    string
	BinPath string
	PGDSN   string
	Kind    string
	Command []string // when non-empty, exec this instead of BinPath (foreign-process agents)
	WorkDir string   // optional working directory for Command
}
```
Modify `SpawnFunc` to branch on `Command`:
```go
func (a AgentProcess) SpawnFunc() func(ctx context.Context) <-chan error {
	return func(ctx context.Context) <-chan error {
		var cmd *exec.Cmd
		if len(a.Command) > 0 {
			cmd = exec.CommandContext(ctx, a.Command[0], a.Command[1:]...)
			if a.WorkDir != "" {
				cmd.Dir = a.WorkDir
			}
		} else {
			cmd = exec.CommandContext(ctx, a.BinPath)
		}
		cmd.Env = append(os.Environ(),
			"RUNTIME_PG_DSN="+a.PGDSN,
			"RUNTIME_LISTEN_ADDR="+a.Addr,
			"RUNTIME_AGENT_ID="+a.AgentID,
			"RUNTIME_AGENT_KIND="+a.Kind,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		ch := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			ch <- err
			return ch
		}
		go func() { ch <- cmd.Wait() }()
		return ch
	}
}
```

- [ ] **Step 4: Pass Command/WorkDir through NewRegistry**

In `controlplane/registry.go`, update the AgentProcess construction:
```go
		r.agents[a.ID] = AgentProcess{
			AgentID: a.ID, Addr: a.ListenAddr, BinPath: binPath, PGDSN: dsn,
			Kind: a.Kind, Command: a.Command, WorkDir: a.WorkDir,
		}
```

- [ ] **Step 5: Run tests**

Run: `go test ./controlplane/ -run TestSpawnFuncCommand -v` → PASS.
Run: `go test ./...` → all green (existing behavior unchanged when Command empty).
Run: `go build ./...`.

- [ ] **Step 6: Run the integration suite (no regression)**

Run: `go test -tags integration ./test/ -count=1 -timeout 300s`
Expected: all PASS — the spawn-path change is backward compatible (existing agents have no `command`, so they still spawn `agentd`).

- [ ] **Step 7: Commit**

```bash
git add controlplane/proxy.go controlplane/registry.go controlplane/proxy_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(controlplane): supervisor can spawn an arbitrary command (polyglot agents)"
```

---

## Work-stream B — Python contract shim (`contrib/shims/python/`)

> All Python files live under `contrib/shims/python/`. They are NOT part of `go test`. Test with `uv run pytest` from that directory. Use only the stdlib + declared deps.

### Task 3: Python project scaffold + event/adapter types

**Files:**
- Create: `contrib/shims/python/pyproject.toml`
- Create: `contrib/shims/python/runtime_contract/__init__.py`
- Create: `contrib/shims/python/runtime_contract/events.py`
- Create: `contrib/shims/python/runtime_contract/adapter.py`

- [ ] **Step 1: Write `pyproject.toml`**

```toml
[project]
name = "runtime-contract-shim"
version = "0.1.0"
description = "Contract server shim hosting foreign-SDK agents under sausheong/runtime"
requires-python = ">=3.12"
dependencies = [
    "fastapi>=0.115",
    "uvicorn>=0.30",
    "openai-agents>=0.17.4",
]

[dependency-groups]
dev = ["pytest>=8", "httpx>=0.27"]

[tool.pytest.ini_options]
asyncio_mode = "auto"
```
(Add `pytest-asyncio` to dev deps if `asyncio_mode` needs it; the plan's tests use `httpx` against the ASGI app synchronously where possible to avoid async-test plumbing — see Task 6.)

- [ ] **Step 2: Write `events.py`**

```python
"""Contract event vocabulary — mirrors the Go agentruntime.WireEvent JSON."""
from __future__ import annotations
from dataclasses import dataclass, field
import json

EventType = str  # "text" | "tool_result" | "done" | "error"


@dataclass
class ContractEvent:
    type: EventType
    text: str = ""
    error: str = ""

    def to_dict(self) -> dict:
        d: dict = {"type": self.type}
        if self.text:
            d["text"] = self.text
        if self.error:
            d["error"] = self.error
        return d

    def to_json(self) -> str:
        return json.dumps(self.to_dict())


@dataclass
class Image:
    mime: str
    data: bytes
```

- [ ] **Step 3: Write `adapter.py`**

```python
"""The AgentAdapter seam: each framework implements run() to yield ContractEvents."""
from __future__ import annotations
from typing import Protocol, AsyncIterator, Sequence, runtime_checkable
from .events import ContractEvent, Image


@runtime_checkable
class AgentAdapter(Protocol):
    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        """Drive the underlying framework for one invocation, yielding events.

        The library frames/persists/fans-out the events; the adapter must never
        raise out of run() — surface failures as a ContractEvent(type="error").
        Do NOT emit the terminal 'done'/'error' lifecycle event; the library
        appends it based on whether run() completed or yielded an error event.
        """
        ...
```

- [ ] **Step 4: Write `runtime_contract/__init__.py`**

```python
from .events import ContractEvent, Image
from .adapter import AgentAdapter

__all__ = ["ContractEvent", "Image", "AgentAdapter"]
```

- [ ] **Step 5: Verify it imports**

Run (from `contrib/shims/python/`): `uv sync && uv run python -c "import runtime_contract; print('ok')"`
Expected: prints `ok`.

- [ ] **Step 6: Commit**

```bash
git add contrib/shims/python/pyproject.toml contrib/shims/python/runtime_contract/__init__.py contrib/shims/python/runtime_contract/events.py contrib/shims/python/runtime_contract/adapter.py
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(shim): Python project scaffold + contract event/adapter types"
```

---

### Task 4: SQLite session+event store (Level-1 persistence)

**Files:**
- Create: `contrib/shims/python/runtime_contract/store.py`
- Create: `contrib/shims/python/tests/__init__.py` (empty)
- Create: `contrib/shims/python/tests/test_store.py`

- [ ] **Step 1: Write the failing test** — `tests/test_store.py`:

```python
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
    # replay since 0 returns both, since seq1 returns only the second
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
    # reopen
    s2 = Store(db)
    row = s2.get_session(sid)
    assert row["status"] == "completed"
    assert row["turn_count"] == 1
    assert [e.type for _, e in s2.events_since(sid, 0)] == ["text"]
    assert any(r["id"] == sid for r in s2.list_sessions())
```

- [ ] **Step 2: Run it (fails — no Store)**

Run (from `contrib/shims/python/`): `uv run pytest tests/test_store.py -q`
Expected: FAIL (ImportError).

- [ ] **Step 3: Implement `store.py`**

```python
"""SQLite-backed session + event store — Level-1 durability (survives restart).

Single writer per session (the run task), so seq = MAX(seq)+1 is safe, mirroring
the Go store's documented assumption. Thread-safe across sessions via a lock +
check_same_thread=False (FastAPI runs the background run task on a threadpool/loop).
"""
from __future__ import annotations
import os
import sqlite3
import threading
import uuid
from typing import Optional
from .events import ContractEvent


class Store:
    def __init__(self, path: str):
        self.path = path
        os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
        self._lock = threading.Lock()
        self._db = sqlite3.connect(path, check_same_thread=False)
        self._db.execute("PRAGMA journal_mode=WAL")
        self._db.execute(
            "CREATE TABLE IF NOT EXISTS sessions ("
            "id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'running', "
            "turn_count INTEGER NOT NULL DEFAULT 0)"
        )
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
        import json
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
                "SELECT id, status, turn_count FROM sessions WHERE id=?", (sid,)
            ).fetchone()
        if not row:
            return None
        return {"id": row[0], "status": row[1], "turn_count": row[2]}

    def list_sessions(self) -> list[dict]:
        with self._lock:
            rows = self._db.execute(
                "SELECT id, status, turn_count FROM sessions ORDER BY rowid"
            ).fetchall()
        return [{"id": r[0], "status": r[1], "turn_count": r[2]} for r in rows]

    def set_status(self, sid: str, status: str) -> None:
        with self._lock:
            self._db.execute("UPDATE sessions SET status=? WHERE id=?", (status, sid))
            self._db.commit()

    def set_turn_count(self, sid: str, n: int) -> None:
        with self._lock:
            self._db.execute("UPDATE sessions SET turn_count=? WHERE id=?", (n, sid))
            self._db.commit()
```

- [ ] **Step 4: Run it (passes)**

Run: `uv run pytest tests/test_store.py -q`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add contrib/shims/python/runtime_contract/store.py contrib/shims/python/tests/__init__.py contrib/shims/python/tests/test_store.py
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(shim): SQLite session+event store with replay (Level-1 durability)"
```

---

### Task 5: SSE framing + the FastAPI contract app

**Files:**
- Create: `contrib/shims/python/runtime_contract/sse.py`
- Create: `contrib/shims/python/runtime_contract/app.py`

- [ ] **Step 1: Write `sse.py`**

```python
"""SSE framing matching the Go agentruntime: 'id: <seq>\\ndata: <json>\\n\\n'."""
from __future__ import annotations
from .events import ContractEvent


def frame(seq: int, ev: ContractEvent) -> str:
    return f"id: {seq}\ndata: {ev.to_json()}\n\n"
```

- [ ] **Step 2: Write `app.py`**

The app owns transport + persistence + lifecycle. It exposes `create_app(adapter, store, agent_id)`.
```python
"""FastAPI app serving the runtime agent contract for a single foreign agent."""
from __future__ import annotations
import asyncio
import base64
import os
from typing import Optional

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, PlainTextResponse, StreamingResponse

from .adapter import AgentAdapter
from .events import ContractEvent, Image
from .sse import frame
from .store import Store

CONTRACT_VERSION = "v1"


def create_app(adapter: AgentAdapter, store: Store, agent_id: str) -> FastAPI:
    app = FastAPI()
    # session_id -> list of asyncio.Queue for live subscribers
    live: dict[str, list[asyncio.Queue]] = {}
    live_lock = asyncio.Lock()

    async def publish(sid: str, ev: ContractEvent) -> None:
        seq = store.append_event(sid, ev)
        async with live_lock:
            subs = list(live.get(sid, []))
        for q in subs:
            q.put_nowait((seq, ev))

    async def run_session(sid: str, message: str, images: list[Image]) -> None:
        store.set_status(sid, "running")
        terminal = ContractEvent(type="done")
        try:
            history = [ev for _, ev in store.events_since(sid, 0)]
            async for ev in adapter.run(sid, message, images, history):
                await publish(sid, ev)
                if ev.type == "error":
                    terminal = ContractEvent(type="error", error=ev.error or "agent error")
        except Exception as e:  # never crash the server
            terminal = ContractEvent(type="error", error=str(e))
        store.set_status(sid, "completed" if terminal.type == "done" else "error")
        store.set_turn_count(sid, 1)
        await publish(sid, terminal)

    @app.get("/healthz", response_class=PlainTextResponse)
    async def healthz() -> str:
        return "ok"

    @app.get("/meta")
    async def meta() -> JSONResponse:
        return JSONResponse({"agent_id": agent_id, "contract_version": CONTRACT_VERSION})

    @app.post("/sessions")
    async def create_session(req: Request) -> JSONResponse:
        body = await req.json()
        message = body.get("message", "")
        images: list[Image] = []
        b64 = body.get("image_b64")
        if b64:
            images.append(Image(mime=body.get("image_mime") or "image/jpeg",
                                data=base64.b64decode(b64)))
        sid = store.create_session()
        asyncio.create_task(run_session(sid, message, images))
        return JSONResponse({"session_id": sid})

    @app.get("/sessions")
    async def list_sessions() -> JSONResponse:
        return JSONResponse(store.list_sessions())

    @app.get("/sessions/{sid}")
    async def get_session(sid: str) -> JSONResponse:
        row = store.get_session(sid)
        if not row:
            return JSONResponse({"error": "not found"}, status_code=404)
        return JSONResponse(row)

    @app.get("/sessions/{sid}/stream")
    async def stream(sid: str, since: int = 0) -> StreamingResponse:
        q: asyncio.Queue = asyncio.Queue()

        async def gen():
            # subscribe for live BEFORE replay, so we don't miss events between.
            async with live_lock:
                live.setdefault(sid, []).append(q)
            try:
                last = since
                buffered = store.events_since(sid, since)
                for seq, ev in buffered:
                    last = seq
                    yield frame(seq, ev)
                    if ev.type in ("done", "error"):
                        return  # pure replay hit terminal
                while True:
                    seq, ev = await q.get()
                    if seq <= last:
                        continue  # already replayed
                    last = seq
                    yield frame(seq, ev)
                    if ev.type in ("done", "error"):
                        return
            finally:
                async with live_lock:
                    subs = live.get(sid, [])
                    if q in subs:
                        subs.remove(q)

        return StreamingResponse(gen(), media_type="text/event-stream")

    return app
```

- [ ] **Step 3: Sanity import**

Run: `uv run python -c "from runtime_contract.app import create_app; print('ok')"`
Expected: `ok`.

- [ ] **Step 4: Commit**

```bash
git add contrib/shims/python/runtime_contract/sse.py contrib/shims/python/runtime_contract/app.py
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(shim): SSE framing + FastAPI contract app (6 endpoints, ?since replay)"
```

---

### Task 6: Contract app tests with a fake adapter (hermetic)

**Files:**
- Create: `contrib/shims/python/tests/test_contract.py`

- [ ] **Step 1: Write the test**

Use FastAPI's `TestClient` (httpx-based, synchronous) with a fake adapter that yields scripted events. TestClient runs the app's event loop, so `asyncio.create_task` in `/sessions` works; the streaming endpoint returns when the terminal event is hit.
```python
import json
from typing import AsyncIterator, Sequence
from fastapi.testclient import TestClient

from runtime_contract.app import create_app
from runtime_contract.events import ContractEvent, Image
from runtime_contract.store import Store


class FakeAdapter:
    async def run(self, session_id, message, images, history) -> AsyncIterator[ContractEvent]:
        yield ContractEvent(type="text", text="hello " + message)
        yield ContractEvent(type="tool_result", text="ran a tool")


def make_client(tmp_path):
    store = Store(str(tmp_path / "shim.db"))
    app = create_app(FakeAdapter(), store, "fake")
    return TestClient(app), store


def test_healthz_and_meta(tmp_path):
    c, _ = make_client(tmp_path)
    assert c.get("/healthz").text == "ok"
    m = c.get("/meta").json()
    assert m["agent_id"] == "fake" and m["contract_version"] == "v1"


def test_session_stream_to_done(tmp_path):
    c, _ = make_client(tmp_path)
    sid = c.post("/sessions", json={"message": "world"}).json()["session_id"]
    assert sid
    body = c.get(f"/sessions/{sid}/stream?since=0").text
    assert '"type":"text"' in body and "hello world" in body
    assert '"type":"tool_result"' in body
    assert '"type":"done"' in body
    # each record has an id: line
    assert "id: 1" in body
    # session is queryable and completed
    row = c.get(f"/sessions/{sid}").json()
    assert row["status"] == "completed"
    rows = c.get("/sessions").json()
    assert any(r["id"] == sid for r in rows)


def test_replay_since(tmp_path):
    c, _ = make_client(tmp_path)
    sid = c.post("/sessions", json={"message": "x"}).json()["session_id"]
    # drain full stream so all events are persisted
    full = c.get(f"/sessions/{sid}/stream?since=0").text
    # the text event was seq 1; since=1 should NOT include it, but should include later events
    tail = c.get(f"/sessions/{sid}/stream?since=1").text
    assert "hello x" not in tail          # seq 1 excluded
    assert '"type":"done"' in tail        # later terminal still present


def test_image_decoded(tmp_path):
    import base64
    captured = {}

    class ImgAdapter:
        async def run(self, session_id, message, images, history):
            captured["n"] = len(images)
            if images:
                captured["mime"] = images[0].mime
            yield ContractEvent(type="text", text="ok")

    store = Store(str(tmp_path / "db.sqlite"))
    c = TestClient(create_app(ImgAdapter(), store, "img"))
    b64 = base64.b64encode(b"\xff\xd8fake").decode()
    sid = c.post("/sessions", json={"message": "m", "image_b64": b64, "image_mime": "image/png"}).json()["session_id"]
    c.get(f"/sessions/{sid}/stream?since=0").text  # drive the run
    assert captured.get("n") == 1
    assert captured.get("mime") == "image/png"
```

- [ ] **Step 2: Run the tests**

Run (from `contrib/shims/python/`): `uv run pytest -q`
Expected: PASS (store tests + contract tests).
NOTE: if `TestClient` streaming buffers oddly, the test reads `.text` (full body) which is fine because each session reaches a terminal event and the generator returns, closing the stream. If a test hangs, ensure the adapter always completes (the fakes do).

- [ ] **Step 3: Commit**

```bash
git add contrib/shims/python/tests/test_contract.py
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "test(shim): hermetic contract-app tests (SSE, replay, image, lifecycle) via fake adapter"
```

---

### Task 7: OpenAI Agents SDK adapter + main.py

**Files:**
- Create: `contrib/shims/python/adapters/__init__.py` (empty)
- Create: `contrib/shims/python/adapters/openai_agents.py`
- Create: `contrib/shims/python/main.py`

- [ ] **Step 1: Implement `adapters/openai_agents.py`**

Reuse the existing demo agent. The demo lives at `../agents_sdk/openai-demo/main.py` with `build_agent()`, `_data_url()`, and `NutritionVerdict`. To avoid a hard cross-repo import, the adapter builds its own agent using the OpenAI SDK directly, mirroring the demo's `build_agent` (provider from env, the 4 tools + prompt). To keep the plan self-contained and avoid coupling to the demo's filesystem layout, **vendor a minimal agent** in the adapter: import the SDK, construct an Agent with the investigator instructions and reuse the demo's tools IF importable; otherwise a minimal agent is acceptable for the shim demo (the goal is proving the contract path, not re-porting the tools).

Concretely:
```python
"""Adapter: OpenAI Agents SDK -> runtime contract events."""
from __future__ import annotations
import os
from typing import AsyncIterator, Sequence

from agents import (
    Agent, Runner, AsyncOpenAI, OpenAIChatCompletionsModel,
    set_tracing_disabled, SQLiteSession,
)
from runtime_contract.events import ContractEvent, Image

set_tracing_disabled(True)

INSTRUCTIONS = (
    "You are a Singapore food label investigator. Given a label (as text or an "
    "image), read the product, additives, and sugar/saturated-fat values, and "
    "return a plain-prose verdict: reasoning, a one-line summary, findings grouped "
    "GREEN/AMBER/RED, then a recommendation. If you cannot read it, say so."
)


def _build_agent() -> Agent:
    key = os.environ["OPENAI_API_KEY"]
    base = os.environ.get("OPENAI_BASE_URL") or None
    model = os.environ.get("OPENAI_MODEL", "gpt-4o")
    client = AsyncOpenAI(base_url=base, api_key=key)
    return Agent(
        name="OpenAI SDK Nutrition Agent",
        model=OpenAIChatCompletionsModel(model=model, openai_client=client),
        instructions=INSTRUCTIONS,
    )


class OpenAIAgentsAdapter:
    def __init__(self, db_path: str):
        self._db = db_path
        self._agent = _build_agent()

    async def run(self, session_id: str, message: str,
                  images: Sequence[Image], history) -> AsyncIterator[ContractEvent]:
        # Build SDK input: text + optional image as a content list.
        if images:
            import base64
            img = images[0]
            data_url = f"data:{img.mime};base64,{base64.b64encode(img.data).decode()}"
            user_input = [{
                "role": "user",
                "content": [
                    {"type": "input_text", "text": message or "Investigate this label."},
                    {"type": "input_image", "image_url": data_url},
                ],
            }]
        else:
            user_input = message

        session = SQLiteSession(session_id, self._db)
        try:
            result = Runner.run_streamed(self._agent, input=user_input, session=session)
            async for ev in result.stream_events():
                # Stream assistant text deltas as they arrive.
                if ev.type == "raw_response_event":
                    delta = getattr(getattr(ev, "data", None), "delta", None)
                    if isinstance(delta, str) and delta:
                        yield ContractEvent(type="text", text=delta)
            # Fallback: if no deltas streamed, emit the final output once.
            final = getattr(result, "final_output", None)
            if final is not None:
                yield ContractEvent(type="text", text=str(final))
        except Exception as e:
            yield ContractEvent(type="error", error=str(e))
```
NOTE for the implementer: the OpenAI Agents SDK streaming event shapes vary by version. The robust contract is: iterate `result.stream_events()`, surface any assistant text you can extract as `text` events, and ALWAYS guarantee at least the final output is emitted once (the fallback above). If delta extraction differs in the installed version, prefer emitting `str(result.final_output)` as a single text event — correctness over granularity. Verify against the installed `openai-agents` version; do not invent attributes — inspect with a quick `uv run python -c "import agents; ..."` if unsure.

- [ ] **Step 2: Implement `main.py`**

```python
"""Entry point: serve the OpenAI Agents SDK agent over the runtime contract."""
from __future__ import annotations
import os
import uvicorn

from runtime_contract.app import create_app
from runtime_contract.store import Store
from adapters.openai_agents import OpenAIAgentsAdapter


def main() -> None:
    addr = os.environ.get("RUNTIME_LISTEN_ADDR", "127.0.0.1:8301")
    host, _, port = addr.partition(":")
    agent_id = os.environ.get("RUNTIME_AGENT_ID", "openai")
    db = os.environ.get("RUNTIME_SHIM_DB", "./shim.db")

    store = Store(db)
    adapter = OpenAIAgentsAdapter(db_path=db)
    app = create_app(adapter, store, agent_id)
    uvicorn.run(app, host=host or "127.0.0.1", port=int(port or "8301"), log_level="info")


if __name__ == "__main__":
    main()
```

- [ ] **Step 3: Verify it imports & boots (no key needed to import)**

Run: `uv run python -c "import adapters.openai_agents, main; print('ok')"`
Expected: `ok` (import must not require the API key — `_build_agent` is called lazily in `OpenAIAgentsAdapter.__init__`, so importing the modules is fine; constructing the adapter needs the key, which is exercised only at serve time).
If construction-at-import is a problem, ensure `_build_agent` runs only inside `__init__`, not at module load (it does).

- [ ] **Step 4: Commit**

```bash
git add contrib/shims/python/adapters/ contrib/shims/python/main.py
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(shim): OpenAI Agents SDK adapter + main entrypoint"
```

---

### Task 8: Example config, README, .gitignore, and end-to-end wiring

**Files:**
- Create: `contrib/shims/python/runtime.openai-shim.yaml`
- Create: `contrib/shims/python/README.md`
- Modify: `.gitignore` (ignore shim.db + Python caches under contrib)
- Modify: `README.md` (root — add a short "Hosting a foreign-SDK agent" pointer)

- [ ] **Step 1: Write `runtime.openai-shim.yaml`**

```yaml
# Host the OpenAI Agents SDK agent via the Python contract shim.
# Run from the repo root:
#   export OPENAI_API_KEY=...  OPENAI_BASE_URL=https://litellm-stg.aip.gov.sg  OPENAI_MODEL=gpt-5.4
#   RUNTIME_CONFIG=contrib/shims/python/runtime.openai-shim.yaml ./bin/runtimed
# (Adjust workdir to an absolute path on your machine.)
agents:
  - id: openai
    name: OpenAI SDK Nutrition Agent
    model: openai/gpt-5.4
    listen_addr: 127.0.0.1:8301
    workdir: ./contrib/shims/python
    command: ["uv", "run", "python", "main.py"]
```
NOTE: `workdir` may be relative to runtimed's cwd. Document that running from the repo root makes `./contrib/shims/python` correct; otherwise use an absolute path.

- [ ] **Step 2: Write `contrib/shims/python/README.md`**

Document: what it is (a contract shim hosting a foreign-SDK agent under runtime); the architecture (reusable `runtime_contract` lib + per-framework adapter); prerequisites (`uv sync`, env vars); how to run standalone (`RUNTIME_LISTEN_ADDR=127.0.0.1:8301 uv run python main.py`) and under runtimed (the example config); how to run tests (`uv run pytest`); the Level-1 durability scope (sessions/events persist in `shim.db`; a mid-run crash is NOT resumed — Level 2 is future); and a "Adding another framework" section showing the `AgentAdapter` stub (one new file in `adapters/`).

- [ ] **Step 3: Update `.gitignore`**

Add:
```
# Python shim artifacts
contrib/shims/python/shim.db
contrib/shims/python/.venv/
contrib/shims/python/**/__pycache__/
contrib/shims/python/.pytest_cache/
*.db-wal
*.db-shm
```

- [ ] **Step 4: Add a pointer in the root README**

Under the deployment section, add a short subsection "### Hosting a foreign-SDK agent (Python shim)" pointing at `contrib/shims/python/` and `runtime.openai-shim.yaml`, noting it uses the generalized `command:`/`workdir:` config and provides Level-1 durability. Cross-check the run commands match (`RUNTIME_CONFIG=...`, the binaries built via `make build`).

- [ ] **Step 5: Build + full hermetic Go tests + Python tests**

Run: `go build ./... && go test ./...` → green (Python-free).
Run: `cd contrib/shims/python && uv run pytest -q` → green.

- [ ] **Step 6: Commit**

```bash
git add contrib/shims/python/runtime.openai-shim.yaml contrib/shims/python/README.md .gitignore README.md
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "docs(shim): example config, README, gitignore for the OpenAI shim"
```

---

## Final verification (after all tasks)

- [ ] `go build ./...` — clean
- [ ] `go vet ./...` — clean
- [ ] `go test ./...` — green, **Python-free** (no shim import in any Go test)
- [ ] `go test -tags integration ./test/ -count=1 -timeout 300s` — existing suite green (spawn-path change backward compatible)
- [ ] `cd contrib/shims/python && uv run pytest -q` — green (store + contract tests)
- [ ] **Manual E2E (documented, needs OPENAI_* + Postgres):**
  1. `make build`
  2. `cd contrib/shims/python && uv sync`
  3. From repo root: `export OPENAI_API_KEY=... OPENAI_BASE_URL=https://litellm-stg.aip.gov.sg OPENAI_MODEL=gpt-5.4`
  4. `RUNTIME_CONFIG=contrib/shims/python/runtime.openai-shim.yaml ./bin/runtimed`
  5. In another shell: `./bin/runtimectl conformance --agent openai` → **PASS** (the acceptance gate)
  6. `./bin/runtimectl invoke --agent openai "Investigate: Product: Test Soda. Sugar 11g/100ml, sat fat 0g/100ml. Beverage. Additives: E211."` → verdict streams
  7. Restart runtimed → `./bin/runtimectl sessions --agent openai` still lists the session; `/stream?since=0` replays it (Level-1 proof)
- [ ] Dispatch a final review, then finish the branch (merge to master).
