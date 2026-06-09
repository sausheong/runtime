//go:build integration

package test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	hmem "github.com/sausheong/harness/tool/memory"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/memory"
)

func TestMemoryE2E_PerTenantIsolationAndEnvInjection(t *testing.T) {
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

	// --- Part A: store-level per-tenant isolation over the real DB ---
	alpha, err := memory.NewStore(ctx, db, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := memory.NewStore(ctx, db, "beta")
	if err != nil {
		t.Fatal(err)
	}
	a1, err := alpha.Save(ctx, hmem.Entry{Content: "alpha-fact", Origin: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alpha.Save(ctx, hmem.Entry{Content: "alpha-fact-2", Origin: "agent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := beta.Save(ctx, hmem.Entry{Content: "beta-fact", Origin: "agent"}); err != nil {
		t.Fatal(err)
	}

	// Two agents in the SAME tenant share the pool: a second alpha store sees a1.
	alpha2, err := memory.NewStore(ctx, db, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := alpha2.Get(ctx, a1.ID); !ok || got.Content != "alpha-fact" {
		t.Fatalf("same-tenant agents must share the pool; ok=%v got=%+v", ok, got)
	}
	// Different tenant cannot see it.
	if _, ok, _ := beta.Get(ctx, a1.ID); ok {
		t.Fatal("cross-tenant leak: beta saw alpha's entry")
	}
	la, _ := alpha.List(ctx, "")
	lb, _ := beta.List(ctx, "")
	if len(la) != 2 {
		t.Fatalf("alpha pool size = %d, want 2", len(la))
	}
	if len(lb) != 1 || lb[0].Content != "beta-fact" {
		t.Fatalf("beta pool wrong: %+v", lb)
	}

	// --- Part B: the platform injects RUNTIME_AGENT_TENANT + _MEMORY into spawn ---
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	cfg := &config.Config{Agents: []config.AgentConfig{{
		ID:         "agent-alpha",
		ListenAddr: "127.0.0.1:0",
		Tenant:     "alpha",
		Memory:     true,
		Command:    []string{"sh", "-c", "env > " + out},
	}}}
	reg := controlplane.NewRegistry(cfg, "./agentd", dsn)
	ap, ok := reg.Get("agent-alpha")
	if !ok {
		t.Fatal("agent-alpha not found")
	}
	env := spawnAndWaitEnv(t, ap, out)
	if !strings.Contains(env, "RUNTIME_AGENT_TENANT=alpha") {
		t.Fatalf("spawn env missing tenant:\n%s", env)
	}
	if !strings.Contains(env, "RUNTIME_AGENT_MEMORY=1") {
		t.Fatalf("spawn env missing memory flag:\n%s", env)
	}
}
