//go:build integration

package identity

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const dsn = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

func freshStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable at %s: %v", dsn, err)
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
	s, err := NewStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func TestStore_TenantsUsersKeys(t *testing.T) {
	ctx := context.Background()
	s, db := freshStore(t)
	defer db.Close()

	if err := s.CreateTenant(ctx, "alpha", "Team Alpha"); err != nil {
		t.Fatal(err)
	}
	ok, _ := s.TenantExists(ctx, "alpha")
	if !ok {
		t.Fatal("alpha should exist")
	}

	if err := s.UpsertUser(ctx, "alpha", "alice@corp", RoleOperator); err != nil {
		t.Fatal(err)
	}
	rows, err := s.UsersBySubject(ctx, "alice@corp")
	if err != nil || len(rows) != 1 || rows[0].TenantID != "alpha" || rows[0].Role != RoleOperator {
		t.Fatalf("UsersBySubject = %+v, %v", rows, err)
	}
	if rows, err := s.UsersBySubject(ctx, "ghost"); err != nil || len(rows) != 0 {
		t.Fatalf("ghost: rows=%v err=%v want empty,nil", rows, err)
	}

	mk, err := MintServiceKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertServiceKey(ctx, mk.ID, "alpha", mk.Hash, RoleViewer, "ci"); err != nil {
		t.Fatal(err)
	}
	k, err := s.ActiveKeyByID(ctx, mk.ID)
	if err != nil || k.TenantID != "alpha" || k.Role != RoleViewer {
		t.Fatalf("ActiveKeyByID = %+v, %v", k, err)
	}
	if err := s.RevokeKey(ctx, "alpha", mk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActiveKeyByID(ctx, mk.ID); err != ErrNoKey {
		t.Fatalf("revoked key: err=%v want ErrNoKey", err)
	}
}

func TestStore_RoleCheckConstraint(t *testing.T) {
	ctx := context.Background()
	s, db := freshStore(t)
	defer db.Close()
	_ = s.CreateTenant(ctx, "alpha", "A")
	_, err := db.ExecContext(ctx,
		`INSERT INTO identity_users (tenant_id, subject, role) VALUES ('alpha','x','superuser')`)
	if err == nil {
		t.Fatal("expected CHECK violation for invalid role")
	}
}

func TestStore_AnyConfigured(t *testing.T) {
	ctx := context.Background()
	s, db := freshStore(t)
	defer db.Close()
	if any, _ := s.AnyConfigured(ctx); any {
		t.Fatal("fresh store should report not configured")
	}
	_ = s.CreateTenant(ctx, "alpha", "A")
	if any, _ := s.AnyConfigured(ctx); !any {
		t.Fatal("after a tenant, should report configured")
	}
}

func TestStore_UpsertUserMultiTenant(t *testing.T) {
	ctx := context.Background()
	s, db := freshStore(t)
	defer db.Close()
	_ = s.CreateTenant(ctx, "alpha", "A")
	_ = s.CreateTenant(ctx, "beta", "B")
	if err := s.UpsertUser(ctx, "alpha", "alice@corp", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	// Same subject into a SECOND tenant must ADD, not rehome.
	if err := s.UpsertUser(ctx, "beta", "alice@corp", RoleViewer); err != nil {
		t.Fatal(err)
	}
	rows, err := s.UsersBySubject(ctx, "alice@corp")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("memberships = %d, want 2", len(rows))
	}
	got := map[string]Role{}
	for _, r := range rows {
		got[r.TenantID] = r.Role
	}
	if got["alpha"] != RoleAdmin || got["beta"] != RoleViewer {
		t.Fatalf("memberships = %v, want alpha=admin beta=viewer", got)
	}
	// Re-upsert into alpha updates role in place, no extra row.
	if err := s.UpsertUser(ctx, "alpha", "alice@corp", RoleOperator); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.UsersBySubject(ctx, "alice@corp")
	if len(rows) != 2 {
		t.Fatalf("after role update memberships = %d, want 2", len(rows))
	}
}

func TestStore_UsersBySubjectEmpty(t *testing.T) {
	ctx := context.Background()
	s, db := freshStore(t)
	defer db.Close()
	rows, err := s.UsersBySubject(ctx, "nobody@nowhere")
	if err != nil {
		t.Fatalf("UsersBySubject empty must not error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

func TestStore_RevokeKeyCrossTenantNoOp(t *testing.T) {
	ctx := context.Background()
	s, db := freshStore(t)
	defer db.Close()
	_ = s.CreateTenant(ctx, "alpha", "A")
	_ = s.CreateTenant(ctx, "beta", "B")
	mk, _ := MintServiceKey()
	if err := s.InsertServiceKey(ctx, mk.ID, "alpha", mk.Hash, RoleOperator, "k"); err != nil {
		t.Fatal(err)
	}
	// Revoking under the WRONG tenant must NOT revoke the key.
	if err := s.RevokeKey(ctx, "beta", mk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActiveKeyByID(ctx, mk.ID); err != nil {
		t.Fatalf("key wrongly revoked by cross-tenant call: %v", err)
	}
	// Correct tenant revokes it.
	if err := s.RevokeKey(ctx, "alpha", mk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActiveKeyByID(ctx, mk.ID); err != ErrNoKey {
		t.Fatalf("key should be revoked now: err=%v want ErrNoKey", err)
	}
}

func TestStore_ListKeysOmitsHashAndShowsRevoked(t *testing.T) {
	ctx := context.Background()
	s, db := freshStore(t)
	defer db.Close()
	_ = s.CreateTenant(ctx, "alpha", "A")
	mk, _ := MintServiceKey()
	_ = s.InsertServiceKey(ctx, mk.ID, "alpha", mk.Hash, RoleViewer, "ci")
	keys, err := s.ListKeys(ctx, "alpha")
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys = %+v, %v", keys, err)
	}
	k := keys[0]
	if k.ID != mk.ID || k.Role != RoleViewer || k.Label != "ci" || k.Revoked {
		t.Fatalf("unexpected key row: %+v", k)
	}
	// KeyRow has no hash field by design — this is a compile-time guarantee that
	// listings never carry the secret. Revoke and confirm the flag flips.
	_ = s.RevokeKey(ctx, "alpha", mk.ID)
	keys, _ = s.ListKeys(ctx, "alpha")
	if len(keys) != 1 || !keys[0].Revoked {
		t.Fatalf("after revoke, Revoked flag should be true: %+v", keys)
	}
}
