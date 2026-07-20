package eval

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeJudge struct {
	pass   bool
	reason string
	err    error
}

func (f fakeJudge) Grade(_ context.Context, _, _, _ string) (bool, string, error) {
	return f.pass, f.reason, f.err
}

func TestScoreDeterministic(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		c    Case
		out  string
		want bool
	}{
		{Case{Scorer: ScorerExact, Expected: "hi"}, "hi", true},
		{Case{Scorer: ScorerExact, Expected: "hi"}, "hix", false},
		{Case{Scorer: ScorerContains, Expected: "ell"}, "hello", true},
		{Case{Scorer: ScorerContains, Expected: "zzz"}, "hello", false},
		{Case{Scorer: ScorerRegex, Expected: "^h.*o$"}, "hello", true},
		{Case{Scorer: ScorerRegex, Expected: "^x"}, "hello", false},
	}
	for i, tc := range cases {
		got, _ := Score(ctx, nil, tc.c, tc.out)
		if got != tc.want {
			t.Errorf("case %d: got %v want %v", i, got, tc.want)
		}
	}
}

func TestScoreJudge(t *testing.T) {
	ctx := context.Background()
	c := Case{Scorer: ScorerJudge, Rubric: "polite"}
	// pass
	if ok, _ := Score(ctx, fakeJudge{pass: true, reason: "ok"}, c, "hi"); !ok {
		t.Error("judge pass not honored")
	}
	// fail
	if ok, _ := Score(ctx, fakeJudge{pass: false, reason: "rude"}, c, "no"); ok {
		t.Error("judge fail not honored")
	}
	// judge error → fail-the-case, detail carries "judge error"
	ok, detail := Score(ctx, fakeJudge{err: errors.New("boom")}, c, "x")
	if ok || detail == "" {
		t.Errorf("judge error should fail case with detail, got ok=%v detail=%q", ok, detail)
	}
	// nil judge on a judge case → fail-the-case with "unavailable"
	ok2, d2 := Score(ctx, nil, c, "x")
	if ok2 || d2 == "" {
		t.Errorf("nil judge should fail judge case, got ok=%v detail=%q", ok2, d2)
	}
}

// newTestJudge points an httpJudge at a test server URL.
func newTestJudge(url string) *httpJudge {
	return &httpJudge{
		baseURL: url,
		model:   "test-model",
		client:  http.DefaultClient,
	}
}

func TestHTTPJudge(t *testing.T) {
	ctx := context.Background()

	t.Run("plain JSON verdict", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"choices":[{"message":{"content":"{\"pass\":true,\"reason\":\"ok\"}"}}]}`))
		}))
		defer srv.Close()
		j := newTestJudge(srv.URL)
		pass, reason, err := j.Grade(ctx, "in", "target", "out")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !pass {
			t.Errorf("pass: got %v want true", pass)
		}
		if reason != "ok" {
			t.Errorf("reason: got %q want %q", reason, "ok")
		}
	})

	t.Run("fenced json verdict", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// content is a ```json-fenced verdict that fails the answer
			w.Write([]byte("{\"choices\":[{\"message\":{\"content\":\"```json\\n{\\\"pass\\\":false,\\\"reason\\\":\\\"nope\\\"}\\n```\"}}]}"))
		}))
		defer srv.Close()
		j := newTestJudge(srv.URL)
		pass, reason, err := j.Grade(ctx, "in", "target", "out")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pass {
			t.Errorf("pass: got %v want false", pass)
		}
		if reason != "nope" {
			t.Errorf("reason: got %q want %q", reason, "nope")
		}
	})

	t.Run("non-200 is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		j := newTestJudge(srv.URL)
		pass, _, err := j.Grade(ctx, "in", "target", "out")
		if err == nil {
			t.Fatal("expected error on 500, got nil")
		}
		if pass {
			t.Errorf("pass: got %v want false on error", pass)
		}
	})

	t.Run("malformed verdict is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// well-formed chat envelope, but content is not JSON verdict
			w.Write([]byte(`{"choices":[{"message":{"content":"not a verdict"}}]}`))
		}))
		defer srv.Close()
		j := newTestJudge(srv.URL)
		pass, _, err := j.Grade(ctx, "in", "target", "out")
		if err == nil {
			t.Fatal("expected error on malformed verdict, got nil")
		}
		if pass {
			t.Errorf("pass: got %v want false on error", pass)
		}
	})
}
