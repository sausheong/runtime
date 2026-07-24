//go:build integration

package test

import (
	"net"
	"net/url"
	"os"
)

// integrationMetricsURL returns the management metrics endpoint inherited by
// runtimed subprocesses. The Makefile assigns a test-only port so assertions do
// not accidentally target the public control-plane listener.
func integrationMetricsURL() string {
	addr := os.Getenv("RUNTIME_METRICS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9091"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/metrics"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/metrics"}).String()
}
