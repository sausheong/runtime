package main

import "testing"

func TestEffectiveBootstrapKey(t *testing.T) {
	tests := []struct {
		name       string
		configured bool
		key        string
		breakGlass bool
		want       string
	}{
		{name: "initial provisioning", key: "bootstrap", want: "bootstrap"},
		{name: "credential configured ignores bootstrap", configured: true, key: "bootstrap"},
		{name: "explicit recovery", configured: true, key: "bootstrap", breakGlass: true, want: "bootstrap"},
		{name: "empty remains empty", configured: true, breakGlass: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveBootstrapKey(tt.configured, tt.key, tt.breakGlass); got != tt.want {
				t.Fatalf("effectiveBootstrapKey() = %q, want %q", got, tt.want)
			}
		})
	}
}
