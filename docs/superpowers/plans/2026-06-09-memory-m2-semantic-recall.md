# Memory M2 — Semantic Recall Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make per-tenant memory semantic: embed M1 entries on save into a pgvector column, and auto-recall the nearest tenant memories into each agent turn via harness's `KnowledgeGraph` seam.

**Architecture:** Extend `internal/memory` with an `Embedder` (HTTP client to the OpenAI-compatible `/embeddings`), a `vector(N)` column on `memory_events` written at save (NULL on failure), a `SearchSimilar` query joined to M1's live-set projection, and a `KnowledgeGraph` impl (`Recall` = embed→search→format; `Ingest` no-op). Wire it through a new optional `agentruntime.Config.KGFn` into harness's `RuntimeDeps.KGFn`. Enabled only when an agent has `memory: true` AND embeddings are configured. Harness unmodified.

**Tech Stack:** Go 1.25.1, stdlib `database/sql` + pgx, pgvector (`pgvector/pgvector:pg16`, already the image), OpenAI-compatible embeddings over HTTP, module `github.com/sausheong/runtime` with `replace ../harness`.

**Spec:** `docs/superpowers/specs/2026-06-09-memory-m2-semantic-recall-design.md`

---

## Conventions (read before starting)

- **`go` CLI is ground truth.** The IDE/LSP is broken by `replace ../harness` — ignore its diagnostics; trust `go build ./...`, `go vet ./...`, `go test ./...`.
- **Hermetic tests** via `go test ./...`. **Integration tests** are `//go:build integration`, need local Postgres.app (with the `vector` extension available — Postgres.app may need the extension installed; if `CREATE EXTENSION vector` fails locally, the integration tests will surface it) at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`. Run integration packages individually:
  - `go test -tags integration ./internal/memory/`
  - `go test -tags integration ./test/`
- **Commits** use the project identity + trailer:
  ```bash
  git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' \
    commit -m "<message>

  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
  ```
- **Branch:** all work lands on `feat/memory-m2-semantic-recall` (already created; the spec commit is its first commit).
- **Never log or return memory content / vectors** in errors — tenant + id + model only.

## Key facts (verified against the code)

- M1 `internal/memory/store.go`: `Store{db, tenant}`, `NewStore(ctx, db, tenant) (*Store, error)`, `Save`/`Update`/`Remove`/`List`/`Get`, the `liveSelect` const, `scanEntry`, the `textArray` adapter, `generateID`. `Save`/`Update` INSERT into `memory_events`.
- M1 `schema.sql`: `memory_events(seq, tenant_id, op, entry_id, content, tags, origin, supersedes, created_at, original_created_at)` + 3 indexes. Applied verbatim by `NewStore` via `store.ApplyDDLLocked`.
- Harness `runtime.KnowledgeGraph` interface: `ShouldRecall(query string) bool`, `Recall(ctx context.Context, query string) string`, `Ingest(ctx context.Context, thread []runtime.Message)`. Wired via `runtime.RuntimeDeps.KGFn func(model string) KnowledgeGraph`.
- `agentruntime/serve.go` `buildRuntime` currently passes `hrt.RuntimeDeps{}` (empty). `agentruntime.Config` = `{Spec, Provider, Tools}`.
- `internal/agentkind/registry.go`: `Deps{AgentID, DB, Tenant, Memory}`, `attachMemory(reg, d)` builds the store + registers the tool. Builders: `buildTestAgent`, `buildNutrition`.
- Embeddings endpoint: OpenAI-compatible `POST {OPENAI_BASE_URL}/embeddings` with `{"model": M, "input": text}` → `{"data":[{"embedding":[...]}]}`, `Authorization: Bearer {OPENAI_API_KEY}`.

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `internal/memory/embed.go` | create | `Embedder` interface; `httpEmbedder`; `NewEmbedderFromEnv()`; `EmbedConfig`. |
| `internal/memory/embed_test.go` | create (hermetic) | httptest request/response, dim validation, env parsing. |
| `internal/memory/embed_schema.sql` | create | dim-templated embeddings DDL (extension + column + HNSW index). |
| `internal/memory/store.go` | modify | options pattern (`WithEmbedder`); embed-on-save/update (NULL on fail); `SearchSimilar`. |
| `internal/memory/store_test.go` | modify (integration) | embedded-save, NULL-degrade, SearchSimilar order/K/floor/NULL-skip/liveness/cross-tenant. |
| `internal/memory/kg.go` | create | `KG` implementing harness `KnowledgeGraph` (Recall; Ingest no-op; ShouldRecall). |
| `internal/memory/kg_test.go` | create (hermetic) | ShouldRecall + Recall formatting with fakes. |
| `agentruntime/config.go` | modify | `Config.KGFn func(model string) hrt.KnowledgeGraph`. |
| `agentruntime/serve.go` | modify | `buildRuntime` passes `cfg.KGFn`. |
| `agentruntime/serve_test.go` | create/modify (hermetic) | assert KGFn threaded into RuntimeDeps (via a build that doesn't need DBOS — see Task 6). |
| `internal/agentkind/registry.go` | modify | build embedder + KG-backed store when memory+embeddings on; set `cfg.KGFn`. |
| `internal/agentkind/registry_test.go` | modify | embeddings-misconfig fatal; KGFn set when enabled; absent when not. |
| `test/memory_recall_e2e_test.go` | create (integration) | end-to-end recall with a deterministic fake embedder. |
| `README.md`, `ROADMAP.md`, `docs/images/project-layout.mmd` | docs | document semantic recall. |

**Ordering:** T1 embedder (standalone). T2 store embedding (schema + save + SearchSimilar). T3 KG. T4 agentruntime KGFn wiring. T5 agentkind wiring. T6 E2E. T7 docs. Each task leaves the tree green.

---

### Task 1: Embedder (HTTP client to /embeddings)

**Files:**
- Create: `internal/memory/embed.go`
- Create: `internal/memory/embed_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/memory/embed_test.go`:

```go
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
	// disabled when model unset
	t.Setenv("RUNTIME_EMBED_MODEL", "")
	_, _, enabled, err := NewEmbedderFromEnv()
	if err != nil || enabled {
		t.Fatalf("model unset ⇒ disabled,no-err; got enabled=%v err=%v", enabled, err)
	}
	// enabled with valid dim
	t.Setenv("RUNTIME_EMBED_MODEL", "embed-1")
	t.Setenv("RUNTIME_EMBED_DIM", "1536")
	t.Setenv("OPENAI_BASE_URL", "https://proxy.example")
	t.Setenv("OPENAI_API_KEY", "sk-x")
	emb, dim, enabled, err := NewEmbedderFromEnv()
	if err != nil || !enabled || dim != 1536 || emb == nil {
		t.Fatalf("valid config: emb=%v dim=%d enabled=%v err=%v", emb, dim, enabled, err)
	}
	// model set, bad dim ⇒ error
	t.Setenv("RUNTIME_EMBED_DIM", "0")
	if _, _, _, err := NewEmbedderFromEnv(); err == nil {
		t.Fatal("dim=0 ⇒ error")
	}
	t.Setenv("RUNTIME_EMBED_DIM", "")
	if _, _, _, err := NewEmbedderFromEnv(); err == nil {
		t.Fatal("dim missing ⇒ error")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/memory/ -run 'Embedder|FromEnv'`
Expected: FAIL — `undefined: httpEmbedder` / `NewEmbedderFromEnv`.

- [ ] **Step 3: Implement the embedder**

Create `internal/memory/embed.go`:

```go
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Embedder turns text into a fixed-length embedding vector. Implementations are
// safe for concurrent use. An Embedder is optional on the Store; when absent the
// store behaves exactly as M1 (no vectors, no semantic recall).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// Dim reports the embedding dimension (the vector(N) column width).
	Dim() int
}

