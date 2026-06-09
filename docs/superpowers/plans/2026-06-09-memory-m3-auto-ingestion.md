# Memory M3 — Auto-ingestion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement harness's `KnowledgeGraph.Ingest` (currently a no-op) so that after each chat turn a background extractor pulls durable facts from the conversation, dedups them against existing memory, and saves the new ones — making them recallable via M2.

**Architecture:** A new `internal/memory/ingest.go` adds an `Extractor` seam (LLM call to the proxy's `/chat/completions`). `internal/memory/kg.go`'s `Ingest` becomes a background orchestrator (growth-gate → goroutine → extract → semantic dedup → save), wired through `internal/agentkind/registry.go` behind a `RUNTIME_INGEST_ENABLED` opt-in layered on M2 semantic recall. Harness is unmodified; no DDL.

**Tech Stack:** Go 1.25.1, `github.com/sausheong/harness` (via `replace ../harness`), pgx/v5, pgvector (reused from M2), Postgres.

**Spec:** `docs/superpowers/specs/2026-06-09-memory-m3-auto-ingestion-design.md`

---

## Critical conventions (read before starting)

- **`go` CLI is ground truth.** The IDE/LSP is broken by the `replace ../harness` cross-module setup — ignore its diagnostics (false "undefined", "unused import", "no packages"). Trust `go build ./...` / `go test ./...` exit codes only.
- **Build/vet/hermetic tests:**
  ```bash
  go build ./...
  go vet ./...
  go test ./internal/memory/ ./internal/agentkind/ ./agentruntime/
  ```
- **Integration tests** need local Postgres.app with pgvector at
  `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`, run per-package:
  ```bash
  go test -tags integration ./internal/memory/
  go test -tags integration ./test/
  ```
  The `vector` extension must already exist in the DB (created once by a superuser:
  `CREATE EXTENSION IF NOT EXISTS vector;`). This is an M2 prerequisite — assume it is present.
- **Commits:** use
  ```bash
  git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "<msg>

  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
  ```
- **Harness is read-only here.** Do not modify anything under `../harness`. The
  `KnowledgeGraph` interface (`runtime/types.go`) and `Message` type
  (`hrt "github.com/sausheong/harness/runtime"`, fields `Role`, `Content`) are fixed.
- **Back-compat is load-bearing.** M2 callers `memory.NewKG(st, k, floor)` must keep
  compiling unchanged and behave exactly as M2 (no-op `Ingest`). Achieved with a
  variadic `KGOption` (mirrors M2's `WithEmbedder`).

---

## File structure

| File | Responsibility |
|---|---|
| `internal/memory/ingest.go` (new) | `Extractor` interface, `httpExtractor` (proxy `/chat/completions`), `parseFacts`, `renderThread`, `NewExtractorFromEnv`. |
| `internal/memory/ingest_test.go` (new, hermetic) | `httpExtractor.Extract` shape, malformed-reply degrade, fact cap, code-fence strip, transport error, env parsing. |
| `internal/memory/kg.go` (modify) | `saver` type; `KGOption`/`WithIngest`; `KG` ingest fields; real `Ingest`/`runIngest`/`isDuplicate`; extend `NewKG`; add `newKGWithIngest` test seam. |
| `internal/memory/kg_test.go` (modify, hermetic) | Gate, happy path, dedup skip, degrade (extract/save/embed-fail), over-cap drop — deterministic via the `ingestDone` hook. |
| `internal/memory/store_test.go` (modify, integration) | Dedup-floor separation against real pgvector. |
| `internal/agentkind/registry.go` (modify) | `envBool`; wire `WithIngest` behind `RUNTIME_INGEST_ENABLED`; validate ingest config before the DB check; warn when enabled without embeddings. |
| `internal/agentkind/registry_test.go` (modify, hermetic) | Ingest-without-model fatal; ingest-without-embeddings warns (not ingest-fatal); ingest-off unaffected. |
| `test/memory_ingest_e2e_test.go` (new, integration) | Real construction path: Ingest → save → recall closes; dedup on re-ingest; cross-tenant isolation. |
| `README.md`, `ROADMAP.md`, `docs/images/project-layout.mmd` (modify) | Docs. |

---

## Task 1: Extractor client (`internal/memory/ingest.go`)

**Files:**
- Create: `internal/memory/ingest.go`
- Create: `internal/memory/ingest_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/memory/ingest_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/memory/ -run 'Extractor|NewExtractorFromEnv'`
Expected: FAIL — `undefined: httpExtractor`, `undefined: NewExtractorFromEnv`.

- [ ] **Step 3: Write the implementation**

Create `internal/memory/ingest.go`:

```go
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	hrt "github.com/sausheong/harness/runtime"
)

// Extractor reads a finished conversation thread and returns durable facts worth
// remembering long-term. Implementations are safe for concurrent use. Optional on
// the KG; when absent, Ingest is a no-op (M2 behavior).
type Extractor interface {
	Extract(ctx context.Context, thread []hrt.Message) ([]string, error)
}

// extractSystemPrompt instructs the model to emit durable facts as a JSON array.
const extractSystemPrompt = "Extract durable, user-specific facts worth remembering long-term from this conversation. Return ONLY a JSON array of short factual statements (strings). Return [] if nothing is worth remembering. Exclude ephemeral details, pleasantries, and the assistant's own reasoning."

// httpExtractor calls an OpenAI-compatible POST {baseURL}/chat/completions.
type httpExtractor struct {
	baseURL  string
	apiKey   string
	model    string
	maxFacts int
	client   *http.Client
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// renderThread joins the thread into a single user-message body.
func renderThread(thread []hrt.Message) string {
	var b strings.Builder
	for _, m := range thread {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}
	return b.String()
}

// Extract requests fact extraction and parses a JSON array of strings from the
// model reply. A malformed (non-JSON / non-array) or empty reply yields zero
// facts (nil, nil) rather than an error — extraction degrades, it does not break
// ingest. A transport error or non-200 status is a real error.
func (e *httpExtractor) Extract(ctx context.Context, thread []hrt.Message) ([]string, error) {
	reqBody := chatRequest{
		Model: e.model,
		Messages: []chatMessage{
			{Role: "system", Content: extractSystemPrompt},
			{Role: "user", Content: renderThread(thread)},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(e.baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("memory: extract request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("memory: extract status %d", resp.StatusCode)
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("memory: extract decode: %w", err)
	}
	if len(cr.Choices) == 0 {
		return nil, nil
	}
	facts := parseFacts(cr.Choices[0].Message.Content)
	if e.maxFacts > 0 && len(facts) > e.maxFacts {
		facts = facts[:e.maxFacts]
	}
	return facts, nil
}

// parseFacts extracts a JSON array of strings from a model reply, tolerating a
// surrounding markdown code fence. Any failure (non-JSON, non-array) yields nil.
func parseFacts(content string) []string {
	s := stripCodeFence(strings.TrimSpace(content))
	var facts []string
	if err := json.Unmarshal([]byte(s), &facts); err != nil {
		return nil
	}
	return facts
}

// stripCodeFence removes a leading ``` (optionally ```json) line and a trailing
// ``` fence if present; otherwise returns s unchanged.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

// NewExtractorFromEnv builds an Extractor from operator env:
//
//	RUNTIME_INGEST_MODEL      extraction chat model (unset ⇒ disabled)
//	RUNTIME_INGEST_MAX_FACTS  hard cap on facts per turn (default 10)
//	OPENAI_BASE_URL           proxy base (reused)
//	OPENAI_API_KEY            proxy bearer (reused)
//
// Returns enabled=false when the model is unset. Construction itself cannot fail
// (there is nothing to validate beyond the model presence the caller checks via
// enabled), so there is no error return; the "enabled but no model" fatal lives
// in the caller (agentkind.wireMemory).
func NewExtractorFromEnv() (ext Extractor, enabled bool) {
	model := os.Getenv("RUNTIME_INGEST_MODEL")
	if model == "" {
		return nil, false
	}
	maxFacts := 10
	if v := os.Getenv("RUNTIME_INGEST_MAX_FACTS"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			maxFacts = n
		} else {
			slog.Warn("memory: ignoring malformed RUNTIME_INGEST_MAX_FACTS; using default", "value", v, "default", maxFacts)
		}
	}
	e := &httpExtractor{
		baseURL:  os.Getenv("OPENAI_BASE_URL"),
		apiKey:   os.Getenv("OPENAI_API_KEY"),
		model:    model,
		maxFacts: maxFacts,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
	return e, true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run 'Extractor|NewExtractorFromEnv'`
Expected: PASS.

- [ ] **Step 5: Build + vet**

Run: `go build ./... && go vet ./internal/memory/`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
git add internal/memory/ingest.go internal/memory/ingest_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(memory): LLM fact extractor for auto-ingestion

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: KG ingest orchestration (`internal/memory/kg.go`)

**Files:**
- Modify: `internal/memory/kg.go`
- Modify: `internal/memory/kg_test.go`

This task makes `KG.Ingest` real: growth-gate → bounded background goroutine →
extract → semantic dedup → save. Back-compat is preserved with a variadic
`KGOption` (M2 `NewKG(st, k, floor)` callers are unaffected and get a no-op
`Ingest`). Tests are deterministic via an injected `ingestDone` hook.

- [ ] **Step 1: Write the failing tests**

Append to `internal/memory/kg_test.go` (keep existing tests). Add `sync` and
`hrt` imports to the file's import block:

```go
// (add to imports at top of kg_test.go)
//   "sync"
//   hrt "github.com/sausheong/harness/runtime"

// fakeExtractor returns preset facts (or an error) regardless of input.
type fakeExtractor struct {
	facts []string
	err   error
}

func (f *fakeExtractor) Extract(_ context.Context, _ []hrt.Message) ([]string, error) {
	return f.facts, f.err
}

// recordingSaver records saved entries; optionally fails on the first call.
type recordingSaver struct {
	mu        sync.Mutex
	saved     []hmem.Entry
	failFirst bool
	calls     int
}

func (r *recordingSaver) save(_ context.Context, e hmem.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.failFirst && r.calls == 1 {
		return fmt.Errorf("save boom")
	}
	r.saved = append(r.saved, e)
	return nil
}

func (r *recordingSaver) snapshot() []hmem.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hmem.Entry(nil), r.saved...)
}

func twoMsgThread() []hrt.Message {
	return []hrt.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}}
}

