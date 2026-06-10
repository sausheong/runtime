# Gateway M2 — Semantic Tool Search Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A search-first gateway mode: consumers at `/gateway/mcp?mode=search` list exactly one tool (`search_tools`) that semantically searches the federated catalog; matched tools are callable by name though unlisted.

**Architecture:** New `internal/gateway/index.go` (Index: content-hash vector cache + brute-force cosine ranking over a view's tools, embedder = `memory.Embedder`). `server.go` grows mode-qualified views: search-mode SDK servers register all visible tools plus `search_tools`, and an `AddReceivingMiddleware` filter rewrites `tools/list` to show only `search_tools`. Config: `AgentConfig.Gateway` becomes a string-or-bool `GatewayMode`; search-mode agents get `?mode=search` appended to the injected URL.

**Tech Stack:** Go 1.25, go-sdk v1.5.0 (`AddReceivingMiddleware`, `MethodHandler func(ctx, method string, req Request) (Result, error)`, method constant value `"tools/list"`), `internal/memory.Embedder`/`NewEmbedderFromEnv` (RUNTIME_EMBED_* / OPENAI_*).

**Spec:** `docs/superpowers/specs/2026-06-10-gateway-m2-semantic-tool-search-design.md`

**Branch:** `feat/gateway-m2` (create from `master` before Task 1).

**Conventions:** go CLI is ground truth (LSP broken by `replace ../harness`); unit tests hermetic; integration tests `//go:build integration` + Postgres.app at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`; run everything from repo root.

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` (modify) | `GatewayMode` type (off/full/search) replacing the `Gateway bool` field; YAML union unmarshal |
| `internal/gateway/index.go` (create) | Index: vector cache (content-hash keyed), Search (embed query → cosine → floor → top-K), Match type |
| `internal/gateway/server.go` (modify) | Mode parsing from request, mode-qualified view keys, search-mode server build (all tools + search_tools + list filter middleware), search_tools handler |
| `controlplane/proxy.go` + `registry.go` (modify) | `GatewaySearch bool` threading; `?mode=search` URL suffix in buildEnv |
| `cmd/runtimed/main.go` (modify) | Index construction from `memory.NewEmbedderFromEnv`, fail-fast `validateGatewaySearch`, pass Index to Handler |
| `test/gateway_search_e2e_test.go` (create) | Through-serve e2e with fake upstream + fake embedding HTTP server |

Current state notes for implementers (read the actual files first; line numbers drift):
- `internal/config/config.go`: `AgentConfig.Gateway bool` at ~line 22; `GatewayConfig{Servers, AgentKeys, SelfURL}` + `Enabled()`.
- `internal/gateway/server.go`: `Handler{m, PrincipalFor, mu, cache map[string]*cachedServer}`; `viewKey(p, ok) (key, tenant)` returns `"*"` / `"t:<id>"` / `"!none"`; `HTTP()` wraps `sdk.NewStreamableHTTPHandler(getServer, nil)` with a nil-PrincipalFor 503 guard; `serverFor(p, ok)` caches per key+generation; `toolHandler(builtFor, t)` does per-call view re-check + viewer role gate; `errResult(msg)` helper; `noneTenant` constant in manager.go.
- `controlplane/proxy.go`: `AgentProcess{..., GatewayOn, GatewayURL, GatewayKey}`; buildEnv appends `RUNTIME_GATEWAY_URL=`+URL when on, empty shadows when off.
- `controlplane/registry.go`: `NewRegistry` sets `GatewayOn: a.Gateway` (bool today); `SetGateway(url, keys)` stamps URL+key.
- `cmd/runtimed/main.go`: gateway block builds Manager+Handler early, `Start` deferred past identity exits; `validateGatewayKeys(cfg)`; `gatewaySelfURL(selfURL, ctlAddr)`.
- `internal/memory/embed.go`: `Embedder{Embed(ctx, text) ([]float32, error); Dim() int}`; `NewEmbedderFromEnv() (emb, dim, enabled, err)`.

---

### Task 1: GatewayMode config type (string-or-bool YAML union)

**Files:**
- Modify: `internal/config/config.go`
- Modify: `controlplane/registry.go` (one-line: `a.Gateway` bool → `a.Gateway.Enabled()`)
- Modify: `cmd/runtimed/main.go` (`a.Gateway` in validateGatewayKeys → `a.Gateway.Enabled()`)
- Test: `internal/config/config_test.go` (append; update existing TestAgentConfigGatewayFlag)

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestGatewayModeYAML(t *testing.T) {
	load := func(t *testing.T, gatewayVal string) (*Config, error) {
		t.Helper()
		dir := t.TempDir()
		p := dir + "/runtime.yaml"
		y := "agents:\n  - {id: a, name: A, model: m, listen_addr: 127.0.0.1:1" + gatewayVal + "}\n"
		if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
			t.Fatal(err)
		}
		return Load(p)
	}

	t.Run("absent means off", func(t *testing.T) {
		c, err := load(t, "")
		if err != nil {
			t.Fatal(err)
		}
		if c.Agents[0].Gateway != GatewayOff {
			t.Fatalf("want off, got %v", c.Agents[0].Gateway)
		}
		if c.Agents[0].Gateway.Enabled() {
			t.Fatal("off must not be enabled")
		}
	})

	t.Run("true means full", func(t *testing.T) {
		c, err := load(t, ", gateway: true")
		if err != nil {
			t.Fatal(err)
		}
		if c.Agents[0].Gateway != GatewayFull {
			t.Fatalf("want full, got %v", c.Agents[0].Gateway)
		}
		if !c.Agents[0].Gateway.Enabled() {
			t.Fatal("full must be enabled")
		}
	})

	t.Run("false means off", func(t *testing.T) {
		c, err := load(t, ", gateway: false")
		if err != nil {
			t.Fatal(err)
		}
		if c.Agents[0].Gateway != GatewayOff {
			t.Fatalf("want off, got %v", c.Agents[0].Gateway)
		}
	})

	t.Run("search string", func(t *testing.T) {
		c, err := load(t, ", gateway: search")
		if err != nil {
			t.Fatal(err)
		}
		if c.Agents[0].Gateway != GatewaySearch {
			t.Fatalf("want search, got %v", c.Agents[0].Gateway)
		}
		if !c.Agents[0].Gateway.Enabled() {
			t.Fatal("search must be enabled")
		}
	})

	t.Run("full string", func(t *testing.T) {
		c, err := load(t, ", gateway: full")
		if err != nil {
			t.Fatal(err)
		}
		if c.Agents[0].Gateway != GatewayFull {
			t.Fatalf("want full, got %v", c.Agents[0].Gateway)
		}
	})

	t.Run("invalid string rejected at load", func(t *testing.T) {
		if _, err := load(t, ", gateway: banana"); err == nil {
			t.Fatal("expected load error for invalid gateway mode")
		}
	})
}
```

Update the existing `TestAgentConfigGatewayFlag` (it sets `Gateway: true` in a struct literal): change `Gateway: true` to `Gateway: GatewayFull` and the assertion `!c.Agents[0].Gateway` to `c.Agents[0].Gateway != GatewayFull`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestGatewayMode|TestAgentConfigGatewayFlag' -v`
Expected: compile FAILURE — `GatewayOff`/`GatewayFull`/`GatewaySearch` undefined; `Gateway: true` type mismatch only after the type changes (red phase is the undefined constants).

