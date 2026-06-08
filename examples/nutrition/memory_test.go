package nutrition

import (
	"path/filepath"
	"testing"
)

func TestMemoryLearnsAlias(t *testing.T) {
	dir := t.TempDir()
	m := newMemory(filepath.Join(dir, "mem.json"))
	if got := m.learnedAlias("frobnicate stabiliser"); got != "" {
		t.Fatalf("expected no prior alias, got %q", got)
	}
	m.learnAlias("frobnicate stabiliser", "500")
	if got := m.learnedAlias("frobnicate stabiliser"); got != "500" {
		t.Errorf("alias not learned: got %q want 500", got)
	}
	m2 := newMemory(filepath.Join(dir, "mem.json"))
	if got := m2.learnedAlias("frobnicate stabiliser"); got != "500" {
		t.Errorf("alias not persisted: got %q want 500", got)
	}
}

func TestRecallProduct(t *testing.T) {
	dir := t.TempDir()
	m := newMemory(filepath.Join(dir, "mem.json"))
	if rec, ok := m.recall("Milo"); ok {
		t.Fatalf("unexpected prior record: %+v", rec)
	}
	m.remember(productRecord{ProductName: "Milo", Summary: "ok", Recommendation: "fine"})
	if rec, ok := m.recall("milo"); !ok || rec.Summary != "ok" {
		t.Errorf("recall failed: %+v ok=%v", rec, ok)
	}
}
