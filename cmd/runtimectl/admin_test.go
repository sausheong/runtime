package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminPost_SendsAuthAndBody(t *testing.T) {
	var gotAuth, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"svk-1","plaintext":"svk-1.secret"}`))
	}))
	defer srv.Close()
	t.Setenv("RUNTIME_TOKEN", "boot-key")

	out, err := adminPost(srv.URL, "/admin/keys", map[string]string{"label": "ci", "role": "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer boot-key" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotPath != "/admin/keys" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"role":"viewer"`) {
		t.Errorf("body = %q", gotBody)
	}
	var resp map[string]string
	json.Unmarshal(out, &resp)
	if resp["plaintext"] != "svk-1.secret" {
		t.Errorf("plaintext not parsed: %v", resp)
	}
}

func TestFlagValue(t *testing.T) {
	args := []string{"--role", "operator", "--label", "ci"}
	if flagValue(args, "--role", "x") != "operator" {
		t.Error("role")
	}
	if flagValue(args, "--label", "x") != "ci" {
		t.Error("label")
	}
	if flagValue(args, "--missing", "def") != "def" {
		t.Error("default")
	}
}