- [ ] **Step 3: Implement**

In `internal/config/config.go`:

1. Add the type (after `TokenConfig`, before `GatewayServer`):

```go
// GatewayMode is the per-agent gateway opt-in. YAML accepts a bool
// (true ⇒ full, false ⇒ off) or a string ("full" | "search"); anything
// else is a load error. The zero value is off.
type GatewayMode string

const (
	GatewayOff    GatewayMode = ""       // not opted in
	GatewayFull   GatewayMode = "full"   // M1 behavior: full federated tools/list
	GatewaySearch GatewayMode = "search" // M2: list only search_tools; catalog via search
)

// Enabled reports whether the agent consumes the gateway at all.
func (g GatewayMode) Enabled() bool { return g == GatewayFull || g == GatewaySearch }

// UnmarshalYAML implements the bool-or-string union.
func (g *GatewayMode) UnmarshalYAML(unmarshal func(any) error) error {
	var b bool
	if err := unmarshal(&b); err == nil {
		if b {
			*g = GatewayFull
		} else {
			*g = GatewayOff
		}
		return nil
	}
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	switch s {
	case "full":
		*g = GatewayFull
	case "search":
		*g = GatewaySearch
	default:
		return fmt.Errorf("config: invalid gateway mode %q (want true|false|full|search)", s)
	}
	return nil
}
```

NOTE: this repo uses `gopkg.in/yaml.v3`. v3 prefers `UnmarshalYAML(value *yaml.Node) error`. The legacy `func(any) error` signature is NOT called by v3 — implement the v3 form instead:

```go
// UnmarshalYAML implements the bool-or-string union (yaml.v3 node form).
func (g *GatewayMode) UnmarshalYAML(value *yaml.Node) error {
	var b bool
	if err := value.Decode(&b); err == nil {
		if b {
			*g = GatewayFull
		} else {
			*g = GatewayOff
		}
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("config: invalid gateway mode (want true|false|full|search)")
	}
	switch s {
	case "full":
		*g = GatewayFull
	case "search":
		*g = GatewaySearch
	default:
		return fmt.Errorf("config: invalid gateway mode %q (want true|false|full|search)", s)
	}
	return nil
}
```

Use ONLY the v3 form (delete the first snippet — it is shown to explain the union, not to be added).

2. Change the field: `Gateway GatewayMode \`yaml:"gateway"\`` (comment: "optional; off (default) | full (true) | search").

3. Fix call sites of the old bool:
- `controlplane/registry.go` `NewRegistry`: `GatewayOn: a.Gateway` → `GatewayOn: a.Gateway.Enabled()` — and add `GatewaySearch: a.Gateway == config.GatewaySearch` ONLY in Task 3 (here just make it compile with Enabled()).
- `cmd/runtimed/main.go` `validateGatewayKeys`: `if a.Gateway && ...` → `if a.Gateway.Enabled() && ...`.
- Search the whole repo for other `.Gateway` boolean uses: `grep -rn "\.Gateway\b" --include="*.go" | grep -v "_test\|GatewayMode\|GatewayConfig"` and fix any remaining (test files referencing `Gateway: true` in config literals: change to `Gateway: config.GatewayFull` or `GatewayFull` in-package).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v && go build ./... && go test ./controlplane/ ./cmd/runtimed/ -count=1`
Expected: ALL PASS, clean build (proves all call sites fixed).

- [ ] **Step 5: Commit**

```bash
git add internal/config/ controlplane/ cmd/runtimed/
git commit -m "feat(gateway): GatewayMode string-or-bool union (off/full/search)"
```

---

### Task 2: Index — vector cache + cosine search

**Files:**
- Create: `internal/gateway/index.go`
- Test: `internal/gateway/index_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/gateway/index_test.go` (package gateway; fakeTool already exists in manager_test.go):

```go
package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/sausheong/harness/tool"
)

// fakeEmbedder returns deterministic unit vectors per registered text and
// counts calls. Unknown text → err if failAll, else a default far vector.
type fakeEmbedder struct {
	vecs    map[string][]float32
	calls   atomic.Int64
	failFor map[string]bool // texts whose embed fails
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.calls.Add(1)
	if f.failFor[text] {
		return nil, errors.New("scripted embed failure")
	}
	if v, ok := f.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil // orthogonal default
}
func (f *fakeEmbedder) Dim() int { return 3 }

// toolText mirrors index.go's embedding input so tests register vectors
// under the right key.
func tt(name, desc string) string { return name + "\n" + desc }

func searchTools() []tool.Tool {
	return []tool.Tool{
		fakeTool{name: "fs__read", out: "x"},   // Description() = "fake fs__read"
		fakeTool{name: "fs__write", out: "x"},  // "fake fs__write"
		fakeTool{name: "web__fetch", out: "x"}, // "fake web__fetch"
	}
}

func TestIndexSearchRanksAndFloors(t *testing.T) {
	emb := &fakeEmbedder{vecs: map[string][]float32{
		"read a file":                         {1, 0, 0},
		tt("fs__read", "fake fs__read"):       {0.9, 0.1, 0}, // close
		tt("fs__write", "fake fs__write"):     {0.5, 0.5, 0}, // mid
		tt("web__fetch", "fake web__fetch"):   {0, 1, 0},     // orthogonal-ish
	}}
	idx := NewIndex(emb, 0.3, 5)
	ms, err := idx.Search(context.Background(), searchTools(), "read a file", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("want 2 above floor 0.3, got %d: %+v", len(ms), ms)
	}
	if ms[0].Name != "fs__read" || ms[1].Name != "fs__write" {
		t.Fatalf("wrong order: %+v", ms)
	}
	if ms[0].Score <= ms[1].Score {
		t.Fatalf("scores not descending: %+v", ms)
	}
	if len(ms[0].InputSchema) == 0 {
		t.Fatal("match missing input schema")
	}
}

