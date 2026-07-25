package agentruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sausheong/runtime/internal/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func newTestManager() *Manager {
	return &Manager{
		agentID:     "a",
		st:          store.NewMemStore(),
		subscribers: map[string][]chan WireEvent{},
	}
}

// TestManager_ReplicaFieldRoundTripsThroughStore guards that the Manager.replica
// field exists and that the value an agentd would stamp survives a store
// round-trip. The full startSession→CreateSession path (which also starts a DBOS
// workflow) is exercised by the Task 9 integration test, not here.
func TestManager_ReplicaFieldRoundTripsThroughStore(t *testing.T) {
	st := store.NewMemStore()
	m := &Manager{agentID: "a", st: st, replica: 3, subscribers: map[string][]chan WireEvent{}}
	id, err := st.CreateSession(context.Background(), "a", m.replica)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r, _ := st.SessionReplica(context.Background(), id)
	if r != 3 {
		t.Fatalf("replica: got %d, want 3", r)
	}
}

func TestHealthzAndMeta(t *testing.T) {
	srv := httptest.NewServer(newTestManager().newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: err=%v status=%v", err, resp.StatusCode)
	}
}

func TestListSessionsEndpoint(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	id1, _ := m.st.CreateSession(ctx, "a", 0)
	_ = m.st.SetSessionStatus(ctx, id1, "completed")
	_ = m.st.SetTurnCount(ctx, id1, 2)
	_, _ = m.st.CreateSession(ctx, "a", 0)

	srv := httptest.NewServer(m.newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), id1) || !strings.Contains(string(body), `"status":"completed"`) || !strings.Contains(string(body), `"turn_count":2`) {
		t.Fatalf("/sessions body = %q", body)
	}
}

func TestSessionListIncludesUsage(t *testing.T) {
	st := store.NewMemStore()
	id, _ := st.CreateSession(context.Background(), "a1", 0)
	_ = st.SetSessionUsage(context.Background(), id, 1234, 0.99)
	m := &Manager{agentID: "a1", st: st, subscribers: map[string][]chan WireEvent{}}

	req := httptest.NewRequest("GET", "/sessions", nil)
	rec := httptest.NewRecorder()
	m.handler().ServeHTTP(rec, req)

	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0]["tokens_total"].(float64) != 1234 || out[0]["cost_usd"].(float64) != 0.99 {
		t.Fatalf("session list missing usage: %v", out)
	}
}

