package main

import "testing"

func TestGatewaySelfURL(t *testing.T) {
	cases := []struct {
		selfURL, ctlAddr, want string
	}{
		{"", ":8080", "http://127.0.0.1:8080/gateway/mcp"},
		{"", "0.0.0.0:9090", "http://127.0.0.1:9090/gateway/mcp"},
		{"", "127.0.0.1:8081", "http://127.0.0.1:8081/gateway/mcp"},
		{"", "10.0.0.5:8080", "http://10.0.0.5:8080/gateway/mcp"},
		{"http://gw.example.com", ":8080", "http://gw.example.com/gateway/mcp"},
		{"http://gw.example.com/", ":8080", "http://gw.example.com/gateway/mcp"},
	}
	for _, c := range cases {
		if got := gatewaySelfURL(c.selfURL, c.ctlAddr); got != c.want {
			t.Errorf("gatewaySelfURL(%q,%q) = %q, want %q", c.selfURL, c.ctlAddr, got, c.want)
		}
	}
}