func TestIndexKAndCap(t *testing.T) {
	emb := &fakeEmbedder{vecs: map[string][]float32{
		"q":                                   {1, 0, 0},
		tt("fs__read", "fake fs__read"):       {1, 0, 0},
		tt("fs__write", "fake fs__write"):     {0.99, 0.01, 0},
		tt("web__fetch", "fake web__fetch"):   {0.98, 0.02, 0},
	}}
	idx := NewIndex(emb, 0.1, 5)
	ms, err := idx.Search(context.Background(), searchTools(), "q", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("k=2 not respected: got %d", len(ms))
	}
	// k<=0 falls back to the default
	ms, err = idx.Search(context.Background(), searchTools(), "q", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("k=0 should use default(5)→all 3, got %d", len(ms))
	}
	// k above cap clamps
	ms, _ = idx.Search(context.Background(), searchTools(), "q", 1000)
	if len(ms) != 3 {
		t.Fatalf("clamped k should still return all 3, got %d", len(ms))
	}
}

func TestIndexVectorCacheReuse(t *testing.T) {
	emb := &fakeEmbedder{vecs: map[string][]float32{"q": {1, 0, 0}}}
	idx := NewIndex(emb, 0.0, 5)
	ts := searchTools()
	if _, err := idx.Search(context.Background(), ts, "q", 5); err != nil {
		t.Fatal(err)
	}
	first := emb.calls.Load() // 3 tools + 1 query
	if first != 4 {
		t.Fatalf("want 4 embed calls on first search, got %d", first)
	}
	if _, err := idx.Search(context.Background(), ts, "q", 5); err != nil {
		t.Fatal(err)
	}
	// Second search: only the query re-embeds; tool vectors cached.
	if got := emb.calls.Load(); got != first+1 {
		t.Fatalf("tool vectors not cached: %d calls after second search", got)
	}
}

func TestIndexQueryEmbedFailure(t *testing.T) {
	emb := &fakeEmbedder{failFor: map[string]bool{"q": true}}
	idx := NewIndex(emb, 0.0, 5)
	if _, err := idx.Search(context.Background(), searchTools(), "q", 5); err == nil {
		t.Fatal("want error on query embed failure")
	}
}

