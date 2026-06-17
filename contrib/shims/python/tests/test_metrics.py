"""Metrics wire-contract tests: the shim's /metrics must match the Go emitter
(internal/obs/obs.go) family names, types, label sets, and histogram buckets, so
the control-plane fan-out merge accepts them."""
from prometheus_client.parser import text_string_to_metric_families

from runtime_contract.metrics import Metrics

# Must mirror obs.go turnBuckets exactly (plus +Inf, which the client adds).
EXPECTED_BUCKETS = [0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120]


def _scrape(m: Metrics) -> str:
    # render() is exactly what the /metrics route serves.
    body, _ = m.render()
    return body.decode()


def test_family_names_types_match_go_contract():
    m = Metrics("agent-x")
    m.observe_turn("completed", 1.5, {"input": 100, "output": 20})
    m.observe_tool("check_sfa_additive")
    fams = {f.name: f for f in text_string_to_metric_families(_scrape(m))}
    # Counter families are exposed without the _total suffix by the parser's
    # .name, so assert on the documented names.
    assert "agent_turns" in fams and fams["agent_turns"].type == "counter"
    assert fams["agent_turn_duration_seconds"].type == "histogram"
    assert "agent_tokens" in fams and fams["agent_tokens"].type == "counter"
    assert "agent_tool_calls" in fams and fams["agent_tool_calls"].type == "counter"


def test_labels_and_values():
    m = Metrics("agent-x")
    m.observe_turn("completed", 0.7, {"input": 100, "output": 20})
    m.observe_tool("recall_product")
    m.observe_tool("recall_product")
    text = _scrape(m)
    fams = {f.name: f for f in text_string_to_metric_families(text)}

    turns = {s.labels.get("outcome"): s.value for s in fams["agent_turns"].samples
             if s.name == "agent_turns_total"}
    assert turns == {"completed": 1.0}

    tokens = {s.labels.get("direction"): s.value for s in fams["agent_tokens"].samples
              if s.name == "agent_tokens_total"}
    assert tokens == {"input": 100.0, "output": 20.0}

    tools = {s.labels.get("tool"): s.value for s in fams["agent_tool_calls"].samples
             if s.name == "agent_tool_calls_total"}
    assert tools == {"recall_product": 2.0}


def test_histogram_buckets_match_go():
    m = Metrics("agent-x")
    m.observe_turn("completed", 0.3, None)
    fams = {f.name: f for f in text_string_to_metric_families(_scrape(m))}
    les = sorted(
        float(s.labels["le"])
        for s in fams["agent_turn_duration_seconds"].samples
        if s.name == "agent_turn_duration_seconds_bucket" and s.labels.get("le") != "+Inf"
    )
    assert les == sorted(EXPECTED_BUCKETS)


def test_no_agent_or_replica_label_emitted():
    # The control plane injects agent/replica server-side; the shim must not set
    # them (fanout overwrites, but clean output is the contract).
    m = Metrics("agent-x")
    m.observe_turn("completed", 0.3, {"input": 1, "output": 1})
    m.observe_tool("t")
    for fam in text_string_to_metric_families(_scrape(m)):
        for s in fam.samples:
            assert "agent" not in s.labels, f"unexpected agent label on {s.name}"
            assert "replica" not in s.labels, f"unexpected replica label on {s.name}"


def test_cache_directions_only_when_positive():
    m = Metrics("agent-x")
    m.observe_turn("completed", 0.3, {"input": 5, "output": 2, "cache_creation": 0, "cache_read": 7})
    fams = {f.name: f for f in text_string_to_metric_families(_scrape(m))}
    directions = {s.labels.get("direction") for s in fams["agent_tokens"].samples
                  if s.name == "agent_tokens_total"}
    assert "cache_read" in directions
    assert "cache_creation" not in directions  # zero ⇒ not emitted


def test_error_outcome_recorded():
    m = Metrics("agent-x")
    m.observe_turn("error", 0.1, None)
    fams = {f.name: f for f in text_string_to_metric_families(_scrape(m))}
    turns = {s.labels.get("outcome"): s.value for s in fams["agent_turns"].samples
             if s.name == "agent_turns_total"}
    assert turns == {"error": 1.0}
