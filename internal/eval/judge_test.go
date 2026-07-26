package eval

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPJudgeRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxJudgeResponseBytes+1)))
	}))
	defer srv.Close()
	judge := &httpJudge{
		baseURL: srv.URL,
		model:   "judge",
		client:  &http.Client{Timeout: time.Second},
	}
	if _, _, err := judge.Grade(context.Background(), "in", "target", "out"); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized judge response error=%v", err)
	}
}