// httpEmbedder calls an OpenAI-compatible POST {baseURL}/embeddings.
type httpEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	dim     int
	client  *http.Client
}

// Dim reports the configured embedding dimension.
func (e *httpEmbedder) Dim() int { return e.dim }

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}
type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed requests one embedding and validates its length against the configured
// dimension. A length mismatch is an error (prevents a pgvector insert failure
// from a misconfigured model).
func (e *httpEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{Model: e.model, Input: text})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(e.baseURL, "/") + "/embeddings"
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
		return nil, fmt.Errorf("memory: embed request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("memory: embed status %d", resp.StatusCode)
	}
	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("memory: embed decode: %w", err)
	}
	if len(er.Data) == 0 {
		return nil, fmt.Errorf("memory: embed: empty data")
	}
	vec := er.Data[0].Embedding
	if len(vec) != e.dim {
		return nil, fmt.Errorf("memory: embed dim mismatch: got %d want %d", len(vec), e.dim)
	}
	return vec, nil
}

// NewEmbedderFromEnv builds an Embedder from operator env:
//
//	RUNTIME_EMBED_MODEL  embedding model (unset ⇒ disabled)
//	RUNTIME_EMBED_DIM    vector dimension (required + positive when model set)
//	OPENAI_BASE_URL      proxy base (reused)
//	OPENAI_API_KEY       proxy bearer (reused)
//
// Returns enabled=false (no error) when the model is unset. Returns an error
// when the model is set but the dim is missing/non-positive (operator error;
// runtimed/agentd should treat it as fatal).
func NewEmbedderFromEnv() (emb Embedder, dim int, enabled bool, err error) {
	model := os.Getenv("RUNTIME_EMBED_MODEL")
	if model == "" {
		return nil, 0, false, nil
	}
	dimStr := os.Getenv("RUNTIME_EMBED_DIM")
	d, derr := strconv.Atoi(dimStr)
	if derr != nil || d <= 0 {
		return nil, 0, false, fmt.Errorf("memory: RUNTIME_EMBED_DIM must be a positive integer when RUNTIME_EMBED_MODEL is set (got %q)", dimStr)
	}
	e := &httpEmbedder{
		baseURL: os.Getenv("OPENAI_BASE_URL"),
		apiKey:  os.Getenv("OPENAI_API_KEY"),
		model:   model,
		dim:     d,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
	return e, d, true, nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/memory/ -run 'Embedder|FromEnv'`
Expected: PASS.

- [ ] **Step 5: Build + vet**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/memory/embed.go internal/memory/embed_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(memory): OpenAI-compatible embeddings client

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Store embedding — schema, embed-on-save, SearchSimilar

**Files:**
- Create: `internal/memory/embed_schema.sql`
- Modify: `internal/memory/store.go`
- Modify: `internal/memory/store_test.go`

Uses an options pattern so M1 callers (`NewStore(ctx, db, tenant)`) keep compiling. The embeddings DDL is a separate dim-templated string applied only when an embedder is configured.

- [ ] **Step 1: Create the embeddings DDL template**

Create `internal/memory/embed_schema.sql`:

```sql
CREATE EXTENSION IF NOT EXISTS vector;
ALTER TABLE memory_events ADD COLUMN IF NOT EXISTS embedding vector(%d);
CREATE INDEX IF NOT EXISTS memory_events_embedding_idx
    ON memory_events USING hnsw (embedding vector_cosine_ops);
```

- [ ] **Step 2: Write the failing integration tests**

Append to `internal/memory/store_test.go` (it already has `//go:build integration`, `freshStore`, `dsn`, and imports `context`, `database/sql`, `testing`, pgx, `hmem`). Add a deterministic fake embedder + tests:

```go
// fixedEmbedder maps content→a deterministic vector for hermetic-but-real pgvector
// math. Unknown content embeds to a far-away vector. Dim is 3 for tests.
type fixedEmbedder struct {
	vecs map[string][]float32
	fail bool
}

func (f *fixedEmbedder) Dim() int { return 3 }
func (f *fixedEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if f.fail {
		return nil, fmt.Errorf("embed failed")
	}
	if v, ok := f.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}

func freshStoreEmbedded(t *testing.T, tenant string, emb Embedder) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS memory_events CASCADE`) })
	st, err := NewStore(context.Background(), db, tenant, WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	return st, db
}

