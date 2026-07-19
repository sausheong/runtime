//go:build integration

package policy

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const pgTestDSN = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

func TestPGStoreConformance(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", pgTestDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	// Isolate from any prior run and clean up after.
	_, _ = db.ExecContext(ctx, `DELETE FROM gateway_policies WHERE tenant IN ('acme','globex')`)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM gateway_policies WHERE tenant IN ('acme','globex')`)
	})
	testStoreConformance(t, s)
}