func TestKG_IngestGateSkipsShortThread(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"should not run"}}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	emb := &kgFakeEmbedder{}
	search := func(_ context.Context, _ []float32, _ int, _ float64) ([]hmem.Entry, error) { return nil, nil }
	k := newKGWithIngest(emb, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), []hrt.Message{{Role: "user", Content: "hi"}}) // len 1 < minMsgs 2
	select {
	case <-done:
		t.Fatal("ingest must not run for a sub-threshold thread")
	default:
	}
	if len(saver.snapshot()) != 0 {
		t.Fatal("nothing should be saved")
	}
}

func TestKG_IngestSavesNewFacts(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"alpha fact", "beta fact"}}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	emb := &kgFakeEmbedder{}
	// search returns no hits → nothing is a duplicate.
	search := func(_ context.Context, _ []float32, _ int, _ float64) ([]hmem.Entry, error) { return nil, nil }
	k := newKGWithIngest(emb, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), twoMsgThread())
	<-done
	got := saver.snapshot()
	if len(got) != 2 || got[0].Content != "alpha fact" || got[1].Content != "beta fact" {
		t.Fatalf("want both facts saved in order: %+v", got)
	}
	if got[0].Origin != "ingest" || len(got[0].Tags) != 1 || got[0].Tags[0] != "auto" {
		t.Fatalf("saved entry must carry ingest origin + auto tag: %+v", got[0])
	}
}

