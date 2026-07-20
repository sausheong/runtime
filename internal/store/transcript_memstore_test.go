package store

import (
	"context"
	"testing"
)

func TestMemStoreTranscriptAndResults(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore() // use the existing constructor; check its real name in memstore.go
	// idempotent transcript upsert on (session, turn)
	if err := m.AppendTranscript(ctx, "s1", 0, "t1", "alice", []byte(`[{"x":1}]`), "completed", "completed"); err != nil {
		t.Fatal(err)
	}
	if err := m.AppendTranscript(ctx, "s1", 0, "t1", "alice", []byte(`[{"x":2}]`), "completed", "completed"); err != nil {
		t.Fatal(err) // re-append same (s1,0) must upsert, not error
	}
	// online results idempotent on (session, criterion)
	_ = m.PutOnlineResult(ctx, "s1", "polite", "t1", "alice", "judge", true, "ok")
	_ = m.PutOnlineResult(ctx, "s1", "polite", "t1", "alice", "judge", false, "changed") // upsert
	res, _ := m.ListOnlineResults(ctx, "s1")
	if len(res) != 1 || res[0].Passed != false || res[0].Detail != "changed" {
		t.Fatalf("results upsert wrong: %+v", res)
	}
	_ = m.PutOnlineResult(ctx, "s1", "fmt", "t1", "alice", "regex", true, "")
	res2, _ := m.ListOnlineResults(ctx, "s1")
	if len(res2) != 2 {
		t.Fatalf("want 2 criteria, got %d", len(res2))
	}
	byT, _ := m.ListOnlineResultsByTenant(ctx, "t1", 100)
	if len(byT) != 2 {
		t.Fatalf("by-tenant want 2, got %d", len(byT))
	}
	if other, _ := m.ListOnlineResultsByTenant(ctx, "t2", 100); len(other) != 0 {
		t.Fatal("cross-tenant leak")
	}
}