func TestIndexToolEmbedFailureDegrades(t *testing.T) {
	emb := &fakeEmbedder{
		vecs: map[string][]float32{
			"q":                                 {1, 0, 0},
			tt("fs__read", "fake fs__read"):     {1, 0, 0},
			tt("web__fetch", "fake web__fetch"): {0.9, 0.1, 0},
		},
		failFor: map[string]bool{tt("fs__write", "fake fs__write"): true},
	}
	idx := NewIndex(emb, 0.0, 5)
	ms, err := idx.Search(context.Background(), searchTools(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("failed-embed tool should be skipped, got %d matches", len(ms))
	}
	for _, m := range ms {
		if m.Name == "fs__write" {
			t.Fatal("failed-embed tool surfaced in results")
		}
	}
	// Retry on next search: now let it succeed.
	delete(emb.failFor, tt("fs__write", "fake fs__write"))
	emb.vecs[tt("fs__write", "fake fs__write")] = []float32{0.8, 0.2, 0}
	ms, _ = idx.Search(context.Background(), searchTools(), "q", 5)
	if len(ms) != 3 {
		t.Fatalf("failed tool not retried on next search: %d", len(ms))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gateway/ -run TestIndex -v`
Expected: compile FAILURE — `NewIndex` undefined.

- [ ] **Step 3: Implement index.go**

```go
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"math"
	"sort"
	"sync"

	"github.com/sausheong/harness/tool"
	"github.com/sausheong/runtime/internal/memory"
)

// Match is one search result: enough for the agent to call the tool
// immediately (full schema inline, no second round-trip).
type Match struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Score       float64         `json:"score"`
}

// Index ranks a view's tools against a natural-language query by embedding
// cosine similarity. Vectors are cached by tool content identity
// (hash of name+description), so each distinct tool text embeds once per
// process lifetime — across views, generations, and reconnects. Lazy only:
// vectors are computed on the first Search that needs them.
type Index struct {
	emb      memory.Embedder
	floor    float64
	defaultK int

	mu     sync.Mutex
	vecs   map[[32]byte][]float32 // content hash → unit-ish vector
	logged map[[32]byte]bool      // embed-failure logged once per text
}

// searchCapK is the hard ceiling on k regardless of the request.
const searchCapK = 20

// NewIndex builds an Index. floor is the minimum cosine similarity for a
// match; defaultK is used when a search request omits k (or passes <=0).
func NewIndex(emb memory.Embedder, floor float64, defaultK int) *Index {
	return &Index{
		emb:      emb,
		floor:    floor,
		defaultK: defaultK,
		vecs:     map[[32]byte][]float32{},
		logged:   map[[32]byte]bool{},
	}
}

// toolText is the embedding input for a tool: name + description. Newline
// separator (not in tool names by construction).
func toolText(t tool.Tool) string { return t.Name() + "\n" + t.Description() }

func toolKey(text string) [32]byte { return sha256.Sum256([]byte(text)) }

// Search embeds query, ensures vectors for tools (embedding misses
// sequentially; a tool whose embed fails is skipped this round, logged once
// per text, and retried next Search), then returns up to k matches with
// cosine >= floor, sorted by score descending. k<=0 ⇒ defaultK; k is
// clamped to searchCapK. A query-embed failure returns an error (the
// caller maps it to an MCP isError result).
func (ix *Index) Search(ctx context.Context, tools []tool.Tool, query string, k int) ([]Match, error) {
	if k <= 0 {
		k = ix.defaultK
	}
	if k > searchCapK {
		k = searchCapK
	}
	qv, err := ix.emb.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	var out []Match
	for _, t := range tools {
		text := toolText(t)
		key := toolKey(text)
		ix.mu.Lock()
		v, ok := ix.vecs[key]
		ix.mu.Unlock()
		if !ok {
			ev, eerr := ix.emb.Embed(ctx, text)
			if eerr != nil {
				ix.mu.Lock()
				if !ix.logged[key] {
					ix.logged[key] = true
					ix.mu.Unlock()
					slog.Warn("gateway: tool embed failed; excluded from search until it succeeds",
						"tool", t.Name(), "err", eerr)
				} else {
					ix.mu.Unlock()
				}
				continue
			}
			ix.mu.Lock()
			ix.vecs[key] = ev
			delete(ix.logged, key)
			ix.mu.Unlock()
			v = ev
		}
		score := cosine(qv, v)
		if score < ix.floor {
			continue
		}
		out = append(out, Match{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: json.RawMessage(t.Parameters()),
			Score:       score,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// cosine computes cosine similarity; 0 on dimension mismatch or zero vector.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gateway/ -run TestIndex -v -race`
Expected: ALL PASS.

- [ ] **Step 5: Full package + vet, commit**

Run: `go test ./internal/gateway/ -race -count=2 && go vet ./...`

```bash
git add internal/gateway/
git commit -m "feat(gateway): Index — content-hash vector cache + cosine tool search"
```

---

### Task 3: Mode-aware server views + search_tools

**Files:**
- Modify: `internal/gateway/server.go`
- Test: `internal/gateway/server_test.go` (append) — NOTE existing helper `dialGateway(t, h, p)` posts to `httptest.NewServer(h.HTTP())` root; search-mode tests need the `?mode=search` URL, so add a mode-aware dial helper.

This is the largest task. The Handler gains: an optional Index, request-mode parsing, mode-qualified view keys, the search-mode server build (all tools registered + list-filter middleware + search_tools), and the search_tools handler.

- [ ] **Step 1: Write the failing tests**

Append to `internal/gateway/server_test.go`:

```go
// dialGatewayMode is dialGateway with an explicit mode query param.
func dialGatewayMode(t *testing.T, h *Handler, p *identity.Principal, mode string) *sdk.ClientSession {
	t.Helper()
	h.PrincipalFor = func(_ context.Context) (identity.Principal, bool) {
		if p == nil {
			return identity.Principal{}, false
		}
		return *p, true
	}
	srv := httptest.NewServer(h.HTTP())
	t.Cleanup(srv.Close)
	url := srv.URL
	if mode != "" {
		url += "?mode=" + mode
	}
	cli := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	sess, err := cli.Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// searchIndex builds an Index whose fake embedder makes "read a file"
// match open__echo strongly and scoped__secret weakly.
func searchIndexForGw() *Index {
	emb := &fakeEmbedder{vecs: map[string][]float32{
		"read a file":                                {1, 0, 0},
		tt("open__echo", "fake mcp__open__echo"):     {0.9, 0.1, 0},
		tt("scoped__secret", "fake mcp__scoped__secret"): {0, 1, 0},
	}}
	return NewIndex(emb, 0.3, 5)
}

func TestSearchModeListsOnlySearchTools(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	h.Index = searchIndexForGw()
	sess := dialGatewayMode(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, "search")
	names := listNames(t, sess)
	if len(names) != 1 || names[0] != "search_tools" {
		t.Fatalf("search mode must list exactly [search_tools], got %v", names)
	}
}

func TestSearchModeFullListUnchanged(t *testing.T) {
	// Same handler serves full mode untouched.
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	h.Index = searchIndexForGw()
	sess := dialGatewayMode(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, "")
	if names := listNames(t, sess); len(names) != 2 {
		t.Fatalf("full mode changed: %v", names)
	}
}

func TestSearchToolsReturnsMatches(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	h.Index = searchIndexForGw()
	sess := dialGatewayMode(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, "search")
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "search_tools", Arguments: map[string]any{"query": "read a file"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError: %+v", res.Content)
	}
	txt := res.Content[0].(*sdk.TextContent).Text
	var ms []Match
	if err := json.Unmarshal([]byte(txt), &ms); err != nil {
		t.Fatalf("result not JSON matches: %v\n%s", err, txt)
	}
	if len(ms) != 1 || ms[0].Name != "open__echo" {
		t.Fatalf("want [open__echo], got %+v", ms)
	}
	if len(ms[0].InputSchema) == 0 {
		t.Fatal("schema missing from match")
	}
}

func TestSearchModeUnlistedToolStillCallable(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	h.Index = searchIndexForGw()
	sess := dialGatewayMode(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, "search")
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: "open__echo"})
	if err != nil {
		t.Fatalf("unlisted tool not callable: %v", err)
	}
	if res.IsError {
		t.Fatalf("unlisted tool call errored: %+v", res.Content)
	}
}

func TestSearchModeTenancyHolds(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	h.Index = searchIndexForGw()
	// globex cannot see scoped__secret: not in search results even with a
	// query aimed at it, and not callable.
	emb := h.Index.emb.(*fakeEmbedder)
	emb.vecs["secret stuff"] = []float32{0, 1, 0} // aligned with scoped__secret
	sess := dialGatewayMode(t, h, &identity.Principal{TenantID: "globex", Role: identity.RoleOperator}, "search")
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "search_tools", Arguments: map[string]any{"query": "secret stuff"},
	})
	if err != nil || res.IsError {
		t.Fatalf("search failed: %v %+v", err, res)
	}
	txt := res.Content[0].(*sdk.TextContent).Text
	if strings.Contains(txt, "scoped__secret") {
		t.Fatalf("cross-tenant tool leaked into search results: %s", txt)
	}
	if _, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: "scoped__secret"}); err == nil {
		t.Fatal("cross-tenant tool callable in search mode")
	}
}

func TestSearchModeViewerCanSearchNotCall(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	h.Index = searchIndexForGw()
	sess := dialGatewayMode(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleViewer}, "search")
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "search_tools", Arguments: map[string]any{"query": "read a file"},
	})
	if err != nil || res.IsError {
		t.Fatalf("viewer search should succeed: %v %+v", err, res)
	}
	res, err = sess.CallTool(context.Background(), &sdk.CallToolParams{Name: "open__echo"})
	if err != nil {
		t.Fatalf("want isError, got transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("viewer must not call catalog tools")
	}
}

func TestSearchToolsZeroMatchesHint(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	h.Index = searchIndexForGw()
	sess := dialGatewayMode(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, "search")
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "search_tools", Arguments: map[string]any{"query": "completely unrelated"},
	})
	if err != nil || res.IsError {
		t.Fatalf("zero matches must be success: %v %+v", err, res)
	}
	txt := res.Content[0].(*sdk.TextContent).Text
	if !strings.Contains(txt, "[]") || !strings.Contains(txt, "broader") {
		t.Fatalf("missing empty array + broaden hint: %s", txt)
	}
}

func TestSearchToolsQueryEmbedFailure(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	emb := &fakeEmbedder{failFor: map[string]bool{"q": true}}
	h.Index = NewIndex(emb, 0.3, 5)
	sess := dialGatewayMode(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, "search")
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "search_tools", Arguments: map[string]any{"query": "q"},
	})
	if err != nil {
		t.Fatalf("want isError, got transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("query embed failure must be isError")
	}
	if txt := res.Content[0].(*sdk.TextContent).Text; !strings.Contains(txt, "unavailable") {
		t.Fatalf("unhelpful error text: %s", txt)
	}
}

func TestSearchModeWithoutIndex400(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m) // no Index
	h.PrincipalFor = OpenMode
	srv := httptest.NewServer(h.HTTP())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"?mode=search", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestJunkMode400(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	h.Index = searchIndexForGw()
	h.PrincipalFor = OpenMode
	srv := httptest.NewServer(h.HTTP())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"?mode=banana", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestCrossModeSessionRejected(t *testing.T) {
	m := startManager(t, gwServers(), gwConns())
	h := NewHandler(m)
	h.Index = searchIndexForGw()
	// Session created in search mode...
	sess := dialGatewayMode(t, h, &identity.Principal{TenantID: "acme", Role: identity.RoleOperator}, "search")
	if names := listNames(t, sess); len(names) != 1 {
		t.Fatalf("setup: %v", names)
	}
	// ...same principal; calls keep working (mode comes from the session's
	// server, principal+tenant from the live ctx — the view re-check binds
	// tenant+mode together, so same-principal same-mode passes).
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: "open__echo"})
	if err != nil || res.IsError {
		t.Fatalf("same-mode call should pass: %v %+v", err, res)
	}
	// A different tenant principal hitting this session is rejected (M1
	// behavior preserved under mode-qualified keys).
	h.PrincipalFor = func(_ context.Context) (identity.Principal, bool) {
		return identity.Principal{TenantID: "globex", Role: identity.RoleOperator}, true
	}
	res, err = sess.CallTool(context.Background(), &sdk.CallToolParams{Name: "open__echo"})
	if err != nil {
		t.Fatalf("want isError: %v", err)
	}
	if !res.IsError {
		t.Fatal("cross-principal call must be rejected in search mode too")
	}
}
```

Add imports as needed (`net/http`, `strings`, `encoding/json` may already be present — check).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gateway/ -run 'TestSearch|TestJunkMode|TestCrossMode' -v`
Expected: compile FAILURE — `h.Index` undefined, `dialGatewayMode` references missing pieces.

- [ ] **Step 3: Implement in server.go**

1. Handler gains the Index field (doc comment updated):

```go
	// Index enables search mode (?mode=search): nil ⇒ search mode is
	// unavailable and requests for it are rejected with 400.
	Index *Index
```

2. Mode type + parsing (top of file):

```go
// viewMode is the consumption mode of a gateway session.
type viewMode string

const (
	modeFull   viewMode = "full"
	modeSearch viewMode = "search"
)

// modeFromRequest parses ?mode=; absent/empty ⇒ full. Returns an error for
// unknown values (HTTP 400 at the edge, before session creation).
func modeFromRequest(r *http.Request) (viewMode, error) {
	switch r.URL.Query().Get("mode") {
	case "", "full":
		return modeFull, nil
	case "search":
		return modeSearch, nil
	default:
		return "", fmt.Errorf("unknown mode %q (want full|search)", r.URL.Query().Get("mode"))
	}
}
```

3. Mode-qualified view keys — change `viewKey` to take and incorporate the mode:

```go
func viewKey(p identity.Principal, ok bool, mode viewMode) (key, tenant string) {
	base, tenant := principalView(p, ok)
	return base + "|" + string(mode), tenant
}

// principalView is the M1 view computation (unscoped/superuser/tenant/none).
func principalView(p identity.Principal, ok bool) (key, tenant string) {
	if !ok || p.Superuser {
		return "*", ""
	}
	if p.TenantID == "" {
		return "!none", noneTenant
	}
	return "t:" + p.TenantID, p.TenantID
}
```

Update ALL viewKey call sites: `serverFor`, `toolHandler`'s re-check, and `Status` (Status has no mode — it should call `principalView` directly instead of `viewKey`).

4. HTTP() — parse mode before the SDK handler, reject junk/unavailable:

```go
func (h *Handler) HTTP() http.Handler {
	mcp := sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		p, ok := h.PrincipalFor(r.Context())
		mode, _ := modeFromRequest(r) // junk already rejected below
		return h.serverFor(p, ok, mode)
	}, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.PrincipalFor == nil {
			http.Error(w, "gateway not wired", http.StatusServiceUnavailable)
			return
		}
		mode, err := modeFromRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if mode == modeSearch && h.Index == nil {
			http.Error(w, "search mode requires embeddings (RUNTIME_EMBED_MODEL)", http.StatusBadRequest)
			return
		}
		mcp.ServeHTTP(w, r)
	})
}
```

5. serverFor gains the mode; search-mode build registers ALL tools + search_tools + the list filter:

```go
func (h *Handler) serverFor(p identity.Principal, ok bool, mode viewMode) *sdk.Server {
	key, tenant := viewKey(p, ok, mode)
	gen := h.m.Generation()
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, hit := h.cache[key]; hit && c.gen == gen {
		return c.srv
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "runtime-gateway", Version: "m2"}, nil)
	tools := h.m.ToolsFor(tenant)
	for _, t := range tools {
		srv.AddTool(&sdk.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: json.RawMessage(t.Parameters()),
		}, h.toolHandler(key, t))
	}
	if mode == modeSearch {
		srv.AddTool(searchToolDef(), h.searchHandler(key, tenant))
		srv.AddReceivingMiddleware(listOnlySearchTools)
	}
	h.cache[key] = &cachedServer{gen: gen, srv: srv}
	return srv
}
```

(`toolHandler`'s `builtFor` re-check now compares mode-qualified keys computed via `viewKey(p, ok, <mode of this server>)` — thread the mode into the closure: simplest is to pass `key` as today since key already embeds the mode, and recompute the caller's key with the SAME mode constant captured at build time:

```go
// inside toolHandler(builtFor string, t tool.Tool) — the re-check becomes:
p, ok := h.PrincipalFor(ctx)
callerBase, _ := principalView(p, ok)
builtBase, _, _ := strings.Cut(builtFor, "|")
if callerBase != builtBase {
	return errResult("forbidden: session does not belong to this principal's view"), nil
}
```

NOTE the deliberate semantics: the per-call re-check binds the PRINCIPAL'S VIEW (tenant); the mode is a property of the session's server, not the caller, so comparing the base (pre-`|`) part keeps M1's protection while letting the same principal use full- and search-mode sessions concurrently. Add `"strings"` import.)

6. search_tools definition + handler:

```go
// searchToolDef describes the search_tools tool. The name cannot collide
// with upstream tools: their names always contain "__" (server__tool).
func searchToolDef() *sdk.Tool {
	return &sdk.Tool{
		Name: "search_tools",
		Description: "Search the tool catalog by describing what you want to do. " +
			"Returns matching tools (name, description, input schema) ranked by relevance; " +
			"call any returned tool directly by name.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "natural-language description of the capability you need"},
				"k": {"type": "integer", "description": "max results (default 5, cap 20)"}
			},
			"required": ["query"]
		}`),
	}
}

// searchHandler serves search_tools for one view. Viewers MAY search
// (search is a read, like tools/list); the M1 call gate still applies to
// the result tools themselves. The principal-view re-check matches
// toolHandler's.
func (h *Handler) searchHandler(builtFor, tenant string) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		p, ok := h.PrincipalFor(ctx)
		callerBase, _ := principalView(p, ok)
		builtBase, _, _ := strings.Cut(builtFor, "|")
		if callerBase != builtBase {
			return errResult("forbidden: session does not belong to this principal's view"), nil
		}
		var in struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil || in.Query == "" {
			return errResult("search_tools requires {\"query\": string}"), nil
		}
		ms, err := h.Index.Search(ctx, h.m.ToolsFor(tenant), in.Query, in.K)
		if err != nil {
			return errResult("search temporarily unavailable: " + err.Error()), nil
		}
		if ms == nil {
			ms = []Match{}
		}
		b, _ := json.Marshal(ms)
		text := string(b)
		if len(ms) == 0 {
			text += "\nNo tools matched; try a broader query."
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}, nil
	}
}

// listOnlySearchTools is receiving middleware that rewrites tools/list
// results to expose only search_tools (the catalog stays callable but
// unlisted — that's the entire point of search mode). Locked by unit test;
// an SDK upgrade that changes the method constant or result type fails
// loudly here.
func listOnlySearchTools(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil || method != "tools/list" {
			return res, err
		}
		lt, ok := res.(*sdk.ListToolsResult)
		if !ok {
			return res, err
		}
		filtered := &sdk.ListToolsResult{Tools: []*sdk.Tool{}}
		for _, t := range lt.Tools {
			if t.Name == "search_tools" {
				filtered.Tools = append(filtered.Tools, t)
			}
		}
		return filtered, nil
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gateway/ -race -count=2 -v`
Expected: ALL (M1 + Index + new mode tests) PASS. M1 tests exercise viewKey — they now go through the mode-qualified path with `modeFull`; if any fail on key format, fix the test only if its assertion hardcoded a key string (none should).

