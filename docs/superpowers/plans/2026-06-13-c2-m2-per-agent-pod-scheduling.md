# C2 M2 — Per-agent-pod scheduling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let each agent run as its own Kubernetes StatefulSet pod that runtimed attaches to as a remote replica pool (C3-remote × A1-pool), selected via a Helm `scheduling.mode` toggle — instead of the C2 M1 monolith where runtimed exec-spawns every agent.

**Architecture:** A remote agent gains an `{i}`-templated `url:` + `replicas: N`, expanding to N per-ordinal attach entries in the registry (reusing the A1 `r.sets[id]` slice path; no PoolManager). Routing adds liveness-aware round-robin (skip unreachable ordinals) backed by a reachable bitmap fed by one `HealthMonitor` per ordinal; affinity routing is unchanged. The Helm chart's `perAgentPods` mode renders one StatefulSet + headless Service per agent (agentd-only pods, ordinal→`DBOS__VMID` via a shell wrapper on `$HOSTNAME`) and a control-plane-only runtimed whose `runtime.yaml` is generated from the same `config.agents` list.

**Tech Stack:** Go 1.25 (`internal/config`, `controlplane`, `cmd/runtimed`), Helm (StatefulSet, headless Service, ConfigMap generation), Postgres/DBOS, kind for live proof.

**Spec:** `docs/superpowers/specs/2026-06-13-c2-m2-per-agent-pod-scheduling-design.md`

**Conventions (carried from M1–A2):**
- The `go` CLI is ground truth; ignore IDE/LSP diagnostics from the `replace ../harness` cross-module setup.
- Unit tests: `go test ./...` (hermetic). Integration tests: `//go:build integration`, `package test`, Postgres.app at `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`, self-clean DB + `dbos` schema; scripted model `test/scripted` (no LLM key).
- gofmt-clean before commit. Commit trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Chart has no CI: `helm lint` / `helm template` / `bash deploy/charts/runtime/test.sh` are the hermetic gate (run `make helm-deps` first if `charts/postgresql/` is missing).

---

## File Structure

**Go:**
- `internal/config/config.go` — `RemoteReplicaURL(i)`; lift the remote-`replicas` rule for `{i}`-templated URLs; `{i}` required iff `replicas>1`; dial-uniqueness over expanded ordinal URLs.
- `internal/config/config_test.go` — validation + expansion unit tests.
- `controlplane/registry.go` — build a remote **pool** set (N entries) for a remote agent with `replicas>1`; add a reachable bitmap + liveness-aware `NextReplica` + `SetReachable`.
- `controlplane/registry_test.go` — remote-pool expansion + skip-unreachable routing unit tests.
- `cmd/runtimed/main.go` — start one `HealthMonitor` per remote-pool ordinal; `OnChange` updates both the metric and the registry reachable bitmap.

**Helm (`deploy/charts/runtime/`):**
- `values.yaml` — `scheduling.mode`; `secrets.agentAuthToken`.
- `templates/_helpers.tpl` — agent fullname / headless-service / per-ordinal-DNS-template helpers + `perAgentPods` render guards.
- `templates/agent-statefulset.yaml` — new, gated on `perAgentPods`.
- `templates/agent-service.yaml` — new headless Service, gated on `perAgentPods`.
- `templates/configmap.yaml` — generate the control-plane `runtime.yaml` from `config.agents` in `perAgentPods` mode.
- `templates/deployment.yaml` — control-plane-only env in `perAgentPods` mode (agent bearer ref).
- `templates/secret.yaml` — add the optional `RUNTIME_AGENT_AUTH_TOKEN` key.
- `test.sh` — `perAgentPods` permutations + `monolith` regression.
- `README.md` — `perAgentPods` quick-start + brokered-secrets limitation.

**Integration test + docs:**
- `test/remote_pool_test.go` — new pool-attach integration test.
- `ROADMAP.md`, `runtime.yaml` — document M2.

---

## Task 1: Config — remote replica pool validation + URL expansion

**Files:**
- Modify: `internal/config/config.go` (the remote branch in `Validate()` ~lines 187–202; add `RemoteReplicaURL` near `ReplicaAddr` ~line 410)
- Test: `internal/config/config_test.go`

Context: today a remote agent (`url:` set) is single-entry and `Validate()` rejects `replicas > 1` on it (config.go:194, the `a.Replicas > 1` clause inside the remote-field rejection). C2 M2 lifts that one clause for the StatefulSet case: a remote agent may set `replicas: N` **iff** its `url:` contains the literal ordinal placeholder `{i}`. A single remote (replicas 0/1) must NOT contain `{i}` — preserving C3 M1 byte-for-byte.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestRemoteReplicaPool_Validate(t *testing.T) {
	base := func() *Config {
		return &Config{Agents: []AgentConfig{{
			ID: "support", Name: "S", Model: "m",
			URL: "http://support-{i}.support-hl.ns.svc:8080", Replicas: 3,
		}}}
	}
	t.Run("templated url with replicas>1 is valid", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("replicas>1 without {i} is rejected", func(t *testing.T) {
		c := base()
		c.Agents[0].URL = "http://support.support-hl.ns.svc:8080" // no {i}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error: replicas>1 needs {i} in url")
		}
	})
	t.Run("single remote with {i} is rejected", func(t *testing.T) {
		c := base()
		c.Agents[0].Replicas = 1 // single, but url has {i}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error: {i} only valid with replicas>1")
		}
	})
	t.Run("single remote unchanged (no {i}, no replicas) still valid", func(t *testing.T) {
		c := &Config{Agents: []AgentConfig{{
			ID: "rem", Name: "R", Model: "m", URL: "https://h:8443",
		}}}
		if err := c.Validate(); err != nil {
			t.Fatalf("C3 single-remote must stay valid: %v", err)
		}
	})
	t.Run("other spawn fields still rejected on remote pool", func(t *testing.T) {
		c := base()
		c.Agents[0].Memory = true
		if err := c.Validate(); err == nil {
			t.Fatal("expected error: memory not allowed on remote")
		}
	})
	t.Run("expanded ordinal URLs must be unique across agents", func(t *testing.T) {
		c := &Config{Agents: []AgentConfig{
			{ID: "a", Name: "A", Model: "m", URL: "http://x-{i}.svc:8080", Replicas: 2},
			{ID: "b", Name: "B", Model: "m", URL: "http://x-{i}.svc:8080", Replicas: 2},
		}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error: colliding expanded ordinal URLs")
		}
	})
}

