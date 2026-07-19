//go:build integration

package test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// This file is the P1.3 end-to-end proof that per-session token/cost metering
// (config `pricing:` → RUNTIME_AGENT_PRICING → per-turn cost math → persisted
// sessions.tokens_total/cost_usd + fanned-out agent_cost_usd_total/
// agent_tokens_total metrics) is actually live against a REAL runtimed +
// agentd + Postgres stack:
//
//	TestMeteringLifecycle — a priced scripted session accrues token usage and
//	                        dollar cost, both persisted to the sessions row and
//	                        exposed on the agent session API and /metrics.
//
// It reuses the P1.2 lifecycle harness (lmBoot/lmOpenDB/lmPostSession/
// lmSessionRow/lmTerminal from limits_test.go, asEventually from
// autoscale_test.go, getBody from observability_e2e_test.go, waitURL from
// multiagent_test.go) — same package, so directly callable.
//
// Ports: ctl 8990, agent 8991 — no collision with any other integration test
// (limits 89xx-8940, autoscale 88xx, replica_pools 87xx, multiagent 81xx).

// TestMeteringLifecycle drives ONE priced scripted session to a terminal state
// and asserts usage was metered three ways:
//
//	(a) the Postgres sessions row has tokens_total > 0 AND cost_usd > 0;
//	(b) the agent session API (GET /agents/met1/sessions/{sid}) returns the same
//	    non-zero tokens_total/cost_usd in its JSON;
//	(c) the control-plane /metrics exposition carries
//	    agent_cost_usd_total{...tenant="acme"...model="test/scripted"...} and
//	    agent_tokens_total{...model="test/scripted"...}.
//
// The agent runs TESTAGENT_MODE=loop (a marker tool call EVERY turn, 150 tokens
// per turn — 100 in + 50 out — so token usage is GUARANTEED > 0). Loop never
// finishes on its own, so a limits: {max_turns: 3} cap terminates the session
// limit_exceeded after 3 turns; usage is still persisted, which is all this test
// asserts. Pricing is deliberately extreme ($1e6/Mtok ⇒ $1 per token) so cost is
// unmistakably positive (3 turns × 150 tok = 450 tok ⇒ $450) regardless of the
// scripted model's exact counts.
func TestMeteringLifecycle(t *testing.T) {
	db := lmOpenDB(t)

	cfg := "" +
		"pricing:\n" +
		"  models:\n" +
		"    test/scripted:\n" +
		"      input: 1000000\n" +
		"      output: 1000000\n" +
		"agents:\n" +
		"  - {id: met1, name: Met1, model: test/scripted, listen_addr: 127.0.0.1:8991, tenant: acme, limits: {max_turns: 3}}\n"
	base := lmBoot(t, db, cfg, "127.0.0.1:8990", "TESTAGENT_MODE=loop")
	waitURL(t, base+"/agents/met1/healthz", 30*time.Second)

	sid := lmPostSession(t, base, "met1")
	t.Logf("session id = %s", sid)

	// Loop + max_turns:3 terminates limit_exceeded; any terminal state means the
	// per-turn usage has been checkpointed/persisted.
	var finalStatus string
	if !asEventually(t, 30*time.Second, func() bool {
		s, _ := lmSessionRow(t, base, "met1", sid)
		finalStatus = s
		return lmTerminal(s)
	}) {
		t.Fatalf("session never terminal; last status %q", finalStatus)
	}
	t.Logf("terminal status = %q", finalStatus)

	// (a) The Postgres sessions row carries persisted usage AND cost.
	var tokens int64
	var cost float64
	if err := db.QueryRow(
		`SELECT tokens_total, cost_usd FROM sessions WHERE id=$1`, sid,
	).Scan(&tokens, &cost); err != nil {
		t.Fatalf("read persisted usage: %v", err)
	}
	if tokens <= 0 || cost <= 0 {
		t.Fatalf("persisted usage must be > 0: tokens=%d cost=%v", tokens, cost)
	}
	t.Logf("DB OK: sessions.tokens_total=%d cost_usd=%v", tokens, cost)

	// (b) The agent session API returns the same non-zero usage in its JSON.
	apiTokens, apiCost := meterSessionUsage(t, base, "met1", sid)
	if apiTokens <= 0 || apiCost <= 0 {
		t.Fatalf("session API usage must be > 0: tokens_total=%d cost_usd=%v",
			apiTokens, apiCost)
	}
	if apiTokens != tokens || apiCost != cost {
		t.Fatalf("session API usage (tokens=%d cost=%v) != DB row (tokens=%d cost=%v)",
			apiTokens, apiCost, tokens, cost)
	}
	t.Logf("API OK: GET /agents/met1/sessions/%s → tokens_total=%d cost_usd=%v",
		sid, apiTokens, apiCost)

	// (c) The control-plane /metrics exposition (agent metrics federate up
	// through the fan-out) carries cost with tenant+model labels and tokens with
	// a model label. The fan-out sub-scrape is live but may lag termination by a
	// beat, so poll briefly.
	if !asEventually(t, 15*time.Second, func() bool {
		body := getBody(t, base+"/metrics", nil, 200)
		return meterHasCost(body, "acme", "test/scripted") &&
			meterHasTokensModel(body, "test/scripted")
	}) {
		body := getBody(t, base+"/metrics", nil, 200)
		var got []string
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "agent_cost_usd_total{") ||
				strings.HasPrefix(line, "agent_tokens_total{") {
				got = append(got, line)
			}
		}
		t.Fatalf("/metrics missing agent_cost_usd_total{tenant=\"acme\",model=\"test/scripted\"} "+
			"and/or agent_tokens_total{...model=\"test/scripted\"...};\ncost/tokens lines present:\n%s",
			strings.Join(got, "\n"))
	}
	t.Log("metric OK: agent_cost_usd_total{tenant=\"acme\",model=\"test/scripted\"} and " +
		"agent_tokens_total{model=\"test/scripted\"} present")
}