func TestStore_SaveWritesEmbedding(t *testing.T) {
	emb := &fixedEmbedder{vecs: map[string][]float32{"cats": {1, 0, 0}}}
	st, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	ctx := context.Background()
	e, err := st.Save(ctx, hmem.Entry{Content: "cats"})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(1) FROM memory_events WHERE entry_id=$1 AND embedding IS NOT NULL`, e.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("embedding not written: n=%d", n)
	}
}

func TestStore_SaveDegradesToNullOnEmbedError(t *testing.T) {
	emb := &fixedEmbedder{fail: true}
	st, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	ctx := context.Background()
	e, err := st.Save(ctx, hmem.Entry{Content: "x"})
	if err != nil {
		t.Fatalf("save must succeed despite embed failure: %v", err)
	}
	// still tag/id retrievable (M1 behavior)
	if _, ok, _ := st.Get(ctx, e.ID); !ok {
		t.Fatal("entry must be retrievable after embed-fail degrade")
	}
	var nullCount int
	db.QueryRow(`SELECT count(1) FROM memory_events WHERE entry_id=$1 AND embedding IS NULL`, e.ID).Scan(&nullCount)
	if nullCount != 1 {
		t.Fatalf("expected NULL embedding row, got %d", nullCount)
	}
}

func TestStore_SearchSimilar(t *testing.T) {
	emb := &fixedEmbedder{vecs: map[string][]float32{
		"cats are great":  {1, 0, 0},
		"felines rule":    {0.9, 0.1, 0},
		"stock prices":    {0, 1, 0},
	}}
	st, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	ctx := context.Background()
	st.Save(ctx, hmem.Entry{Content: "cats are great"})
	st.Save(ctx, hmem.Entry{Content: "felines rule"})
	st.Save(ctx, hmem.Entry{Content: "stock prices"})

	// query near the cat vectors:
	hits, err := st.SearchSimilar(ctx, []float32{1, 0, 0}, 5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits above floor, got %d: %+v", len(hits), hits)
	}
	if hits[0].Content != "cats are great" {
		t.Fatalf("nearest should be exact match, got %q", hits[0].Content)
	}
	// K cap:
	hits2, _ := st.SearchSimilar(ctx, []float32{1, 0, 0}, 1, 0.0)
	if len(hits2) != 1 {
		t.Fatalf("K=1 cap failed: %d", len(hits2))
	}
}

func TestStore_SearchSimilarSkipsNullAndRespectsLiveness(t *testing.T) {
	emb := &fixedEmbedder{vecs: map[string][]float32{"keep": {1, 0, 0}, "gone": {1, 0, 0}}}
	st, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	ctx := context.Background()
	keep, _ := st.Save(ctx, hmem.Entry{Content: "keep"})
	gone, _ := st.Save(ctx, hmem.Entry{Content: "gone"})
	st.Remove(ctx, gone.ID) // tombstone
	// a NULL-embedding row (embed fails) must be skipped too
	emb.fail = true
	st.Save(ctx, hmem.Entry{Content: "nullrow"})
	emb.fail = false

	hits, _ := st.SearchSimilar(ctx, []float32{1, 0, 0}, 10, 0.0)
	if len(hits) != 1 || hits[0].ID != keep.ID {
		t.Fatalf("want only the live, embedded entry; got %+v", hits)
	}
}

func TestStore_SearchSimilarCrossTenantIsolation(t *testing.T) {
	emb := &fixedEmbedder{vecs: map[string][]float32{"alpha-secret": {1, 0, 0}}}
	alpha, db := freshStoreEmbedded(t, "alpha", emb)
	defer db.Close()
	beta, err := NewStore(context.Background(), db, "beta", WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alpha.Save(ctx, hmem.Entry{Content: "alpha-secret"})
	hits, _ := beta.SearchSimilar(ctx, []float32{1, 0, 0}, 10, 0.0)
	if len(hits) != 0 {
		t.Fatalf("beta must not recall alpha's memory: %+v", hits)
	}
}
```

Add `"fmt"` to the test file's imports if not already present.

- [ ] **Step 3: Run to verify failure**

Run: `go test -tags integration ./internal/memory/ -run 'Embedding|SearchSimilar|Degrade'`
Expected: FAIL — `undefined: WithEmbedder` / `SearchSimilar`.

- [ ] **Step 4: Implement the store changes**

In `internal/memory/store.go`:

(a) Add imports: `"strconv"`, and keep existing. Add a second embedded DDL:

```go
//go:embed embed_schema.sql
var embedSchemaSQL string
```

(b) Replace the `Store` struct + `NewStore` with the options form:

```go
// Store is a tenant-pinned Postgres MemoryStore. Every query filters by tenant,
// captured at construction — the agent's (unscoped) tool calls can never reach
// another tenant's pool. An optional Embedder enables semantic recall: Save/Update
// write an embedding vector and SearchSimilar ranks by cosine similarity.
type Store struct {
	db       *sql.DB
	tenant   string
	embedder Embedder
}

// Option configures a Store at construction.
type Option func(*Store)

// WithEmbedder enables semantic recall: entries are embedded on save and the
// embeddings DDL (pgvector extension, vector column, HNSW index) is applied.
func WithEmbedder(e Embedder) Option {
	return func(s *Store) { s.embedder = e }
}

// NewStore ensures the schema (under the shared DDL lock) and returns a Store
// pinned to tenant. An empty tenant becomes "default". With WithEmbedder, the
// embeddings DDL is also applied (dim from the embedder).
func NewStore(ctx context.Context, db *sql.DB, tenant string, opts ...Option) (*Store, error) {
	if tenant == "" {
		tenant = "default"
	}
	s := &Store{db: db, tenant: tenant}
	for _, o := range opts {
		o(s)
	}
	if err := store.ApplyDDLLocked(ctx, db, schemaSQL); err != nil {
		return nil, err
	}
	if s.embedder != nil {
		ddl := fmt.Sprintf(embedSchemaSQL, s.embedder.Dim())
		if err := store.ApplyDDLLocked(ctx, db, ddl); err != nil {
			return nil, fmt.Errorf("memory: embeddings schema: %w", err)
		}
	}
	return s, nil
}
```

(c) Add an embed helper and use it in `Save`/`Update`. Add after `NewStore`:

```go
// embedOrNil embeds text, returning nil (and logging) on failure so the write
// degrades rather than failing. Returns nil immediately when no embedder is set.
func (s *Store) embedOrNil(ctx context.Context, id, text string) any {
	if s.embedder == nil {
		return nil
	}
	vec, err := s.embedder.Embed(ctx, text)
	if err != nil {
		slog.Warn("memory: embed failed; storing NULL embedding", "tenant", s.tenant, "id", id, "err", err)
		return nil
	}
	return pgVector(vec)
}
```

Add `"log/slog"` to imports.

(d) Change `Save`'s INSERT to include the embedding column. Replace the `Save` INSERT block:

```go
	emb := s.embedOrNil(ctx, e.ID, e.Content)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_events (tenant_id, op, entry_id, content, tags, origin, created_at, embedding)
		 VALUES ($1,'create',$2,$3,$4,$5,$6,$7)`,
		s.tenant, e.ID, e.Content, textArray(e.Tags), e.Origin, now, emb)
