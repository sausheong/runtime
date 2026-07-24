package netpolicy

import (
	"net"
	"testing"
)

func TestValidatePublicHTTPURL(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:8080",
		"http://[::1]/",
		"http://169.254.169.254/latest/meta-data",
		"http://10.0.0.1",
		"http://localhost",
		"http://metadata.google.internal",
		"http://user:pass@example.com",
		"file:///etc/passwd",
	} {
		if err := ValidatePublicHTTPURL(raw); err == nil {
			t.Errorf("ValidatePublicHTTPURL(%q) succeeded, want rejection", raw)
		}
	}
	for _, raw := range []string{
		"https://example.com",
		"https://8.8.8.8/dns-query",
	} {
		if err := ValidatePublicHTTPURL(raw); err != nil {
			t.Errorf("ValidatePublicHTTPURL(%q): %v", raw, err)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.0", "10.1.2.3", "100.64.0.1", "127.0.0.1",
		"169.254.169.254", "172.16.0.1", "192.168.1.1",
		"198.18.0.1", "192.0.2.1", "203.0.113.1",
		"::1", "fc00::1", "fe80::1", "2001:db8::1",
	} {
		if IsPublicIP(net.ParseIP(raw)) {
			t.Errorf("IsPublicIP(%s)=true, want false", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !IsPublicIP(net.ParseIP(raw)) {
			t.Errorf("IsPublicIP(%s)=false, want true", raw)
		}
	}
}
