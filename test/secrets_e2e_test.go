//go:build integration

package test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/identity"
)

// spawnAndWaitEnv runs an agent's SpawnFunc with a Command that dumps its env to
// outPath, waits for exit, and returns the file contents.
func spawnAndWaitEnv(t *testing.T, ap controlplane.AgentProcess, outPath string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wait := ap.SpawnFunc()(ctx)
	select {
	case err := <-wait:
		if err != nil {
			t.Fatalf("spawn exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spawn did not exit in time")
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	return string(b)
}

func TestSecretsE2E_PerTenantInjection(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	for _, q := range []string{
		`DROP TABLE IF EXISTS secrets CASCADE`,
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range []string{
			`DROP TABLE IF EXISTS secrets CASCADE`,
			`DROP TABLE IF EXISTS service_keys CASCADE`,
			`DROP TABLE IF EXISTS identity_users CASCADE`,
			`DROP TABLE IF EXISTS tenants CASCADE`,
		} {
			_, _ = cdb.Exec(q)
		}
	})

	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := identity.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := identity.NewKeyring(map[string]*identity.Cipher{"v1": cipher}, "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	broker := identity.NewBroker(st, kr)

	for _, tn := range []string{"alpha", "beta", "gamma"} {
		if err := st.CreateTenant(ctx, tn, tn); err != nil {
			t.Fatal(err)
		}
	}
	if err := broker.SetSecret(ctx, "alpha", "OPENAI_API_KEY", "sk-alpha"); err != nil {
		t.Fatal(err)
	}
	if err := broker.SetSecret(ctx, "beta", "OPENAI_API_KEY", "sk-beta"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	outFile := func(tn string) string { return filepath.Join(dir, tn+".env") }
	mkAgent := func(tn string) config.AgentConfig {
		out := outFile(tn)
		return config.AgentConfig{
			ID:         "agent-" + tn,
			ListenAddr: "127.0.0.1:0",
			Tenant:     tn,
			Command:    []string{"sh", "-c", "env > " + out},
		}
	}
	cfg := &config.Config{Agents: []config.AgentConfig{mkAgent("alpha"), mkAgent("beta"), mkAgent("gamma")}}
	reg := controlplane.NewRegistry(cfg, "./agentd", dsn)
	reg.SetBroker(broker)

	// Set the operator fallback BEFORE spawning: buildEnv snapshots os.Environ()
	// at spawn time, so this must precede the spawnAndWaitEnv calls below. The
	// no-secret tenant (gamma) inherits this value.
	t.Setenv("OPENAI_API_KEY", "sk-operator-fallback")

	getAgent := func(id string) controlplane.AgentProcess {
		ap, ok := reg.Get(id)
		if !ok {
			t.Fatalf("agent %q not found in registry", id)
		}
		return ap
	}
	apAlpha := getAgent("agent-alpha")
	apBeta := getAgent("agent-beta")
	apGamma := getAgent("agent-gamma")

	envAlpha := spawnAndWaitEnv(t, apAlpha, outFile("alpha"))
	envBeta := spawnAndWaitEnv(t, apBeta, outFile("beta"))
	envGamma := spawnAndWaitEnv(t, apGamma, outFile("gamma"))

	if !strings.Contains(envAlpha, "OPENAI_API_KEY=sk-alpha") {
		t.Fatalf("alpha did not get its secret:\n%s", envAlpha)
	}
	if !strings.Contains(envBeta, "OPENAI_API_KEY=sk-beta") {
		t.Fatalf("beta did not get its secret:\n%s", envBeta)
	}
	if strings.Contains(envAlpha, "sk-beta") || strings.Contains(envBeta, "sk-alpha") {
		t.Fatal("cross-tenant secret leak")
	}
	if !strings.Contains(envGamma, "OPENAI_API_KEY=sk-operator-fallback") {
		t.Fatalf("gamma did not fall back to operator env:\n%s", envGamma)
	}
}

func TestSecretsE2E_KeyRotation(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	for _, q := range []string{
		`DROP TABLE IF EXISTS secrets CASCADE`,
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range []string{
			`DROP TABLE IF EXISTS secrets CASCADE`,
			`DROP TABLE IF EXISTS service_keys CASCADE`,
			`DROP TABLE IF EXISTS identity_users CASCADE`,
			`DROP TABLE IF EXISTS tenants CASCADE`,
		} {
			_, _ = cdb.Exec(q)
		}
	})

	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "alpha", "A"); err != nil {
		t.Fatal(err)
	}

	mkKey := func(seed byte) []byte {
		k := make([]byte, 32)
		for i := range k {
			k[i] = seed + byte(i)
		}
		return k
	}
	cOld, _ := identity.NewCipher(mkKey(1))
	cNew, _ := identity.NewCipher(mkKey(100))

	// Phase 1: seal under an old-only ring.
	krOld, _ := identity.NewKeyring(map[string]*identity.Cipher{"v1": cOld}, "v1", "v1")
	if err := identity.NewBroker(st, krOld).SetSecret(ctx, "alpha", "OPENAI_API_KEY", "sk-rot"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	mkAgent := func(out string) config.AgentConfig {
		return config.AgentConfig{
			ID:         "agent-alpha",
			ListenAddr: "127.0.0.1:0",
			Tenant:     "alpha",
			Command:    []string{"sh", "-c", "env > " + out},
		}
	}
	spawnWith := func(broker controlplane.SecretBroker, out string) string {
		cfg := &config.Config{Agents: []config.AgentConfig{mkAgent(out)}}
		reg := controlplane.NewRegistry(cfg, "./agentd", dsn)
		reg.SetBroker(broker)
		ap, ok := reg.Get("agent-alpha")
		if !ok {
			t.Fatal("agent-alpha not found")
		}
		return spawnAndWaitEnv(t, ap, out)
	}

	env1 := spawnWith(identity.NewBroker(st, krOld), filepath.Join(dir, "p1.env"))
	if !strings.Contains(env1, "OPENAI_API_KEY=sk-rot") {
		t.Fatalf("phase1 missing secret:\n%s", env1)
	}

	// Phase 2: add a new primary (v2), keep v1 as legacy, rotate.
	krBoth, _ := identity.NewKeyring(map[string]*identity.Cipher{"v1": cOld, "v2": cNew}, "v2", "v1")
	rstat, err := identity.NewBroker(st, krBoth).Rotate(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if rstat.Rotated != 1 || rstat.Failed != 0 {
		t.Fatalf("rotate stats = %+v", rstat)
	}
	env2 := spawnWith(identity.NewBroker(st, krBoth), filepath.Join(dir, "p2.env"))
	if !strings.Contains(env2, "OPENAI_API_KEY=sk-rot") {
		t.Fatalf("phase2 (post-rotate) missing secret:\n%s", env2)
	}

	// Phase 3: retire the old key entirely (v2-only ring) and prove spawn works.
	krNew, _ := identity.NewKeyring(map[string]*identity.Cipher{"v2": cNew}, "v2", "")
	env3 := spawnWith(identity.NewBroker(st, krNew), filepath.Join(dir, "p3.env"))
	if !strings.Contains(env3, "OPENAI_API_KEY=sk-rot") {
		t.Fatalf("phase3 (old key retired) missing secret:\n%s", env3)
	}
}
