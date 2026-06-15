package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sausheong/runtime/controlplane"
)

func TestHTTPAgentClient_ListSessionsAndEvents(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/sessions":
			_, _ = w.Write([]byte(`[{"id":"s1","status":"running","turn_count":2}]`))
		case "/sessions/s1/events":
			if r.URL.Query().Get("limit") != "5" {
				t.Errorf("limit not forwarded: %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"seq":1,"type":"text","text":"hi"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &httpAgentClient{}
	ap := controlplane.AgentProcess{Addr: srv.Listener.Addr().String(), AuthToken: "tok"}

	sess, err := c.ListSessions(context.Background(), ap)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sess) != 1 || sess[0].ID != "s1" || sess[0].Status != "running" {
		t.Fatalf("sessions wrong: %+v", sess)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("bearer not attached: %q", gotAuth)
	}

	evs, err := c.ListEvents(context.Background(), ap, "s1", 5)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != "text" || evs[0].Text != "hi" {
		t.Fatalf("events wrong: %+v", evs)
	}
}

func TestHTTPAgentClient_Non200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &httpAgentClient{}
	ap := controlplane.AgentProcess{Addr: srv.Listener.Addr().String()}
	if _, err := c.ListSessions(context.Background(), ap); err == nil {
		t.Fatal("expected error on non-200")
	}
}
