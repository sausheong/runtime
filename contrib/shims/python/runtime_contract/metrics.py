"""Prometheus metrics for a contract-shim agent — wire-compatible with the Go
agentruntime emitter (internal/obs/obs.go).

The control plane fan-out-scrapes each agent's /metrics and MERGES families BY
NAME. So the family names, metric TYPES, label sets, and histogram buckets here
MUST match the Go emitter exactly, or the merge drops them (type_conflict) or the
p50/p95 dashboard queries fail to aggregate across Go and Python agents:

    agent_turns_total            counter   label: outcome
    agent_turn_duration_seconds  histogram (buckets match Go turnBuckets)
    agent_tokens_total           counter   label: direction
    agent_tool_calls_total       counter   label: tool

The control plane INJECTS agent=<id>/replica=<n> server-side (fanout.go), so this
module deliberately does NOT set those labels — fanout overwrites them anyway, and
omitting them keeps the standalone exposition clean.
"""
from __future__ import annotations

from typing import Optional

from prometheus_client import (
    CONTENT_TYPE_LATEST,
    CollectorRegistry,
    Counter,
    Histogram,
    generate_latest,
)

# Must match obs.go turnBuckets exactly so histogram_quantile aggregates cleanly
# across the Go and Python agents' merged buckets.
_TURN_BUCKETS = (0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120)


class Metrics:
    """One Prometheus registry per shim process. Nil of this is not a concern in
    Python; callers pass `None` when metrics are disabled (app.py guards on it)."""

    def __init__(self, agent_id: str) -> None:
        # agent_id is accepted for symmetry with the Go NewAgentMetrics(agentID)
        # and future use, but is intentionally NOT a label: the control plane
        # injects the authoritative agent label at scrape time.
        self.agent_id = agent_id
        self.registry = CollectorRegistry()
        self._turns = Counter(
            "agent_turns_total",
            "Agent turns by outcome (completed/error/aborted/continue).",
            ["outcome"],
            registry=self.registry,
        )
        self._turn_dur = Histogram(
            "agent_turn_duration_seconds",
            "Agent turn wall time.",
            buckets=_TURN_BUCKETS,
            registry=self.registry,
        )
        self._tokens = Counter(
            "agent_tokens_total",
            "LLM tokens consumed, by direction (input/output/cache_creation/cache_read).",
            ["direction"],
            registry=self.registry,
        )
        self._tool_calls = Counter(
            "agent_tool_calls_total",
            "Tool calls dispatched by the agent loop.",
            ["tool"],
            registry=self.registry,
        )

    def observe_turn(
        self, outcome: str, duration_s: float, usage: Optional[dict] = None
    ) -> None:
        """Record one turn: outcome + wall time, and token counts if provided.

        `usage` is an optional dict {input, output, cache_creation, cache_read};
        cache directions are emitted only when > 0 (mirrors obs.go:289)."""
        self._turns.labels(outcome=outcome).inc()
        self._turn_dur.observe(duration_s)
        if usage:
            inp = int(usage.get("input", 0) or 0)
            out = int(usage.get("output", 0) or 0)
            if inp:
                self._tokens.labels(direction="input").inc(inp)
            if out:
                self._tokens.labels(direction="output").inc(out)
            cc = int(usage.get("cache_creation", 0) or 0)
            cr = int(usage.get("cache_read", 0) or 0)
            if cc > 0:
                self._tokens.labels(direction="cache_creation").inc(cc)
            if cr > 0:
                self._tokens.labels(direction="cache_read").inc(cr)

    def observe_tool(self, tool: str) -> None:
        """Record one tool call by tool name."""
        if not tool:
            return
        self._tool_calls.labels(tool=tool).inc()

    def render(self) -> tuple[bytes, str]:
        """Return (body, content_type) for this registry's exposition. Served at
        exactly /metrics (no trailing-slash redirect) so the control-plane
        fan-out scraper — which GETs /metrics and does not follow redirects —
        reaches it directly."""
        return generate_latest(self.registry), CONTENT_TYPE_LATEST