func TestCreateSessionRejectsOversizedBody(t *testing.T) {
	m := newTestManager()
	srv := httptest.NewServer(m.newMux())
	defer srv.Close()

	body := strings.NewReader(`{"message":"` + strings.Repeat("x", int(maxSessionBodyBytes)) + `"}`)
	resp, err := http.Post(srv.URL+"/sessions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

func TestCreateSessionRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	m := newTestManager()
	for _, body := range []string{
		`{"message":"hi","unexpected":true}`,
		`{"message":"hi"} {"message":"again"}`,
	} {
		req := httptest.NewRequest("POST", "/sessions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		m.newMux().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%q status=%d want 400", body, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "unexpected") {
			t.Fatalf("decoder detail leaked: %q", rec.Body.String())
		}
	}
}

type failingListStore struct {
	store.Store
}

func (f failingListStore) ListSessionsForTenant(context.Context, string, string) ([]store.SessionRow, error) {
	return nil, errors.New("database host secret.internal.example unavailable")
}

func TestInternalStoreErrorsAreNotReturnedToClients(t *testing.T) {
	m := newTestManager()
	m.st = failingListStore{Store: m.st}
	rec := httptest.NewRecorder()
	m.newMux().ServeHTTP(rec, httptest.NewRequest("GET", "/sessions", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret.internal") {
		t.Fatalf("internal error leaked: %q", rec.Body.String())
	}
}

func TestRequestConcurrencyLimitRejectsExcess(t *testing.T) {
	m := newTestManager()
	m.requestSem = make(chan struct{}, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	h := m.limitRequests(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		close(done)
	}()
	<-started
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rec.Code)
	}
	close(release)
	<-done
}

func TestStreamConcurrencyLimitRejectsExcess(t *testing.T) {
	m := newTestManager()
	m.streamSem = make(chan struct{}, 1)
	release, ok := m.acquireStream()
	if !ok {
		t.Fatal("first stream rejected")
	}
	if _, ok := m.acquireStream(); ok {
		t.Fatal("second stream accepted")
	}
	release()
	if release2, ok := m.acquireStream(); !ok {
		t.Fatal("stream slot not released")
	} else {
		release2()
	}
}

func TestSlowSessionBodyTimesOut(t *testing.T) {
	m := newTestManager()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: m.handler(), ReadTimeout: 40 * time.Millisecond}
	go srv.Serve(ln)
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "POST /sessions HTTP/1.1\r\nHost: test\r\nContent-Length: 20\r\n\r\n{")
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("read timeout response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestSSEOutlivesRequestReadTimeout(t *testing.T) {
	m := newTestManager()
	id, _ := m.st.CreateSession(context.Background(), "a", 0)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: m.handler(), ReadTimeout: 40 * time.Millisecond}
	go srv.Serve(ln)
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	resp, err := http.Get("http://" + ln.Addr().String() + "/sessions/" + id + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	time.Sleep(80 * time.Millisecond)
	if err := m.publish(id, "late", WireEvent{Type: "done"}); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"type":"done"`) {
		t.Fatalf("late SSE event missing: %q", body)
	}
}

func TestStreamReplaysBufferedTerminal(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	id, _ := m.st.CreateSession(ctx, "a", 0)
	_, _ = m.st.AppendEvent(ctx, id, "text", []byte(`{"type":"text","text":"a"}`))
	_, _ = m.st.AppendEvent(ctx, id, "done", []byte(`{"type":"done"}`))

	srv := httptest.NewServer(m.newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/sessions/" + id + "/stream")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body) // returns because replay hit terminal "done"
	s := string(body)
	if !strings.Contains(s, `"text":"a"`) || !strings.Contains(s, `"type":"done"`) {
		t.Fatalf("stream body missing replayed events: %q", s)
	}
	// Replayed events must carry an SSE id: line (the store-assigned seq) so
	// clients get Last-Event-ID dedupe/resume semantics (I1 wire-level seq).
	if !strings.Contains(s, "id: ") {
		t.Fatalf("stream body missing id: line on replay: %q", s)
	}
}

func TestRequireBearer(t *testing.T) {
	const token = "agent-tok"
	mkSrv := func(tok string) *httptest.Server {
		m := newTestManager()
		m.authToken = tok
		return httptest.NewServer(m.handler())
	}

	// get issues GET path with the given Authorization header value ("" = none).
	get := func(t *testing.T, srv *httptest.Server, path, auth string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("no token configured: all paths open (back-compat)", func(t *testing.T) {
		srv := mkSrv("")
		defer srv.Close()
		if got := get(t, srv, "/healthz", ""); got != 200 {
			t.Fatalf("healthz open: status=%d, want 200", got)
		}
		if got := get(t, srv, "/meta", ""); got == http.StatusUnauthorized {
			t.Fatalf("meta open: status=%d, want not-401", got)
		}
	})

	// /healthz is EXEMPT from the bearer (K8s probes never send Authorization;
	// the handler returns a static no-data "ok"). Everything else stays guarded.
	t.Run("token set: /healthz exempt regardless of Authorization", func(t *testing.T) {
		srv := mkSrv(token)
		defer srv.Close()
		if got := get(t, srv, "/healthz", ""); got != 200 {
			t.Fatalf("healthz no auth: status=%d, want 200", got)
		}
		if got := get(t, srv, "/healthz", "Bearer wrong"); got != 200 {
			t.Fatalf("healthz wrong bearer: status=%d, want 200", got)
		}
		if got := get(t, srv, "/healthz", "Bearer "+token); got != 200 {
			t.Fatalf("healthz correct bearer: status=%d, want 200", got)
		}
	})

	// /metrics exposes per-agent metric values, so it MUST stay guarded — proves
	// we did not over-exempt by opening all probe paths.
	t.Run("token set: /metrics stays guarded", func(t *testing.T) {
		srv := mkSrv(token)
		defer srv.Close()
		if got := get(t, srv, "/metrics", ""); got != http.StatusUnauthorized {
			t.Fatalf("metrics no auth: status=%d, want 401", got)
		}
		if got := get(t, srv, "/metrics", "Bearer wrong"); got != http.StatusUnauthorized {
			t.Fatalf("metrics wrong bearer: status=%d, want 401", got)
		}
	})

	t.Run("token set: normal routes stay guarded", func(t *testing.T) {
		srv := mkSrv(token)
		defer srv.Close()
		if got := get(t, srv, "/meta", ""); got != http.StatusUnauthorized {
			t.Fatalf("meta no auth: status=%d, want 401", got)
		}
		if got := get(t, srv, "/meta", "Bearer wrong"); got != http.StatusUnauthorized {
			t.Fatalf("meta wrong bearer: status=%d, want 401", got)
		}
		if got := get(t, srv, "/meta", "Bearer "+token); got == http.StatusUnauthorized {
			t.Fatalf("meta correct bearer: status=%d, want not-401", got)
		}
	})
}

func TestHandler_ContinuesInboundTrace(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	srv := httptest.NewServer(newTestManager().handler())
	defer srv.Close()

	// Build a parent span context and inject it into the request headers.
	ctx, parent := tp.Tracer("test").Start(context.Background(), "client")
	req, _ := http.NewRequest("GET", srv.URL+"/healthz", nil)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	parentTID := parent.SpanContext().TraceID()
	parent.End()

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: err=%v status=%v", err, resp.StatusCode)
	}
	resp.Body.Close()

	var found bool
	for _, s := range rec.Ended() {
		if s.SpanContext().TraceID() == parentTID && s.SpanKind() == trace.SpanKindServer {
			found = true
		}
	}
	if !found {
		t.Fatal("no agentd server span continued the inbound trace id")
	}
}

func TestEventsEndpointNonBlocking(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	id, _ := m.st.CreateSession(ctx, "a", 0)
	_, _ = m.st.AppendEvent(ctx, id, "text", []byte(`{"type":"text","text":"one"}`))
	_, _ = m.st.AppendEvent(ctx, id, "text", []byte(`{"type":"text","text":"two"}`))
	_, _ = m.st.AppendEvent(ctx, id, "text", []byte(`{"type":"text","text":"three"}`))
	// Note: session is NOT terminal (no done/error). The endpoint must still
	// return promptly — it must not block waiting for live events.

	srv := httptest.NewServer(m.newMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sessions/" + id + "/events?limit=2")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("code=%d want 200", resp.StatusCode)
	}
	var got []struct {
		Seq  int64  `json:"seq"`
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d want 2 (limit honored, last 2)", len(got))
	}
	if got[0].Text != "two" || got[1].Text != "three" {
		t.Fatalf("want last two in seq order [two,three], got %+v", got)
	}
}

func TestEventsEndpointUnknownSessionNotFound(t *testing.T) {
	m := newTestManager()
	srv := httptest.NewServer(m.newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/sessions/nope/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session status=%d, want 404", resp.StatusCode)
	}
}

func TestSessionEndpointsRejectAnotherAgentsKnownSession(t *testing.T) {
	m := newTestManager()
	otherID, err := m.st.CreateSession(context.Background(), "other-agent", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = m.st.AppendEvent(context.Background(), otherID, "done", []byte(`{"type":"done"}`))
	srv := httptest.NewServer(m.newMux())
	defer srv.Close()

	for _, suffix := range []string{"", "/events", "/stream"} {
		resp, err := http.Get(srv.URL + "/sessions/" + otherID + suffix)
		if err != nil {
			t.Fatalf("GET %s: %v", suffix, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status=%d, want 404", suffix, resp.StatusCode)
		}
	}
}