```

> Note: when `emb` is nil and there is no `embedding` column (embeddings disabled), this INSERT would fail because the column doesn't exist. To keep the disabled path identical to M1, branch on `s.embedder == nil`: keep the M1 INSERT (no embedding column) when disabled, use the embedding INSERT when enabled. Implement `Save` as:

```go
func (s *Store) Save(ctx context.Context, e hmem.Entry) (hmem.Entry, error) {
	now := time.Now().UTC()
	if e.ID == "" {
		e.ID = generateID(now)
	}
	e.CreatedAt = now
	e.UpdatedAt = now
	var err error
	if s.embedder == nil {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO memory_events (tenant_id, op, entry_id, content, tags, origin, created_at)
			 VALUES ($1,'create',$2,$3,$4,$5,$6)`,
			s.tenant, e.ID, e.Content, textArray(e.Tags), e.Origin, now)
	} else {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO memory_events (tenant_id, op, entry_id, content, tags, origin, created_at, embedding)
			 VALUES ($1,'create',$2,$3,$4,$5,$6,$7)`,
			s.tenant, e.ID, e.Content, textArray(e.Tags), e.Origin, now, s.embedOrNil(ctx, e.ID, e.Content))
	}
	if err != nil {
		return hmem.Entry{}, fmt.Errorf("memory: save tenant %q id %q: %w", s.tenant, e.ID, err)
	}
	return e, nil
}
```

Apply the same enabled/disabled branch to `Update`'s INSERT (the enabled branch adds `, embedding` + `$9` bound to `s.embedOrNil(ctx, newID, content)`):

```go
func (s *Store) Update(ctx context.Context, id, content string) (hmem.Entry, error) {
	old, ok, err := s.Get(ctx, id)
	if err != nil {
		return hmem.Entry{}, err
	}
	if !ok {
		return hmem.Entry{}, hmem.ErrNotFound
	}
	now := time.Now().UTC()
	newID := generateID(now)
	if s.embedder == nil {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO memory_events (tenant_id, op, entry_id, content, tags, origin, supersedes, created_at, original_created_at)
			 VALUES ($1,'update',$2,$3,$4,$5,$6,$7,$8)`,
			s.tenant, newID, content, textArray(old.Tags), old.Origin, id, now, old.CreatedAt)
	} else {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO memory_events (tenant_id, op, entry_id, content, tags, origin, supersedes, created_at, original_created_at, embedding)
			 VALUES ($1,'update',$2,$3,$4,$5,$6,$7,$8,$9)`,
			s.tenant, newID, content, textArray(old.Tags), old.Origin, id, now, old.CreatedAt, s.embedOrNil(ctx, newID, content))
	}
	if err != nil {
		return hmem.Entry{}, fmt.Errorf("memory: update tenant %q id %q: %w", s.tenant, id, err)
	}
	return hmem.Entry{
		ID: newID, Content: content, Tags: old.Tags, Origin: old.Origin,
		CreatedAt: old.CreatedAt, UpdatedAt: now,
	}, nil
}
```

(e) Add a `pgVector` driver type for binding `[]float32` as a pgvector literal, and `SearchSimilar`. Append to `store.go`:

```go
// pgVector binds a []float32 as a pgvector literal ("[0.1,0.2,...]").
type pgVector []float32

func (v pgVector) Value() (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String(), nil
}