- [ ] **Step 5: Vet + full suite, commit**

Run: `go vet ./... && go build ./... && go test ./...`

```bash
git add internal/gateway/
git commit -m "feat(gateway): search mode — mode-qualified views, list filter, search_tools"
```

---

### Task 4: Spawn-path wiring (?mode=search URL) + runtimed assembly

**Files:**
- Modify: `controlplane/proxy.go` (GatewaySearch field + URL suffix)
- Modify: `controlplane/registry.go` (thread search mode)
- Modify: `cmd/runtimed/main.go` (Index construction, fail-fast validateGatewaySearch)
- Test: `controlplane/proxy_test.go`, `controlplane/registry_test.go`, `cmd/runtimed/gateway_keys_test.go` (append to each)

- [ ] **Step 1: Write the failing tests**

Append to `controlplane/proxy_test.go`:

```go
func TestBuildEnvGatewaySearchMode(t *testing.T) {
	a := AgentProcess{
		AgentID: "x", Addr: "127.0.0.1:1", BinPath: "bin", PGDSN: "dsn",
		Tenant: "acme", GatewayOn: true, GatewaySearch: true,
		GatewayURL: "http://127.0.0.1:8080/gateway/mcp",
		GatewayKey: "svk-test",
	}
	env, err := a.buildEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertHasEnv(t, env, "RUNTIME_GATEWAY_URL=http://127.0.0.1:8080/gateway/mcp?mode=search")
}
```

