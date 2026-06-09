package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	hrt "github.com/sausheong/harness/runtime"
)

func TestHTTPExtractor_Extract(t *testing.T) {
	var gotPath, gotAuth, gotModel string
	var gotMsgs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		gotMsgs = len(body.Messages)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `["fact one","fact two"]`}},
			},
		})
	}))
	defer srv.Close()

	e := &httpExtractor{baseURL: srv.URL, apiKey: "sk-test", model: "chat-1", maxFacts: 10, client: srv.Client()}
	facts, err := e.Extract(context.Background(), []hrt.Message{
		{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 || facts[0] != "fact one" || facts[1] != "fact two" {
		t.Fatalf("bad facts: %v", facts)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotModel != "chat-1" {
		t.Fatalf("model = %q", gotModel)
	}
	if gotMsgs != 2 {
		t.Fatalf("messages = %d, want 2 (system+user)", gotMsgs)
	}
}

func TestHTTPExtractor_MalformedRepliesDegradeToZeroFacts(t *testing.T) {
	cases := map[string]string{
		"prose":       "Sure, here are some facts!",
		"json object": `{"fact":"x"}`,
		"empty array": `[]`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{"message": map[string]any{"content": content}}},
				})
			}))
			defer srv.Close()
			e := &httpExtractor{baseURL: srv.URL, apiKey: "k", model: "m", maxFacts: 10, client: srv.Client()}
			facts, err := e.Extract(context.Background(), []hrt.Message{{Role: "user", Content: "x"}})
			if err != nil {
				t.Fatalf("malformed reply must not error: %v", err)
			}
			if len(facts) != 0 {
				t.Fatalf("malformed reply must yield zero facts, got %v", facts)
			}
		})
	}
}

func TestHTTPExtractor_NoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	}))
	defer srv.Close()
	e := &httpExtractor{baseURL: srv.URL, apiKey: "k", model: "m", maxFacts: 10, client: srv.Client()}
	facts, err := e.Extract(context.Background(), []hrt.Message{{Role: "user", Content: "x"}})
	if err != nil || len(facts) != 0 {
		t.Fatalf("no choices ⇒ zero facts, no error; got facts=%v err=%v", facts, err)
	}
}

func TestHTTPExtractor_FactCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": `["a","b","c","d"]`}}},
		})
	}))
	defer srv.Close()
	e := &httpExtractor{baseURL: srv.URL, apiKey: "k", model: "m", maxFacts: 2, client: srv.Client()}
	facts, _ := e.Extract(context.Background(), []hrt.Message{{Role: "user", Content: "x"}})
	if len(facts) != 2 {
		t.Fatalf("fact cap failed: got %d, want 2", len(facts))
	}
}

func TestHTTPExtractor_CodeFenceStripped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "```json\n[\"fenced\"]\n```"}}},
		})
	}))
	defer srv.Close()
	e := &httpExtractor{baseURL: srv.URL, apiKey: "k", model: "m", maxFacts: 10, client: srv.Client()}
	facts, _ := e.Extract(context.Background(), []hrt.Message{{Role: "user", Content: "x"}})
	if len(facts) != 1 || facts[0] != "fenced" {
		t.Fatalf("code fence not stripped: %v", facts)
	}
}

func TestHTTPExtractor_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed → connection refused
	e := &httpExtractor{baseURL: srv.URL, apiKey: "k", model: "m", maxFacts: 10, client: srv.Client()}
	if _, err := e.Extract(context.Background(), []hrt.Message{{Role: "user", Content: "x"}}); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestHTTPExtractor_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	e := &httpExtractor{baseURL: srv.URL, apiKey: "k", model: "m", maxFacts: 10, client: srv.Client()}
	if _, err := e.Extract(context.Background(), []hrt.Message{{Role: "user", Content: "x"}}); err == nil {
		t.Fatal("expected non-200 error")
	}
}

func TestNewExtractorFromEnv(t *testing.T) {
	t.Setenv("RUNTIME_INGEST_MODEL", "")
	if _, enabled := NewExtractorFromEnv(); enabled {
		t.Fatal("model unset ⇒ disabled")
	}
	t.Setenv("RUNTIME_INGEST_MODEL", "chat-1")
	t.Setenv("OPENAI_BASE_URL", "https://proxy.example")
	t.Setenv("OPENAI_API_KEY", "sk-x")
	ext, enabled := NewExtractorFromEnv()
	if !enabled || ext == nil {
		t.Fatalf("valid config: ext=%v enabled=%v", ext, enabled)
	}
}