func TestKG_IngestSkipsDuplicates(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"dup fact", "new fact"}}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	emb := &kgFakeEmbedder{}
	// "dup fact" → a hit (duplicate); "new fact" → no hit.
	search := func(_ context.Context, _ []float32, _ int, _ float64) ([]hmem.Entry, error) {
		return nil, nil
	}
	// Override per-fact via embedder vectors + a search that keys on the vector.
	emb.vecs = map[string][]float32{"dup fact": {1, 0, 0}, "new fact": {0, 1, 0}}
	search = func(_ context.Context, vec []float32, _ int, _ float64) ([]hmem.Entry, error) {
		if vec[0] == 1 { // dup fact's vector
			return []hmem.Entry{{Content: "already stored"}}, nil
		}
		return nil, nil
	}
	k := newKGWithIngest(emb, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), twoMsgThread())
	<-done
	got := saver.snapshot()
	if len(got) != 1 || got[0].Content != "new fact" {
		t.Fatalf("duplicate must be skipped, only new fact saved: %+v", got)
	}
}

func TestKG_IngestExtractErrorDegrades(t *testing.T) {
	ext := &fakeExtractor{err: fmt.Errorf("extract boom")}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	search := func(_ context.Context, _ []float32, _ int, _ float64) ([]hmem.Entry, error) { return nil, nil }
	k := newKGWithIngest(&kgFakeEmbedder{}, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), twoMsgThread())
	<-done
	if len(saver.snapshot()) != 0 {
		t.Fatal("extractor error ⇒ nothing saved")
	}
}

func TestKG_IngestSaveErrorContinues(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"first", "second"}}
	saver := &recordingSaver{failFirst: true}
	done := make(chan struct{}, 1)
	search := func(_ context.Context, _ []float32, _ int, _ float64) ([]hmem.Entry, error) { return nil, nil }
	k := newKGWithIngest(&kgFakeEmbedder{}, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), twoMsgThread())
	<-done
	got := saver.snapshot()
	if len(got) != 1 || got[0].Content != "second" {
		t.Fatalf("save error on first ⇒ second still saved: %+v", got)
	}
}

func TestKG_IngestEmbedFailSavesAnyway(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"fact"}}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	emb := &kgFakeEmbedder{fail: true} // dedup embed fails → cannot dedup → save anyway
	searchCalled := false
	search := func(_ context.Context, _ []float32, _ int, _ float64) ([]hmem.Entry, error) {
		searchCalled = true
		return nil, nil
	}
	k := newKGWithIngest(emb, ext, search, saver.save, 0.85, 2, 4, func() { done <- struct{}{} })

	k.Ingest(context.Background(), twoMsgThread())
	<-done
	if searchCalled {
		t.Fatal("search must be skipped when dedup embed fails")
	}
	if got := saver.snapshot(); len(got) != 1 || got[0].Content != "fact" {
		t.Fatalf("embed-fail dedup ⇒ save anyway: %+v", got)
	}
}

