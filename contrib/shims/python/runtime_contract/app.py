"""FastAPI app serving the runtime agent contract for a single foreign agent."""
from __future__ import annotations
import asyncio
import base64

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, PlainTextResponse, StreamingResponse

from .adapter import AgentAdapter
from .events import ContractEvent, Image
from .sse import frame
from .store import Store

CONTRACT_VERSION = "v1"


def create_app(adapter: AgentAdapter, store: Store, agent_id: str) -> FastAPI:
    app = FastAPI()
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
            async with live_lock:
                live.setdefault(sid, []).append(q)
            try:
                last = since
                buffered = store.events_since(sid, since)
                for seq, ev in buffered:
                    last = seq
                    yield frame(seq, ev)
                    if ev.type in ("done", "error"):
                        return
                while True:
                    seq, ev = await q.get()
                    if seq <= last:
                        continue
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
