package gateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/obs"
)

// scrapeControlRegistry renders cm's registry through the fan-out handler
// (no agent targets) and returns the exposition body.
func scrapeControlRegistry(t *testing.T, cm *obs.ControlMetrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	obs.FanoutHandler(cm, func() []obs.ScrapeTarget { return nil }).
		ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestToolCallMetricsRecorded(t *testing.T) {
	conns := map[string]*fakeConn{
		"open": {tools: []tool.Tool{
			fakeTool{name: "mcp__open__echo", out: "hi"},
			fakeTool{name: "mcp__open__boom", err: "kaput"},
		}},
	}
	m := startManager(t, []config.GatewayServer{{Name: "open", Command: "x"}}, conns)
	h := NewHandler(m)
	cm := obs.NewControlMetrics()
	h.Metrics = cm
	sess := dialGateway(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator})

	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "open__echo", Arguments: map[string]any{},
	})
	if err != nil || res.IsError {
		t.Fatalf("success call failed: %v %+v", err, res)
	}
	res, err = sess.CallTool(context.Background(), &sdk.CallToolParams{Name: "open__boom"})
	if err != nil {
		t.Fatalf("error call: transport error, want isError result: %v", err)
	}
	if !res.IsError {
		t.Fatal("boom call should be IsError")
	}

	body := scrapeControlRegistry(t, cm)
	okSeries := `runtime_gateway_tool_calls_total{outcome="ok",server="open",tool="open__echo"} 1`
	errSeries := `runtime_gateway_tool_calls_total{outcome="error",server="open",tool="open__boom"} 1`
	if !strings.Contains(body, okSeries) {
		t.Fatalf("missing ok series %q in scrape:\n%s", okSeries, body)
	}
	if !strings.Contains(body, errSeries) {
		t.Fatalf("missing error series %q in scrape:\n%s", errSeries, body)
	}
}

func TestAuthzRejectionNotCounted(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	cm := obs.NewControlMetrics()
	h.Metrics = cm
	// Viewer role gate: rejected before the call reaches the upstream.
	viewer := dialGateway(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleViewer})
	res, err := viewer.CallTool(context.Background(), &sdk.CallToolParams{Name: "open__echo"})
	if err != nil {
		t.Fatalf("expected isError result, got transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("viewer call should be IsError")
	}
	// Cross-principal view mismatch: also rejected before Execute.
	h.PrincipalFor = func(_ context.Context) (identity.Principal, bool) {
		return identity.Principal{TenantID: "globex", Role: identity.RoleOperator}, true
	}
	res, err = viewer.CallTool(context.Background(), &sdk.CallToolParams{Name: "open__echo"})
	if err != nil {
		t.Fatalf("expected isError result, got transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("cross-principal call should be IsError")
	}

	body := scrapeControlRegistry(t, cm)
	if n := strings.Count(body, "runtime_gateway_tool_calls_total{"); n != 0 {
		t.Fatalf("authz rejections counted as gateway calls (%d series):\n%s", n, body)
	}
}

func TestUpstreamUpGaugeTransitions(t *testing.T) {
	fc := &fakeConn{tools: []tool.Tool{fakeTool{name: "mcp__fs__read", out: "d"}}}
	// First dial yields fc; redials are gated off so the gauge stays 0 after
	// markDown (mirrors TestManagerMarkDownTriggersRedial's gating rationale).
	var dials atomic.Int32
	dial := func(_ context.Context, _ config.GatewayServer) (upstreamConn, error) {
		if dials.Add(1) == 1 {
			return fc, nil
		}
		return nil, errors.New("redial gated")
	}
	cm := obs.NewControlMetrics()
	m := NewManager([]config.GatewayServer{{Name: "fs", Command: "x"}},
		WithDial(dial),
		WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	m.Metrics = cm // BEFORE Start: first connect transition must be recorded
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return len(m.AllTools()) == 1 })

	upSeries := `runtime_gateway_upstream_up{server="fs"} 1`
	if body := scrapeControlRegistry(t, cm); !strings.Contains(body, upSeries) {
		t.Fatalf("missing %q after connect:\n%s", upSeries, body)
	}

	// Force the down transition the way TestServerRebuildsOnGenerationChange
	// does: markDown with the conn identity captured under the lock.
	u := m.ups[0]
	u.mu.Lock()
	observed := u.conn
	u.mu.Unlock()
	m.markDown(u, observed, errors.New("session died"))

	downSeries := `runtime_gateway_upstream_up{server="fs"} 0`
	if body := scrapeControlRegistry(t, cm); !strings.Contains(body, downSeries) {
		t.Fatalf("missing %q after markDown:\n%s", downSeries, body)
	}
}

func TestUpstreamUpGaugeZeroOnFirstFailedDial(t *testing.T) {
	// The gauge must exist as 0 from the first failed connect attempt, not
	// only after a previous up state.
	dial := func(_ context.Context, _ config.GatewayServer) (upstreamConn, error) {
		return nil, errors.New("scripted dial failure")
	}
	cm := obs.NewControlMetrics()
	m := NewManager([]config.GatewayServer{{Name: "fs", Command: "x"}},
		WithDial(dial),
		WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	m.Metrics = cm
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.Start(ctx)
	waitFor(t, 2*time.Second, func() bool {
		s := m.Status("")
		return len(s) == 1 && s[0].LastError != ""
	})

	downSeries := `runtime_gateway_upstream_up{server="fs"} 0`
	if body := scrapeControlRegistry(t, cm); !strings.Contains(body, downSeries) {
		t.Fatalf("missing %q after failed dial:\n%s", downSeries, body)
	}
}