func TestKG_IngestDropsOverCapacity(t *testing.T) {
	ext := &fakeExtractor{facts: []string{"x"}}
	saver := &recordingSaver{}
	done := make(chan struct{}, 1)
	search := func(_ context.Context, _ []float32, _ int, _ float64) ([]hmem.Entry, error) { return nil, nil }
	// maxInflight 1; pre-fill the slot so the next Ingest is dropped.
	k := newKGWithIngest(&kgFakeEmbedder{}, ext, search, saver.save, 0.85, 2, 1, func() { done <- struct{}{} })
	k.sem <- struct{}{} // occupy the only slot

	k.Ingest(context.Background(), twoMsgThread()) // must drop, not block
	<-done                                          // drop path still fires ingestDone
	if len(saver.snapshot()) != 0 {
		t.Fatal("over-capacity ingest must drop (no extract/save)")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/memory/ -run 'TestKG_Ingest'`
Expected: FAIL — `undefined: newKGWithIngest`, `k.sem undefined`.

- [ ] **Step 3: Modify `internal/memory/kg.go`**

Replace the file with this version (extends the M2 KG; existing `searcher`,
`ShouldRecall`, `Recall`, `NewKG`, `newKGWithSearch` semantics preserved):

```go
package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	hrt "github.com/sausheong/harness/runtime"
	hmem "github.com/sausheong/harness/tool/memory"
)

// searcher is the slice of *Store the KG needs (declared as a func type so the
// KG is unit-testable without Postgres).
type searcher func(ctx context.Context, queryVec []float32, k int, floor float64) ([]hmem.Entry, error)

// saver is the slice of *Store the ingest path needs (func type for testability).
type saver func(ctx context.Context, e hmem.Entry) error

// ingestOrigin / ingestTags mark auto-captured memories so they are
// distinguishable from tool-saved ones (List/audits, a future GC pass).
const ingestOrigin = "ingest"

var ingestTags = []string{"auto"}

// KG implements harness's runtime.KnowledgeGraph over the tenant-pinned Store.
// Recall embeds the query, finds the nearest live memories, and formats them for
// the prompt. Ingest (optional, enabled via WithIngest) extracts durable facts
// from a finished turn and saves the new ones in a background goroutine.
type KG struct {
	embedder Embedder
	search   searcher
	k        int
	floor    float64

	// Ingest path. Nil extractor ⇒ Ingest is a no-op (M2 behavior).
	extractor  Extractor
	save       saver
	dedupFloor float64
	minMsgs    int
	sem        chan struct{}
	ingestDone func() // test hook; nil in production
}

var _ hrt.KnowledgeGraph = (*KG)(nil)

// KGOption configures the optional ingest path on a KG.
type KGOption func(*KG)

// WithIngest enables auto-ingestion: after each chat turn, extract durable facts,
// dedup them against existing memory (skip when an entry is >= dedupFloor
// similar), and save the new ones. minMsgs is the growth gate (threads shorter
// than this are skipped); maxInflight bounds concurrent extractions (excess turns
// are dropped, not queued).
func WithIngest(ext Extractor, dedupFloor float64, minMsgs, maxInflight int) KGOption {
	return func(g *KG) {
		if maxInflight < 1 {
			maxInflight = 1
		}
		g.extractor = ext
		g.dedupFloor = dedupFloor
		g.minMsgs = minMsgs
		g.sem = make(chan struct{}, maxInflight)
	}
}

// NewKG builds a KnowledgeGraph backed by a tenant-pinned Store. Without any
// KGOption the Ingest path is a no-op (M2 semantic-recall-only behavior).
func NewKG(st *Store, k int, floor float64, opts ...KGOption) *KG {
	g := &KG{
		embedder: st.embedder,
		search:   st.SearchSimilar,
		k:        k,
		floor:    floor,
		save: func(ctx context.Context, e hmem.Entry) error {
			_, err := st.Save(ctx, e)
			return err
		},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// newKGWithSearch is the recall test seam: inject a fake embedder + search.
func newKGWithSearch(emb Embedder, k int, floor float64, s searcher) *KG {
	return &KG{embedder: emb, search: s, k: k, floor: floor}
}

// newKGWithIngest is the ingest test seam: inject fakes for every dependency so
// Ingest is unit-testable without Postgres or a live proxy. done (if non-nil) is
// called when an Ingest goroutine finishes or a turn is dropped.
func newKGWithIngest(emb Embedder, ext Extractor, s searcher, sv saver, dedupFloor float64, minMsgs, maxInflight int, done func()) *KG {
	if maxInflight < 1 {
		maxInflight = 1
	}
	return &KG{
		embedder:   emb,
		search:     s,
		save:       sv,
		extractor:  ext,
		dedupFloor: dedupFloor,
		minMsgs:    minMsgs,
		sem:        make(chan struct{}, maxInflight),
		ingestDone: done,
	}
}

// ShouldRecall is a cheap gate: skip empty/whitespace/very short inputs where
// recall would not help. Called synchronously at Run start.
func (g *KG) ShouldRecall(query string) bool {
	return len(strings.Fields(query)) >= 3
}

// Recall embeds the query and returns a formatted block of the nearest live
// memories, or "" when there is no embedder, nothing relevant, or any error
// (best-effort: recall never breaks a turn).
func (g *KG) Recall(ctx context.Context, query string) string {
	if g.embedder == nil || g.search == nil {
		return ""
	}
	vec, err := g.embedder.Embed(ctx, query)
	if err != nil {
		return ""
	}
	hits, err := g.search(ctx, vec, g.k, g.floor)
	if err != nil || len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Relevant memories:\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s\n", h.Content)
	}
	return b.String()
}

// Ingest extracts durable facts from a finished chat turn and saves the new ones
// in a background goroutine, so the turn never waits. A no-op when the ingest
// path is unconfigured (M2 behavior). Best-effort throughout: every failure
// degrades silently — ingestion never affects a turn. The growth gate and the
// inflight cap bound cost; over capacity, the turn's ingest is dropped.
func (g *KG) Ingest(_ context.Context, thread []hrt.Message) {
	if g.extractor == nil || g.save == nil {
		return
	}
	if len(thread) < g.minMsgs {
		return
	}
	select {
	case g.sem <- struct{}{}:
	default:
		slog.Warn("memory: ingest at capacity, dropping turn")
		if g.ingestDone != nil {
			g.ingestDone()
		}
		return
	}
	go g.runIngest(thread)
}

// runIngest is the background body: extract → per-fact dedup → save. Holds one
// sem slot; releases it (and recovers any panic) on exit.
func (g *KG) runIngest(thread []hrt.Message) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("memory: ingest goroutine panic recovered", "panic", r)
		}
		<-g.sem
		if g.ingestDone != nil {
			g.ingestDone()
		}
	}()
	// Fresh context: the request ctx is typically cancelled by the time the
	// harness fires Ingest in its end-of-Run defer.
	ctx := context.Background()
	facts, err := g.extractor.Extract(ctx, thread)
	if err != nil {
		slog.Warn("memory: ingest extract failed", "err", err)
		return
	}
	for _, f := range facts {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if g.isDuplicate(ctx, f) {
			continue
		}
		if err := g.save(ctx, hmem.Entry{Content: f, Origin: ingestOrigin, Tags: ingestTags}); err != nil {
			slog.Warn("memory: ingest save failed", "err", err)
		}
	}
}

