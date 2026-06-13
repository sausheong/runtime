//go:build integration

package identity

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func regDSN() string {
	if v := os.Getenv("RUNTIME_PG_DSN"); v != "" {
		return v
	}
	return "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"
}

func TestRegistrationTokenCRUD(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", regDSN())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	// Self-clean.
	_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS registration_tokens`)

	st, err := NewStore(ctx, db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mk, err := MintServiceKey()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := st.InsertRegistrationToken(ctx, mk.ID, "agent-x", mk.Hash); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Active lookup resolves agent_id + hash.
	agentID, hash, err := st.ActiveRegTokenByID(ctx, mk.ID)
	if err != nil || agentID != "agent-x" || hash != mk.Hash {
		t.Fatalf("active lookup: agent=%q hash=%q err=%v", agentID, hash, err)
	}
	// List shows it, never the secret.
	rows, err := st.ListRegistrationTokens(ctx)
	if err != nil || len(rows) != 1 || rows[0].AgentID != "agent-x" || rows[0].Revoked {
		t.Fatalf("list: %+v err=%v", rows, err)
	}
	// Revoke → active lookup fails closed.
	if err := st.RevokeRegistrationToken(ctx, mk.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := st.ActiveRegTokenByID(ctx, mk.ID); err != ErrNoRegToken {
		t.Fatalf("want ErrNoRegToken after revoke, got %v", err)
	}
	rows, _ = st.ListRegistrationTokens(ctx)
	if len(rows) != 1 || !rows[0].Revoked {
		t.Fatalf("list after revoke: %+v", rows)
	}
}