Append to `controlplane/registry_test.go` inside or alongside `TestRegistryThreadsGateway` (new test):

```go
func TestRegistryThreadsGatewaySearch(t *testing.T) {
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{ID: "s", Name: "S", Model: "m", ListenAddr: "127.0.0.1:1", Gateway: config.GatewaySearch},
			{ID: "f", Name: "F", Model: "m", ListenAddr: "127.0.0.1:2", Gateway: config.GatewayFull},
		},
		Gateway: config.GatewayConfig{Servers: []config.GatewayServer{{Name: "fs", Command: "x"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(cfg, "bin", "dsn")
	s, _ := r.Get("s")
	if !s.GatewayOn || !s.GatewaySearch {
		t.Fatalf("search agent not threaded: %+v", s)
	}
	f, _ := r.Get("f")
	if !f.GatewayOn || f.GatewaySearch {
		t.Fatalf("full agent wrong: %+v", f)
	}
}
```

Append to `cmd/runtimed/gateway_keys_test.go` (same package main):

```go
func TestValidateGatewaySearch(t *testing.T) {
	mk := func(mode config.GatewayMode) *config.Config {
		return &config.Config{
			Agents: []config.AgentConfig{
				{ID: "a", Name: "A", Model: "m", ListenAddr: "127.0.0.1:1", Gateway: mode, Tenant: "default"},
			},
			Gateway: config.GatewayConfig{Servers: []config.GatewayServer{{Name: "fs", Command: "x"}}},
		}
	}
	t.Run("search agent without embeddings is an error", func(t *testing.T) {
		if err := validateGatewaySearch(mk(config.GatewaySearch), false); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("search agent with embeddings ok", func(t *testing.T) {
		if err := validateGatewaySearch(mk(config.GatewaySearch), true); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("full agent without embeddings ok", func(t *testing.T) {
		if err := validateGatewaySearch(mk(config.GatewayFull), false); err != nil {
			t.Fatal(err)
		}
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./controlplane/ ./cmd/runtimed/ -run 'GatewaySearch|ValidateGatewaySearch' -v`
Expected: compile FAILURE — `GatewaySearch` field / `validateGatewaySearch` undefined.

- [ ] **Step 3: Implement**

`controlplane/proxy.go` — add after `GatewayKey`:

```go
	GatewaySearch bool // search-mode opt-in: appends ?mode=search to the injected gateway URL.
```

In buildEnv, change the GatewayOn branch:

