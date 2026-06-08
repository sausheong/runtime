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
	u, err := s.UserBySubject(ctx, "alice@corp")
	if err != nil || u.TenantID != "alpha" || u.Role != RoleOperator {
		t.Fatalf("UserBySubject = %+v, %v", u, err)
	}
	if _, err := s.UserBySubject(ctx, "ghost"); err != ErrNoUser {
		t.Fatalf("ghost: err=%v want ErrNoUser", err)
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
