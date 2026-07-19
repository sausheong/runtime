package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/sausheong/runtime/internal/identity"
)

// fakeVerifier accepts exactly one known token→subject; anything else errors.
type fakeVerifier struct {
	token   string
	subject string
}

func (f fakeVerifier) Verify(_ context.Context, raw string) (string, error) {
	if raw == f.token {
		return f.subject, nil
	}
	return "", errors.New("invalid token")
}

// fakeUsers maps a subject to a fixed set of tenant rows. A subject absent from
// the map returns an empty slice (no error), matching the store's contract.
type fakeUsers struct {
	rows map[string][]identity.UserRow
	err  error
}

func (f fakeUsers) UsersBySubject(_ context.Context, subject string) ([]identity.UserRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[subject], nil
}

func newVerifyHandler(v identity.OIDCVerifier, u UserTenantSource, agentTenant string) *Handler {
	h := &Handler{Assertion: v, Users: u}
	h.PrincipalFor = func(context.Context) (identity.Principal, bool) {
		return identity.Principal{TenantID: agentTenant, Subject: "svk-agent", Kind: identity.KindServiceKey}, true
	}
	return h
}

func TestVerifyCallerAssertion(t *testing.T) {
	row := func(tenant, sub string) identity.UserRow {
		return identity.UserRow{TenantID: tenant, Subject: sub, Role: identity.RoleOperator}
	}

	t.Run("lands on verify+match", func(t *testing.T) {
		h := newVerifyHandler(
			fakeVerifier{token: "good.jwt", subject: "alice"},
			fakeUsers{rows: map[string][]identity.UserRow{"alice": {row("acme", "alice")}}},
			"acme",
		)
		ctx, ok := h.verifyCallerAssertion(context.Background(), "good.jwt")
		if !ok {
			t.Fatal("want ok=true (verified, tenant match)")
		}
		sub, jwt, carried := CallerAssertionFrom(ctx)
		if !carried || sub != "alice" || jwt != "good.jwt" {
			t.Fatalf("carrier = (%q,%q,%v), want ('alice','good.jwt',true)", sub, jwt, carried)
		}
	})

	t.Run("fail-closed on verify error", func(t *testing.T) {
		h := newVerifyHandler(
			fakeVerifier{token: "good.jwt", subject: "alice"},
			fakeUsers{rows: map[string][]identity.UserRow{"alice": {row("acme", "alice")}}},
			"acme",
		)
		ctx, ok := h.verifyCallerAssertion(context.Background(), "bad.jwt")
		assertNotLanded(t, ctx, ok)
	})

	t.Run("fail-closed on subject not found (0 rows)", func(t *testing.T) {
		h := newVerifyHandler(
			fakeVerifier{token: "good.jwt", subject: "ghost"},
			fakeUsers{rows: map[string][]identity.UserRow{}},
			"acme",
		)
		ctx, ok := h.verifyCallerAssertion(context.Background(), "good.jwt")
		assertNotLanded(t, ctx, ok)
	})

	t.Run("fail-closed on ambiguous subject (>1 rows)", func(t *testing.T) {
		h := newVerifyHandler(
			fakeVerifier{token: "good.jwt", subject: "alice"},
			fakeUsers{rows: map[string][]identity.UserRow{"alice": {row("acme", "alice"), row("other", "alice")}}},
			"acme",
		)
		ctx, ok := h.verifyCallerAssertion(context.Background(), "good.jwt")
		assertNotLanded(t, ctx, ok)
	})

	t.Run("fail-closed on store error", func(t *testing.T) {
		h := newVerifyHandler(
			fakeVerifier{token: "good.jwt", subject: "alice"},
			fakeUsers{err: errors.New("db down")},
			"acme",
		)
		ctx, ok := h.verifyCallerAssertion(context.Background(), "good.jwt")
		assertNotLanded(t, ctx, ok)
	})

	t.Run("fail-closed on tenant mismatch", func(t *testing.T) {
		h := newVerifyHandler(
			fakeVerifier{token: "bob.jwt", subject: "bob"},
			fakeUsers{rows: map[string][]identity.UserRow{"bob": {row("other", "bob")}}},
			"acme",
		)
		ctx, ok := h.verifyCallerAssertion(context.Background(), "bob.jwt")
		assertNotLanded(t, ctx, ok)
	})

	t.Run("fail-closed on no agent principal", func(t *testing.T) {
		h := newVerifyHandler(
			fakeVerifier{token: "good.jwt", subject: "alice"},
			fakeUsers{rows: map[string][]identity.UserRow{"alice": {row("acme", "alice")}}},
			"acme",
		)
		h.PrincipalFor = func(context.Context) (identity.Principal, bool) {
			return identity.Principal{}, false
		}
		ctx, ok := h.verifyCallerAssertion(context.Background(), "good.jwt")
		assertNotLanded(t, ctx, ok)
	})
}

func assertNotLanded(t *testing.T, ctx context.Context, ok bool) {
	t.Helper()
	if ok {
		t.Fatal("want ok=false (fail-closed)")
	}
	if _, _, carried := CallerAssertionFrom(ctx); carried {
		t.Fatal("carrier must not be set on the returned ctx when fail-closed")
	}
}
