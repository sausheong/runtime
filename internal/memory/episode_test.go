package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	hrt "github.com/sausheong/harness/runtime"
)

func TestEpisodeExtractor_ParsesArrayAndCapsMax(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Model reply: a JSON array of 3 events (the extractor caps to maxEpisodes).
		w.Write([]byte(`{"choices":[{"message":{"content":"[\"e1\",\"e2\",\"e3\"]"}}]}`))
	}))
	defer srv.Close()
	e := &httpEpisodeExtractor{baseURL: srv.URL, model: "m", maxEpisodes: 2, client: srv.Client()}
	got, err := e.Extract(context.Background(), []hrt.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "e1" || got[1] != "e2" {
		t.Fatalf("must parse array and cap at maxEpisodes=2, got %v", got)
	}
}

func TestNewEpisodeExtractorFromEnv_ModelFallback(t *testing.T) {
	t.Setenv("RUNTIME_EPISODIC_MODEL", "")
	t.Setenv("RUNTIME_INGEST_MODEL", "")
	if _, ok := NewEpisodeExtractorFromEnv(); ok {
		t.Fatal("must be disabled when no model resolves")
	}
	t.Setenv("RUNTIME_INGEST_MODEL", "gpt-ingest")
	ext, ok := NewEpisodeExtractorFromEnv()
	if !ok || ext == nil {
		t.Fatal("must fall back to RUNTIME_INGEST_MODEL")
	}
	t.Setenv("RUNTIME_EPISODIC_MODEL", "gpt-ep")
	if _, ok := NewEpisodeExtractorFromEnv(); !ok {
		t.Fatal("must enable with RUNTIME_EPISODIC_MODEL")
	}
}
