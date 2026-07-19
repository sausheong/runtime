package store

import (
	"context"
	"testing"
)

func TestMemStoreSetSessionUsage(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	id, err := s.CreateSession(ctx, "a1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionUsage(ctx, id, 1500, 0.42); err != nil {
		t.Fatal(err)
	}
	row, err := s.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.TokensTotal != 1500 || row.CostUSD != 0.42 {
		t.Fatalf("got tokens=%d cost=%v, want 1500/0.42", row.TokensTotal, row.CostUSD)
	}
	// Idempotent absolute-set: calling again with the same value is stable.
	if err := s.SetSessionUsage(ctx, id, 1500, 0.42); err != nil {
		t.Fatal(err)
	}
	row, _ = s.GetSession(ctx, id)
	if row.TokensTotal != 1500 || row.CostUSD != 0.42 {
		t.Fatalf("absolute-set not idempotent: tokens=%d cost=%v", row.TokensTotal, row.CostUSD)
	}
}
