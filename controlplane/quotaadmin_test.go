package controlplane

import (
	"context"
	"testing"

	"github.com/sausheong/runtime/internal/quota"
)

// RegisterQuotaShared must reject a '*' tenant write from a non-superuser and
// a cross-tenant write, but allow an own-tenant write.
func TestQuotaSharedRBAC(t *testing.T) {
	ctx := context.Background()
	st := quota.NewMemStore()

	// Own-tenant write ok.
	if err := RegisterQuotaShared(ctx, st, "acme", false, "acme", "orders", 60); err != nil {
		t.Fatalf("own-tenant write must succeed: %v", err)
	}
	// '*' tenant by non-superuser rejected.
	if err := RegisterQuotaShared(ctx, st, "acme", false, "*", "orders", 60); err == nil {
		t.Error("non-superuser '*' tenant write must be rejected")
	}
	// Superuser may write '*'.
	if err := RegisterQuotaShared(ctx, st, "", true, "*", "orders", 60); err != nil {
		t.Errorf("superuser '*' write must succeed: %v", err)
	}
}