// isDuplicate reports whether a memory at least dedupFloor-similar to fact
// already exists. On any embed/search failure it returns false (save anyway —
// degrade rather than silently drop a fact).
func (g *KG) isDuplicate(ctx context.Context, fact string) bool {
	if g.embedder == nil || g.search == nil {
		return false
	}
	vec, err := g.embedder.Embed(ctx, fact)
	if err != nil {
		return false
	}
	hits, err := g.search(ctx, vec, 1, g.dedupFloor)
	if err != nil {
		return false
	}
	return len(hits) > 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run 'TestKG'`
Expected: PASS (all recall tests + all ingest tests).

- [ ] **Step 5: Build + vet + full hermetic memory package**

Run: `go build ./... && go vet ./internal/memory/ && go test ./internal/memory/`
Expected: exit 0; PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/memory/kg.go internal/memory/kg_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(memory): KG auto-ingestion (gate, async, dedup, save)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Dedup-floor separation against real pgvector (integration)

**Files:**
- Modify: `internal/memory/store_test.go`

Proves the dedup floor (0.85) behaves correctly against real pgvector cosine math:
a near-duplicate (cosine ≥ 0.85) is found by a `k=1, floor=0.85` search (→ would
be skipped); a merely-related fact (cosine between recall 0.7 and dedup 0.85) is
NOT found (→ would be saved). This validates the floor separation the KG relies on.

- [ ] **Step 1: Write the failing test**

Append to `internal/memory/store_test.go` (it already has `fixedEmbedder` and
`freshStoreEmbedded`):

```go
func TestStore_DedupFloorSeparation(t *testing.T) {
	// vectors chosen for known cosine similarities to the query {1,0,0}:
	//   near  = {0.97, 0.243, 0}  → cosine ≈ 0.970 (>= 0.85 dedup floor)
	//   related = {0.8, 0.6, 0}   → cosine = 0.800 (between recall 0.7 and dedup 0.85)
	emb := &fixedEmbedder{vecs: map[string][]float32{
		"near":    {0.97, 0.243, 0},
		"related": {0.8, 0.6, 0},
	}}
	st, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	ctx := context.Background()
	st.Save(ctx, hmem.Entry{Content: "near"})
	st.Save(ctx, hmem.Entry{Content: "related"})

	// At the dedup floor, only "near" is a duplicate of the query.
	dupHits, err := st.SearchSimilar(ctx, []float32{1, 0, 0}, 1, 0.85)
	if err != nil {
		t.Fatal(err)
	}
	if len(dupHits) != 1 || dupHits[0].Content != "near" {
		t.Fatalf("dedup floor 0.85 should match only 'near': %+v", dupHits)
	}

	// "related" sits above the recall floor (0.7) but below dedup (0.85): it would
	// be recalled, but is NOT treated as a duplicate.
	recallHits, _ := st.SearchSimilar(ctx, []float32{1, 0, 0}, 5, 0.7)
	var sawRelated bool
	for _, h := range recallHits {
		if h.Content == "related" {
			sawRelated = true
		}
	}
	if !sawRelated {
		t.Fatalf("'related' should clear the recall floor 0.7: %+v", recallHits)
	}
}
```

- [ ] **Step 2: Run the test to verify it passes**

Run: `go test -tags integration ./internal/memory/ -run TestStore_DedupFloorSeparation`
Expected: PASS. (If Postgres is unreachable the test self-skips — that is not a pass; ensure Postgres.app is running.)

- [ ] **Step 3: Run the full integration package**

Run: `go test -tags integration ./internal/memory/`
Expected: PASS (all M1/M2 store tests + the new one).

- [ ] **Step 4: Commit**

```bash
git add internal/memory/store_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "test(memory): dedup-floor separation against pgvector

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Wire ingestion in `agentkind` (`internal/agentkind/registry.go`)

**Files:**
- Modify: `internal/agentkind/registry.go`
- Modify: `internal/agentkind/registry_test.go`

Adds `envBool`, resolves ingest config **before** the DB check (so a
misconfiguration is fatal regardless of DB state — mirrors M2's embeddings
ordering), warns when ingest is enabled without embeddings, and builds the KG with
`WithIngest` when enabled.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agentkind/registry_test.go`:

```go
func TestWireMemory_IngestEnabledWithoutModelFatal(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "embed-1")
	t.Setenv("RUNTIME_EMBED_DIM", "3")
	t.Setenv("OPENAI_BASE_URL", "https://proxy.invalid")
	t.Setenv("OPENAI_API_KEY", "k")
	t.Setenv("RUNTIME_INGEST_ENABLED", "1")
	t.Setenv("RUNTIME_INGEST_MODEL", "") // enabled but no model
	build, _ := Get("testagent")
	// DB nil is fine: ingest config is validated BEFORE the DB check, so this
	// genuinely exercises the ingest-misconfig-fatal path.
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	if err == nil {
		t.Fatal("ingest enabled + no model must error")
	}
	if !strings.Contains(err.Error(), "ingest config") {
		t.Fatalf("expected ingest-config error, got: %v", err)
	}
}

func TestWireMemory_IngestWithoutEmbeddingsNotFatal(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "") // embeddings off
	t.Setenv("RUNTIME_INGEST_ENABLED", "1")
	t.Setenv("RUNTIME_INGEST_MODEL", "chat-1")
	build, _ := Get("testagent")
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	// Ingest-without-embeddings warns and is ignored; the only error is the nil DB.
	if err == nil {
		t.Fatal("memory enabled + nil DB still errors")
	}
	if strings.Contains(err.Error(), "ingest config") {
		t.Fatalf("ingest-without-embeddings must not be an ingest-config fatal: %v", err)
	}
	if !strings.Contains(err.Error(), "no DB handle") {
		t.Fatalf("expected the nil-DB error, got: %v", err)
	}
}

