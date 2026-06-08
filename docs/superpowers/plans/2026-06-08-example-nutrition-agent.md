# Example Nutrition Agent — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deploy the Singapore Nutrition Label Investigator (ported from the Python OpenAI Agents SDK demo) as a harness-native agent running inside the runtime platform's durable spine, reachable through the control plane with image input.

**Architecture:** Port the agent to `examples/nutrition/` (harness `tool.Tool`s + OpenAI provider against the LiteLLM proxy + investigator system prompt, embedded SFA additive data, cross-run JSON memory). Add agent-kind selection so `agentd` can build it (kind registry + `RUNTIME_AGENT_KIND` env + optional `kind:` in `runtime.yaml`). Extend the agent contract's `POST /sessions` and the checkpointed `turnInput` to carry an optional base64 image, plumbed into `RunTurn` (which already accepts images).

**Tech Stack:** Go 1.25, harness (`../harness`), DBOS, Postgres.app, `github.com/sashabaranov/go-openai` (via harness's openai provider).

**Spec:** `docs/superpowers/specs/2026-06-08-example-nutrition-agent-design.md`

**Conventions (read before starting):**
- The `go` CLI is ground truth; ignore IDE/LSP diagnostics (the `replace ../harness` setup confuses gopls). Verify with `go build ./...` / `go test ./...`.
- Integration tests use `//go:build integration` and need Postgres.app at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`. Run: `go test -tags integration ./test/ -count=1 -timeout 300s`.
- Commit with `git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com'`.
- Branch is already `feat/example-nutrition-agent`.

---

## Task 1: Copy SFA additive data + build the additive index

**Files:**
- Create: `examples/nutrition/data/sfa_additives.json` (copy of the original)
- Create: `examples/nutrition/additives.go`
- Test: `examples/nutrition/additives_test.go`

Port the Python additive resolution (`_norm`, `_BY_E`, `_BY_ALIAS`, `COLLOQUIAL`, `CONSUMER_NOTES`, `_resolve_additive`, `_format_entry`) to Go. The JSON entry shape is:
```json
{"ins": "621", "e_number": "621", "name": "Monosodium L- glutamate",
 "name_in_regs": "Monosodium L- glutamate / Mono- sodium salt of L-",
 "schedule": "...", "sfa_notes": "GMP", "table": 1}
```
(`ins` and number fields may be JSON null → use `*string` or `omitempty`/`""`.)

- [ ] **Step 1: Copy the data file**

```bash
mkdir -p examples/nutrition/data
cp ../agents_sdk/openai-demo/sfa_additives.json examples/nutrition/data/sfa_additives.json
```
Expected: file exists, ~111 KB, 541 entries.

- [ ] **Step 2: Write the failing test**

```go
package nutrition

import "testing"

func TestResolveAdditive(t *testing.T) {
	idx := newAdditiveIndex()
	cases := []struct {
		name, query string
		wantFound   bool
		wantInName  string // substring expected in the formatted output
	}{
		{"by e-number", "E621", true, "621"},
		{"by e-number lowercase no prefix", "621", true, "621"},
		{"by name", "Monosodium L- glutamate", true, "621"},
		{"by colloquial msg", "MSG", true, "621"},
		{"by colloquial vitamin c", "vitamin c", true, "ascorbic"},
		{"miss", "unobtainium", false, "Not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := idx.format(idx.resolve(c.query, ""), c.query)
			if c.wantFound && !containsFold(out, "Permitted") {
				t.Errorf("resolve(%q): want permitted, got %q", c.query, out)
			}
			if !c.wantFound && !containsFold(out, "not found") {
				t.Errorf("resolve(%q): want not-found, got %q", c.query, out)
			}
		})
	}
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (indexFold(s, sub) >= 0)
}
```
(Helper `indexFold` can use `strings.Contains(strings.ToLower(s), strings.ToLower(sub))` — write it in the test file.)

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./examples/nutrition/ -run TestResolveAdditive -v`
Expected: FAIL — `newAdditiveIndex` undefined.

- [ ] **Step 4: Implement `additives.go`**

Implement:
- `//go:embed data/sfa_additives.json` into a `[]byte`.
- `type additive struct` with fields `INS, ENumber, Name, NameInRegs, Schedule, SFANotes string` (json tags `ins,e_number,name,name_in_regs,schedule,sfa_notes`; decode nulls to "").
- `type additiveIndex struct { byE, byAlias map[string]additive }`.
- `newAdditiveIndex() *additiveIndex` — unmarshal embedded JSON, build `byE` (index `e_number` and `ins`, both as-is and base via stripping `(...)`) and `byAlias` (normalized `name` and `name_in_regs`, split on `/`).
- `norm(s string) string` — port `_norm`: lower, drop `en:`, drop parentheticals, drop stereo markers `L- DL- L(+)-`, `-`→space, strip non-alnum-space, collapse spaces.
- `var colloquial = map[string]string{...}` and `var consumerNotes = map[string]string{...}` — copy from Python.
- `(idx) resolve(additive, hint string) *additive` — port `_resolve_additive` (learned-alias lookup is added in Task 2; here resolve via byE/byAlias/colloquial only; accept `hint` param now, ignore until Task 2 — actually wire hint: if direct miss and hint non-empty, resolve hint).
- `(idx) format(e *additive, raw string) string` — port `_format_entry` (nil → "not found" text; else "E<num> (<name>): Permitted by SFA under <schedule>. <consumer note>").

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./examples/nutrition/ -run TestResolveAdditive -v`
Expected: PASS (all subtests).

- [ ] **Step 6: Commit**

```bash
git add examples/nutrition/data/sfa_additives.json examples/nutrition/additives.go examples/nutrition/additives_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(nutrition): SFA additive index ported from the Python demo"
```

---

## Task 2: Cross-run memory with alias learning

**Files:**
- Create: `examples/nutrition/memory.go`
- Test: `examples/nutrition/memory_test.go`

Port the JSON memory (`learned_aliases`, `products`) and wire learned-alias resolution into the index.

- [ ] **Step 1: Write the failing test**

```go
package nutrition

import (
	"path/filepath"
	"testing"
)

func TestMemoryLearnsAlias(t *testing.T) {
	dir := t.TempDir()
	m := newMemory(filepath.Join(dir, "mem.json"))
	// First: an unknown name with a hint number → learns, resolves.
	if got := m.learnedAlias("frobnicate stabiliser"); got != "" {
		t.Fatalf("expected no prior alias, got %q", got)
	}
	m.learnAlias("frobnicate stabiliser", "500")
	if got := m.learnedAlias("frobnicate stabiliser"); got != "500" {
		t.Errorf("alias not learned: got %q want 500", got)
	}
	// Persisted: a fresh memory over the same file sees it.
	m2 := newMemory(filepath.Join(dir, "mem.json"))
	if got := m2.learnedAlias("frobnicate stabiliser"); got != "500" {
		t.Errorf("alias not persisted: got %q want 500", got)
	}
}

func TestRecallProduct(t *testing.T) {
	dir := t.TempDir()
	m := newMemory(filepath.Join(dir, "mem.json"))
	if rec, ok := m.recall("Milo"); ok {
		t.Fatalf("unexpected prior record: %+v", rec)
	}
	m.remember(productRecord{ProductName: "Milo", Summary: "ok", Recommendation: "fine"})
	if rec, ok := m.recall("milo"); !ok || rec.Summary != "ok" {
		t.Errorf("recall failed: %+v ok=%v", rec, ok)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./examples/nutrition/ -run 'TestMemory|TestRecall' -v`
Expected: FAIL — `newMemory` undefined.

- [ ] **Step 3: Implement `memory.go`**

```go
package nutrition

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
)

type productRecord struct {
	ProductName    string `json:"product_name"`
	Summary        string `json:"summary"`
	Recommendation string `json:"recommendation"`
}

type memory struct {
	path string
	mu   sync.Mutex
	LearnedAliases map[string]string        `json:"learned_aliases"`
	Products       map[string]productRecord `json:"products"`
}

func newMemory(path string) *memory {
	m := &memory{path: path, LearnedAliases: map[string]string{}, Products: map[string]productRecord{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, m) // best-effort; empty on parse error
		if m.LearnedAliases == nil { m.LearnedAliases = map[string]string{} }
		if m.Products == nil { m.Products = map[string]productRecord{} }
	}
	return m
}

func (m *memory) save() {
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(m.path, b, 0o644)
}

func (m *memory) learnedAlias(normName string) string {
	m.mu.Lock(); defer m.mu.Unlock()
	return m.LearnedAliases[normName]
}

func (m *memory) learnAlias(normName, number string) {
	m.mu.Lock()
	m.LearnedAliases[normName] = number
	m.mu.Unlock()
	m.save()
}

func (m *memory) recall(productName string) (productRecord, bool) {
	m.mu.Lock(); defer m.mu.Unlock()
	rec, ok := m.Products[strings.ToLower(strings.TrimSpace(productName))]
	return rec, ok
}

func (m *memory) remember(rec productRecord) {
	m.mu.Lock()
	m.Products[strings.ToLower(strings.TrimSpace(rec.ProductName))] = rec
	m.mu.Unlock()
	m.save()
}
```
Note: `MarshalIndent` on `*memory` will include `path`/`mu`? No — unexported fields are not marshaled by encoding/json. Good. (Only exported `LearnedAliases`/`Products` serialize.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./examples/nutrition/ -run 'TestMemory|TestRecall' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add examples/nutrition/memory.go examples/nutrition/memory_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(nutrition): cross-run JSON memory with alias learning + product recall"
```

---

## Task 3: The four harness tools

**Files:**
- Create: `examples/nutrition/tools.go`
- Test: `examples/nutrition/tools_test.go`

Implement four `tool.Tool`s wired to the index + memory. `check_sfa_additive` learns aliases (so it's the integration point of Tasks 1+2). HCS uses a small `httpDoer` interface so the test can stub it.

- [ ] **Step 1: Write the failing test**

```go
package nutrition

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func newTestTools(t *testing.T) *tools {
	t.Helper()
	return newTools(newAdditiveIndex(), newMemory(filepath.Join(t.TempDir(), "m.json")), nil)
}

func TestNutriGradeBands(t *testing.T) {
	tl := newTestTools(t)
	cases := []struct{ sugar, sat float64; want string }{
		{0.5, 0.5, "A"}, {3, 1.0, "B"}, {8, 2.0, "C"}, {15, 5, "D"},
	}
	for _, c := range cases {
		in, _ := json.Marshal(map[string]float64{"sugar_per_100ml": c.sugar, "saturated_fat_per_100ml": c.sat})
		res, err := tl.nutriGrade().Execute(context.Background(), in)
		if err != nil { t.Fatal(err) }
		if !strings.Contains(res.Output, "Nutri-Grade: "+c.want) {
			t.Errorf("sugar=%v sat=%v: want grade %s, got %q", c.sugar, c.sat, c.want, res.Output)
		}
	}
}

func TestCheckAdditiveLearnsFromHint(t *testing.T) {
	idx := newAdditiveIndex()
	mem := newMemory(filepath.Join(t.TempDir(), "m.json"))
	tl := newTools(idx, mem, nil)
	// A name the table doesn't know, with a known E-number hint, learns the alias.
	in, _ := json.Marshal(map[string]string{"additive": "frobnicate gum", "e_number_hint": "415"})
	res, _ := tl.checkAdditive().Execute(context.Background(), in)
	if !strings.Contains(strings.ToLower(res.Output), "permitted") {
		t.Fatalf("hint did not resolve: %q", res.Output)
	}
	if mem.learnedAlias(norm("frobnicate gum")) == "" {
		t.Error("alias was not learned from hint")
	}
}

func TestRecallProductTool(t *testing.T) {
	tl := newTestTools(t)
	in, _ := json.Marshal(map[string]string{"product_name": "Nothing"})
	res, _ := tl.recallProduct().Execute(context.Background(), in)
	if !strings.Contains(res.Output, "first investigation") {
		t.Errorf("want first-investigation, got %q", res.Output)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./examples/nutrition/ -run 'TestNutriGrade|TestCheckAdditive|TestRecallProductTool' -v`
Expected: FAIL — `newTools` undefined.

- [ ] **Step 3: Implement `tools.go`**

Define `type httpDoer interface { Do(*http.Request) (*http.Response, error) }`. `type tools struct { idx *additiveIndex; mem *memory; http httpDoer }`. `newTools(idx, mem, h)` sets `http` to `http.DefaultClient` when `h == nil`.

Implement four small `tool.Tool` structs (or one generic adapter) returned by methods `checkAdditive()`, `recallProduct()`, `checkHCS()`, `nutriGrade()`. Each:
- `Name()/Description()/Parameters()` (JSON Schema matching the Python docstrings/args).
- `Execute` ports the Python body.
- `IsConcurrencySafe`: `checkAdditive` → **false** (writes learned alias), others → **true**.

For `check_sfa_additive`: resolve via `idx.resolve(additive, "")`; if nil and hint set, `idx.resolve(hint,"")`; on hint hit, `mem.learnAlias(norm(additive), baseNumber)`. ALSO: `idx.resolve` must consult `mem` learned aliases first — pass `mem` into the index OR have the tool check `mem.learnedAlias(norm(additive))` and resolve that number via `idx`. Choose the tool-checks-mem approach to keep the index pure:
```go
func (t *tools) resolveWithMemory(additive, hint string) *additive {
	if num := t.mem.learnedAlias(norm(additive)); num != "" {
		if e := t.idx.resolve(num, ""); e != nil { return e }
	}
	if e := t.idx.resolve(additive, ""); e != nil { return e }
	if hint != "" { return t.idx.resolve(hint, "") }
	return nil
}
```

For `check_hcs`: GET `https://data.gov.sg/api/action/datastore_search?resource_id=d_6725eed000bf5b3c5d310eb08de0851f&q=<name>&limit=5` via `t.http`; parse records; return certified/not-found text. Network error → graceful message (never error the turn): return `tool.ToolResult{Output: "HCS check failed: ..."}`.

JSON number parsing for nutri-grade: accept floats.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./examples/nutrition/ -run 'TestNutriGrade|TestCheckAdditive|TestRecallProductTool' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add examples/nutrition/tools.go examples/nutrition/tools_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(nutrition): four harness tools (additive/recall/hcs/nutri-grade)"
```

---

## Task 4: Agent config builder + system prompt

**Files:**
- Create: `examples/nutrition/prompt.go`
- Create: `examples/nutrition/agent.go`
- Test: `examples/nutrition/agent_test.go`

`BuildConfig` assembles the `agentruntime.Config`: openai provider against the proxy, the 4 tools, the investigator prompt. It does NOT open Postgres/serve — it returns Config for `agentd` to serve.

- [ ] **Step 1: Write `prompt.go`**

```go
package nutrition

// investigatorPrompt is the system prompt, ported from the Python INSTRUCTIONS.
const investigatorPrompt = `You are a Singapore food label investigator. ...`
```
Copy the full Python `INSTRUCTIONS` text (the 0–5 numbered procedure), adapting only the output instruction: instead of "Return a NutritionVerdict" (typed), say "Return your verdict as prose: start with your step-by-step reasoning, then a one-line summary, then findings grouped under GREEN / AMBER / RED (each citing the tool or 'label' that produced it), then a final recommendation." Note input may be a photo OR pasted label text.

- [ ] **Step 2: Write the failing test**

```go
package nutrition

import (
	"os"
	"testing"
)

func TestBuildConfigRequiresKey(t *testing.T) {
	os.Unsetenv("OPENAI_API_KEY")
	if _, err := BuildConfig(Deps{AgentID: "nutrition", ListenAddr: "127.0.0.1:0", PostgresDSN: "x"}); err == nil {
		t.Fatal("expected error when OPENAI_API_KEY unset")
	}
}

func TestBuildConfigOK(t *testing.T) {
	os.Setenv("OPENAI_API_KEY", "test-key")
	os.Setenv("OPENAI_BASE_URL", "https://example.invalid")
	os.Setenv("OPENAI_MODEL", "gpt-5.4")
	defer func() { os.Unsetenv("OPENAI_API_KEY"); os.Unsetenv("OPENAI_BASE_URL"); os.Unsetenv("OPENAI_MODEL") }()
	cfg, err := BuildConfig(Deps{AgentID: "nutrition", ListenAddr: "127.0.0.1:9999", PostgresDSN: "dsn"})
	if err != nil { t.Fatal(err) }
	if cfg.Spec.ID != "nutrition" || cfg.Provider == nil || cfg.Tools == nil { t.Fatalf("bad config: %+v", cfg) }
	if cfg.Spec.SystemPrompt == "" { t.Error("missing system prompt") }
	if len(cfg.Tools.Names()) != 4 { t.Errorf("want 4 tools, got %v", cfg.Tools.Names()) }
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./examples/nutrition/ -run TestBuildConfig -v`
Expected: FAIL — `BuildConfig`/`Deps` undefined.

- [ ] **Step 4: Implement `agent.go`**

```go
package nutrition

import (
	"errors"
	"os"
	"path/filepath"

	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"
	"github.com/sausheong/harness/providers/openai"

	"github.com/sausheong/runtime/agentruntime"
)

// Deps is what agentd hands the builder.
type Deps struct {
	AgentID     string
	ListenAddr  string
	PostgresDSN string
}

func BuildConfig(d Deps) (agentruntime.Config, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return agentruntime.Config{}, errors.New("nutrition: OPENAI_API_KEY is required")
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	model := os.Getenv("OPENAI_MODEL")
	if model == "" { model = "gpt-4o" }

	dataDir := os.Getenv("RUNTIME_NUTRITION_DATA_DIR")
	if dataDir == "" { dataDir = "." }

	idx := newAdditiveIndex()
	mem := newMemory(filepath.Join(dataDir, "nutrition_memory.json"))
	tl := newTools(idx, mem, nil)

	reg := tool.NewRegistry()
	reg.Register(tl.checkAdditive())
	reg.Register(tl.recallProduct())
	reg.Register(tl.checkHCS())
	reg.Register(tl.nutriGrade())

	provider := openai.NewOpenAIProviderWithKind(key, baseURL, "openai-compatible")

	return agentruntime.Config{
		Spec: hrt.AgentSpec{
			ID:           d.AgentID,
			Name:         "SG Nutrition Investigator",
			Model:        "openai/" + model,
			SystemPrompt: investigatorPrompt,
			MaxTurns:     12,
		},
		Provider:    provider,
		Tools:       reg,
		ListenAddr:  d.ListenAddr,
		PostgresDSN: d.PostgresDSN,
	}, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./examples/nutrition/ -run TestBuildConfig -v`
Expected: PASS.

- [ ] **Step 6: Run the whole package + build**

Run: `go test ./examples/nutrition/ && go build ./...`
Expected: ok.

- [ ] **Step 7: Commit**

```bash
git add examples/nutrition/prompt.go examples/nutrition/agent.go examples/nutrition/agent_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(nutrition): BuildConfig + investigator system prompt"
```

---

## Task 5: Agent-kind registry

**Files:**
- Create: `internal/agentkind/registry.go`
- Test: `internal/agentkind/registry_test.go`

A kind→builder map so `agentd` can build either the test agent or the nutrition agent. The test agent's builder needs a `*sql.DB` (marker tool); nutrition does not — so `Deps` carries an optional `DB`.

- [ ] **Step 1: Write the failing test**

```go
package agentkind

import "testing"

func TestGetKnownKinds(t *testing.T) {
	for _, k := range []string{"", "testagent", "nutrition"} {
		if _, ok := Get(k); !ok {
			t.Errorf("kind %q: expected a builder", k)
		}
	}
	if _, ok := Get("does-not-exist"); ok {
		t.Error("unknown kind should not resolve")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agentkind/ -v`
Expected: FAIL — package/`Get` undefined.

- [ ] **Step 3: Implement `registry.go`**

```go
// Package agentkind maps an agent "kind" string to a builder that produces an
// agentruntime.Config. Keeps cmd/agentd thin and the mapping unit-testable.
package agentkind

import (
	"database/sql"

	"github.com/sausheong/harness/tool"
	hrt "github.com/sausheong/harness/runtime"

	"github.com/sausheong/runtime/agentruntime"
	"github.com/sausheong/runtime/examples/nutrition"
	"github.com/sausheong/runtime/testagent"
)

// Deps carries everything any builder might need. DB is non-nil only when the
// caller opened Postgres (the test agent's marker tool needs it).
type Deps struct {
	AgentID     string
	ListenAddr  string
	PostgresDSN string
	DB          *sql.DB
}

// Builder turns Deps into a serveable Config.
type Builder func(Deps) (agentruntime.Config, error)

var builders = map[string]Builder{
	"":          buildTestAgent, // default
	"testagent": buildTestAgent,
	"nutrition": buildNutrition,
}

func Get(kind string) (Builder, bool) {
	b, ok := builders[kind]
	return b, ok
}

func buildNutrition(d Deps) (agentruntime.Config, error) {
	return nutrition.BuildConfig(nutrition.Deps{
		AgentID: d.AgentID, ListenAddr: d.ListenAddr, PostgresDSN: d.PostgresDSN,
	})
}

func buildTestAgent(d Deps) (agentruntime.Config, error) {
	reg := tool.NewRegistry()
	reg.Register(testagent.MarkerTool{DB: d.DB})
	return agentruntime.Config{
		Spec: hrt.AgentSpec{
			ID: d.AgentID, Name: d.AgentID, Model: "test/scripted", MaxTurns: 10,
		},
		Provider:    testagent.New(),
		Tools:       reg,
		ListenAddr:  d.ListenAddr,
		PostgresDSN: d.PostgresDSN,
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agentkind/ -v && go build ./...`
Expected: PASS + build ok.

- [ ] **Step 5: Commit**

```bash
git add internal/agentkind/
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(agentkind): kind→builder registry (testagent | nutrition)"
```

---

## Task 6: Wire kind selection through agentd + config + control plane

**Files:**
- Modify: `cmd/agentd/main.go`
- Modify: `internal/config/config.go:11-17` (AgentConfig) and `internal/config/config.go:54-66` (Validate loop — no change needed beyond field)
- Modify: `controlplane/proxy.go:12-18` (AgentProcess) and `controlplane/proxy.go:22-29` (SpawnFunc env)
- Modify: `controlplane/registry.go:26` (pass Kind)
- Test: `internal/config/config_test.go` (add a kind round-trip case)

- [ ] **Step 1: Add `Kind` to AgentConfig**

In `internal/config/config.go`, add to `AgentConfig`:
```go
	Kind       string `yaml:"kind"` // optional; "" ⇒ testagent. Resolved by agentd's kind registry.
```
No `Validate` change (empty is valid; unknown kinds are caught at agentd build).

- [ ] **Step 2: Add a config test for kind**

In `internal/config/config_test.go`, add:
```go
func TestLoadKind(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("agents:\n  - {id: n, name: N, model: openai/gpt, kind: nutrition, listen_addr: 127.0.0.1:8201}\n"), 0o644)
	c, err := Load(p)
	if err != nil { t.Fatal(err) }
	if c.Agents[0].Kind != "nutrition" { t.Errorf("kind not parsed: %q", c.Agents[0].Kind) }
}
```
(Ensure `filepath`/`os` imports exist in the test file; add if missing.)

Run: `go test ./internal/config/ -run TestLoadKind -v` → expect FAIL first (field missing if Step 1 skipped), then PASS after Step 1.

- [ ] **Step 3: Thread Kind through AgentProcess + SpawnFunc**

In `controlplane/proxy.go`, add `Kind string` to `AgentProcess`, and in `SpawnFunc` add to `cmd.Env`:
```go
			"RUNTIME_AGENT_KIND="+a.Kind,
```

- [ ] **Step 4: Pass Kind in NewRegistry**

In `controlplane/registry.go`, change the agent construction to:
```go
		r.agents[a.ID] = AgentProcess{AgentID: a.ID, Addr: a.ListenAddr, BinPath: binPath, PGDSN: dsn, Kind: a.Kind}
```

- [ ] **Step 5: Rewrite `cmd/agentd/main.go` to use the kind registry**

Replace the hardcoded testagent wiring with:
```go
func main() {
	dsn := mustEnv("RUNTIME_PG_DSN")
	addr := mustEnv("RUNTIME_LISTEN_ADDR")
	agentID := mustEnv("RUNTIME_AGENT_ID")
	kind := os.Getenv("RUNTIME_AGENT_KIND") // "" ⇒ testagent

	db, err := sql.Open("pgx", dsn)
	if err != nil { log.Fatalf("agentd: open postgres: %v", err) }
	defer db.Close()

	// Marker table for the test agent (under the shared DDL lock). Harmless for
	// other kinds; kept so the testagent kind needs no special-casing here.
	if err := store.ApplyDDLLocked(context.Background(), db,
		`CREATE TABLE IF NOT EXISTS markers (id BIGSERIAL PRIMARY KEY, ran_at TIMESTAMPTZ)`,
	); err != nil {
		log.Fatalf("agentd: create markers table: %v", err)
	}

	build, ok := agentkind.Get(kind)
	if !ok { log.Fatalf("agentd: unknown agent kind %q", kind) }
	cfg, err := build(agentkind.Deps{AgentID: agentID, ListenAddr: addr, PostgresDSN: dsn, DB: db})
	if err != nil { log.Fatalf("agentd: build agent kind %q: %v", kind, err) }

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := agentruntime.Serve(ctx, cfg); err != nil {
		log.Fatalf("agentd: serve: %v", err)
	}
}
```
Update imports: drop `harness/tool`, `harness/runtime` (hrt), `testagent`; add `github.com/sausheong/runtime/internal/agentkind`. Keep `store`, `agentruntime`, `os`, `sql`, pgx.

- [ ] **Step 6: Build + run the existing integration suite (no regressions)**

Run: `go build ./... && go test ./...`
Then: `go test -tags integration ./test/ -count=1 -timeout 300s`
Expected: all existing integration tests (resume, multiagent, operability) still PASS — they use the default ("" → testagent) kind, which now flows through the registry.

- [ ] **Step 7: Commit**

```bash
git add cmd/agentd/main.go internal/config/config.go internal/config/config_test.go controlplane/proxy.go controlplane/registry.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat: agent-kind selection through agentd/config/control-plane"
```

---

## Task 7: Image input on the agent contract

**Files:**
- Modify: `agentruntime/turnstep.go:20-23` (turnInput)
- Modify: `agentruntime/server.go:21-35` (POST /sessions)
- Modify: `agentruntime/serve.go:85-164` (sessionWorkflow: decode + pass image), `serve.go:168-177` (startSession signature)
- Test: `agentruntime/turnstep_test.go` (turnInput JSON round-trip with image), `agentruntime/server_test.go` (POST accepts image)

- [ ] **Step 1: Write the failing turnInput round-trip test**

In `agentruntime/turnstep_test.go` add:
```go
func TestTurnInputImageRoundTrip(t *testing.T) {
	in := turnInput{UserMsg: "hi", ImageB64: "QUJD", ImageMime: "image/png"}
	b, _ := json.Marshal(in)
	var out turnInput
	if err := json.Unmarshal(b, &out); err != nil { t.Fatal(err) }
	if out.ImageB64 != "QUJD" || out.ImageMime != "image/png" || out.UserMsg != "hi" {
		t.Fatalf("round-trip lost fields: %+v", out)
	}
}
```
Run: `go test ./agentruntime/ -run TestTurnInputImageRoundTrip` → FAIL (fields missing).

- [ ] **Step 2: Add image fields to turnInput**

In `agentruntime/turnstep.go`:
```go
type turnInput struct {
	UserMsg   string `json:"user_msg"`             // non-empty only on the first turn
	ImageB64  string `json:"image_b64,omitempty"`  // optional base64 image, first turn only
	ImageMime string `json:"image_mime,omitempty"` // defaults to image/jpeg when ImageB64 set
}
```
Run the Step 1 test → PASS.

- [ ] **Step 3: startSession + workflow accept the image**

In `agentruntime/serve.go`, change `startSession` to accept the image and pass it into `turnInput`:
```go
func (m *Manager) startSession(ctx context.Context, userMsg, imageB64, imageMime string) (string, error) {
	sessionID, err := m.st.CreateSession(ctx, m.agentID)
	if err != nil { return "", err }
	in := turnInput{UserMsg: userMsg, ImageB64: imageB64, ImageMime: imageMime}
	if _, err := dbos.RunWorkflow(m.dbosCtx, m.sessionWorkflow, in, dbos.WithWorkflowID(sessionID)); err != nil {
		return "", err
	}
	return sessionID, nil
}
```
In `sessionWorkflow`, decode the image for the FIRST turn only. Add before the loop:
```go
	var firstImages []llm.ImageContent
	if in.ImageB64 != "" {
		if raw, err := base64.StdEncoding.DecodeString(in.ImageB64); err == nil {
			mime := in.ImageMime
			if mime == "" { mime = "image/jpeg" }
			firstImages = []llm.ImageContent{{MimeType: mime, Data: raw}}
		} else {
			slog.Warn("session image decode failed; proceeding text-only", "session", wfID, "err", err)
		}
	}
```
Inside the turn step closure, pass images on the first turn only. Replace the `RunTurn` call:
```go
			var images []llm.ImageContent
			if turn == 0 { images = firstImages }
			tr, terr := rt.RunTurn(stepCtx, userMsg, images, nil)
```
Add imports to serve.go: `"encoding/base64"`, `"github.com/sausheong/harness/llm"`. `slog` is already imported.

Note on replay-safety: `firstImages` is derived from `in` (the checkpointed workflow input), and only applied when `turn == 0`. On recovery DBOS re-supplies `in`, so the re-driven first turn sees the same image — deterministic. Continuation turns never carry it.

- [ ] **Step 4: POST /sessions accepts the image**

In `agentruntime/server.go`, change the `POST /sessions` handler body struct + call:
```go
		var body struct {
			Message   string `json:"message"`
			ImageB64  string `json:"image_b64"`
			ImageMime string `json:"image_mime"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, err := m.startSession(r.Context(), body.Message, body.ImageB64, body.ImageMime)
```

- [ ] **Step 5: Add a server test that POST accepts an image**

In `agentruntime/server_test.go` (or wherever the mux is tested), add a test that POSTs `{"message":"hi","image_b64":"QUJD","image_mime":"image/png"}` and asserts a 200 + non-empty session_id. If the existing server tests need a Manager with a memstore + stub provider, follow the pattern already in that file. If `server_test.go` doesn't already construct a Manager, place this assertion in the integration test (Task 8) instead and note here that the unit-level coverage is the turnInput round-trip (Step 1).

Run: `go test ./agentruntime/ -v`
Expected: PASS (all, including existing).

- [ ] **Step 6: Build + full hermetic tests**

Run: `go build ./... && go test ./...`
Expected: ok.

- [ ] **Step 7: Commit**

```bash
git add agentruntime/turnstep.go agentruntime/serve.go agentruntime/server.go agentruntime/turnstep_test.go agentruntime/server_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "feat(agentruntime): optional image input on POST /sessions (durable, replay-safe)"
```

---

## Task 8: Integration test — boot the nutrition agent through the durable loop with an image

**Files:**
- Create: `test/nutrition_test.go` (`//go:build integration`)

This test must NOT hit the network. It boots `agentruntime.Serve` directly (in-process) with a **deterministic scripted provider** that emits one tool call (`recall_product`) then a final text, so it exercises the real durable loop + tool dispatch + the new image plumbing without a real LLM. (`test/` already has `dsn` + `mustExec` in resume_test.go.)

- [ ] **Step 1: Write the test**

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/agentruntime"
)

// scripted is a network-free provider: first turn → one tool call; then → text.
type scripted struct{}
func (scripted) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	ch := make(chan llm.ChatEvent, 4)
	hasResult := false
	for _, m := range req.Messages { if m.ToolCallID != "" { hasResult = true } }
	go func() {
		defer close(ch)
		if hasResult {
			ch <- llm.ChatEvent{Type: llm.EventTextDelta, Text: "VERDICT: GREEN ok"}
			ch <- llm.ChatEvent{Type: llm.EventDone, Usage: &llm.Usage{InputTokens: 1, OutputTokens: 1}}
			return
		}
		tc := &llm.ToolCall{ID: "c1", Name: "recall_product", Input: []byte(`{"product_name":"Test"}`)}
		ch <- llm.ChatEvent{Type: llm.EventToolCallStart, ToolCall: tc}
		ch <- llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: tc}
		ch <- llm.ChatEvent{Type: llm.EventDone, Usage: &llm.Usage{InputTokens: 1, OutputTokens: 1}}
	}()
	return ch, nil
}
func (scripted) Models() []llm.ModelInfo { return []llm.ModelInfo{{ID: "scripted"}} }
func (scripted) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) { return t, nil }

// recallTool is a tiny stand-in matching the tool the scripted provider calls.
type recallTool struct{}
func (recallTool) Name() string { return "recall_product" }
func (recallTool) Description() string { return "recall" }
func (recallTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object","properties":{"product_name":{"type":"string"}}}`) }
func (recallTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (recallTool) Execute(context.Context, json.RawMessage) (tool.ToolResult, error) {
	return tool.ToolResult{Output: "first investigation"}, nil
}

func TestNutritionAgentImageSession(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil { t.Fatal(err) }
	defer db.Close()
	if err := db.Ping(); err != nil { t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err) }

	// Clean slate.
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)

	reg := tool.NewRegistry()
	reg.Register(recallTool{})

	addr := "127.0.0.1:8211"
	cfg := agentruntime.Config{
		Spec:        hrt.AgentSpec{ID: "nutrition", Name: "Nutrition", Model: "openai/scripted", MaxTurns: 5},
		Provider:    scripted{},
		Tools:       reg,
		ListenAddr:  addr,
		PostgresDSN: dsn,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agentruntime.Serve(ctx, cfg) }()

	base := "http://" + addr
	waitURL(t, base+"/healthz", 20*time.Second)

	// POST a session WITH an image (base64 of a tiny payload), then stream to done.
	img := base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xff\xe0fake-jpeg"))
	body, _ := json.Marshal(map[string]string{
		"message": "Investigate this label.", "image_b64": img, "image_mime": "image/jpeg",
	})
	resp, err := http.Post(base+"/sessions", "application/json", strings.NewReader(string(body)))
	if err != nil { t.Fatal(err) }
	var out struct{ SessionID string `json:"session_id"` }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.SessionID == "" { t.Fatal("no session id") }

	final := streamURL(t, base+"/sessions/"+out.SessionID+"/stream?since=0", 30*time.Second)
	if !strings.Contains(final, `"type":"done"`) {
		t.Fatalf("session did not complete:\n%s", final)
	}
	if !strings.Contains(final, "GREEN") {
		t.Fatalf("expected verdict text in stream:\n%s", final)
	}
}
```
Note: `waitURL` and `streamURL` already exist in `test/multiagent_test.go` (same package). Reuse them.

- [ ] **Step 2: Run the integration test**

Run: `go test -tags integration ./test/ -run TestNutritionAgentImageSession -count=1 -timeout 120s -v`
Expected: PASS — health ok, session created with image, stream reaches `done` with "GREEN".

- [ ] **Step 3: Run the FULL integration suite (no regressions)**

Run: `go test -tags integration ./test/ -count=1 -timeout 300s`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add test/nutrition_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "test(integration): boot nutrition agent + image session through durable loop"
```

---

## Task 9: Live smoke test (optional, skips without key)

**Files:**
- Create: `examples/nutrition/live_test.go` (`//go:build live`)

- [ ] **Step 1: Write the live test**

```go
//go:build live

package nutrition

import (
	"context"
	"os"
	"testing"

	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/session"
)

// TestLiveOneTurn drives a single real turn against the configured proxy.
// Skips unless OPENAI_API_KEY is set. Run:
//   OPENAI_API_KEY=... OPENAI_BASE_URL=https://litellm-stg.aip.gov.sg OPENAI_MODEL=gpt-5.4 \
//     go test -tags live ./examples/nutrition/ -run TestLiveOneTurn -v
func TestLiveOneTurn(t *testing.T) {
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY not set; skipping live smoke")
	}
	cfg, err := BuildConfig(Deps{AgentID: "nutrition", ListenAddr: "127.0.0.1:0", PostgresDSN: "unused"})
	if err != nil { t.Fatal(err) }

	sess := session.NewSession("nutrition", "live-1")
	rt, err := hrt.BuildRuntime(hrt.RuntimeDeps{}, hrt.RuntimeInputs{
		Provider: cfg.Provider, Tools: cfg.Tools, Session: sess,
	}, cfg.Spec)
	if err != nil { t.Fatal(err) }

	msg := "Investigate this label (text): Product: Test Soda. Ingredients: water, sugar, E211, soy lecithin. Sugar 11g/100ml, sat fat 0g/100ml. It is a beverage."
	res, err := rt.RunTurn(context.Background(), msg, nil, nil)
	if err != nil { t.Fatal(err) }
	t.Logf("turn done=%v reason=%s entries=%d", res.Done, res.StopReason, len(res.Entries))
	if res.StopReason == "error" { t.Fatalf("turn errored: %v", res.Err) }
}
```

- [ ] **Step 2: Verify it builds (live tag) and skips cleanly without a key**

Run: `go vet -tags live ./examples/nutrition/` then `go test -tags live ./examples/nutrition/ -run TestLiveOneTurn -v`
Expected: builds; SKIP (no key) — or PASS if a key happens to be set.

- [ ] **Step 3: Commit**

```bash
git add examples/nutrition/live_test.go
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "test(live): nutrition one-turn smoke against the proxy (skips without key)"
```

---

## Task 10: Deployment config + README documentation

**Files:**
- Create: `runtime.nutrition.yaml`
- Modify: `README.md` (add a "Deploying an example agent" section)

- [ ] **Step 1: Write `runtime.nutrition.yaml`**

```yaml
# Single-agent registry for the SG Nutrition Investigator example.
# Run with: RUNTIME_CONFIG=runtime.nutrition.yaml ./runtimed
# Requires env: OPENAI_API_KEY, OPENAI_BASE_URL, OPENAI_MODEL, RUNTIME_PG_DSN.
agents:
  - id: nutrition
    name: SG Nutrition Investigator
    model: openai/gpt-5.4
    kind: nutrition
    listen_addr: 127.0.0.1:8201
```

- [ ] **Step 2: Add the README section**

Add a top-level section "## Deploying an example agent (SG Nutrition Investigator)" to `README.md` documenting the full path from the spec §7:
1. What the agent is (ported from the OpenAI Agents SDK demo; 4 tools; SFA data; memory; vision via image input).
2. Build the three binaries.
3. The `runtime.nutrition.yaml` (show it).
4. Required env (proxy key/base/model + PG DSN + RUNTIME_CONFIG + RUNTIME_AGENTD_BIN if needed + RUNTIME_NUTRITION_DATA_DIR for the memory file).
5. Run `runtimed`.
6. Invoke **text**: `runtimectl invoke --agent nutrition "Investigate: <label text>"`.
7. Invoke **image** with the curl one-liner (base64 a photo into `image_b64`), then stream.
8. Observe in `/ui` and `runtimectl sessions --agent nutrition`.
9. A "How this maps to the platform" note: agent = supervised subprocess; each session = a durable DBOS workflow (survives restart); image is part of the checkpointed input; new agents are added by writing a kind builder + a `kind:` line.

Also add a short "### Adding your own agent kind" subsection: implement a `Builder` in `internal/agentkind`, register it in the `builders` map, set `kind:` in the config.

- [ ] **Step 3: Verify docs reference real commands**

Re-read the section; confirm every command/flag matches what exists (`runtimectl invoke|sessions|logs --agent`, the `/agents/{id}/sessions` routes, env var names). Fix any drift.

- [ ] **Step 4: Final full build + hermetic tests**

Run: `go build ./... && go test ./...`
Expected: ok.

- [ ] **Step 5: Commit**

```bash
git add runtime.nutrition.yaml README.md
git -c user.name='sausheong' -c user.email='sausheong@users.noreply.github.com' commit -m "docs: deploy the nutrition example agent + runtime.nutrition.yaml"
```

---

## Final verification (after all tasks)

- [ ] `go build ./...` — clean
- [ ] `go test ./...` — hermetic suite green (includes nutrition unit tests, agentkind, config, agentruntime)
- [ ] `go test -tags integration ./test/ -count=1 -timeout 300s` — resume + multiagent + operability + **nutrition** all green
- [ ] `go vet ./...` — clean
- [ ] Optional, if a proxy key is available: `OPENAI_API_KEY=... OPENAI_BASE_URL=https://litellm-stg.aip.gov.sg OPENAI_MODEL=gpt-5.4 go test -tags live ./examples/nutrition/ -v` — real verdict
- [ ] Dispatch a final code reviewer over the whole branch, then finish the branch (merge to master).