```go
	if a.GatewayOn {
		u := a.GatewayURL
		if a.GatewaySearch {
			u += "?mode=search"
		}
		env = append(env, "RUNTIME_GATEWAY_URL="+u)
		if a.GatewayKey != "" {
			env = append(env, "RUNTIME_GATEWAY_KEY="+a.GatewayKey)
		}
	} else {
		env = append(env, "RUNTIME_GATEWAY_URL=", "RUNTIME_GATEWAY_KEY=")
	}
```

`controlplane/registry.go` — NewRegistry literal adds `GatewaySearch: a.Gateway == config.GatewaySearch,`.

`cmd/runtimed/main.go`:

1. Helper (near validateGatewayKeys):

```go
// validateGatewaySearch returns an error naming the first agent that opted
// into gateway search mode while embeddings are not configured — search mode
// cannot work without an embedder, so refuse to start (fail-fast, like
// validateGatewayKeys).
func validateGatewaySearch(cfg *config.Config, embeddingsOn bool) error {
	if embeddingsOn {
		return nil
	}
	for _, a := range cfg.Agents {
		if a.Gateway == config.GatewaySearch {
			return fmt.Errorf("agent %q has gateway: search but embeddings are not configured (RUNTIME_EMBED_MODEL)", a.ID)
		}
	}
	return nil
}
```

2. In the gateway block (where Manager+Handler are built), construct the Index:

```go
	if cfg.Gateway.Enabled() {
		// ... existing SetGateway/NewManager/NewHandler ...
		emb, _, embOn, eerr := memory.NewEmbedderFromEnv()
		if eerr != nil {
			slog.Error("gateway: embeddings config invalid", "err", eerr)
			os.Exit(1)
		}
		if embOn {
			floor := envFloatOr("RUNTIME_GATEWAY_SEARCH_FLOOR", 0.2)
			k := envIntOr("RUNTIME_GATEWAY_SEARCH_K", 5)
			gwHandler.Index = gateway.NewIndex(emb, floor, k)
			slog.Info("gateway search enabled", "floor", floor, "k", k)
		}
		if err := validateGatewaySearch(cfg, embOn); err != nil {
			slog.Error("gateway search misconfigured", "err", err)
			os.Exit(1)
		}
	}
```

3. Add the small env helpers to cmd/runtimed/main.go (package main has none yet):

```go
// envFloatOr reads a float env var with a default (malformed ⇒ default + warn).
func envFloatOr(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Warn("ignoring malformed env float", "key", key, "value", v, "default", def)
		return def
	}
	return f
}

// envIntOr reads an int env var with a default (malformed ⇒ default + warn).
func envIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("ignoring malformed env int", "key", key, "value", v, "default", def)
		return def
	}
	return n
}
```

Imports: `strconv`, `github.com/sausheong/runtime/internal/memory`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./controlplane/ ./cmd/runtimed/ -count=1 -v && go build ./...`
Expected: ALL PASS.

- [ ] **Step 5: Commit**

```bash
git add controlplane/ cmd/runtimed/
git commit -m "feat(gateway): search-mode spawn wiring + Index assembly + fail-fast validation"
```

---

### Task 5: Through-serve integration test

**Files:**
- Create: `test/gateway_search_e2e_test.go`

Reuses the package's helpers (mustExec, waitURL, invokeOn) and the Task-7-M1 patterns. The fake embedding server makes "search without real API keys" possible: it serves `POST /embeddings` with deterministic vectors (hash-based), and `RUNTIME_EMBED_MODEL/DIM + OPENAI_BASE_URL` point at it.

- [ ] **Step 1: Write the test**

```go
//go:build integration