// meterSessionUsage fetches GET /agents/{agent}/sessions/{sid} through the
// control-plane router and returns the reported tokens_total/cost_usd.
func meterSessionUsage(t *testing.T, base, agent, sid string) (int64, float64) {
	t.Helper()
	resp, err := http.Get(base + "/agents/" + agent + "/sessions/" + sid)
	if err != nil {
		t.Fatalf("get session %s: %v", sid, err)
	}
	defer resp.Body.Close()
	var row struct {
		TokensTotal int64   `json:"tokens_total"`
		CostUSD     float64 `json:"cost_usd"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&row); err != nil {
		t.Fatalf("decode session %s: %v", sid, err)
	}
	return row.TokensTotal, row.CostUSD
}

// meterHasCost reports whether the merged exposition carries an
// agent_cost_usd_total series with the given tenant and model labels and a
// strictly-positive value. The fan-out injects a replica label server-side, so
// we match on label content, not an exact rendered series string.
func meterHasCost(body, tenant, model string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "agent_cost_usd_total{") {
			continue
		}
		if strings.Contains(line, `tenant="`+tenant+`"`) &&
			strings.Contains(line, `model="`+model+`"`) &&
			meterPositiveSample(line) {
			return true
		}
	}
	return false
}

// meterHasTokensModel reports whether agent_tokens_total carries the model
// label (P1.3 widened the token series with tenant/model beyond the original
// direction label) with a strictly-positive value.
func meterHasTokensModel(body, model string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "agent_tokens_total{") {
			continue
		}
		if strings.Contains(line, `model="`+model+`"`) && meterPositiveSample(line) {
			return true
		}
	}
	return false
}

// meterPositiveSample reports whether a Prometheus text-exposition line's
// trailing sample value parses as a number > 0.
func meterPositiveSample(line string) bool {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false
	}
	v := fields[len(fields)-1]
	// Cheap positive check without importing strconv: a value > 0 is any sample
	// that is neither "0" nor starts with "0" mantissa forms nor a "-" sign, and
	// contains a nonzero digit. Simplest robust route: reject "0"/"-…" and
	// require a nonzero digit somewhere.
	if strings.HasPrefix(v, "-") || v == "0" || v == "0.0" {
		return false
	}
	for _, r := range v {
		if r >= '1' && r <= '9' {
			return true
		}
	}
	return false
}
