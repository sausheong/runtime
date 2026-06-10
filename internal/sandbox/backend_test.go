package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFakeBackendLifecycle(t *testing.T) {
	ctx := context.Background()
	be := NewFakeBackend()

	id, err := be.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}

	if err := be.WriteFile(ctx, id, "/workspace/a.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, truncated, err := be.ReadFile(ctx, id, "/workspace/a.txt", 1024)
	if err != nil || truncated || string(got) != "hello" {
		t.Fatalf("got %q, trunc=%v, err=%v", got, truncated, err)
	}

	got, truncated, err = be.ReadFile(ctx, id, "/workspace/a.txt", 3)
	if err != nil || !truncated || string(got) != "hel" {
		t.Fatalf("got %q, trunc=%v, err=%v", got, truncated, err)
	}

	res, err := be.Exec(ctx, id, []string{"python3", "-c", "print(1)"}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "python3 -c print(1)") {
		t.Fatalf("unexpected exec result: %+v", res)
	}

	if err := be.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := be.ReadFile(ctx, id, "/workspace/a.txt", 10); err == nil {
		t.Fatal("read after remove should error")
	}
}

func TestFakeBackendLeftovers(t *testing.T) {
	ctx := context.Background()
	be := NewFakeBackend()
	a, _ := be.Create(ctx, "t1")
	b, _ := be.Create(ctx, "t2")
	ids, err := be.ListLeftovers(ctx)
	if err != nil || len(ids) != 2 {
		t.Fatalf("got %v, %v", ids, err)
	}
	_ = be.Remove(ctx, a)
	ids, _ = be.ListLeftovers(ctx)
	if len(ids) != 1 || ids[0] != b {
		t.Fatalf("got %v", ids)
	}
}