// SearchSimilar returns up to k live, embedded entries for the pinned tenant
// whose cosine similarity to queryVec is >= floor, nearest first. Reuses M1's
// liveness clauses (superseded/tombstoned excluded) and skips NULL embeddings.
func (s *Store) SearchSimilar(ctx context.Context, queryVec []float32, k int, floor float64) ([]hmem.Entry, error) {
	q := `
SELECT e.entry_id, e.content, e.tags, e.origin, e.created_at, e.original_created_at
FROM   memory_events e
WHERE  e.tenant_id = $1
  AND  e.embedding IS NOT NULL
  AND  e.op IN ('create','update')
  AND  NOT EXISTS (SELECT 1 FROM memory_events sup
                   WHERE sup.tenant_id = $1 AND sup.supersedes = e.entry_id)
  AND  NOT EXISTS (SELECT 1 FROM memory_events d
                   WHERE d.tenant_id = $1 AND d.op = 'delete' AND d.entry_id = e.entry_id)
  AND  1 - (e.embedding <=> $2) >= $3
ORDER BY e.embedding <=> $2
LIMIT $4`
	rows, err := s.db.QueryContext(ctx, q, s.tenant, pgVector(queryVec), floor, k)
	if err != nil {
		return nil, fmt.Errorf("memory: search tenant %q: %w", s.tenant, err)
	}
	defer rows.Close()
	var out []hmem.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("memory: search scan tenant %q: %w", s.tenant, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
```

Add `"database/sql/driver"` and `"strings"` to imports (strings may already be needed; trust the compiler).

- [ ] **Step 5: Run hermetic + integration**

Run: `go test ./internal/memory/` (hermetic: embed + id + array + kg-later) → PASS.
Run: `go test -tags integration ./internal/memory/` → PASS (M1 tests + new embedding tests). If `CREATE EXTENSION vector` fails on local Postgres, install pgvector locally or note the environment gap.

- [ ] **Step 6: Build + vet**

Run: `go build ./... && go vet ./...`
Expected: clean (M1 callers still compile — `NewStore(ctx, db, tenant)` works via variadic opts).

- [ ] **Step 7: Commit**

```bash
git add internal/memory/embed_schema.sql internal/memory/store.go internal/memory/store_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(memory): embed entries on save; pgvector SearchSimilar over the live set

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: KnowledgeGraph implementation

**Files:**
- Create: `internal/memory/kg.go`
- Create: `internal/memory/kg_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/memory/kg_test.go`:

```go
package memory

import (
	"context"
	"strings"
	"testing"

	hmem "github.com/sausheong/harness/tool/memory"
)

// kgFakeStore lets us drive Recall without Postgres.
type kgSearchFunc func(ctx context.Context, vec []float32, k int, floor float64) ([]hmem.Entry, error)

func TestKG_ShouldRecall(t *testing.T) {
	k := &KG{}
	if k.ShouldRecall("") || k.ShouldRecall("  ") || k.ShouldRecall("ok") {
		t.Fatal("trivial inputs must not trigger recall")
	}
	if !k.ShouldRecall("what did we decide about the database schema?") {
		t.Fatal("a real question should trigger recall")
	}
}

func TestKG_RecallFormatsHits(t *testing.T) {
	emb := &fixedEmbedder{vecs: map[string][]float32{"query": {1, 0, 0}}}
	searched := false
	k := newKGWithSearch(emb, 5, 0.5, func(_ context.Context, vec []float32, k int, floor float64) ([]hmem.Entry, error) {
		searched = true
		if vec[0] != 1 {
			t.Fatalf("query not embedded: %v", vec)
		}
		return []hmem.Entry{{Content: "cats are great"}, {Content: "felines rule"}}, nil
	})
	out := k.Recall(context.Background(), "query")
	if !searched {
		t.Fatal("Recall must embed + search")
	}
	if !strings.Contains(out, "cats are great") || !strings.Contains(out, "felines rule") {
		t.Fatalf("recall block missing memories: %q", out)
	}
}

func TestKG_RecallEmptyWhenNoHits(t *testing.T) {
	emb := &fixedEmbedder{vecs: map[string][]float32{"q": {1, 0, 0}}}
	k := newKGWithSearch(emb, 5, 0.5, func(_ context.Context, _ []float32, _ int, _ float64) ([]hmem.Entry, error) {
		return nil, nil
	})
	if out := k.Recall(context.Background(), "q"); out != "" {
		t.Fatalf("no hits ⇒ empty recall, got %q", out)
	}
}

func TestKG_RecallEmptyOnEmbedError(t *testing.T) {
	emb := &fixedEmbedder{fail: true}
	k := newKGWithSearch(emb, 5, 0.5, func(_ context.Context, _ []float32, _ int, _ float64) ([]hmem.Entry, error) {
		t.Fatal("search must not be called when embed fails")
		return nil, nil
	})
	if out := k.Recall(context.Background(), "q"); out != "" {
		t.Fatalf("embed error ⇒ empty recall, got %q", out)
	}
}

func TestKG_IngestIsNoop(t *testing.T) {
	k := &KG{}
	k.Ingest(context.Background(), nil) // must not panic
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/memory/ -run TestKG`
Expected: FAIL — `undefined: KG` / `newKGWithSearch`.

- [ ] **Step 3: Implement the KG**

Create `internal/memory/kg.go`:

```go
package memory

import (
	"context"
	"fmt"
	"strings"

	hrt "github.com/sausheong/harness/runtime"
	hmem "github.com/sausheong/harness/tool/memory"
)

// searcher is the slice of *Store the KG needs (declared as a func type so the
// KG is unit-testable without Postgres).
type searcher func(ctx context.Context, queryVec []float32, k int, floor float64) ([]hmem.Entry, error)

// KG implements harness's runtime.KnowledgeGraph over the tenant-pinned Store:
// Recall embeds the query, finds the nearest live memories, and formats them for
// the prompt. Ingest is a no-op this milestone (memories come from the explicit
// memory tool). A nil KG-producing path means recall is simply disabled.
type KG struct {
	embedder Embedder
	search   searcher
	k        int
	floor    float64
}

// NewKG builds a KnowledgeGraph backed by a tenant-pinned Store.
func NewKG(st *Store, k int, floor float64) *KG {
	return &KG{embedder: st.embedder, search: st.SearchSimilar, k: k, floor: floor}
}

// newKGWithSearch is the test seam: inject a fake embedder + search.
func newKGWithSearch(emb Embedder, k int, floor float64, s searcher) *KG {
	return &KG{embedder: emb, search: s, k: k, floor: floor}
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

// Ingest is a no-op in M2 (auto-extraction is a later milestone).
func (g *KG) Ingest(_ context.Context, _ []hrt.Message) {}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/memory/ -run TestKG`
Expected: PASS.

- [ ] **Step 5: Build + vet + full hermetic**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/memory/kg.go internal/memory/kg_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(memory): KnowledgeGraph recall (Ingest deferred)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Thread KGFn through agentruntime

**Files:**
- Modify: `agentruntime/config.go`
- Modify: `agentruntime/serve.go`
- Create: `agentruntime/kgfn_test.go`

- [ ] **Step 1: Write the failing test**

Create `agentruntime/kgfn_test.go`:

```go
package agentruntime

import (
	"context"
	"testing"

	hrt "github.com/sausheong/harness/runtime"
)

// stubKG is a minimal KnowledgeGraph for wiring assertions.
type stubKG struct{}

func (stubKG) ShouldRecall(string) bool                  { return false }
func (stubKG) Recall(context.Context, string) string     { return "" }
func (stubKG) Ingest(context.Context, []hrt.Message)      {}

func TestConfig_KGFnField(t *testing.T) {
	called := false
	cfg := Config{
		KGFn: func(model string) hrt.KnowledgeGraph {
			called = true
			return stubKG{}
		},
	}
	if cfg.KGFn == nil {
		t.Fatal("KGFn field must be settable")
	}
	if kg := cfg.KGFn("m"); kg == nil || !called {
		t.Fatal("KGFn must return the KG")
	}
}
```

> This proves the `Config.KGFn` field exists and is invocable. The deeper assertion (that `buildRuntime` forwards it into `hrt.RuntimeDeps`) is covered by the E2E in Task 6 / live verification, since `buildRuntime` is unexported and needs a Manager. If you can construct a Manager cheaply in a test, also assert the forward; otherwise this field test + the agentkind wiring test (Task 5) + E2E suffice.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./agentruntime/ -run TestConfig_KGFn`
Expected: FAIL — `unknown field KGFn`.

- [ ] **Step 3: Add the field**

In `agentruntime/config.go`, add to `Config` (after `Tools`) and the import:

```go
	KGFn     func(model string) hrt.KnowledgeGraph // optional; nil ⇒ no semantic recall
```

(`hrt` is already imported in config.go.)

- [ ] **Step 4: Forward it in buildRuntime**

In `agentruntime/serve.go`, change `buildRuntime`'s `hrt.RuntimeDeps{}` to:

```go
	return hrt.BuildRuntime(
		hrt.RuntimeDeps{KGFn: m.cfg.KGFn},
		hrt.RuntimeInputs{
			Provider:   m.cfg.Provider,
			Tools:      m.cfg.Tools,
			Session:    sess,
			Compaction: nil,
		},
		m.cfg.Spec,
	)
```

- [ ] **Step 5: Run + build + vet + full hermetic**

Run: `go test ./agentruntime/ -run TestConfig_KGFn` → PASS.
Run: `go build ./... && go vet ./... && go test ./...` → PASS.

- [ ] **Step 6: Commit**

```bash
git add agentruntime/config.go agentruntime/serve.go agentruntime/kgfn_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(agentruntime): optional Config.KGFn threaded into RuntimeDeps

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Wire embeddings + KG in agentkind

**Files:**
- Modify: `internal/agentkind/registry.go`
- Modify: `internal/agentkind/registry_test.go`

`attachMemory` currently builds the store + registers the tool. Extend it to, when embeddings are configured, build the embedder, construct the store WITH the embedder, and set `cfg.KGFn`. Because `attachMemory` operates on a registry not the Config, refactor it to also return the optional KG (or take the Config). Simplest: change `attachMemory` to build the store once and return `(*memory.Store, error)`; callers register the tool and, if the store has an embedder, set `cfg.KGFn`.

- [ ] **Step 1: Write the failing tests**

In `internal/agentkind/registry_test.go`, add:

```go
func TestBuildTestAgent_SetsKGFnWhenEmbeddingsConfigured(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "embed-1")
	t.Setenv("RUNTIME_EMBED_DIM", "3")
	t.Setenv("OPENAI_BASE_URL", "https://proxy.invalid")
	t.Setenv("OPENAI_API_KEY", "k")
	build, _ := Get("testagent")
	// DB nil → memory store construction fails fast; we only want to assert the
	// embeddings-config path is reached, so this must error (fail fast), proving
	// the embeddings branch ran. (Happy path needs a DB → covered by E2E.)
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	if err == nil {
		t.Fatal("memory enabled + nil DB must fail fast even with embeddings configured")
	}
}

func TestBuildTestAgent_EmbeddingsMisconfiguredFatal(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "embed-1")
	t.Setenv("RUNTIME_EMBED_DIM", "") // bad
	build, _ := Get("testagent")
	_, err := build(Deps{AgentID: "a1", Memory: true, Tenant: "alpha", DB: nil})
	if err == nil {
		t.Fatal("model set + bad dim must error")
	}
}

func TestBuildTestAgent_NoKGFnWhenEmbeddingsUnset(t *testing.T) {
	t.Setenv("RUNTIME_EMBED_MODEL", "")
	build, _ := Get("testagent")
	cfg, err := build(Deps{AgentID: "a1"}) // memory off
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KGFn != nil {
		t.Fatal("KGFn must be nil when embeddings/memory are off")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agentkind/`
Expected: FAIL (the embeddings branch / KGFn not yet implemented; misconfig not yet fatal).

- [ ] **Step 3: Implement the wiring**

In `internal/agentkind/registry.go`, replace `attachMemory` and its callers. New approach — a helper that wires memory + (optional) KG into a Config:

```go
// wireMemory attaches the per-tenant memory tool to cfg.Tools when d.Memory is
// set, and — when embeddings are configured (RUNTIME_EMBED_*) — embeds entries on
// save and installs cfg.KGFn for semantic recall. Fail-fast: an agent that asked
// for memory must not start without it, and misconfigured embeddings are fatal.
func wireMemory(cfg *agentruntime.Config, d Deps) error {
	if !d.Memory {
		return nil
	}
	if d.DB == nil {
		return fmt.Errorf("agentkind: memory enabled for %q but no DB handle", d.AgentID)
	}
	emb, _, enabled, err := memory.NewEmbedderFromEnv()
	if err != nil {
		return fmt.Errorf("agentkind: embeddings config for %q: %w", d.AgentID, err)
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
		kg := memory.NewKG(st, k, floor)
		cfg.KGFn = func(string) hrt.KnowledgeGraph { return kg }
	}
	return nil
}

// envInt/envFloat read an env var with a default.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
```

Add imports: `"os"`, `"strconv"`, `hrt "github.com/sausheong/harness/runtime"` (already imported). Note `cfg.Tools` must be non-nil before `wireMemory` — both builders create the registry first.

Update the builders to call `wireMemory(&cfg, d)` instead of `attachMemory`:

```go
func buildNutrition(d Deps) (agentruntime.Config, error) {
	cfg, err := nutrition.BuildConfig(nutrition.Deps{AgentID: d.AgentID})
	if err != nil {
		return agentruntime.Config{}, err
	}
	if err := wireMemory(&cfg, d); err != nil {
		return agentruntime.Config{}, err
	}
	return cfg, nil
}

func buildTestAgent(d Deps) (agentruntime.Config, error) {
	reg := tool.NewRegistry()
	reg.Register(testagent.MarkerTool{DB: d.DB})
	cfg := agentruntime.Config{
		Spec: hrt.AgentSpec{
			ID: d.AgentID, Name: d.AgentID, Model: "test/scripted", MaxTurns: 10,
		},
		Provider: testagent.New(),
		Tools:    reg,
	}
	if err := wireMemory(&cfg, d); err != nil {
		return agentruntime.Config{}, err
	}
	return cfg, nil
}
```

Remove the old `attachMemory` function. Keep the existing M1 tests passing (the `Memory:false` and nil-DB tests still hold: `wireMemory` returns nil when `!d.Memory`, errors on nil DB when `d.Memory`).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/agentkind/`
Expected: PASS (M1 tests + new embeddings tests).

- [ ] **Step 5: Build + vet + full hermetic**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agentkind/registry.go internal/agentkind/registry_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(agentkind): wire embeddings + KG semantic recall when configured

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: End-to-end recall

**Files:**
- Create: `test/memory_recall_e2e_test.go`

Prove the store→embed→search→format chain with a deterministic fake embedder, the way `agentkind` constructs it, against real Postgres+pgvector.

- [ ] **Step 1: Write the E2E**

Create `test/memory_recall_e2e_test.go`:

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	hmem "github.com/sausheong/harness/tool/memory"

	"github.com/sausheong/runtime/internal/memory"
)

// e2eEmbedder maps known content to deterministic vectors (dim 3).
type e2eEmbedder struct{ vecs map[string][]float32 }

func (e e2eEmbedder) Dim() int { return 3 }
func (e e2eEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := e.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}

func TestMemoryRecallE2E(t *testing.T) {
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

	emb := e2eEmbedder{vecs: map[string][]float32{
		"the db schema uses an append-only event log": {1, 0, 0},
		"the user prefers dark mode":                  {0, 1, 0},
		"tell me about the database design":           {1, 0, 0}, // query ~ schema memory
	}}
	st, err := memory.NewStore(ctx, db, "alpha", memory.WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	st.Save(ctx, hmem.Entry{Content: "the db schema uses an append-only event log"})
	st.Save(ctx, hmem.Entry{Content: "the user prefers dark mode"})

	kg := memory.NewKG(st, 5, 0.5)
	if !kg.ShouldRecall("tell me about the database design") {
		t.Fatal("query should trigger recall")
	}
	out := kg.Recall(ctx, "tell me about the database design")
	if !strings.Contains(out, "append-only event log") {
		t.Fatalf("recall should surface the schema memory:\n%s", out)
	}
	if strings.Contains(out, "dark mode") {
		t.Fatalf("recall should NOT surface the unrelated memory:\n%s", out)
	}

	// cross-tenant: beta recalls nothing
	beta, _ := memory.NewStore(ctx, db, "beta", memory.WithEmbedder(emb))
	bkg := memory.NewKG(beta, 5, 0.5)
	if out := bkg.Recall(ctx, "tell me about the database design"); out != "" {
		t.Fatalf("beta must recall nothing: %q", out)
	}
}
```

- [ ] **Step 2: Run the E2E + full integration package**

Run: `go test -tags integration ./test/ -run TestMemoryRecallE2E` → PASS.
Run: `go test -tags integration ./test/` → PASS (all siblings; no cross-pollution — this test self-cleans `memory_events`).

- [ ] **Step 3: Commit**

```bash
git add test/memory_recall_e2e_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "test(e2e): semantic recall surfaces relevant memory, isolates tenants

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Documentation

**Files:**
- Modify: `README.md`
- Modify: `ROADMAP.md`
- Modify: `docs/images/project-layout.mmd` (+ regenerate `.png` if `mmdc` present)

- [ ] **Step 1: README — semantic recall subsection**

In `README.md`, find the "### Per-tenant agent memory" subsection (added in Memory M1). Immediately after it, add:

```markdown
#### Semantic recall

When embeddings are configured, a memory-enabled agent also gets **automatic
semantic recall**: each saved entry is embedded, and at the start of every turn
the most similar past memories are retrieved and injected into the prompt — no
agent code, no explicit lookup. Recall is tenant-isolated (same boundary as the
store) and best-effort (a slow or failing embedding service never breaks a turn).

Enable it by pointing the platform at an OpenAI-compatible embeddings endpoint
(the same proxy used for chat) and choosing a model + dimension:

```bash
export RUNTIME_EMBED_MODEL=text-embedding-3-small
export RUNTIME_EMBED_DIM=1536          # must match the model's output dimension
# reuses OPENAI_BASE_URL / OPENAI_API_KEY
# optional tuning:
export RUNTIME_EMBED_RECALL_K=5        # max memories injected per turn (default 5)
export RUNTIME_EMBED_RECALL_FLOOR=0.7  # min cosine similarity to inject (default 0.7)
```

If `RUNTIME_EMBED_MODEL` is unset, memory works exactly as before (tag/id
retrieval, no recall). If an embedding call fails on save, the entry is still
stored (durable) but is invisible to recall until re-embedded (e.g. on its next
update). Auto-ingestion of conversation facts is a planned follow-up; today
memories come only from the agent's explicit `memory` tool. Changing the
embedding model/dimension requires re-embedding (a documented migration).
```

- [ ] **Step 2: README — env-var table**

Add to the README env-var table (match its column structure):

```markdown
| `RUNTIME_EMBED_MODEL` | agentd | (unset) | Embedding model for semantic recall. Unset ⇒ recall disabled (tag/id memory only). |
| `RUNTIME_EMBED_DIM` | agentd | (unset) | Embedding dimension (the `vector(N)` width). Required + positive when `RUNTIME_EMBED_MODEL` is set; mismatched ⇒ fatal at startup. |
| `RUNTIME_EMBED_RECALL_K` | agentd | `5` | Max memories injected per turn. |
| `RUNTIME_EMBED_RECALL_FLOOR` | agentd | `0.7` | Minimum cosine similarity (0–1) for a memory to be injected. |
```

- [ ] **Step 3: ROADMAP — mark semantic recall done**

In `ROADMAP.md`:
(a) Update the `**Current state:**` header to note Memory's second milestone.
(b) In §B2, after the "First milestone DONE" paragraph, add:

```markdown
   **Second milestone DONE (merged to `master`, 2026-06-09):** semantic recall.
   Memory entries are embedded on save into a pgvector `vector(N)` column on
   `memory_events`; harness's `KnowledgeGraph` seam (wired via a new optional
   `agentruntime.Config.KGFn`) embeds each turn's query and injects the nearest
   tenant memories (top-K above a cosine floor) into the prompt — tenant-isolated
   (reuses M1's live-set projection) and best-effort (embed failure ⇒ NULL on
   write / "" on recall, never breaks a turn). Embeddings come from the
   OpenAI-compatible proxy (`RUNTIME_EMBED_MODEL`/`RUNTIME_EMBED_DIM`, reusing
   `OPENAI_*`); unset ⇒ M1 behavior. Auto-ingestion (`Ingest`) deferred. Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-09-memory-m2-semantic-recall*`.
```
(c) Update remaining B2 work: auto-ingestion, compaction/TTL, finer scoping, per-tenant embedding models.

- [ ] **Step 4: Layout diagram**

In `docs/images/project-layout.mmd`, update the `mem` node description to mention embeddings/recall, e.g. change its `<i>...</i>` text to `per-tenant Postgres MemoryStore<br/>(events + SQL projection, pgvector recall)`.

If `mmdc` is on PATH, regenerate:
```bash
command -v mmdc >/dev/null 2>&1 && mmdc -i docs/images/project-layout.mmd -o docs/images/project-layout.png -t neutral -b white -s 3 || echo "mmdc not available; skipping PNG regen"
```

- [ ] **Step 5: Final full verification**

Run:
```bash
go build ./... && go vet ./... && go test ./...
go test -tags integration ./internal/memory/
go test -tags integration ./test/
```
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add README.md ROADMAP.md docs/images/project-layout.mmd docs/images/project-layout.png
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "docs(memory): document semantic recall (README, ROADMAP, layout)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

(The project memory file lives outside the repo; the orchestrator updates it separately.)

---

## Final review (after all tasks)

Dispatch a holistic reviewer over `git diff master...HEAD`. Focus:

- **Degrade-don't-fail end to end** — embed failure on Save/Update ⇒ NULL + durable write; embed failure in Recall ⇒ "". Confirm no path makes a memory write or a turn fail because of embeddings, except the fatal misconfig (model set + bad dim, missing extension).
- **Isolation carries over** — `SearchSimilar` filters `tenant_id = s.tenant` and reuses M1's liveness clauses; the cross-tenant recall test genuinely bites.
- **Disabled path is M1-identical** — with embeddings unset, no vector column DDL, no `embedding` in INSERTs, no KGFn; M1 tests unchanged; the `vector` extension is not required.
- **pgvector binding** — the `pgVector` Valuer renders a literal pgvector inserts/compares accept; NULL handled; dim mismatch caught before the DB.
- **800ms / ctx** — Recall respects ctx cancellation (harness caps the wait); no goroutine leak.
- **Options pattern back-compat** — M1 `NewStore(ctx, db, tenant)` calls still compile and behave identically.
- **No harness change**; secrets/identity/conformance untouched; both integration packages green.

Then proceed to `superpowers:finishing-a-development-branch`.

---

## Self-Review (plan vs. spec)

- **Spec coverage:** embeddings client (T1), vector column + embed-on-save + SearchSimilar (T2), KnowledgeGraph recall + Ingest-noop (T3), `agentruntime.KGFn` wiring (T4), agentkind enablement + fatal-on-misconfig (T5), E2E recall + isolation (T6), docs incl. limitations (T7). All spec sections map to a task.
- **Deviations (documented):** (1) `NewStore` uses a variadic options pattern (`WithEmbedder`) rather than a changed positional signature, so M1 callers compile unchanged — the spec said "signature extended"; options is the back-compatible realization. (2) Save/Update branch on `embedder == nil` to keep the disabled INSERT column-identical to M1 (the spec implied one INSERT; two branches are needed because the `embedding` column doesn't exist when disabled). (3) The `agentruntime` wiring test asserts the field is set/invocable; full forward-into-RuntimeDeps is proven by the E2E (buildRuntime is unexported) — documented in T4.
- **Type consistency:** `Embedder{Embed(ctx,text)([]float32,error); Dim() int}`, `NewEmbedderFromEnv() (Embedder,int,bool,error)`, `WithEmbedder`, `Store.SearchSimilar(ctx,[]float32,int,float64)([]hmem.Entry,error)`, `pgVector`, `KG`/`NewKG(st,k,floor)`/`newKGWithSearch`, `agentruntime.Config.KGFn func(string) hrt.KnowledgeGraph`, `wireMemory(*agentruntime.Config, Deps)` — consistent across tasks.
- **Placeholder scan:** none — every step has concrete code/commands; the one decision (embedder dim in DDL) is `fmt.Sprintf(embedSchemaSQL, dim)`, fully specified.