package test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeEmbedSrv serves an OpenAI-compatible /embeddings endpoint with
// deterministic vectors: dimension 4, derived from the input hash, except
// that texts containing "greet" (the tool) and "say hello" (the query)
// share a fixed direction so they match strongly.
func fakeEmbedSrv(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		// Accept both "input": "str" and ["str"] forms.
		var raw map[string]any
		body, _ := json.Marshal(raw)
		_ = body
		dec := json.NewDecoder(r.Body)
		var generic map[string]any
		if err := dec.Decode(&generic); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		switch v := generic["input"].(type) {
		case string:
			req.Input = []string{v}
		case []any:
			for _, x := range v {
				req.Input = append(req.Input, x.(string))
			}
		}
		type datum struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		var out struct {
			Data []datum `json:"data"`
		}
		for i, text := range req.Input {
			var vec []float32
			lower := strings.ToLower(text)
			if strings.Contains(lower, "greet") || strings.Contains(lower, "say hello") {
				vec = []float32{1, 0, 0, 0}
			} else {
				h := sha256.Sum256([]byte(text))
				vec = []float32{0, float32(h[0])/255 + 0.01, float32(h[1])/255 + 0.01, float32(h[2])/255 + 0.01}
			}
			out.Data = append(out.Data, datum{Embedding: vec, Index: i})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// TestGatewaySearchE2E: runtimed + fake upstream + fake embeddings; an
// external search-mode MCP client lists only search_tools, searches, and
// calls the discovered tool; a gateway:search agent boots and completes a
// turn (proving the ?mode=search URL injection works end-to-end).
func TestGatewaySearchE2E(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	mustExec(t, db, `DROP TABLE IF EXISTS markers`)
	mustExec(t, db, `CREATE TABLE markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`)
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)

	// Fake upstream with two tools, distinct semantics.
	upstream := sdk.NewServer(&sdk.Implementation{Name: "fake-upstream", Version: "v0"}, nil)
	upstream.AddTool(&sdk.Tool{
		Name: "greet", Description: "greets the user with a hello message",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "hello from upstream"}}}, nil
	})
	upstream.AddTool(&sdk.Tool{
		Name: "sum", Description: "adds two numbers together",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "42"}}}, nil
	})
	upSrv := httptest.NewServer(sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return upstream }, nil))
	defer upSrv.Close()

	embSrv := fakeEmbedSrv(t)
	defer embSrv.Close()

	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}

	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: s-agent, name: S, model: test/scripted, listen_addr: 127.0.0.1:8141, gateway: search}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - {name: fake, url: " + upSrv.URL + "}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8140"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
		"RUNTIME_EMBED_MODEL=fake-embed",
		"RUNTIME_EMBED_DIM=4",
		"OPENAI_BASE_URL="+embSrv.URL,
		"OPENAI_API_KEY=fake",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	base := "http://" + ctlAddr
	waitURL(t, base+"/healthz", 15*time.Second)

	// External search-mode client.
	cli := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "v0"}, nil)
	sess, err := cli.Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: base + "/gateway/mcp?mode=search"}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	// tools/list shows ONLY search_tools (retry while the upstream connects;
	// list shows search_tools immediately but search needs the upstream up,
	// so wait for the federated tool to be searchable).
	lt, err := sess.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(lt.Tools) != 1 || lt.Tools[0].Name != "search_tools" {
		t.Fatalf("want [search_tools], got %+v", lt.Tools)
	}

	// Search until the upstream's tools are indexed (bounded retry).
	deadline := time.Now().Add(10 * time.Second)
	var matches []struct {
		Name string `json:"name"`
	}
	for {
		res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
			Name: "search_tools", Arguments: map[string]any{"query": "say hello to someone"},
		})
		if err != nil {
			t.Fatalf("search call: %v", err)
		}
		if !res.IsError {
			txt := res.Content[0].(*sdk.TextContent).Text
			jsonPart, _, _ := strings.Cut(txt, "\n")
			if err := json.Unmarshal([]byte(jsonPart), &matches); err == nil && len(matches) > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("search never returned matches")
		}
		time.Sleep(300 * time.Millisecond)
	}
	if matches[0].Name != "fake__greet" {
		t.Fatalf("want fake__greet top-1, got %+v", matches)
	}

	// Call the discovered (unlisted) tool.
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: "fake__greet"})
	if err != nil {
		t.Fatalf("call discovered tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("discovered tool errored: %+v", res.Content)
	}
	if txt := res.Content[0].(*sdk.TextContent).Text; txt != "hello from upstream" {
		t.Fatalf("wrong result: %q", txt)
	}

	// The gateway:search agent boots and completes a turn (its injected URL
	// carries ?mode=search; BuildRuntime connects per-turn and lists only
	// search_tools — the scripted provider doesn't call it, but the turn
	// completing proves the search-mode endpoint accepted the agent).
	waitURL(t, base+"/agents/s-agent/healthz", 30*time.Second)
	_, body := invokeOn(t, base, "s-agent")
	if !strings.Contains(body, "final answer") {
		t.Fatalf("search-mode agent turn did not complete:\n%s", body)
	}
}
```

CLEANUP NOTE: the fakeEmbedSrv decode block has vestigial lines (`var raw map[string]any` etc.) — implementer: write it cleanly (decode into `map[string]any` once, handle string vs []any input). Check `internal/memory/embed.go` for the EXACT request/response shape httpEmbedder sends/expects (read it first — field names like `input`/`model` and response `data[].embedding`) and match it.

- [ ] **Step 2: Run it**

```bash
go test -tags integration ./test/ -run TestGatewaySearchE2E -v -timeout 180s
```

Iterate until green. Ports 8140/8141 chosen clear of existing tests (8091, 8111/2, 8120, 8130/1, 8211, 8230/1) — re-grep to confirm.

- [ ] **Step 3: Full integration suite**

```bash
go test -tags integration ./test/ -timeout 900s -count=1
```

ALL 15 pass.

- [ ] **Step 4: Commit**

```bash
git add test/
git commit -m "test(gateway): search-mode through-serve e2e with fake embeddings"
```

---

### Task 6: Docs + live proof + ROADMAP

**Files:**
- Modify: `README.md` (search mode subsection in the MCP Gateway section)
- Modify: `runtime.yaml` (extend the commented gateway example)
- Modify: `ROADMAP.md` (Gateway M2 DONE entry + header)

- [ ] **Step 1: runtime.yaml** — in the commented gateway example, change the agent comment line to show both modes:

```yaml
# agents:
#   - id: support
#     gateway: true          # full federated tool list (also: gateway: full)
#   - id: researcher
#     gateway: search        # search mode: lists only search_tools; discover via natural-language query
```

(Adapt to the file's actual commented structure — read it first.)

- [ ] **Step 2: README** — add a "Search mode (M2)" subsection inside the existing MCP Gateway section: what it is (one listed tool, semantic discovery, callable-but-unlisted catalog), how to enable (`gateway: search` per agent; external clients `?mode=search`), requirements (RUNTIME_EMBED_* configured; fail-fast otherwise; HTTP 400 for sessions), tunables (RUNTIME_GATEWAY_SEARCH_FLOOR default 0.2 with the OpenAI-cosine note, RUNTIME_GATEWAY_SEARCH_K default 5 cap 20), search_tools contract (query/k in; JSON matches with schemas out; empty + hint), and limitations (viewer can search not call; lazy embedding — first search after connect pays the embed cost; no persistence).

- [ ] **Step 3: Live proof** (operator-run; same shape as M1's):

```bash
# /tmp/runtime.gateway-search-live.yaml: M1 live config but gateway: search on the agent,
# plus RUNTIME_EMBED_MODEL/DIM + OPENAI_BASE_URL/KEY pointing at the real LiteLLM proxy.
go build -o /tmp/agentd ./cmd/agentd && go build -o /tmp/runtimed ./cmd/runtimed
RUNTIME_CONFIG=/tmp/runtime.gateway-search-live.yaml RUNTIME_AGENTD_BIN=/tmp/agentd \
  RUNTIME_EMBED_MODEL=text-embedding-3-small RUNTIME_EMBED_DIM=1536 /tmp/runtimed
# Other shell: MCP client at /gateway/mcp?mode=search →
#   search_tools("read a file's contents") must surface fs__read_text_file top-1;
#   then call it on a known file. Record top-1 + its cosine score.
```

Record the result (tool surfaced, score, floor sanity) in the ROADMAP entry or commit message.

- [ ] **Step 4: ROADMAP** — header checkpoint → "2026-06-10 (Gateway M2 — semantic tool search)"; current-state sentence extended; §B1 gains a "**Second milestone DONE (merged to `master`, 2026-06-10):** semantic tool search." paragraph in the house style: search-first mode (`gateway: search` / `?mode=search`), one listed tool `search_tools`, embedding cosine over the federated catalog (in-memory content-hash vector cache, `RUNTIME_EMBED_*` reuse, floor 0.2 + K 5 tunables), callable-but-unlisted via `AddReceivingMiddleware` list filter, viewer-may-search, fail-fast misconfig, degrade-don't-fail embed errors; remaining B1 list (drop semantic search from it); spec/plan paths.

- [ ] **Step 5: Final verification + commit**

```bash
go vet ./... && go build ./... && go test ./... -count=1
go test -tags integration ./test/ -timeout 900s -count=1
git add README.md runtime.yaml ROADMAP.md
git commit -m "docs(gateway): search mode docs + ROADMAP through Gateway M2"
```

---

## Completion

After all tasks: final whole-branch review, then **superpowers:finishing-a-development-branch** to merge `feat/gateway-m2` to `master`.