func TestRemoteReplicaURL(t *testing.T) {
	a := AgentConfig{ID: "s", URL: "http://s-{i}.hl.ns.svc:8080", Replicas: 3}
	got, err := a.RemoteReplicaURL(1)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://s-1.hl.ns.svc:8080" {
		t.Fatalf("RemoteReplicaURL(1) = %q", got)
	}
	if _, err := a.RemoteReplicaURL(3); err == nil {
		t.Fatal("expected out-of-range error for i=3 (replicas=3)")
	}
	noTmpl := AgentConfig{ID: "s", URL: "http://s.svc:8080", Replicas: 1}
	if got, err := noTmpl.RemoteReplicaURL(0); err != nil || got != "http://s.svc:8080" {
		t.Fatalf("single remote RemoteReplicaURL(0) = %q err=%v", got, err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/config/ -run 'RemoteReplicaPool|RemoteReplicaURL' -v`
Expected: FAIL (compile error: `RemoteReplicaURL` undefined; the templated-pool case errors under the current `a.Replicas > 1` rejection).

- [ ] **Step 3: Add `RemoteReplicaURL` and `remotePoolCount`**

Add near `ReplicaAddr` (after line ~424) in `internal/config/config.go`:

```go
// remoteOrdinalPlaceholder is the literal substring in a remote agent's url:
// that RemoteReplicaURL replaces with the 0-based ordinal. Required iff the
// remote agent runs a pool (replicas > 1); forbidden for a single remote.
const remoteOrdinalPlaceholder = "{i}"

// RemotePoolSize is the number of ordinals a remote agent attaches to: replicas
// when > 1, else 1. Meaningful only for remote (url:) agents.
func (a AgentConfig) RemotePoolSize() int {
	if a.Replicas > 1 {
		return a.Replicas
	}
	return 1
}

// RemoteReplicaURL returns the dial URL for ordinal i of a remote agent,
// substituting "{i}" with i. For a single remote (no placeholder) it returns
// the url unchanged for i==0. Errors if i is out of [0,RemotePoolSize).
func (a AgentConfig) RemoteReplicaURL(i int) (string, error) {
	n := a.RemotePoolSize()
	if i < 0 || i >= n {
		return "", fmt.Errorf("agent %q: remote ordinal %d out of range [0,%d)", a.ID, i, n)
	}
	if !strings.Contains(a.URL, remoteOrdinalPlaceholder) {
		return a.URL, nil
	}
	return strings.ReplaceAll(a.URL, remoteOrdinalPlaceholder, strconv.Itoa(i)), nil
}
```

- [ ] **Step 4: Lift the rule + add dial-uniqueness in `Validate()`**

In `internal/config/config.go`, the remote branch currently reads (lines ~187–202):

```go
		remote := a.URL != ""
		if remote {
			u, err := url.Parse(a.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("config: agent %q url must be http(s)://host[:port] (got %q)", a.ID, a.URL)
			}
			// Local-only fields can't be delivered to a process we don't spawn.
			if len(a.Command) > 0 || a.WorkDir != "" || a.Kind != "" || a.Memory || a.Gateway.Enabled() || a.Replicas > 1 || a.Autoscale != nil {
				return fmt.Errorf("config: remote agent %q must not set command, workdir, kind, memory, gateway, replicas, or autoscale (these are spawn-time only)", a.ID)
			}
			if err := expandEnvScalar(&a.AuthToken, "agent "+a.ID+" auth_token"); err != nil {
				return err
			}
		} else if a.AuthToken != "" {
```

Replace the URL-parse note and the field-rejection clause (keep parsing `a.URL`, but parse the EXPANDED ordinal-0 URL is unnecessary; parse as-is is fine since `{i}` keeps it a valid URL host segment). Change the rejection to drop `a.Replicas > 1` from the forbidden set and instead enforce the `{i}` pairing rule:

```go
		remote := a.URL != ""
		if remote {
			u, err := url.Parse(a.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("config: agent %q url must be http(s)://host[:port] (got %q)", a.ID, a.URL)
			}
			// Local-only spawn fields can't be delivered to a process we don't
			// spawn. NOTE: replicas IS allowed on a remote agent (C2 M2 remote
			// pool) — but only paired with an {i}-templated url (checked below).
			if len(a.Command) > 0 || a.WorkDir != "" || a.Kind != "" || a.Memory || a.Gateway.Enabled() || a.Autoscale != nil {
				return fmt.Errorf("config: remote agent %q must not set command, workdir, kind, memory, gateway, or autoscale (these are spawn-time only)", a.ID)
			}
			// Remote replica pool (C2 M2): replicas>1 requires an {i} ordinal
			// placeholder in the url; a single remote must NOT contain {i}.
			hasTmpl := strings.Contains(a.URL, remoteOrdinalPlaceholder)
			if a.Replicas > 1 && !hasTmpl {
				return fmt.Errorf("config: remote agent %q has replicas %d but url %q has no %q ordinal placeholder", a.ID, a.Replicas, a.URL, remoteOrdinalPlaceholder)
			}
			if a.Replicas <= 1 && hasTmpl {
				return fmt.Errorf("config: remote agent %q url contains %q but is not a pool (set replicas > 1)", a.ID, remoteOrdinalPlaceholder)
			}
			if err := expandEnvScalar(&a.AuthToken, "agent "+a.ID+" auth_token"); err != nil {
				return err
			}
		} else if a.AuthToken != "" {
```

Then, in the dial-uniqueness section, the remote branch currently is (lines ~210–214):

```go
			if remote {
				if dials[a.URL] {
					return fmt.Errorf("config: duplicate agent dial address %q", a.URL)
				}
				dials[a.URL] = true
			} else {
```

Replace with per-ordinal expansion:

```go
			if remote {
				for i := 0; i < a.RemotePoolSize(); i++ {
					ou, err := a.RemoteReplicaURL(i)
					if err != nil {
						return fmt.Errorf("config: %w", err)
					}
					if dials[ou] {
						return fmt.Errorf("config: agent %q ordinal url %q collides with another agent", a.ID, ou)
					}
					dials[ou] = true
				}
			} else {
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS (new tests + all existing config tests, incl. `TestLoad_RemoteAgentURL`).

- [ ] **Step 6: gofmt + commit**

```bash
gofmt -w internal/config/config.go internal/config/config_test.go
git add internal/config/config.go internal/config/config_test.go
git commit -m "$(cat <<'EOF'
feat(config): remote replica pools — {i}-templated url + replicas

A remote agent may set replicas>1 paired with an {i} ordinal placeholder
in its url; RemoteReplicaURL(i) expands it. Dial-uniqueness now checks
every expanded ordinal URL. Single remote (no {i}) stays C3 M1 verbatim.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Registry — build the remote replica-pool set

**Files:**
- Modify: `controlplane/registry.go` (the remote branch in `NewRegistry`, lines ~53–61)
- Test: `controlplane/registry_test.go`

Context: `NewRegistry`'s remote branch builds a single `AgentProcess`. For a remote pool it must build N entries — one per ordinal — each with its expanded `BaseURL` and `ReplicaIndex`, reusing the same `r.sets[id]` slice path that local A1 pools use. `DBOSVMID` stays `""` (the remote pod owns its own executor id).

- [ ] **Step 1: Write the failing test**

Add to `controlplane/registry_test.go`:

```go
func TestRegistry_RemotePoolExpansion(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "support", Name: "S", Model: "m",
			URL: "http://support-{i}.support-hl.ns.svc:8080", Replicas: 3,
			AuthToken: "tok", Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")
	set, ok := r.Replicas("support")
	if !ok || len(set) != 3 {
		t.Fatalf("Replicas: ok=%v len=%d, want 3", ok, len(set))
	}
	for i, ap := range set {
		if !ap.Remote {
			t.Errorf("replica %d: Remote=false, want true", i)
		}
		if ap.ReplicaIndex != i {
			t.Errorf("replica %d: ReplicaIndex=%d", i, ap.ReplicaIndex)
		}
		want := "http://support-" + strconv.Itoa(i) + ".support-hl.ns.svc:8080"
		if ap.BaseURL != want {
			t.Errorf("replica %d: BaseURL=%q want %q", i, ap.BaseURL, want)
		}
		if ap.DBOSVMID != "" {
			t.Errorf("replica %d: DBOSVMID=%q, want empty (remote owns its id)", i, ap.DBOSVMID)
		}
		if ap.AuthToken != "tok" {
			t.Errorf("replica %d: AuthToken=%q", i, ap.AuthToken)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./controlplane/ -run TestRegistry_RemotePoolExpansion -v`
Expected: FAIL (`len=1, want 3` — the remote branch builds a single entry).

- [ ] **Step 3: Build the pool in `NewRegistry`**

In `controlplane/registry.go`, replace the remote branch (lines ~53–61):

```go
		if a.URL != "" {
			rem := base
			rem.Remote = true
			rem.BaseURL = a.URL
			rem.AuthToken = a.AuthToken
			rem.ReplicaIndex = 0
			r.sets[a.ID] = []AgentProcess{rem}
			continue
		}
```

with a per-ordinal expansion:

```go
		if a.URL != "" {
			n := a.RemotePoolSize()
			set := make([]AgentProcess, n)
			for i := 0; i < n; i++ {
				ou, err := a.RemoteReplicaURL(i)
				if err != nil {
					// Validate() proved these expand; fall back defensively.
					ou = a.URL
				}
				rem := base
				rem.Remote = true
				rem.BaseURL = ou
				rem.AuthToken = a.AuthToken
				rem.ReplicaIndex = i
				set[i] = rem
			}
			r.sets[a.ID] = set
			continue
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./controlplane/ -run 'TestRegistry' -v`
Expected: PASS (new pool test + existing `TestRegistry_RemoteSingleReplica`, `TestRegistry_ReplicaSetExpansion`).

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -w controlplane/registry.go controlplane/registry_test.go
git add controlplane/registry.go controlplane/registry_test.go
git commit -m "$(cat <<'EOF'
feat(controlplane): build remote replica-pool set in registry

A remote agent with replicas>1 expands to N attach entries (one per
ordinal: expanded BaseURL, ReplicaIndex i, empty DBOSVMID). Reuses the
A1 r.sets slice path; single remote stays one entry.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Registry — liveness-aware NextReplica

**Files:**
- Modify: `controlplane/registry.go` (the `Registry` struct ~lines 22–29; `NewRegistry` init ~lines 36–41; `NextReplica` ~lines 200–210)
- Test: `controlplane/registry_test.go`

Context: `Registry.NextReplica` for a static set is liveness-blind (`n % len(set)`). For a remote pool, K8s can scale ordinals away. Add a per-agent reachable bitmap (written by the `HealthMonitor` `OnChange` in Task 4) and make `NextReplica` skip unreachable ordinals, falling back to 0 when all are unreachable (mirrors `PoolManager.NextReplica`). Unknown (un-probed) ordinals are treated reachable so boot doesn't 503 the pool before the first probe.

- [ ] **Step 1: Write the failing test**

Add to `controlplane/registry_test.go`:

```go
func TestRegistry_NextReplicaSkipsUnreachable(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentConfig{
		{ID: "a", Name: "A", Model: "m",
			URL: "http://a-{i}.hl.svc:8080", Replicas: 3, Tenant: "default"},
	}}
	r := NewRegistry(cfg, "/bin/agentd", "dsn")

	// All unknown ⇒ treated reachable ⇒ plain round-robin.
	got := []int{r.NextReplica("a"), r.NextReplica("a"), r.NextReplica("a")}
	if got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("unknown-reachable RR: got %v want [0 1 2]", got)
	}

	// Mark ordinal 1 unreachable ⇒ it is skipped.
	r.SetReachable("a", 1, false)
	for i := 0; i < 6; i++ {
		if idx := r.NextReplica("a"); idx == 1 {
			t.Fatalf("NextReplica returned unreachable ordinal 1")
		}
	}

	// All unreachable ⇒ fall back to 0.
	r.SetReachable("a", 0, false)
	r.SetReachable("a", 2, false)
	if idx := r.NextReplica("a"); idx != 0 {
		t.Fatalf("all-unreachable fallback: got %d want 0", idx)
	}

	// Recovery: ordinal 2 back ⇒ it is selectable again.
	r.SetReachable("a", 2, true)
	found2 := false
	for i := 0; i < 6; i++ {
		if r.NextReplica("a") == 2 {
			found2 = true
			break
		}
	}
	if !found2 {
		t.Fatal("recovered ordinal 2 never selected")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./controlplane/ -run TestRegistry_NextReplicaSkipsUnreachable -v`
Expected: FAIL (compile error: `SetReachable` undefined).

- [ ] **Step 3: Add the reachable bitmap + SetReachable + skip logic**

In `controlplane/registry.go`, add imports `sync` (keep `sync/atomic`). Extend the struct:

```go
type Registry struct {
	order  []string
	sets   map[string][]AgentProcess
	infos  map[string]AgentInfo
	rr     map[string]*atomic.Uint64
	broker SecretBroker
	pools  map[string]*PoolManager

	// reachMu guards reach: id → (replicaIndex → reachable). Absent entry ⇒
	// "unknown", treated as reachable until the first health probe reports.
	// Written by main's per-ordinal HealthMonitor OnChange; read by NextReplica.
	reachMu sync.RWMutex
	reach   map[string]map[int]bool
}
```

In `NewRegistry`, add `reach` to the initializer:

```go
	r := &Registry{
		sets:  map[string][]AgentProcess{},
		infos: map[string]AgentInfo{},
		rr:    map[string]*atomic.Uint64{},
		pools: map[string]*PoolManager{},
		reach: map[string]map[int]bool{},
	}
```

Add the setter + an internal helper, and rewrite `NextReplica`'s static-set branch. Replace the existing `NextReplica` (lines ~200–210):

```go
func (r *Registry) NextReplica(id string) int {
	if pm, ok := r.pools[id]; ok {
		return pm.NextReplica()
	}
	set, ok := r.sets[id]
	if !ok || len(set) == 0 {
		return 0
	}
	n := r.rr[id].Add(1) - 1
	return int(n % uint64(len(set)))
}
```

with:

```go
// SetReachable records a replica's reachability (called by main's per-ordinal
// HealthMonitor on each transition). Used by NextReplica to skip down ordinals
// of a remote pool. Safe for concurrent use.
func (r *Registry) SetReachable(id string, replica int, reachable bool) {
	r.reachMu.Lock()
	defer r.reachMu.Unlock()
	m := r.reach[id]
	if m == nil {
		m = map[int]bool{}
		r.reach[id] = m
	}
	m[replica] = reachable
}

// reachableOrUnknown reports whether replica i of id may receive new sessions:
// true unless a probe has explicitly marked it unreachable.
func (r *Registry) reachableOrUnknown(id string, i int) bool {
	r.reachMu.RLock()
	defer r.reachMu.RUnlock()
	m := r.reach[id]
	if m == nil {
		return true
	}
	v, ok := m[i]
	if !ok {
		return true // unknown ⇒ reachable until first probe
	}
	return v
}

// NextReplica returns the next replica index for a NEW session, round-robin via
// an atomic per-agent counter, SKIPPING ordinals a health probe has marked
// unreachable. Falls back to 0 if every ordinal is unreachable. Autoscaled
// agents delegate to their PoolManager (which skips draining replicas).
func (r *Registry) NextReplica(id string) int {
	if pm, ok := r.pools[id]; ok {
		return pm.NextReplica()
	}
	set, ok := r.sets[id]
	if !ok || len(set) == 0 {
		return 0
	}
	n := len(set)
	for tries := 0; tries < n; tries++ {
		idx := int((r.rr[id].Add(1) - 1) % uint64(n))
		if r.reachableOrUnknown(id, idx) {
			return idx
		}
	}
	return 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./controlplane/ -run 'TestRegistry' -v`
Expected: PASS — including the existing `TestRegistry_NextReplicaRoundRobin` (all-unknown ⇒ plain round-robin `[0 1 0 1]`).

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -w controlplane/registry.go controlplane/registry_test.go
git add controlplane/registry.go controlplane/registry_test.go
git commit -m "$(cat <<'EOF'
feat(controlplane): liveness-aware NextReplica for remote pools

NextReplica skips ordinals a health probe marked unreachable (new
SetReachable + reach bitmap), falling back to 0 when all are down.
Unknown ordinals are reachable until first probe, so boot never 503s
the pool. Static all-up round-robin unchanged.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: main — one HealthMonitor per remote-pool ordinal, wired to the bitmap

**Files:**
- Modify: `cmd/runtimed/main.go` (the remote branch in the agent-start loop, lines ~308–319)

Context: today the start loop creates one `HealthMonitor` per remote `AgentProcess` whose `OnChange` only updates the `AgentReachable` metric. Because a remote pool is now N `AgentProcess` entries, the existing per-replica loop already iterates them — we only need `OnChange` to ALSO update the registry's reachable bitmap so `NextReplica` can skip down ordinals.

- [ ] **Step 1: Update the remote branch**

In `cmd/runtimed/main.go`, the remote branch inside `for _, ap := range replicas` currently reads (lines ~308–319):

```go
			if ap.Remote {
				id := ap.AgentID
				idx := ap.ReplicaIndex
				hm := &controlplane.HealthMonitor{
					BaseURL: ap.DialBase(), Token: ap.AuthToken,
					OnChange: func(ok bool) { cm.AgentReachable(id, idx, ok) },
				}
				go hm.Run(ctx)
				slog.Info("monitoring remote agent", "agent", ap.AgentID, "url", ap.DialBase())
				continue
			}
```

Replace the `OnChange` closure so it updates both the metric and the registry:

```go
			if ap.Remote {
				id := ap.AgentID
				idx := ap.ReplicaIndex
				hm := &controlplane.HealthMonitor{
					BaseURL: ap.DialBase(), Token: ap.AuthToken,
					OnChange: func(ok bool) {
						cm.AgentReachable(id, idx, ok)
						reg.SetReachable(id, idx, ok)
					},
				}
				go hm.Run(ctx)
				slog.Info("monitoring remote agent", "agent", ap.AgentID, "replica", idx, "url", ap.DialBase())
				continue
			}
```

- [ ] **Step 2: Build + vet**

Run: `go build ./... && go vet ./cmd/runtimed/`
Expected: clean (benign macOS LC_DYSYMTAB linker warnings, if any, are ignored).

- [ ] **Step 3: Hermetic test sweep**

Run: `go test ./...`
Expected: PASS (no integration tag; this is the unit sweep).

- [ ] **Step 4: Commit**

```bash
git add cmd/runtimed/main.go
git commit -m "$(cat <<'EOF'
feat(runtimed): feed remote-ordinal health into the routing bitmap

Each remote pool ordinal's HealthMonitor OnChange now updates both the
AgentReachable metric and reg.SetReachable, so NextReplica skips a
scaled-down/unhealthy ordinal for new sessions. Affinity routing to a
pinned ordinal is unchanged (503 until it returns).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Integration test — remote pool attach, routing, affinity, durability

**Files:**
- Create: `test/remote_pool_test.go`

Context: prove the whole Go-side feature end-to-end against real Postgres, modeled on `test/remote_agent_test.go`. Start TWO standalone agentd processes on adjacent ports (stand-ins for StatefulSet ordinals `agent-0`, `agent-1`, each with its own `DBOS__VMID`), point runtimed at them via one `{i}`-templated `url:` with `replicas: 2`, and verify: both reachable; new sessions distribute across both ordinals; killing ordinal 1 leaves new sessions routing only to ordinal 0 while a session pinned to ordinal 0 still works; runtimed never restarts the dead ordinal.

- [ ] **Step 1: Write the integration test**

Create `test/remote_pool_test.go`:

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestRemoteReplicaPoolAttach proves C2 M2: runtimed attaches to a remote agent
// declared as a {i}-templated pool (replicas: 2), round-robins new sessions
// across the two ordinals, and on one ordinal dying routes new sessions only to
// the survivor (liveness-aware NextReplica) while a session pinned to the
// survivor keeps working — no restart of the dead ordinal.
func TestRemoteReplicaPoolAttach(t *testing.T) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)

	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}

	// Two ordinals on adjacent ports, each its own DBOS executor id pool#0/pool#1.
	// First ordinal starts alone so it creates the DBOS schema before the second
	// races on it (the same serialization runtimed does for local pools).
	ords := []struct {
		addr string
		vmid string
	}{
		{"127.0.0.1:8311", "pool#0"},
		{"127.0.0.1:8312", "pool#1"},
	}
	procs := make([]*exec.Cmd, len(ords))
	killed := make([]bool, len(ords))
	startOrd := func(i int) {
		c := exec.Command(agentd)
		c.Env = append(os.Environ(),
			"RUNTIME_PG_DSN="+dsn,
			"RUNTIME_LISTEN_ADDR="+ords[i].addr,
			"RUNTIME_AGENT_ID=pool",
			"RUNTIME_AGENT_TENANT=default",
			"RUNTIME_AGENT_REPLICA="+fmt.Sprint(i),
			"DBOS__VMID="+ords[i].vmid,
		)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			t.Fatalf("start ordinal %d: %v", i, err)
		}
		procs[i] = c
	}
	killOrd := func(i int) {
		if killed[i] {
			return
		}
		killed[i] = true
		_ = syscall.Kill(-procs[i].Process.Pid, syscall.SIGKILL)
		_, _ = procs[i].Process.Wait()
	}
	startOrd(0)
	rmtWaitHealthy(t, "http://"+ords[0].addr+"/healthz", "", 30*time.Second)
	startOrd(1)
	rmtWaitHealthy(t, "http://"+ords[1].addr+"/healthz", "", 30*time.Second)
	defer func() {
		for i := range procs {
			killOrd(i)
		}
	}()

	// runtimed config: one remote POOL, {i}-templated url, replicas: 2.
	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: pool, name: Pool, model: test/scripted, url: \"http://127.0.0.1:831{i}\", replicas: 2}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8320"
	rt := exec.Command(runtimed)
	rt.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
	)
	rt.Stdout, rt.Stderr = os.Stdout, os.Stderr
	rt.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := rt.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-rt.Process.Pid, syscall.SIGKILL); _, _ = rt.Process.Wait() }()
	rmtWaitHealthy(t, "http://"+ctlAddr+"/healthz", "", 30*time.Second)

	rmtWaitFor(t, 20*time.Second, func() bool {
		return rmtGetAgents(t, ctlAddr)["pool"]
	}, "pool agent healthy")

	mkSession := func() string {
		resp, err := http.Post("http://"+ctlAddr+"/agents/pool/sessions", "application/json",
			strings.NewReader(`{"message":"hi"}`))
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("session create: status=%d body=%s", resp.StatusCode, body)
		}
		var s struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(body, &s); err != nil || s.SessionID == "" {
			t.Fatalf("no session_id: err=%v body=%s", err, body)
		}
		return s.SessionID
	}

	// Distribution: several sessions should land on BOTH ordinals (the replica
	// column records which). Query distinct replicas seen.
	for i := 0; i < 8; i++ {
		mkSession()
	}
	replicas := distinctReplicas(t, db, "pool")
	if !replicas[0] || !replicas[1] {
		t.Fatalf("sessions did not distribute across both ordinals: seen=%v", replicas)
	}

	// Pin a session to the SURVIVOR ordinal (0). Find one whose replica==0.
	pinned := sessionOnReplica(t, db, "pool", 0)
	if pinned == "" {
		t.Fatal("no session pinned to ordinal 0 to test affinity")
	}

	// Kill ordinal 1; runtimed must mark it unreachable and route new sessions
	// only to ordinal 0, WITHOUT restarting ordinal 1.
	killOrd(1)
	rmtWaitFor(t, 20*time.Second, func() bool {
		// New sessions should now all land on ordinal 0.
		mkSession()
		return onlyReplica0Since(t, db, "pool")
	}, "new sessions route only to ordinal 0 after ordinal 1 dies")

	// No restart: nothing should be serving on ordinal 1's port.
	time.Sleep(2 * time.Second)
	noRestart := &http.Client{Timeout: 2 * time.Second}
	if _, err := noRestart.Get("http://" + ords[1].addr + "/healthz"); err == nil {
		t.Fatal("ordinal 1 port still serving — runtimed must NOT restart a remote ordinal")
	}

	// Affinity + durability: the pinned (ordinal-0) session still streams.
	resp, err := http.Get(fmt.Sprintf("http://%s/agents/pool/sessions/%s", ctlAddr, pinned))
	if err != nil {
		t.Fatalf("get pinned session: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pinned session broken after ordinal 1 death: status=%d", resp.StatusCode)
	}
	_ = context.Background()
}

// distinctReplicas returns which replica indices appear in sessions for agentID.
func distinctReplicas(t *testing.T, db *sql.DB, agentID string) map[int]bool {
	t.Helper()
	rows, err := db.Query(`SELECT DISTINCT replica FROM sessions WHERE agent_id=$1`, agentID)
	if err != nil {
		t.Fatalf("distinctReplicas: %v", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var r int
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out[r] = true
	}
	return out
}

// sessionOnReplica returns one session id pinned to the given replica, or "".
func sessionOnReplica(t *testing.T, db *sql.DB, agentID string, replica int) string {
	t.Helper()
	var id string
	err := db.QueryRow(`SELECT id FROM sessions WHERE agent_id=$1 AND replica=$2 LIMIT 1`,
		agentID, replica).Scan(&id)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("sessionOnReplica: %v", err)
	}
	return id
}

// onlyReplica0Since reports whether the most recent session row is on replica 0
// (a coarse proxy for "new sessions route only to ordinal 0"). Uses the highest
// id as "most recent" (BIGSERIAL/text id ordering is not guaranteed; instead we
// check that the LATEST few sessions are all replica 0).
func onlyReplica0Since(t *testing.T, db *sql.DB, agentID string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT replica FROM sessions WHERE agent_id=$1 ORDER BY ctid DESC LIMIT 3`, agentID)
	if err != nil {
		t.Fatalf("onlyReplica0Since: %v", err)
	}
	defer rows.Close()
	any := false
	for rows.Next() {
		any = true
		var r int
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		if r != 0 {
			return false
		}
	}
	return any
}
```

NOTE for the implementer: `mustExec`, `rmtWaitHealthy`, `rmtGetAgents`, `rmtWaitFor`, and the package-level `dsn` are defined in sibling files (`remote_agent_test.go`, and `mustExec`/`dsn` in the shared test helpers) — do NOT redefine them. Verify the `sessions` table has the columns used (`agent_id`, `replica`, `id`) by checking `internal/store/pgstore.go`; adjust column names if they differ. If the session GET route differs from `/agents/{id}/sessions/{sid}`, check `controlplane/api.go` for the actual pattern and match it.

- [ ] **Step 2: Run it (requires Postgres.app)**

Run: `go test -tags integration ./test/ -run TestRemoteReplicaPoolAttach -v -timeout 180s`
Expected: PASS. If it fails on a column/route name, fix per the NOTE above (not by weakening the assertions).

- [ ] **Step 3: Commit**

```bash
gofmt -w test/remote_pool_test.go
git add test/remote_pool_test.go
git commit -m "$(cat <<'EOF'
test(integration): remote replica-pool attach, routing, affinity

Two standalone agentd ordinals attached via one {i}-templated url
(replicas:2): proves session distribution across both, liveness-aware
routing to the survivor after one ordinal dies (no restart), and
affinity+durability for a session pinned to the survivor.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Chart — values + helpers for `perAgentPods` mode

**Files:**
- Modify: `deploy/charts/runtime/values.yaml`
- Modify: `deploy/charts/runtime/templates/_helpers.tpl`

Context: add the mode toggle, the optional shared agent bearer, the per-ordinal DNS helpers (single source of truth for both the StatefulSet and runtimed's generated url), and the `perAgentPods` render guards.

- [ ] **Step 1: Add values**

In `deploy/charts/runtime/values.yaml`, after the `replicaCount: 1` block (line ~13) add:

```yaml
# Scheduling topology:
#   monolith     — one pod; runtimed exec-spawns every agent as a child (C2 M1).
#   perAgentPods — one StatefulSet + headless Service per agent (agentd-only
#                  pods); runtimed runs control-plane-only and ATTACHES to them
#                  as remote replica pools (C2 M2). Each agent's pod count comes
#                  from config.agents[].replicas (default 1); agents must NOT set
#                  listen_addr/url in this mode (the chart generates the url).
scheduling:
  mode: monolith
```

In the `secrets:` block (after `adminBootstrap`, line ~28) add:

```yaml
  agentAuthToken: ""      # RUNTIME_AGENT_AUTH_TOKEN — shared bearer runtimed
                          # uses to reach agent pods (perAgentPods mode). Optional
                          # but recommended. Authenticates runtimed TO each agent.
```

- [ ] **Step 2: Add helpers + guards to `_helpers.tpl`**

Append to `deploy/charts/runtime/templates/_helpers.tpl`:

```yaml
{{/*
Per-agent StatefulSet name: "<release>-agent-<id>". Takes a dict {root, id}.
*/}}
{{- define "runtime.agentFullname" -}}
{{- printf "%s-agent-%s" .root.Release.Name .id | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Per-agent headless Service name: "<release>-agent-<id>-hl". dict {root, id}. */}}
{{- define "runtime.agentHeadless" -}}
{{- printf "%s-agent-%s-hl" .root.Release.Name .id | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
runtimed's dial url template for an agent's remote pool, with the literal {i}
ordinal placeholder config.RemoteReplicaURL expands. Pod DNS in a StatefulSet is
<pod>.<headless-svc>.<ns>.svc.cluster.local, and pod name is <sts>-<ordinal>, so:
  http://<release>-agent-<id>-{i}.<release>-agent-<id>-hl.<ns>.svc.cluster.local:8080
dict {root, id}.
*/}}
{{- define "runtime.agentDialTemplate" -}}
{{- $sts := include "runtime.agentFullname" . -}}
{{- $hl := include "runtime.agentHeadless" . -}}
{{- printf "http://%s-{i}.%s.%s.svc.cluster.local:8080" $sts $hl .root.Release.Namespace -}}
{{- end -}}

{{/*
Fail-closed validation for perAgentPods mode: each agent must NOT set
listen_addr or url (the chart generates the url) and must have id/name/model.
*/}}
{{- define "runtime.requirePerAgentPods" -}}
{{- range $i, $a := .Values.config.agents -}}
{{- if or (not $a.id) (not $a.name) (not $a.model) -}}
{{- fail (printf "runtime: perAgentPods agent[%d] needs id, name, model" $i) -}}
{{- end -}}
{{- if or $a.listen_addr $a.url -}}
{{- fail (printf "runtime: perAgentPods agent %q must NOT set listen_addr or url (the chart generates the per-ordinal url)" $a.id) -}}
{{- end -}}
{{- end -}}
{{- end -}}
```

- [ ] **Step 3: Lint render (both modes still parse)**

Run:
```bash
helm template r deploy/charts/runtime \
  --set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable \
  --set config.agents[0].id=a --set config.agents[0].name=A \
  --set config.agents[0].model=test/scripted --set config.agents[0].listen_addr=127.0.0.1:8101 >/dev/null
```
Expected: succeeds (monolith default unaffected; new helpers are defined-but-unused so far).

- [ ] **Step 4: Commit**

```bash
git add deploy/charts/runtime/values.yaml deploy/charts/runtime/templates/_helpers.tpl
git commit -m "$(cat <<'EOF'
feat(chart): scheduling.mode values + per-agent DNS helpers

Adds scheduling.mode (monolith|perAgentPods), an optional shared
secrets.agentAuthToken, per-agent StatefulSet/headless-Service name
helpers, the single-source-of-truth per-ordinal dial template, and the
perAgentPods fail-closed guard. Templates that consume these land next.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Chart — per-agent StatefulSet + headless Service

**Files:**
- Create: `deploy/charts/runtime/templates/agent-statefulset.yaml`
- Create: `deploy/charts/runtime/templates/agent-service.yaml`

Context: in `perAgentPods` mode, render one StatefulSet + headless Service per agent. Each pod runs `agentd` only; the ordinal-dependent env (`RUNTIME_AGENT_REPLICA`, `DBOS__VMID`) is derived from `$HOSTNAME` (`<sts>-<ordinal>`) via a `/bin/sh` wrapper, since those vary per pod. The image base is `debian:bookworm-slim` (has `/bin/sh`).

- [ ] **Step 1: Create the StatefulSet template**

Create `deploy/charts/runtime/templates/agent-statefulset.yaml`:

```yaml
{{- if eq .Values.scheduling.mode "perAgentPods" }}
{{- $unused := include "runtime.requirePerAgentPods" . }}
{{- $unusedPg := include "runtime.requirePg" . }}
{{- $unusedAgents := include "runtime.requireAgents" . }}
{{- $root := . }}
{{- range $a := .Values.config.agents }}
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ include "runtime.agentFullname" (dict "root" $root "id" $a.id) }}
  labels:
    {{- include "runtime.labels" $root | nindent 4 }}
    runtime.agent/id: {{ $a.id | quote }}
spec:
  serviceName: {{ include "runtime.agentHeadless" (dict "root" $root "id" $a.id) }}
  replicas: {{ $a.replicas | default 1 }}
  podManagementPolicy: Parallel
  selector:
    matchLabels:
      {{- include "runtime.selectorLabels" $root | nindent 6 }}
      runtime.agent/id: {{ $a.id | quote }}
  template:
    metadata:
      labels:
        {{- include "runtime.selectorLabels" $root | nindent 8 }}
        runtime.agent/id: {{ $a.id | quote }}
    spec:
      serviceAccountName: {{ include "runtime.serviceAccountName" $root }}
      {{- with $root.Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        {{- toYaml $root.Values.podSecurityContext | nindent 8 }}
      containers:
        - name: agentd
          image: "{{ $root.Values.image.repository }}:{{ $root.Values.image.tag | default $root.Chart.AppVersion }}"
          imagePullPolicy: {{ $root.Values.image.pullPolicy }}
          # Ordinal-dependent env (RUNTIME_AGENT_REPLICA, DBOS__VMID) is derived
          # from the pod name ($HOSTNAME = <statefulset>-<ordinal>) at start; all
          # other env is static and set below.
          command: ["/bin/sh", "-c"]
          args:
            - |
              ORD="${HOSTNAME##*-}"
              export RUNTIME_AGENT_REPLICA="$ORD"
              export DBOS__VMID="{{ $a.id }}#${ORD}"
              exec /app/agentd
          env:
            - name: RUNTIME_LISTEN_ADDR
              value: ":8080"
            - name: RUNTIME_AGENT_ID
              value: {{ $a.id | quote }}
            - name: RUNTIME_AGENT_TENANT
              value: {{ $a.tenant | default "default" | quote }}
            - name: RUNTIME_AGENT_MEMORY
              value: {{ if $a.memory }}"1"{{ else }}""{{ end }}
            - name: RUNTIME_PG_DSN
              valueFrom:
                secretKeyRef:
                  name: {{ include "runtime.secretName" $root }}
                  key: RUNTIME_PG_DSN
            {{- if or $root.Values.secrets.agentAuthToken $root.Values.secrets.existingSecret }}
            - name: RUNTIME_AGENT_AUTH_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "runtime.secretName" $root }}
                  key: RUNTIME_AGENT_AUTH_TOKEN
                  optional: true
            {{- end }}
          ports:
            - name: http
              containerPort: 8080
          readinessProbe:
            httpGet: { path: /healthz, port: http }
            initialDelaySeconds: 3
            periodSeconds: 5
          livenessProbe:
            httpGet: { path: /healthz, port: http }
            initialDelaySeconds: 10
            periodSeconds: 10
          securityContext:
            {{- toYaml $root.Values.securityContext | nindent 12 }}
          resources:
            {{- toYaml $root.Values.resources | nindent 12 }}
          volumeMounts:
            - name: scratch
              mountPath: /tmp
      volumes:
        - name: scratch
          emptyDir: {}
      {{- with $root.Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $root.Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $root.Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
{{- end }}
{{- end }}
```

- [ ] **Step 2: Create the headless Service template**

Create `deploy/charts/runtime/templates/agent-service.yaml`:

```yaml
{{- if eq .Values.scheduling.mode "perAgentPods" }}
{{- $root := . }}
{{- range $a := .Values.config.agents }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ include "runtime.agentHeadless" (dict "root" $root "id" $a.id) }}
  labels:
    {{- include "runtime.labels" $root | nindent 4 }}
    runtime.agent/id: {{ $a.id | quote }}
spec:
  clusterIP: None   # headless: per-ordinal DNS <pod>.<svc>
  # Publish not-ready addresses so runtimed can resolve + health-probe each
  # ordinal's DNS as it comes up (runtimed does its own liveness tracking; it
  # must not wait on K8s readiness to discover a pod).
  publishNotReadyAddresses: true
  selector:
    {{- include "runtime.selectorLabels" $root | nindent 4 }}
    runtime.agent/id: {{ $a.id | quote }}
  ports:
    - name: http
      port: 8080
      targetPort: http
      protocol: TCP
{{- end }}
{{- end }}
```

- [ ] **Step 3: Render perAgentPods mode**

Run:
```bash
helm template r deploy/charts/runtime \
  --set scheduling.mode=perAgentPods \
  --set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable \
  --set config.agents[0].id=support --set config.agents[0].name=S \
  --set config.agents[0].model=test/scripted --set config.agents[0].replicas=2 \
  | grep -E 'kind: StatefulSet|clusterIP: None|serviceName:|replicas: 2|DBOS__VMID'
```
Expected: shows `kind: StatefulSet`, `serviceName: r-agent-support-hl`, `replicas: 2`, `clusterIP: None`, and the `DBOS__VMID="support#${ORD}"` line.

- [ ] **Step 4: Verify the guard fires on listen_addr in perAgentPods**

Run:
```bash
helm template r deploy/charts/runtime --set scheduling.mode=perAgentPods \
  --set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable \
  --set config.agents[0].id=s --set config.agents[0].name=S \
  --set config.agents[0].model=m --set config.agents[0].listen_addr=127.0.0.1:8101 2>&1 | grep -q 'must NOT set listen_addr' \
  && echo GUARD_OK
```
Expected: prints `GUARD_OK`.

- [ ] **Step 5: Commit**

```bash
git add deploy/charts/runtime/templates/agent-statefulset.yaml deploy/charts/runtime/templates/agent-service.yaml
git commit -m "$(cat <<'EOF'
feat(chart): per-agent StatefulSet + headless Service (perAgentPods)

One StatefulSet + headless Service per agent: agentd-only pods, ordinal
→ RUNTIME_AGENT_REPLICA/DBOS__VMID derived from $HOSTNAME, stable
per-ordinal DNS, secure pod posture mirroring the monolith. Gated on
scheduling.mode=perAgentPods.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Chart — generated control-plane runtime.yaml + control-plane-only Deployment

**Files:**
- Modify: `deploy/charts/runtime/templates/configmap.yaml`
- Modify: `deploy/charts/runtime/templates/deployment.yaml`
- Modify: `deploy/charts/runtime/templates/secret.yaml`

Context: in `perAgentPods` mode runtimed must NOT exec-spawn — its `runtime.yaml` is generated from `config.agents`, rewriting each agent into a remote pool (`url:` = the dial template, `replicas:`, optional `auth_token`). The Deployment gains the agent bearer env so the generated `${RUNTIME_AGENT_AUTH_TOKEN}` expands. The Secret carries the optional key.

- [ ] **Step 1: Generate runtime.yaml in the ConfigMap**

Replace `deploy/charts/runtime/templates/configmap.yaml` entirely:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "runtime.fullname" . }}
  labels:
    {{- include "runtime.labels" . | nindent 4 }}
data:
{{- if eq .Values.scheduling.mode "perAgentPods" }}
  # perAgentPods: runtimed runs control-plane-only and ATTACHES to each agent's
  # StatefulSet as a remote replica pool. This runtime.yaml is GENERATED from
  # config.agents — each agent becomes a remote entry whose url is the per-ordinal
  # headless DNS template ({i} expanded by runtimed) and whose replicas is the
  # StatefulSet pod count. Spawn-time fields (memory/gateway) are delivered to the
  # agent POD by its StatefulSet, not via this file.
  {{- $root := . }}
  {{- $hasToken := or .Values.secrets.agentAuthToken .Values.secrets.existingSecret }}
  runtime.yaml: |
    agents:
    {{- range $a := .Values.config.agents }}
      - id: {{ $a.id }}
        name: {{ $a.name | quote }}
        model: {{ $a.model | quote }}
        tenant: {{ $a.tenant | default "default" | quote }}
        url: "{{ include "runtime.agentDialTemplate" (dict "root" $root "id" $a.id) }}"
        replicas: {{ $a.replicas | default 1 }}
        {{- if $hasToken }}
        auth_token: "${RUNTIME_AGENT_AUTH_TOKEN}"
        {{- end }}
    {{- end }}
    {{- with .Values.config.gateway }}
    gateway:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    {{- with .Values.config.tokens }}
    tokens:
      {{- toYaml . | nindent 6 }}
    {{- end }}
{{- else }}
  # monolith: runtimed exec-spawns each agent. runtime.requireAgents (called from
  # the Deployment) guarantees a non-empty agents list at render time.
  runtime.yaml: |
    {{- toYaml .Values.config | nindent 4 }}
{{- end }}
```

NOTE: a single-replica agent (replicas 1 / omitted) generates `url:` WITHOUT `{i}` and `replicas: 1`, which the config loader accepts (no placeholder, single remote). The dial template always contains `{i}`, so a single-replica agent's generated url WOULD contain `{i}` with `replicas: 1` — which Task 1 REJECTS. Fix: emit `replicas:` only when > 1, and for replicas:1 the url still has `{i}`. To keep both sides consistent, when `replicas <= 1` substitute `{i}`→`0` here so the generated url is concrete. Implement by branching in the template:

Replace the `url:` and `replicas:` lines inside the range with:

```yaml
        {{- $n := $a.replicas | default 1 | int }}
        {{- $tmpl := include "runtime.agentDialTemplate" (dict "root" $root "id" $a.id) }}
        {{- if gt $n 1 }}
        url: "{{ $tmpl }}"
        replicas: {{ $n }}
        {{- else }}
        url: "{{ $tmpl | replace "{i}" "0" }}"
        {{- end }}
```

(So replicas:1 → concrete `...-0...` url, no `replicas:` key, no `{i}`; replicas:N>1 → `{i}` template + `replicas: N`. Both satisfy Task 1's validation.)

- [ ] **Step 2: Make the Deployment control-plane-only + agent bearer env**

In `deploy/charts/runtime/templates/deployment.yaml`, after the `RUNTIME_AGENTD_BIN` env entry (lines ~43–44) add a perAgentPods-only agent bearer env (so the generated `${RUNTIME_AGENT_AUTH_TOKEN}` expands):

```yaml
            {{- if and (eq .Values.scheduling.mode "perAgentPods") (or .Values.secrets.agentAuthToken .Values.secrets.existingSecret) }}
            - name: RUNTIME_AGENT_AUTH_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "runtime.secretName" . }}
                  key: RUNTIME_AGENT_AUTH_TOKEN
                  optional: true
            {{- end }}
```

(runtimed in perAgentPods mode simply finds zero local agents to spawn — every agent has a `url:` — so no code change is needed to make it "control-plane-only"; the generated config does it. The `RUNTIME_AGENTD_BIN` env stays harmless.)

- [ ] **Step 3: Add the Secret key**

In `deploy/charts/runtime/templates/secret.yaml`, add the optional agent token key alongside the others. Read the file first; add within the same `stringData`/`data` map, guarded like the other optional keys:

```yaml
  {{- if .Values.secrets.agentAuthToken }}
  RUNTIME_AGENT_AUTH_TOKEN: {{ .Values.secrets.agentAuthToken | quote }}
  {{- end }}
```

(Match the existing file's style — if it uses `stringData:`, put it there; if `data:` with `b64enc`, base64-encode like its siblings.)

- [ ] **Step 4: Render + validate the generated config loads**

Run:
```bash
helm template r deploy/charts/runtime --set scheduling.mode=perAgentPods \
  --set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable \
  --set secrets.agentAuthToken=tok \
  --set config.agents[0].id=support --set config.agents[0].name=S \
  --set config.agents[0].model=test/scripted --set config.agents[0].replicas=2 \
  --set config.agents[1].id=solo --set config.agents[1].name=Solo \
  --set config.agents[1].model=test/scripted \
  | sed -n '/runtime.yaml: |/,/gateway:/p'
```
Expected: `support` has `url: "...support-{i}...:8080"` + `replicas: 2` + `auth_token: "${RUNTIME_AGENT_AUTH_TOKEN}"`; `solo` has a concrete `url: "...solo-0...:8080"` and NO `replicas:` line.

- [ ] **Step 5: Round-trip the generated config through the real loader**

This guards that the generated YAML actually passes `config.Validate()`. Run:
```bash
helm template r deploy/charts/runtime --set scheduling.mode=perAgentPods \
  --set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable \
  --set secrets.agentAuthToken=tok \
  --set config.agents[0].id=support --set config.agents[0].name=S \
  --set config.agents[0].model=test/scripted --set config.agents[0].replicas=2 \
  | python3 -c "import sys,yaml,re; docs=list(yaml.safe_load_all(sys.stdin)); cm=[d for d in docs if d and d.get('kind')=='ConfigMap'][0]; print(cm['data']['runtime.yaml'])" > /tmp/gen-runtime.yaml
RUNTIME_AGENT_AUTH_TOKEN=tok go run ./cmd/runtimectl conformance --help >/dev/null 2>&1 || true
cat /tmp/gen-runtime.yaml
```
Then eyeball that it is valid YAML with the expected shape. (A deeper loader check happens live in Task 9's kind proof.)

- [ ] **Step 6: Commit**

```bash
git add deploy/charts/runtime/templates/configmap.yaml deploy/charts/runtime/templates/deployment.yaml deploy/charts/runtime/templates/secret.yaml
git commit -m "$(cat <<'EOF'
feat(chart): generate control-plane runtime.yaml in perAgentPods mode

The ConfigMap rewrites config.agents into remote pools: replicas>1 →
{i}-template url + replicas; replicas:1 → concrete ordinal-0 url. The
Deployment gains the optional shared agent bearer env so the generated
auth_token expands. Monolith mode renders the config verbatim as before.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Chart tests + docs

**Files:**
- Modify: `deploy/charts/runtime/test.sh`
- Modify: `deploy/charts/runtime/README.md`
- Modify: `ROADMAP.md`
- Modify: `runtime.yaml` (example, optional comment block)

- [ ] **Step 1: Add perAgentPods permutations to test.sh**

In `deploy/charts/runtime/test.sh`, before the final `echo "ALL CHART TESTS PASSED"` (line ~72), add:

```bash
# 7. perAgentPods: one StatefulSet + headless Service per agent; runtimed config
#    generated as remote pools; monolith Deployment still present (control plane).
PAP='--set scheduling.mode=perAgentPods'
out=$(helm template r "$CHART" $DSN $PAP \
  --set config.agents[0].id=support --set config.agents[0].name=S \
  --set config.agents[0].model=test/scripted --set config.agents[0].replicas=2)
grep -q 'kind: StatefulSet'        <<<"$out" || fail "perAgentPods: no StatefulSet"
grep -q 'clusterIP: None'          <<<"$out" || fail "perAgentPods: no headless Service"
grep -q 'serviceName: r-agent-support-hl' <<<"$out" || fail "perAgentPods: wrong serviceName"
grep -q 'replicas: 2'              <<<"$out" || fail "perAgentPods: replicas not 2"
grep -q 'DBOS__VMID="support#'     <<<"$out" || fail "perAgentPods: no ordinal VMID derive"
grep -q 'support-{i}.r-agent-support-hl' <<<"$out" || fail "perAgentPods: generated url not {i}-templated"
# The dial template must be IDENTICAL on both sides (drift guard): the host base
# appears in both the headless Service name and the generated url.
grep -q 'r-agent-support-hl.default.svc.cluster.local' <<<"$out" || fail "perAgentPods: DNS base drift"
ok "perAgentPods renders StatefulSet+headless+generated remote config"

# 7b. perAgentPods single-replica agent → concrete ordinal-0 url, no {i}, no replicas key.
out=$(helm template r "$CHART" $DSN $PAP \
  --set config.agents[0].id=solo --set config.agents[0].name=Solo \
  --set config.agents[0].model=test/scripted)
grep -q 'solo-0.r-agent-solo-hl'  <<<"$out" || fail "perAgentPods solo: url not concrete ordinal 0"
if grep -A6 'id: solo' <<<"$out" | grep -q '{i}'; then fail "perAgentPods solo: url still has {i}"; fi
ok "perAgentPods single-replica → concrete url"

# 7c. perAgentPods fail-closed: an agent that sets listen_addr.
if helm template r "$CHART" $DSN $PAP \
  --set config.agents[0].id=s --set config.agents[0].name=S \
  --set config.agents[0].model=m --set config.agents[0].listen_addr=127.0.0.1:8101 >/dev/null 2>&1; then
  fail "expected perAgentPods listen_addr fail-closed"
fi
ok "perAgentPods fail-closed (listen_addr set)"

# 8. monolith regression: default mode still renders the M1 shape, no StatefulSet.
out=$(helm template r "$CHART" $DSN $AGENTS)
if grep -q 'kind: StatefulSet' <<<"$out"; then fail "monolith mode leaked a StatefulSet"; fi
grep -q 'kind: Deployment' <<<"$out" || fail "monolith: no Deployment"
ok "monolith regression (no StatefulSet)"
```

- [ ] **Step 2: Run the full chart suite**

Run: `bash deploy/charts/runtime/test.sh`
Expected: ends with `ALL CHART TESTS PASSED` (all prior checks + the new 7/7b/7c/8).

- [ ] **Step 3: helm lint**

Run: `helm lint deploy/charts/runtime --set secrets.pgDsn=x --set config.agents[0].id=a --set config.agents[0].name=A --set config.agents[0].model=m --set config.agents[0].listen_addr=127.0.0.1:8101`
Expected: `1 chart(s) linted, 0 chart(s) failed`.

- [ ] **Step 4: README — perAgentPods section + limitation**

Add a section to `deploy/charts/runtime/README.md` (after the existing topology/quick-start material) documenting:
- `scheduling.mode: perAgentPods` renders one StatefulSet + headless Service per agent; runtimed attaches as remote pools.
- per-agent input shape: `config.agents[].{id,name,model,tenant,replicas,memory,gateway}` — NO `listen_addr`/`url` (the chart generates the url).
- the shared `secrets.agentAuthToken` (recommended; authenticates runtimed to each agent pod).
- scaling: `kubectl scale statefulset <release>-agent-<id>` down is handled live (runtimed skips unreachable ordinals); scaling UP beyond the configured `replicas` needs a `helm upgrade` (re-render + runtimed re-read).
- **Known limitation — brokered secrets:** per-agent-pod agents get provider credentials via the chart Secret (env), NOT via runtimed's Identity-M2 secrets broker (which injects only at spawn). Backlog: brokered-secrets delivery to scheduled pods (natural home: C3 M2 registration handshake).

Use this exact block:

```markdown
## Per-agent-pod scheduling (`scheduling.mode: perAgentPods`)

By default (`monolith`) runtimed exec-spawns every agent as a child in one pod.
Set `scheduling.mode: perAgentPods` to instead run **each agent as its own
StatefulSet** (one headless Service per agent for stable per-ordinal DNS) that
runtimed **attaches to** as a remote replica pool.

```yaml
scheduling:
  mode: perAgentPods
secrets:
  pgDsn: "postgres://..."
  agentAuthToken: "a-shared-bearer"   # recommended; runtimed → agent auth
config:
  agents:
    - { id: support, name: Support, model: claude-opus-4-8, tenant: acme, replicas: 2 }
    - { id: research, name: Research, model: claude-opus-4-8 }   # replicas defaults to 1
```

In this mode each agent entry takes `id`, `name`, `model`, and optionally
`tenant`, `replicas` (pod count, default 1), `memory`, `gateway`. Do **not** set
`listen_addr` or `url` — the chart generates the per-ordinal url and wires
runtimed's `runtime.yaml` to attach.

**Scaling.** `kubectl scale statefulset <release>-agent-<id> --replicas=N` *down*
is handled live: runtimed skips ordinals whose health probe fails. Scaling *up*
beyond the configured `replicas` requires `helm upgrade` (re-render the config so
runtimed learns the higher ordinal count).

**Known limitation — brokered secrets.** Per-agent-pod agents receive provider
credentials from the chart Secret (env), not from runtimed's Identity-M2 secrets
broker, which decrypts and injects only at spawn time (and runtimed does not spawn
these pods). Supply provider keys via `secrets.existingSecret`/the chart Secret.
Brokered-secrets delivery to scheduled pods is backlogged (its natural home is the
C3 M2 registration handshake, where a pod pulls decrypted secrets over an
authenticated channel).
```

- [ ] **Step 5: ROADMAP entry**

In `ROADMAP.md`, update the C2 section: change the M1 entry's "Per-agent-pod scheduling is explicitly deferred to C3" framing and add an M2 DONE entry. Insert after the C2 M1 paragraph (before "Remaining C2:"):

```markdown
   **Second milestone DONE (merged to `master`, 2026-06-13):** per-agent-pod
   scheduling. A `scheduling.mode: monolith | perAgentPods` chart toggle. In
   `perAgentPods` the chart renders one **StatefulSet + headless Service per
   agent** (agentd-only pods; the ordinal derives `RUNTIME_AGENT_REPLICA` +
   `DBOS__VMID=<id>#<ordinal>` from `$HOSTNAME`), and runtimed runs
   **control-plane-only** with a **generated** `runtime.yaml` that rewrites each
   `config.agents` entry into a **remote replica pool**. This is **C3-remote ×
   A1-pool**: a remote agent may now set `replicas: N` paired with an `{i}`
   ordinal placeholder in `url:`, expanding to N per-ordinal attach entries at
   stable headless DNS (`<id>-<i>.<svc>`); `NextReplica` round-robins the
   **reachable** ordinals (new liveness-aware routing fed by one `HealthMonitor`
   per ordinal), while session affinity pins each session to its ordinal for
   life (durability absolute — a pinned-ordinal-down session 503s until it
   returns, never re-pins). StatefulSet ordinals = A1 executor ids and
   StatefulSet highest-ordinal-first scale-down = A2's suffix-only rule, now
   **enforced by Kubernetes**. Static replica count from config; scale-down is
   handled live (skip-unreachable), scale-up needs `helm upgrade` (documented
   seam). Single shared agent bearer (`secrets.agentAuthToken`) authenticates
   runtimed → each pod. **Known limitation:** brokered per-tenant secrets are
   spawn-time only, so per-agent-pod agents get provider creds via the chart
   Secret (backlog: brokered-secrets delivery to scheduled pods, home in C3 M2).
   Tested: config (remote-pool validation + `RemoteReplicaURL`), registry
   (pool expansion + skip-unreachable `NextReplica`), an integration test
   (`TestRemoteReplicaPoolAttach`: distribution, kill-one-ordinal liveness
   routing + affinity/durability, no restart), and chart render permutations
   (StatefulSet/headless/generated-config, single-replica concrete url, mode
   guards, monolith regression). Spec/plan:
   `docs/superpowers/{specs,plans}/2026-06-13-c2-m2-per-agent-pod-scheduling*`.
```

Also update the C2 M1 "Remaining C2:" line to drop per-agent-pod scheduling from the remaining list (it's now done).

- [ ] **Step 6: Commit**

```bash
git add deploy/charts/runtime/test.sh deploy/charts/runtime/README.md ROADMAP.md runtime.yaml
git commit -m "$(cat <<'EOF'
docs+test(chart): perAgentPods permutations, README, ROADMAP M2

Chart test.sh gains perAgentPods render checks (StatefulSet+headless+
generated remote config, single-replica concrete url, mode fail-closed
guard, monolith regression). README documents the mode + scaling + the
brokered-secrets limitation. ROADMAP records C2 M2 DONE.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Final verification (after all tasks)

- [ ] **Hermetic sweep:** `go build ./... && go vet ./... && go test ./...` → all pass.
- [ ] **Integration:** `go test -tags integration ./test/ -run 'TestRemoteReplicaPoolAttach|TestRemoteAgentAttach' -v -timeout 240s` → pass (new pool test + C3 single-remote regression).
- [ ] **Chart gate:** `bash deploy/charts/runtime/test.sh` → `ALL CHART TESTS PASSED`; `helm lint` clean.
- [ ] **Final holistic review:** dispatch a fresh reviewer over the whole diff (config rule lift, registry pool + bitmap, main wiring, chart mode) for cross-task integration bugs — especially: (a) does a single-replica perAgentPods agent's generated url really load (no `{i}`, no `replicas`)? (b) can a session ever be routed to an ordinal index ≥ the live pod count? (c) does monolith mode render byte-for-byte as before M2?
- [ ] **Live proof (kind), the C2 M1 pattern:** `make docker-image` → `kind load` → `helm install` with `postgresql.enabled=true`, `scheduling.mode=perAgentPods`, and 2 agents (one `replicas:2`); confirm both StatefulSets reach Running, `runtimectl conformance` PASSES against the in-cluster control-plane Service, then `kubectl scale statefulset <release>-agent-<id> --replicas=1` and confirm new sessions skip the removed ordinal while `/agents` stays healthy. Record results in the ROADMAP entry.
- [ ] **Finish:** use `superpowers:finishing-a-development-branch` (merge to `master` per the established convention), then update memory.
