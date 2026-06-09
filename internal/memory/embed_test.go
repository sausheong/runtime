package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPEmbedder_Embed(t *testing.T) {
	var gotPath, gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.1, 0.2, 0.3}}},
		})
	}))
	defer srv.Close()

	e := &httpEmbedder{baseURL: srv.URL, apiKey: "sk-test", model: "embed-1", dim: 3, client: srv.Client()}
	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Fatalf("bad vector: %v", vec)
	}
	if gotPath != "/embeddings" {
		t.Fatalf("path = %q, want /embeddings", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotModel != "embed-1" {
		t.Fatalf("model = %q", gotModel)
	}
}

func TestHTTPEmbedder_DimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.1, 0.2}}}, // len 2
		})
	}))
	defer srv.Close()
	e := &httpEmbedder{baseURL: srv.URL, apiKey: "k", model: "m", dim: 3, client: srv.Client()}
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected dim-mismatch error")
	}
}

func TestHTTPEmbedder_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed → connection refused
	e := &httpEmbedder{baseURL: srv.URL, apiKey: "k", model: "m", dim: 3, client: srv.Client()}
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestNewEmbedderFromEnv(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "")
	_, _, enabled, err := NewEmbedderFromEnv()
	if err != nil || enabled {
		t.Fatalf("model unset ⇒ disabled,no-err; got enabled=%v err=%v", enabled, err)
	}
	t.Setenv("RUNTIME_EMBED_MODEL", "embed-1")
	t.Setenv("RUNTIME_EMBED_DIM", "1536")
	t.Setenv("OPENAI_BASE_URL", "https://proxy.example")
	t.Setenv("OPENAI_API_KEY", "sk-x")
	emb, dim, enabled, err := NewEmbedderFromEnv()
	if err != nil || !enabled || dim != 1536 || emb == nil {
		t.Fatalf("valid config: emb=%v dim=%d enabled=%v err=%v", emb, dim, enabled, err)
	}
	t.Setenv("RUNTIME_EMBED_DIM", "0")
	if _, _, _, err := NewEmbedderFromEnv(); err == nil {
		t.Fatal("dim=0 ⇒ error")
	}
	t.Setenv("RUNTIME_EMBED_DIM", "")
	if _, _, _, err := NewEmbedderFromEnv(); err == nil {
		t.Fatal("dim missing ⇒ error")
	}
}
