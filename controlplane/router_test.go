package controlplane

import (
	"context"
	"encoding/json"
	"errors"
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

type failingGetStore struct{ store.Store }

func (f failingGetStore) GetSession(context.Context, string) (store.SessionRow, error) {
	return store.SessionRow{}, errors.New("database unavailable")
}

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

	srv := httptest.NewServer(NewAPI(reg, nil, store.NewMemStore(), false))
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
	owned, _ := st.CreateSessionForIdentity(
		context.Background(), "default", "a", "test-generation-a", 1,
	) // owned by replica 1

	srv := httptest.NewServer(NewAPI(reg, nil, st, false))
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

func TestAPI_RemotePoolPersistsExternalSessionAffinity(t *testing.T) {
	var followupHits [2]atomic.Int32
	mk := func(i int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/sessions" {
				_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "remote-" + strconv.Itoa(i)})
				return
			}
			if strings.HasPrefix(r.URL.Path, "/sessions/") {
				followupHits[i].Add(1)
			}
			w.WriteHeader(http.StatusOK)
		}))
	}
	b0, b1 := mk(0), mk(1)
	defer b0.Close()
	defer b1.Close()

	cfg := &config.Config{Agents: []config.AgentConfig{{
		ID: "remote", Name: "Remote", Model: "m", Tenant: "alpha",
		URL: "http://remote-{i}.example:8080", Replicas: 2,
		RegistrationGeneration: "remote-pool-generation",
	}}}
	reg := NewRegistry(cfg, "", "")
	reg.sets["remote"] = []AgentProcess{
		{AgentID: "remote", Tenant: "alpha", Remote: true, BaseURL: b0.URL, ReplicaIndex: 0,
			RegistrationGeneration: "remote-pool-generation"},
		{AgentID: "remote", Tenant: "alpha", Remote: true, BaseURL: b1.URL, ReplicaIndex: 1,
			RegistrationGeneration: "remote-pool-generation"},
	}
	st := store.NewMemStore()
	srv := httptest.NewServer(NewAPI(reg, nil, st, false))
	defer srv.Close()

	httpPost(t, srv.URL+"/agents/remote/sessions")
	httpPost(t, srv.URL+"/agents/remote/sessions")
	row, err := st.GetSession(context.Background(), "remote-1")
	if err != nil || row.TenantID != "alpha" || row.AgentID != "remote" || row.Replica != 1 {
		t.Fatalf("external affinity binding: row=%+v err=%v", row, err)
	}
	if code := httpGetCode(t, srv.URL+"/agents/remote/sessions/remote-1"); code != http.StatusOK {
		t.Fatalf("follow-up status=%d", code)
	}
	if followupHits[1].Load() != 1 || followupHits[0].Load() != 0 {
		t.Fatalf("follow-up hits=[%d %d], want ordinal 1 only", followupHits[0].Load(), followupHits[1].Load())
	}
}

func TestAPI_CommandPoolAffinitySurvivesControlPlaneRestart(t *testing.T) {
	var followupHits [2]atomic.Int32
	mk := func(i int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/sessions" {
				_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "command-" + strconv.Itoa(i)})
				return
			}
			if strings.HasPrefix(r.URL.Path, "/sessions/") {
				followupHits[i].Add(1)
			}
			w.WriteHeader(http.StatusOK)
		}))
	}
	b0, b1 := mk(0), mk(1)
	defer b0.Close()
	defer b1.Close()

	newRegistry := func() *Registry {
		reg := twoReplicaRegistry(t, "command", b0.URL, b1.URL)
		for i := range reg.sets["command"] {
			reg.sets["command"][i].Command = []string{"python", "serve.py"}
		}
		return reg
	}
	st := store.NewMemStore()
	first := httptest.NewServer(NewAPI(newRegistry(), nil, st, false))
	httpPost(t, first.URL+"/agents/command/sessions")
	httpPost(t, first.URL+"/agents/command/sessions")
	first.Close()

	// A new API and registry model a control-plane restart. Only the durable
	// store object is retained, as PostgreSQL would be in production.
	restarted := httptest.NewServer(NewAPI(newRegistry(), nil, st, false))
	defer restarted.Close()
	if code := httpGetCode(t, restarted.URL+"/agents/command/sessions/command-1"); code != http.StatusOK {
		t.Fatalf("follow-up after restart status=%d", code)
	}
	if followupHits[1].Load() != 1 || followupHits[0].Load() != 0 {
		t.Fatalf("follow-up after restart hits=[%d %d], want ordinal 1 only",
			followupHits[0].Load(), followupHits[1].Load())
	}
	if code := httpGetCode(t, restarted.URL+"/agents/command/sessions/unbound"); code != http.StatusNotFound {
		t.Fatalf("unbound pooled session status=%d, want safe 404", code)
	}
}

