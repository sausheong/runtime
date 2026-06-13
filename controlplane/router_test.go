package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/store"
)

func TestRouter_DispatchAndList(t *testing.T) {
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "A:"+r.URL.Path)
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "B:"+r.URL.Path)
	}))
	defer backendB.Close()

	addrOf := func(s string) string { return strings.TrimPrefix(s, "http://") }
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: addrOf(backendA.URL)},
		{ID: "b", Name: "B", Model: "m", ListenAddr: addrOf(backendB.URL)},
	}}
	reg := NewRegistry(cfg, "/bin/agentd", "dsn")

	srv := httptest.NewServer(NewAPI(reg, nil, store.NewMemStore()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/agents/a/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "A:/sessions" {
		t.Fatalf("dispatch a = %q, want A:/sessions", body)
	}

	resp, _ = http.Get(srv.URL + "/agents/b/healthz")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "B:/healthz" {
		t.Fatalf("dispatch b = %q, want B:/healthz", body)
	}

	resp, _ = http.Get(srv.URL + "/agents/zzz/sessions")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown agent status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = http.Get(srv.URL + "/agents")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"id":"a"`) || !strings.Contains(string(body), `"id":"b"`) {
		t.Fatalf("/agents list = %q", body)
	}

	resp, _ = http.Get(srv.URL + "/healthz")
	if resp.StatusCode != 200 {
		t.Fatalf("/healthz status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPI_NewSessionRoundRobinsAndPins(t *testing.T) {
	var hits [2]int32
	mk := func(i int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits[i], 1)
			if r.URL.Path == "/sessions" && r.Method == "POST" {
				_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "ses-from-" + strconv.Itoa(i)})
				return
			}
			w.WriteHeader(200)
		}))
	}
	b0, b1 := mk(0), mk(1)
	defer b0.Close()
	defer b1.Close()

	reg := twoReplicaRegistry(t, "a", b0.URL, b1.URL)

	st := store.NewMemStore()
	owned, _ := st.CreateSession(context.Background(), "a", 1) // owned by replica 1

	srv := httptest.NewServer(NewAPI(reg, nil, st))
	defer srv.Close()

	// Two POSTs round-robin across replicas 0 then 1.
	httpPost(t, srv.URL+"/agents/a/sessions")
	httpPost(t, srv.URL+"/agents/a/sessions")
	if atomic.LoadInt32(&hits[0]) == 0 || atomic.LoadInt32(&hits[1]) == 0 {
		t.Fatalf("round-robin: hits=%v, want both replicas hit", hits)
	}

	// A session-scoped GET for `owned` must hit replica 1 only.
	before := atomic.LoadInt32(&hits[1])
	httpGet(t, srv.URL+"/agents/a/sessions/"+owned)
	if atomic.LoadInt32(&hits[1]) != before+1 {
		t.Fatalf("affinity: owned session did not pin to replica 1")
	}

	// Unknown session ⇒ 404 (no proxy).
	code := httpGetCode(t, srv.URL+"/agents/a/sessions/ses-nope")
	if code != http.StatusNotFound {
		t.Fatalf("unknown session: got %d want 404", code)
	}
}

// twoReplicaRegistry builds a registry for one agent whose two replicas dial the
// given full base URLs (httptest servers), bypassing port derivation.
func twoReplicaRegistry(t *testing.T, id, base0, base1 string) *Registry {
	t.Helper()
	host0 := strings.TrimPrefix(base0, "http://")
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: id, Name: id, Model: "m", ListenAddr: host0, Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	r.sets[id] = []AgentProcess{
		{AgentID: id, Addr: strings.TrimPrefix(base0, "http://"), BaseURL: base0, ReplicaIndex: 0, DBOSVMID: id + "#0", Tenant: "default"},
		{AgentID: id, Addr: strings.TrimPrefix(base1, "http://"), BaseURL: base1, ReplicaIndex: 1, DBOSVMID: id + "#1", Tenant: "default"},
	}
	return r
}

func httpPost(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
}

func httpGet(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
}

func httpGetCode(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
