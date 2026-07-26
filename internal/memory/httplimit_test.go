package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// oversizeServer writes one byte more than the memory-client response ceiling.
func oversizeServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxMemoryResponseBytes+1)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbedRejectsOversizedResponse(t *testing.T) {
	srv := oversizeServer(t)
	e := &httpEmbedder{baseURL: srv.URL, model: "m", dim: 3, client: &http.Client{Timeout: 10 * time.Second}}
	if _, err := e.Embed(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized embed response error=%v", err)
	}
}

func TestExtractRejectsOversizedResponse(t *testing.T) {
	srv := oversizeServer(t)
	e := &httpExtractor{baseURL: srv.URL, model: "m", maxFacts: 10, client: &http.Client{Timeout: 10 * time.Second}}
	if _, err := e.Extract(context.Background(), twoMsgThread()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized extract response error=%v", err)
	}
}

func TestSummarizeRejectsOversizedResponse(t *testing.T) {
	srv := oversizeServer(t)
	s := &httpSummarizer{baseURL: srv.URL, model: "m", client: &http.Client{Timeout: 10 * time.Second}}
	if _, err := s.Summarize(context.Background(), twoMsgThread()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized summarize response error=%v", err)
	}
}

func TestEpisodeExtractRejectsOversizedResponse(t *testing.T) {
	srv := oversizeServer(t)
	e := &httpEpisodeExtractor{baseURL: srv.URL, model: "m", maxEpisodes: 5, client: &http.Client{Timeout: 10 * time.Second}}
	if _, err := e.Extract(context.Background(), twoMsgThread()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized episode response error=%v", err)
	}
}