func TestAPI_SessionStoreFailureReturnsUnavailable(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("store failure must not proxy")
	}))
	defer backend.Close()
	reg := remoteRegistry(t, "a", backend.URL)
	st := failingGetStore{Store: store.NewMemStore()}
	srv := httptest.NewServer(NewAPI(reg, nil, st, false))
	defer srv.Close()
	if code := httpGetCode(t, srv.URL+"/agents/a/sessions/ses-any"); code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", code)
	}
}

func TestAPI_AgentTenantChangeDoesNotTransferSession(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("old-tenant session must not proxy")
	}))
	defer backend.Close()
	cfg := &config.Config{Agents: []config.AgentConfig{{
		ID: "shared", Name: "Shared", Model: "m", Tenant: "beta",
		ListenAddr: strings.TrimPrefix(backend.URL, "http://"),
	}}}
	reg := NewRegistry(cfg, "", "")
	st := store.NewMemStore()
	old, err := st.CreateSessionForTenant(context.Background(), "alpha", "shared", 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewAPI(reg, nil, st, false))
	defer srv.Close()
	if code := httpGetCode(t, srv.URL+"/agents/shared/sessions/"+old); code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", code)
	}
}

func TestAPI_RejectsSessionOwnedByAnotherAgent(t *testing.T) {
	var hitsA int32
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hitsA, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backendB.Close()

	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m", ListenAddr: strings.TrimPrefix(backendA.URL, "http://"), Tenant: "alpha"},
		{ID: "b", Name: "B", Model: "m", ListenAddr: strings.TrimPrefix(backendB.URL, "http://"), Tenant: "beta"},
	}}
	reg := NewRegistry(cfg, "/bin/agentd", "dsn")
	st := store.NewMemStore()
	sessionB, err := st.CreateSession(context.Background(), "b", 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewAPI(reg, nil, st, false))
	defer srv.Close()

	code := httpGetCode(t, srv.URL+"/agents/a/sessions/"+sessionB+"/events")
	if code != http.StatusNotFound {
		t.Fatalf("cross-agent known session id: got %d, want 404", code)
	}
	if got := atomic.LoadInt32(&hitsA); got != 0 {
		t.Fatalf("cross-agent session request reached authorized agent A backend: hits=%d", got)
	}
}

