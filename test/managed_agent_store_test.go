//go:build integration

package test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sausheong/runtime/internal/agentstore"
	"github.com/sausheong/runtime/internal/identity"
)

func TestManagedAgentStoreCRUD(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := identity.NewStore(ctx, db); err != nil {
		t.Fatal(err)
	}
	st, err := agentstore.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM managed_agents WHERE tenant_id='mat'`)
	_, _ = db.ExecContext(ctx, `DELETE FROM tenants WHERE id='mat'`)
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants(id,name) VALUES('mat','mat')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM managed_agents WHERE tenant_id='mat'`)
		_, _ = db.ExecContext(ctx, `DELETE FROM tenants WHERE id='mat'`)
	})

	row := agentstore.AgentRow{
		ID: "ma-1", TenantID: "mat", Name: "Hello", Model: "claude-sonnet-4-6",
		URL: "http://10.0.0.4:8080", AuthSecret: "",
		RegistrationGeneration: "test-generation",
	}
	if err := st.Insert(ctx, row); err != nil {
		t.Fatal(err)
	}
	// duplicate id must fail (PK)
	if err := st.Insert(ctx, row); err == nil {
		t.Fatal("expected duplicate id to fail")
	}

	got, err := st.List(ctx, "mat")
	if err != nil || len(got) != 1 || got[0].Name != "Hello" || !got[0].Enabled {
		t.Fatalf("list mismatch: %+v err=%v", got, err)
	}
	if got[0].URL != "http://10.0.0.4:8080" {
		t.Fatalf("url mismatch: %q", got[0].URL)
	}

	one, ok, err := st.Get(ctx, "ma-1")
	if err != nil || !ok || one.Model != "claude-sonnet-4-6" {
		t.Fatalf("get mismatch: %+v ok=%v err=%v", one, ok, err)
	}

	// disable, confirm it persists
	updated, err := st.SetEnabled(ctx, "mat", "ma-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatal("SetEnabled(false) did not report the tenant-owned row")
	}
	one, _, _ = st.Get(ctx, "ma-1")
	if one.Enabled {
		t.Fatal("SetEnabled(false) did not persist")
	}

	// cross-tenant delete is a no-op (scoped)
	removed, err := st.Delete(ctx, "other", "ma-1")
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("cross-tenant delete reported a removed row")
	}
	if _, ok, _ := st.Get(ctx, "ma-1"); !ok {
		t.Fatal("cross-tenant delete must not remove the row")
	}

	removed, err = st.Delete(ctx, "mat", "ma-1")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("tenant-owned delete did not report the removed row")
	}
	got, _ = st.List(ctx, "mat")
	if len(got) != 0 {
		t.Fatalf("expected empty after delete, got %+v", got)
	}
}
