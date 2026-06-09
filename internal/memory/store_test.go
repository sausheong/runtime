//go:build integration

package memory

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	hmem "github.com/sausheong/harness/tool/memory"
)

const dsn = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

// freshStore opens the DB, drops + recreates memory_events, and returns a Store
// pinned to tenant. t.Cleanup drops the table so sibling tests don't see it.
func freshStore(t *testing.T, tenant string) (*Store, *sql.DB) {
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
	st, err := NewStore(context.Background(), db, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return st, db
}

// compile-time proof the Store satisfies the harness interface.
var _ hmem.MemoryStore = (*Store)(nil)

func TestStore_SaveGetRoundTrip(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()

	saved, err := st.Save(ctx, hmem.Entry{Content: "hello", Tags: []string{"x"}, Origin: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" || saved.Content != "hello" {
		t.Fatalf("bad saved entry: %+v", saved)
	}
	if !saved.CreatedAt.Equal(saved.UpdatedAt) {
		t.Fatalf("fresh entry CreatedAt != UpdatedAt: %+v", saved)
	}
	got, ok, err := st.Get(ctx, saved.ID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Content != "hello" || got.Origin != "agent" || len(got.Tags) != 1 || got.Tags[0] != "x" {
		t.Fatalf("get mismatch: %+v", got)
	}
}

func TestStore_ListOrderingAndTagFilter(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	a, _ := st.Save(ctx, hmem.Entry{Content: "first", Tags: []string{"k"}})
	b, _ := st.Save(ctx, hmem.Entry{Content: "second"})
	all, err := st.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != a.ID || all[1].ID != b.ID {
		t.Fatalf("list order wrong: %+v", all)
	}
	tagged, _ := st.List(ctx, "k")
	if len(tagged) != 1 || tagged[0].ID != a.ID {
		t.Fatalf("tag filter wrong: %+v", tagged)
	}
}

func TestStore_UpdateSupersedes(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	orig, _ := st.Save(ctx, hmem.Entry{Content: "v1", Tags: []string{"t"}, Origin: "agent"})
	upd, err := st.Update(ctx, orig.ID, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if upd.ID == orig.ID {
		t.Fatal("Update must mint a fresh id")
	}
	if !upd.CreatedAt.Equal(orig.CreatedAt) {
		t.Fatalf("Update must preserve birth CreatedAt: orig=%v new=%v", orig.CreatedAt, upd.CreatedAt)
	}
	if upd.UpdatedAt.Before(orig.UpdatedAt) {
		t.Fatalf("UpdatedAt not advanced: %v", upd.UpdatedAt)
	}
	if len(upd.Tags) != 1 || upd.Tags[0] != "t" || upd.Origin != "agent" {
		t.Fatalf("Update must carry tags+origin: %+v", upd)
	}
	if _, ok, _ := st.Get(ctx, orig.ID); ok {
		t.Fatal("old id must be invalid after update")
	}
	all, _ := st.List(ctx, "")
	if len(all) != 1 || all[0].ID != upd.ID || all[0].Content != "v2" {
		t.Fatalf("after update want one live row v2: %+v", all)
	}
	if _, err := st.Update(ctx, "mem_2000-01-01_deadbeef", "x"); err != hmem.ErrNotFound {
		t.Fatalf("update unknown id: want ErrNotFound, got %v", err)
	}
}

func TestStore_RemoveTombstone(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, _ := st.Save(ctx, hmem.Entry{Content: "doomed"})
	if err := st.Remove(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Get(ctx, e.ID); ok {
		t.Fatal("removed entry must be gone")
	}
	all, _ := st.List(ctx, "")
	if len(all) != 0 {
		t.Fatalf("list after remove must be empty: %+v", all)
	}
	if err := st.Remove(ctx, "mem_2000-01-01_deadbeef"); err != nil {
		t.Fatalf("remove unknown id must be nil: %v", err)
	}
}

func TestStore_OriginPersistedVerbatim(t *testing.T) {
	st, db := freshStore(t, "alpha")
	defer db.Close()
	ctx := context.Background()
	e, _ := st.Save(ctx, hmem.Entry{Content: "r", Origin: "review"})
	got, _, _ := st.Get(ctx, e.ID)
	if got.Origin != "review" {
		t.Fatalf("origin not persisted verbatim: %q", got.Origin)
	}
}

func TestStore_CrossTenantIsolation(t *testing.T) {
	alpha, db := freshStore(t, "alpha")
	defer db.Close()
	beta, err := NewStore(context.Background(), db, "beta")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ae, _ := alpha.Save(ctx, hmem.Entry{Content: "alpha-secret"})

	if list, _ := beta.List(ctx, ""); len(list) != 0 {
		t.Fatalf("beta must not see alpha's entries: %+v", list)
	}
	if _, ok, _ := beta.Get(ctx, ae.ID); ok {
		t.Fatal("beta must not Get alpha's entry")
	}
	if _, err := beta.Update(ctx, ae.ID, "hijack"); err != hmem.ErrNotFound {
		t.Fatalf("beta update of alpha id: want ErrNotFound, got %v", err)
	}
	if err := beta.Remove(ctx, ae.ID); err != nil {
		t.Fatalf("beta remove of alpha id must no-op nil: %v", err)
	}
	if _, ok, _ := alpha.Get(ctx, ae.ID); !ok {
		t.Fatal("alpha lost its entry after beta's no-op remove")
	}
}
