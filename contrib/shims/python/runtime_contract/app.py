"""FastAPI app serving the runtime agent contract for a single foreign agent."""
from __future__ import annotations
import asyncio
import base64
import binascii
import datetime
import hmac
import json
import logging
import time
from contextlib import asynccontextmanager

from fastapi import FastAPI, HTTPException, Request, Response
from fastapi.responses import JSONResponse, PlainTextResponse, StreamingResponse

from .adapter import AgentAdapter
from .events import ContractEvent, Image
from .metrics import Metrics
from .sse import frame
from .store import Store

CONTRACT_VERSION = "v1"
MAX_SESSION_BODY_BYTES = 16 << 20
_SHUTDOWN_GRACE_SECONDS = 10
log = logging.getLogger(__name__)

# Telemetry events an adapter may yield for metrics only; never published to the
# client SSE stream nor persisted (see events.py).
_TELEMETRY_TYPES = ("usage", "tool_call")


def create_app(
    adapter: AgentAdapter,
    store: Store,
    agent_id: str,
    metrics: Metrics | None = None,
    auth_token: str = "",
) -> FastAPI:
    background_tasks: set[asyncio.Task] = set()

    @asynccontextmanager
    async def lifespan(_app: FastAPI):
        yield
        if background_tasks:
            _, pending = await asyncio.wait(set(background_tasks), timeout=_SHUTDOWN_GRACE_SECONDS)
            for task in pending:
                task.cancel()
            if pending:
                await asyncio.gather(*pending, return_exceptions=True)
        store.close()

    app = FastAPI(lifespan=lifespan)
    live: dict[str, list[asyncio.Queue]] = {}
    live_lock = asyncio.Lock()
    active_sessions: set[str] = set()
    active_lock = asyncio.Lock()
    # Per-session token usage buffered from a "usage" telemetry event until the
    # turn completes, so each turn records one duration sample carrying its tokens.
    _pending_usage: dict[str, dict | None] = {}

    if auth_token:
        @app.middleware("http")
        async def require_bearer(req: Request, call_next):  # noqa: ANN202
            if req.method == "GET" and req.url.path in ("/healthz", "/readyz"):
                return await call_next(req)
            if not hmac.compare_digest(
                req.headers.get("authorization", ""),
                f"Bearer {auth_token}",
            ):
                return JSONResponse({"error": "unauthorized"}, status_code=401)
            return await call_next(req)

    async def read_session_json(req: Request) -> dict:
        """Read a bounded JSON object without letting Starlette buffer an
        arbitrary chunked upload first."""
        content_length = req.headers.get("content-length")
        if content_length and content_length.isdigit() and int(content_length) > MAX_SESSION_BODY_BYTES:
            raise HTTPException(status_code=413, detail="request body too large")
        raw = bytearray()
        async for chunk in req.stream():
            if len(raw) + len(chunk) > MAX_SESSION_BODY_BYTES:
                raise HTTPException(status_code=413, detail="request body too large")
            raw.extend(chunk)
        try:
            body = json.loads(raw)
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            raise HTTPException(status_code=400, detail="invalid JSON") from exc
        if not isinstance(body, dict):
            raise HTTPException(status_code=400, detail="JSON body must be an object")
        return body

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
                    if ev.type == "usage":
                        # Persist tokens onto the session so GET /sessions/{id}
                        # reports tokens_total (the control plane surfaces it like
                        # a native agent's metering). Independent of metrics: the
                        # store carries the durable per-session total, Prometheus
                        # the per-turn sample.
                        u = ev.usage or {}
                        store.add_tokens(sid, int(u.get("input", 0) or 0) + int(u.get("output", 0) or 0))
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
        except asyncio.CancelledError:
            terminal = ContractEvent(type="error", error="agent execution interrupted")
        except Exception:  # never crash the server or expose provider internals
            log.exception("adapter execution failed", extra={"session_id": sid})
            terminal = ContractEvent(type="error", error="agent execution failed")
        finally:
            duration_ms = int((time.monotonic() - start) * 1000)
            completed_at = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
            final_status = "completed" if terminal.type == "done" else "error"
            store.set_completed(sid, final_status, completed_at, duration_ms)
            store.increment_turn_count(sid)
            if metrics is not None:
                outcome = "completed" if terminal.type == "done" else "error"
                metrics.observe_turn(outcome, time.monotonic() - start, _pending_usage.pop(sid, None))
            await publish(sid, terminal)
            async with active_lock:
                active_sessions.discard(sid)

    async def launch_session(sid: str, message: str, images: list[Image]) -> None:
        async with active_lock:
            if sid in active_sessions:
                raise HTTPException(status_code=409, detail="session already has an active turn")
            active_sessions.add(sid)
        task = asyncio.create_task(run_session(sid, message, images))
        background_tasks.add(task)
        task.add_done_callback(background_tasks.discard)

    def parse_images(body: dict) -> list[Image]:
        images: list[Image] = []
        def decode_image(data: object, mime: object) -> Image:
            if not isinstance(data, str):
                raise HTTPException(status_code=400, detail="image data must be base64 text")
            if mime is not None and not isinstance(mime, str):
                raise HTTPException(status_code=400, detail="image mime must be text")
            try:
                raw = base64.b64decode(data, validate=True)
            except (binascii.Error, ValueError) as exc:
                raise HTTPException(status_code=400, detail="invalid base64 image") from exc
            return Image(mime=mime or "image/jpeg", data=raw)

        # Single-image legacy form: image_b64 + image_mime
        b64 = body.get("image_b64")
        if b64 is not None:
            images.append(decode_image(b64, body.get("image_mime")))
        # Multi-image form: images=[{data: <b64>, mime: <mime>}, ...]
        multi = body.get("images") or []
        if not isinstance(multi, list):
            raise HTTPException(status_code=400, detail="images must be an array")
        for img in multi:
            if not isinstance(img, dict):
                raise HTTPException(status_code=400, detail="each image must be an object")
            if "data" in img:
                images.append(decode_image(img["data"], img.get("mime")))
        return images

    @app.get("/healthz", response_class=PlainTextResponse)
    async def healthz() -> str:
        return "ok"

    @app.get("/readyz", response_class=PlainTextResponse)
    async def readyz() -> str:
        if not store.ready():
            raise HTTPException(status_code=503, detail="not ready")
        return "ready"

    @app.get("/meta")
    async def meta() -> JSONResponse:
        return JSONResponse({"agent_id": agent_id, "contract_version": CONTRACT_VERSION})

    @app.post("/sessions")
    async def create_session(req: Request) -> JSONResponse:
        body = await read_session_json(req)
        images = parse_images(body)
        sid = store.create_session()
        await launch_session(sid, body.get("message", ""), images)
        return JSONResponse({"session_id": sid})

    @app.post("/sessions/{sid}/messages")
    async def post_message(sid: str, req: Request) -> JSONResponse:
        # Follow-up turn on an EXISTING session: the adapter is re-invoked with
        # the same session id, so adapters with per-session memory (SQLiteSession,
        # SDK resume maps) continue the conversation. Additive to the v1 contract
        # (conformance does not require it).
        if not store.get_session(sid):
            return JSONResponse({"error": "not found"}, status_code=404)
        body = await read_session_json(req)
        images = parse_images(body)
        await launch_session(sid, body.get("message", ""), images)
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
        if not store.get_session(sid):
            raise HTTPException(status_code=404, detail="session not found")
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
                    if not subs:
                        live.pop(sid, None)

        return StreamingResponse(gen(), media_type="text/event-stream")

    @app.get("/sessions/{sid}/events")
    async def events(sid: str, since: int = 0, limit: int = 50) -> JSONResponse:
        if not store.get_session(sid):
            raise HTTPException(status_code=404, detail="session not found")
        limit = min(max(limit, 1), 1000)
        rows = store.events_since(sid, since)
        rows = rows[-limit:]
        out = []
        for seq, ev in rows:
            item = ev.to_dict()
            item["seq"] = seq
            out.append(item)
        return JSONResponse(out)

    return app