func TestWireMemory_IngestDisabledUnaffected(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "embed-1")
	t.Setenv("RUNTIME_EMBED_DIM", "3")
	t.Setenv("OPENAI_BASE_URL", "https://proxy.invalid")
	t.Setenv("OPENAI_API_KEY", "k")
	t.Setenv("RUNTIME_INGEST_ENABLED", "") // off
	build, _ := Get("testagent")
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	// With ingest off this is exactly the M2 path: nil DB ⇒ the DB error, no
	// ingest-config error.
	if err == nil || strings.Contains(err.Error(), "ingest config") {
		t.Fatalf("ingest off should not change M2 behavior; got: %v", err)
	}
}

func TestEnvBool(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " On "} {
		t.Setenv("X_FLAG", v)
		if !envBool("X_FLAG") {
			t.Fatalf("%q should be truthy", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "nope"} {
		t.Setenv("X_FLAG", v)
		if envBool("X_FLAG") {
			t.Fatalf("%q should be falsy", v)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agentkind/ -run 'WireMemory_Ingest|TestEnvBool'`
Expected: FAIL — `undefined: envBool`, and the ingest-config error not produced yet.

- [ ] **Step 3: Modify `internal/agentkind/registry.go`**

Add `"strings"` to the import block. Replace the `wireMemory` function with:

```go
// wireMemory attaches the per-tenant memory tool to cfg.Tools when d.Memory is
// set, and — when embeddings are configured (RUNTIME_EMBED_*) — embeds entries on
// save and installs cfg.KGFn for semantic recall. When RUNTIME_INGEST_ENABLED is
// truthy (and recall is on), it also installs auto-ingestion. Fail-fast: an agent
// that asked for memory must not start without it; misconfigured embeddings or a
// requested-but-modelless ingest are fatal.
func wireMemory(cfg *agentruntime.Config, d Deps) error {
	if !d.Memory {
		return nil
	}
	emb, _, enabled, err := memory.NewEmbedderFromEnv()
	if err != nil {
		return fmt.Errorf("agentkind: embeddings config for %q: %w", d.AgentID, err)
	}

	// Resolve auto-ingestion config BEFORE the DB check so a misconfiguration is
	// fatal regardless of DB state (mirrors the embeddings-config ordering).
	var ingestExt memory.Extractor
	if envBool("RUNTIME_INGEST_ENABLED") {
		if !enabled {
			slog.Warn("agentkind: RUNTIME_INGEST_ENABLED set but embeddings are not configured; ingestion disabled", "agent", d.AgentID)
		} else {
			ext, ingEnabled := memory.NewExtractorFromEnv()
			if !ingEnabled {
				return fmt.Errorf("agentkind: ingest config for %q: RUNTIME_INGEST_ENABLED set but RUNTIME_INGEST_MODEL is empty", d.AgentID)
			}
			ingestExt = ext
		}
	}

	if d.DB == nil {
		return fmt.Errorf("agentkind: memory enabled for %q but no DB handle", d.AgentID)
	}
	var opts []memory.Option
	if enabled {
		opts = append(opts, memory.WithEmbedder(emb))
	}
	st, err := memory.NewStore(context.Background(), d.DB, d.Tenant, opts...)
	if err != nil {
		return fmt.Errorf("agentkind: memory store for %q: %w", d.AgentID, err)
	}
	cfg.Tools.Register(&hmemory.MemoryTool{Store: st})
	if enabled {
		k := envInt("RUNTIME_EMBED_RECALL_K", 5)
		floor := envFloat("RUNTIME_EMBED_RECALL_FLOOR", 0.7)
		var kgOpts []memory.KGOption
		if ingestExt != nil {
			dedupFloor := envFloat("RUNTIME_INGEST_DEDUP_FLOOR", 0.85)
			minMsgs := envInt("RUNTIME_INGEST_MIN_MESSAGES", 2)
			maxInflight := envInt("RUNTIME_INGEST_MAX_INFLIGHT", 4)
			kgOpts = append(kgOpts, memory.WithIngest(ingestExt, dedupFloor, minMsgs, maxInflight))
		}
		kg := memory.NewKG(st, k, floor, kgOpts...)
		cfg.KGFn = func(string) hrt.KnowledgeGraph { return kg }
	}
	return nil
}

// envBool reports whether key is set to a truthy value (1/true/yes/on,
// case-insensitive, surrounding spaces ignored).
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agentkind/`
Expected: PASS (existing M2 tests + the new ingest tests).

- [ ] **Step 5: Build + vet**

Run: `go build ./... && go vet ./internal/agentkind/`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
git add internal/agentkind/registry.go internal/agentkind/registry_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(agentkind): wire auto-ingestion behind RUNTIME_INGEST_ENABLED

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: End-to-end ingest → recall (integration)

**Files:**
- Create: `test/memory_ingest_e2e_test.go`

Builds the real construction path the way `agentkind` does (tenant-pinned `Store`
+ embedder + KG with `WithIngest`) using a fake deterministic extractor and the
deterministic embedder pattern from M2 — no live proxy. Proves: an ingested fact
becomes recallable (M3 → M2 loop closes); re-ingesting the same thread saves no
duplicate; a different tenant recalls nothing. Async completion is awaited by
polling the store (no test-only prod API).

- [ ] **Step 1: Write the test**

Create `test/memory_ingest_e2e_test.go`:

```go
//go:build integration

package test

import (
	"context"
	"strings"
	"testing"
	"time"

	"database/sql"

	_ "github.com/jackc/pgx/v5/stdlib"
	hrt "github.com/sausheong/harness/runtime"
	hmem "github.com/sausheong/harness/tool/memory"

	"github.com/sausheong/runtime/internal/memory"
)

// ingestEmbedder maps known content→deterministic vectors (dim 3).
type ingestEmbedder struct{ vecs map[string][]float32 }

func (e ingestEmbedder) Dim() int { return 3 }
func (e ingestEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := e.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}

// fixedExtractor returns preset facts regardless of input.
type fixedExtractor struct{ facts []string }

func (f fixedExtractor) Extract(_ context.Context, _ []hrt.Message) ([]string, error) {
	return f.facts, nil
}

func waitForContent(t *testing.T, st *memory.Store, content string) {
	t.Helper()
	for i := 0; i < 100; i++ { // up to ~2s
		list, err := st.List(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list {
			if e.Content == content {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("content %q never appeared", content)
}

func countContent(t *testing.T, st *memory.Store, content string) int {
	t.Helper()
	list, err := st.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range list {
		if e.Content == content {
			n++
		}
	}
	return n
}

func TestMemoryIngestE2E(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`) })

	const fact = "the user lives in Singapore"
	emb := ingestEmbedder{vecs: map[string][]float32{
		fact:                        {1, 0, 0},
		"where does the user live?": {1, 0, 0}, // query ~ the fact
	}}
	st, err := memory.NewStore(ctx, db, "alpha", memory.WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	ext := fixedExtractor{facts: []string{fact}}
	kg := memory.NewKG(st, 5, 0.5, memory.WithIngest(ext, 0.85, 2, 4))

	thread := []hrt.Message{
		{Role: "user", Content: "I live in Singapore"},
		{Role: "assistant", Content: "Noted!"},
	}

	// 1. Ingest → the fact is saved (async; poll) and carries ingest origin/tag.
	kg.Ingest(ctx, thread)
	waitForContent(t, st, fact)
	auto, _ := st.List(ctx, "auto")
	if len(auto) != 1 || auto[0].Origin != "ingest" {
		t.Fatalf("ingested entry must carry auto tag + ingest origin: %+v", auto)
	}

	// 2. It is now recallable (M3 → M2 loop closes).
	out := kg.Recall(ctx, "where does the user live?")
	if !strings.Contains(out, "Singapore") {
		t.Fatalf("ingested fact should be recallable:\n%s", out)
	}

	// 3. Re-ingesting the same thread saves no duplicate (semantic dedup).
	kg.Ingest(ctx, thread)
	time.Sleep(300 * time.Millisecond) // let the second goroutine run
	if n := countContent(t, st, fact); n != 1 {
		t.Fatalf("dedup failed: want 1 live copy of the fact, got %d", n)
	}

	// 4. Cross-tenant isolation: beta recalls nothing.
	beta, err := memory.NewStore(ctx, db, "beta", memory.WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	bkg := memory.NewKG(beta, 5, 0.5)
	if out := bkg.Recall(ctx, "where does the user live?"); out != "" {
		t.Fatalf("beta must recall nothing: %q", out)
	}
	_ = hmem.Entry{} // keep hmem import if otherwise unused
}
```

> Note: if `go vet` flags the `hmem` import as unused, delete both the import line
> and the final `_ = hmem.Entry{}` line. It is included only in case a future
> assertion needs it; remove if unused to keep vet clean.

- [ ] **Step 2: Run the test**

Run: `go test -tags integration ./test/ -run TestMemoryIngestE2E`
Expected: PASS. (Ensure Postgres.app with the `vector` extension is running; a skip is not a pass.)

- [ ] **Step 3: Run the full test-package integration suite**

Run: `go test -tags integration ./test/`
Expected: PASS (all existing e2e tests + the new one).

- [ ] **Step 4: Commit**

```bash
git add test/memory_ingest_e2e_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "test(memory): e2e ingest→recall, dedup, cross-tenant isolation

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Documentation

**Files:**
- Modify: `README.md`
- Modify: `ROADMAP.md`
- Modify: `docs/images/project-layout.mmd`

- [ ] **Step 1: Locate the memory sections to extend**

Run:
```bash
grep -n "Semantic recall\|RUNTIME_EMBED_\|## .*Memory\|memory" README.md | head -40
grep -n "B2\|Memory\|semantic recall" ROADMAP.md | head -40
grep -n "memory" docs/images/project-layout.mmd
```
Expected: shows the M2 "Semantic recall" subsection + env-var table in README, the §B2 block in ROADMAP, and the `memory/` node in the diagram.

- [ ] **Step 2: README — add an "Auto-ingestion" subsection**

Immediately after the M2 "Semantic recall" subsection, add:

```markdown
#### Auto-ingestion

When semantic recall is enabled **and** `RUNTIME_INGEST_ENABLED` is set, the
agent also *captures* memories automatically. After each chat turn, a background
extractor reads the conversation, pulls out durable facts, dedups them against
existing memory, and saves the new ones — which embed-on-save makes recallable on
the next turn. The agent does not have to call the memory tool to remember.

It is best-effort and never affects a turn: extraction runs in a bounded
background goroutine after the response is delivered; any failure (extraction,
embedding, save) degrades silently. A trivial turn is skipped by a cheap
message-count gate; when too many extractions are already in flight, a turn's
ingest is dropped rather than queued. Auto-captured entries carry origin
`ingest` and the `auto` tag, distinguishing them from tool-saved memories.

Per-turn only (no whole-session synthesis), and append-or-skip (a near-duplicate
is skipped, not merged). Conversation content is sent to the same proxy used for
chat and embeddings — no new egress.
```

- [ ] **Step 3: README — extend the env-var table**

Find the env-var table containing `RUNTIME_EMBED_MODEL` and add these rows after
the `RUNTIME_EMBED_*` rows (match the existing table's column format):

```markdown
| `RUNTIME_INGEST_ENABLED` | enable auto-ingestion (requires semantic recall on) | _unset_ |
| `RUNTIME_INGEST_MODEL` | chat model for fact extraction (reuses `OPENAI_BASE_URL`/`OPENAI_API_KEY`); required when ingestion is enabled | _unset_ |
| `RUNTIME_INGEST_MIN_MESSAGES` | growth gate: minimum thread messages to extract | `2` |
| `RUNTIME_INGEST_MAX_INFLIGHT` | max concurrent extraction goroutines (drop over) | `4` |
| `RUNTIME_INGEST_DEDUP_FLOOR` | cosine floor at/above which a candidate is a duplicate (skip) | `0.85` |
| `RUNTIME_INGEST_MAX_FACTS` | hard cap on facts saved per turn | `10` |
```

- [ ] **Step 4: ROADMAP — mark auto-ingestion done under §B2**

In the §B2 Memory section, after the M2 semantic-recall entry, add a line marking
M3 done and update the remaining-work list. Use the surrounding format; the
content must convey:

```markdown
- [x] **M3 — auto-ingestion**: `KnowledgeGraph.Ingest` implemented — background
  LLM fact extraction per chat turn, semantic dedup, embed-on-save → recallable.
  Opt-in via `RUNTIME_INGEST_ENABLED`, layered on semantic recall.

Remaining B2: compaction/TTL/GC of dead rows, finer (per-agent/per-user) scoping,
per-tenant embedding models, refinement/merge dedup (Update-on-similar),
session-level synthesis.
```

- [ ] **Step 5: project-layout diagram — note ingest in the memory node**

In `docs/images/project-layout.mmd`, find the `memory/` node label (it mentions
embeddings/KG from M2) and append `+ auto-ingestion` (or the diagram's existing
phrasing style) so the node reads e.g. `memory/ (MemoryStore, embeddings, KG recall + auto-ingestion)`. Do not regenerate the PNG (no rendering toolchain assumed); the `.mmd` source is the artifact of record.

- [ ] **Step 6: Verify build still green and commit**

Run: `go build ./...`
Expected: exit 0 (docs-only, but confirm nothing else drifted).

```bash
git add README.md ROADMAP.md docs/images/project-layout.mmd
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "docs(memory): document M3 auto-ingestion

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Final verification (after all tasks)

- [ ] **Build + vet + hermetic tests**

```bash
go build ./...
go vet ./...
go test ./internal/memory/ ./internal/agentkind/ ./agentruntime/
```
Expected: all exit 0 / PASS.

- [ ] **Integration tests (both packages, individually)**

```bash
go test -tags integration ./internal/memory/
go test -tags integration ./test/
```
Expected: PASS (a skip means Postgres.app/pgvector is not running — fix and re-run).

- [ ] **Then:** REQUIRED SUB-SKILL `superpowers:finishing-a-development-branch`.

---

## Self-review notes (plan author)

- **Spec coverage:** Extractor + `NewExtractorFromEnv` (T1) ↔ spec "ingest.go".
  `KG.Ingest` orchestration / gate / async / dedup / drop / degrade (T2) ↔ spec
  "Data flow" + "Error handling". Dedup-floor separation (T3) ↔ spec integration
  testing. `wireMemory` enablement + ordering + warn (T4) ↔ spec "Architecture" +
  "Enablement matrix". e2e ingest→recall + dedup + isolation (T5) ↔ spec "End-to-end".
  Docs (T6) ↔ spec "Documentation updates".
- **Deliberate divergence from the spec sketch:** `NewExtractorFromEnv` returns
  `(Extractor, bool)` — no error — because construction cannot fail (there is no
  dim-equivalent to validate); the "enabled but no model" fatal lives in
  `wireMemory`. This avoids a dead error-return. The spec's 3-return sketch was
  illustrative; behavior (fatal on enabled-without-model) is unchanged.
- **`RUNTIME_INGEST_MAX_FACTS`** is read in `NewExtractorFromEnv` (the cap is
  applied inside `Extract`), not in `wireMemory` — the cap is an extractor concern.
  The other knobs (min-messages, max-inflight, dedup-floor) are KG-orchestration
  concerns, read in `wireMemory` and passed to `WithIngest`.
- **Type consistency:** `Extractor.Extract(ctx, []hrt.Message) ([]string, error)`,
  `saver func(ctx, hmem.Entry) error`, `WithIngest(ext, dedupFloor float64,
  minMsgs, maxInflight int)`, `NewKG(st, k int, floor float64, ...KGOption)`,
  `newKGWithIngest(emb, ext, s, sv, dedupFloor, minMsgs, maxInflight, done)` — used
  identically across T2/T4/T5.
- **Back-compat checked:** the existing `NewKG(st, 5, 0.5)` call in
  `test/memory_recall_e2e_test.go` and `internal/memory/kg_test.go`'s
  `newKGWithSearch` are unchanged and keep compiling (variadic option + preserved
  seam).
```