// twoReplicaRegistry builds a registry for one agent whose two replicas dial the
// given full base URLs (httptest servers), bypassing port derivation.
func twoReplicaRegistry(t *testing.T, id, base0, base1 string) *Registry {
	t.Helper()
	host0 := strings.TrimPrefix(base0, "http://")
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: id, Name: id, Model: "m", ListenAddr: host0, Tenant: "default",
			RegistrationGeneration: "test-generation-" + id},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	r.sets[id] = []AgentProcess{
		{AgentID: id, Addr: strings.TrimPrefix(base0, "http://"), BaseURL: base0, ReplicaIndex: 0, DBOSVMID: id + "#0", Tenant: "default",
			RegistrationGeneration: "test-generation-" + id},
		{AgentID: id, Addr: strings.TrimPrefix(base1, "http://"), BaseURL: base1, ReplicaIndex: 1, DBOSVMID: id + "#1", Tenant: "default",
			RegistrationGeneration: "test-generation-" + id},
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

// An unbound external session has no durable proof of tenant, agent generation,
// or replica ownership. Even a single-replica remote must therefore fail closed
// rather than treating its URL as sufficient authority.
func TestAPI_RemoteAgentSessionWithoutBindingFailsClosed(t *testing.T) {
	var streamHit int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sessions" && r.Method == "POST" {
			_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "ses-remote-123"})
			return
		}
		// Any session-scoped path (the remote owns this session).
		if strings.HasPrefix(r.URL.Path, "/sessions/") {
			atomic.AddInt32(&streamHit, 1)
		}
		w.WriteHeader(200)
	}))
	defer backend.Close()

	reg := remoteRegistry(t, "rem", backend.URL)

	// Control-plane store is EMPTY — it never saw this remote's session, exactly
	// as in a separate-instance deployment.
	srv := httptest.NewServer(NewAPI(reg, nil, store.NewMemStore(), false))
	defer srv.Close()

	code := httpGetCode(t, srv.URL+"/agents/rem/sessions/ses-remote-123/stream?since=0")
	if code != http.StatusNotFound {
		t.Fatalf("remote unbound session: got %d, want fail-closed 404", code)
	}
	if atomic.LoadInt32(&streamHit) != 0 {
		t.Fatalf("remote unbound session reached the backend (hits=%d)", streamHit)
	}
}

// Command agents with private stores have the same requirement: their POST
// response must be durably bound by the control plane before follow-up routing.
func TestAPI_CommandAgentSessionWithoutBindingFailsClosed(t *testing.T) {
	var streamHit int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sessions" && r.Method == "POST" {
			_ = json.NewEncoder(w).Encode(map[string]string{"session_id": "ses-shim-123"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/sessions/") {
			atomic.AddInt32(&streamHit, 1)
		}
		w.WriteHeader(200)
	}))
	defer backend.Close()

	reg := commandRegistry(t, "shim", backend.URL)

	// Control-plane store is EMPTY — the shim wrote the session to its own SQLite.
	srv := httptest.NewServer(NewAPI(reg, nil, store.NewMemStore(), false))
	defer srv.Close()

	code := httpGetCode(t, srv.URL+"/agents/shim/sessions/ses-shim-123/stream?since=0")
	if code != http.StatusNotFound {
		t.Fatalf("command-agent unbound session: got %d, want fail-closed 404", code)
	}
	if atomic.LoadInt32(&streamHit) != 0 {
		t.Fatalf("command-agent unbound session reached the backend (hits=%d)", streamHit)
	}
}

// commandRegistry builds a registry for one LOCAL command-spawned agent (a
// foreign-process shim) whose dial base is the given full URL. It is not remote
// (no url:), but it owns its own session store, signalled by a non-empty Command.
func commandRegistry(t *testing.T, id, base string) *Registry {
	t.Helper()
	host := strings.TrimPrefix(base, "http://")
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: id, Name: id, Model: "m", ListenAddr: host, Tenant: "default",
			Command:                []string{"uv", "run", "python", "serve.py"},
			RegistrationGeneration: "test-generation-" + id},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	r.sets[id] = []AgentProcess{
		{AgentID: id, Addr: host, BaseURL: base, ReplicaIndex: 0,
			Command: []string{"uv", "run", "python", "serve.py"}, Tenant: "default",
			RegistrationGeneration: "test-generation-" + id},
	}
	return r
}

// remoteRegistry builds a registry for one REMOTE agent (single attach target)
// whose dial base is the given full URL (an httptest server).
func remoteRegistry(t *testing.T, id, base string) *Registry {
	t.Helper()
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: id, Name: id, Model: "m", URL: base, Tenant: "default",
			RegistrationGeneration: "test-generation-" + id},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	r.sets[id] = []AgentProcess{
		{AgentID: id, BaseURL: base, Remote: true, ReplicaIndex: 0, Tenant: "default",
			RegistrationGeneration: "test-generation-" + id},
	}
	return r
}
