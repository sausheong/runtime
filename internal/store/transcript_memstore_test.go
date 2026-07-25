package store

import (
	"context"
	"testing"
)

func TestMemStoreTranscriptAndResults(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore() // use the existing constructor; check its real name in memstore.go
	sessionID, err := m.CreateSessionForTenant(ctx, "t1", "agent", 0)
	if err != nil {
		t.Fatal(err)
	}
	// idempotent transcript upsert on (session, turn)
	if err := m.AppendTranscript(ctx, sessionID, 0, "t1", "alice", []byte(`[{"x":1}]`), "completed", "completed"); err != nil {
		t.Fatal(err)
	}
	if err := m.AppendTranscript(ctx, sessionID, 0, "t1", "alice", []byte(`[{"x":2}]`), "completed", "completed"); err != nil {
		t.Fatal(err) // re-append same (s1,0) must upsert, not error
	}
	if err := m.AppendTranscript(ctx, sessionID, 1, "t2", "mallory", []byte(`[]`), "", ""); err != nil {
		t.Fatalf("transcript did not derive its tenant from the parent: %v", err)
	}
	// online results idempotent on (session, criterion)
	_ = m.PutOnlineResult(ctx, sessionID, "polite", "t1", "alice", "judge", true, "ok")
	_ = m.PutOnlineResult(ctx, sessionID, "polite", "t1", "alice", "judge", false, "changed") // upsert
	if err := m.PutOnlineResult(ctx, sessionID, "forged", "t2", "mallory", "judge", true, ""); err != nil {
		t.Fatalf("online result did not derive its tenant from the parent: %v", err)
	}
	res, _ := m.ListOnlineResults(ctx, sessionID)
	if len(res) != 2 {
		t.Fatalf("results upsert wrong: %+v", res)
	}
	var politeFound bool
	for _, result := range res {
		if result.Criterion == "polite" {
			politeFound = true
			if result.Passed || result.Detail != "changed" {
				t.Fatalf("polite result upsert wrong: %+v", result)
			}
		}
		if result.Tenant != "t1" {
			t.Fatalf("result tenant was not derived from parent: %+v", result)
		}
	}
	if !politeFound {
		t.Fatalf("polite result missing after upsert: %+v", res)
	}
	_ = m.PutOnlineResult(ctx, sessionID, "fmt", "t1", "alice", "regex", true, "")
	res2, _ := m.ListOnlineResults(ctx, sessionID)
	if len(res2) != 3 {
		t.Fatalf("want 3 criteria, got %d", len(res2))
	}
	byT, _ := m.ListOnlineResultsByTenant(ctx, "t1", 100)
	if len(byT) != 3 {
		t.Fatalf("by-tenant want 3, got %d", len(byT))
	}
	if other, _ := m.ListOnlineResultsByTenant(ctx, "t2", 100); len(other) != 0 {
		t.Fatal("cross-tenant leak")
	}
}
