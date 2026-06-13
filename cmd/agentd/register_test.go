package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestOrdinalFromHostname(t *testing.T) {
	cases := map[string]int{"support-0": 0, "support-3": 3, "x-y-12": 12, "nohyphen": 0, "": 0, "x-": 0, "support-x": 0}
	for host, want := range cases {
		if got := ordinalFromHostname(host); got != want {
			t.Fatalf("ordinalFromHostname(%q)=%d want %d", host, got, want)
		}
	}
}

func TestFetchRegistrationSetsEnv(t *testing.T) {
	var gotOrdinal int
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct{ Ordinal int }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotOrdinal = body.Ordinal
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"env": map[string]string{"RUNTIME_PG_DSN": "dsn://fetched", "OPENAI_API_KEY": "sk-1"},
		})
	}))
	defer srv.Close()

	t.Setenv("RUNTIME_REGISTRATION_URL", srv.URL)
	t.Setenv("RUNTIME_REGISTRATION_TOKEN", "svk-abc.def")
	t.Setenv("HOSTNAME", "support-2")
	// Ensure target var starts empty.
	os.Unsetenv("RUNTIME_PG_DSN")

	fetchRegistration()

	if gotAuth != "Bearer svk-abc.def" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotOrdinal != 2 {
		t.Fatalf("ordinal = %d want 2", gotOrdinal)
	}
	if os.Getenv("RUNTIME_PG_DSN") != "dsn://fetched" || os.Getenv("OPENAI_API_KEY") != "sk-1" {
		t.Fatalf("env not applied: DSN=%q KEY=%q", os.Getenv("RUNTIME_PG_DSN"), os.Getenv("OPENAI_API_KEY"))
	}
}

// TestFetchRegistrationSkipsEmpty proves an empty-valued delta entry does NOT
// clobber an inherited infra-provided var. For a REMOTE agent the control plane
// returns RUNTIME_LISTEN_ADDR="" (it sets BaseURL, not Addr); applying it would
// blank the operator/pod-provided bind addr and agentd could not start.
func TestFetchRegistrationSkipsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"env": map[string]string{
				"RUNTIME_PG_DSN":      "dsn://fetched",
				"RUNTIME_LISTEN_ADDR": "", // empty ⇒ must NOT overwrite inherited
			},
		})
	}))
	defer srv.Close()

	t.Setenv("RUNTIME_REGISTRATION_URL", srv.URL)
	t.Setenv("RUNTIME_REGISTRATION_TOKEN", "svk-abc.def")
	t.Setenv("HOSTNAME", "support-0")
	t.Setenv("RUNTIME_LISTEN_ADDR", ":8080") // infra-provided, must survive
	os.Unsetenv("RUNTIME_PG_DSN")

	fetchRegistration()

	if got := os.Getenv("RUNTIME_LISTEN_ADDR"); got != ":8080" {
		t.Fatalf("RUNTIME_LISTEN_ADDR clobbered by empty delta value: got %q want \":8080\"", got)
	}
	if got := os.Getenv("RUNTIME_PG_DSN"); got != "dsn://fetched" {
		t.Fatalf("RUNTIME_PG_DSN not applied: got %q", got)
	}
}

func TestFetchRegistrationNoopWhenUnset(t *testing.T) {
	os.Unsetenv("RUNTIME_REGISTRATION_URL")
	os.Unsetenv("RUNTIME_REGISTRATION_TOKEN")
	// Must not panic / must not block.
	fetchRegistration()
}
