"""FastAPI app serving the runtime agent contract for a single foreign agent."""
from __future__ import annotations
import asyncio
import base64
import datetime
import time

from fastapi import FastAPI, Request, Response
from fastapi.responses import JSONResponse, PlainTextResponse, StreamingResponse

from .adapter import AgentAdapter
from .events import ContractEvent, Image
from .metrics import Metrics
from .sse import frame
from .store import Store

CONTRACT_VERSION = "v1"

# Telemetry events an adapter may yield for metrics only; never published to the
# client SSE stream nor persisted (see events.py).
_TELEMETRY_TYPES = ("usage", "tool_call")


def create_app(
    adapter: AgentAdapter,
    store: Store,
    agent_id: str,
    metrics: Metrics | None = None,
) -> FastAPI:
    app = FastAPI()
    live: dict[str, list[asyncio.Queue]] = {}
    live_lock = asyncio.Lock()
    # Per-session token usage buffered from a "usage" telemetry event until the
    # turn completes, so each turn records one duration sample carrying its tokens.
    _pending_usage: dict[str, dict | None] = {}

    # /metrics: served at EXACTLY /metrics (a plain route, not app.mount, which
    # would 307-redirect /metrics -> /metrics/ — the fan-out scraper GETs
    # /metrics and does not follow redirects). Only registered when metrics is
    # enabled; otherwise /metrics 404s, which the scraper treats as "no_metrics"
    # (the agent serves HTTP but exposes no metrics), leaving its up gauge at 1.
    if metrics is not None:
        @app.get("/metrics")
        async def metrics_endpoint():  # noqa: ANN202 (FastAPI route)
            body, content_type = metrics.render()
            return Response(content=body, media_type=content_type)

    async def publish(sid: str, ev: ContractEvent) -> None:
        seq = store.append_event(sid, ev)
        async with live_lock:
            subs = list(live.get(sid, []))
        for q in subs:
            q.put_nowait((seq, ev))

    async def run_session(sid: str, message: str, images: list[Image]) -> None:
        store.set_status(sid, "running")
        terminal = ContractEvent(type="done")
        start = time.monotonic()
        try:
            history = [ev for _, ev in store.events_since(sid, 0)]
            async for ev in adapter.run(sid, message, images, history):
                # Telemetry events (usage/tool_call) feed metrics ONLY — they are
                # not published to the client stream nor persisted.
                if ev.type in _TELEMETRY_TYPES:
                    if metrics is not None:
                        if ev.type == "tool_call":
                            metrics.observe_tool(ev.tool)
                        elif ev.type == "usage":
                            # Buffer usage for the single observe_turn below, so a
                            # turn yields exactly one duration sample with tokens.
                            _pending_usage[sid] = ev.usage
                    continue
                await publish(sid, ev)
                if ev.type == "error":
                    terminal = ContractEvent(type="error", error=ev.error or "agent error")
        except Exception as e:  # never crash the server
            terminal = ContractEvent(type="error", error=str(e))
        duration_ms = int((time.monotonic() - start) * 1000)
        completed_at = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        final_status = "completed" if terminal.type == "done" else "error"
        store.set_completed(sid, final_status, completed_at, duration_ms)
        row = store.get_session(sid)
        store.set_turn_count(sid, (row["turn_count"] if row else 0) + 1)
        if metrics is not None:
            outcome = "completed" if terminal.type == "done" else "error"
            metrics.observe_turn(outcome, time.monotonic() - start, _pending_usage.pop(sid, None))
        await publish(sid, terminal)

    def parse_images(body: dict) -> list[Image]:
        images: list[Image] = []
        # Single-image legacy form: image_b64 + image_mime
        b64 = body.get("image_b64")
        if b64:
            images.append(Image(mime=body.get("image_mime") or "image/jpeg",
                                data=base64.b64decode(b64)))
        # Multi-image form: images=[{data: <b64>, mime: <mime>}, ...]
        for img in body.get("images") or []:
            if img.get("data"):
                images.append(Image(mime=img.get("mime") or "image/jpeg",
                                    data=base64.b64decode(img["data"])))
        return images

    @app.get("/healthz", response_class=PlainTextResponse)
    async def healthz() -> str:
        return "ok"

    @app.get("/meta")
    async def meta() -> JSONResponse:
        return JSONResponse({"agent_id": agent_id, "contract_version": CONTRACT_VERSION})

    @app.post("/sessions")
    async def create_session(req: Request) -> JSONResponse:
        body = await req.json()
        sid = store.create_session()
        asyncio.create_task(run_session(sid, body.get("message", ""), parse_images(body)))
        return JSONResponse({"session_id": sid})

    @app.post("/sessions/{sid}/messages")
    async def post_message(sid: str, req: Request) -> JSONResponse:
        # Follow-up turn on an EXISTING session: the adapter is re-invoked with
        # the same session id, so adapters with per-session memory (SQLiteSession,
        # SDK resume maps) continue the conversation. Additive to the v1 contract
        # (conformance does not require it).
        if not store.get_session(sid):
            return JSONResponse({"error": "not found"}, status_code=404)
        body = await req.json()
        asyncio.create_task(run_session(sid, body.get("message", ""), parse_images(body)))
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
