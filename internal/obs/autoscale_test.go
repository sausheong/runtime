package obs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestAutoscaleMetricsExposed(t *testing.T) {
	c := NewControlMetrics()
	c.AutoscaleDesired("ag", 3)
	c.AutoscaleCurrent("ag", 2)
	c.AutoscaleActive("ag", 5)
	c.AutoscaleEvent("ag", "up")
	c.AutoscaleEvent("ag", "up")

	srv := httptest.NewServer(promhttp.HandlerFor(c.reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	_, _ = io.Copy(buf, resp.Body)
	body := buf.String()

	for _, want := range []string{
		`runtime_agent_replicas_desired{agent="ag"} 3`,
		`runtime_agent_replicas_current{agent="ag"} 2`,
		`runtime_agent_active_sessions{agent="ag"} 5`,
		`runtime_autoscale_events_total{action="up",agent="ag"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}

func TestAutoscaleMetricsNilSafe(t *testing.T) {
	var c *ControlMetrics
	c.AutoscaleDesired("ag", 1)
	c.AutoscaleCurrent("ag", 1)
	c.AutoscaleActive("ag", 1)
	c.AutoscaleEvent("ag", "up")
}
