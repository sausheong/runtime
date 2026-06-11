//go:build integration

package test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/identity"
)

func TestIdentityE2E_TwoTenants(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	for _, q := range []string{
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	// Clean up identity tables at test end so we leave the shared DB as we
	// found it. Otherwise leftover tenant/key rows make AnyConfigured() true,
	// flipping runtimed into enforced mode for sibling integration tests whose
	// unauthenticated health probes then 401. Use a fresh connection because
	// the deferred db.Close() above runs before t.Cleanup functions.
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range []string{
			`DROP TABLE IF EXISTS service_keys CASCADE`,
			`DROP TABLE IF EXISTS identity_users CASCADE`,
			`DROP TABLE IF EXISTS tenants CASCADE`,
		} {
			_, _ = cdb.Exec(q)
		}
	})
	// Two tenants, each owning one agent (a1→alpha, b1→beta).
	if err := st.CreateTenant(ctx, "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "beta", "Beta"); err != nil {
		t.Fatal(err)
	}
	az := identity.NewAuthorizer(map[string]string{"a1": "alpha", "b1": "beta"})

	// alpha operator key + alpha viewer key.
	opKey, err := identity.MintServiceKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertServiceKey(ctx, opKey.ID, "alpha", opKey.Hash, identity.RoleOperator, "op"); err != nil {
		t.Fatal(err)
	}
	viewKey, err := identity.MintServiceKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertServiceKey(ctx, viewKey.ID, "alpha", viewKey.Hash, identity.RoleViewer, "view"); err != nil {
		t.Fatal(err)
	}

	// beta operator key (for two-tenant symmetry checks).
	betaOpKey, err := identity.MintServiceKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertServiceKey(ctx, betaOpKey.ID, "beta", betaOpKey.Hash, identity.RoleOperator, "betaop"); err != nil {
		t.Fatal(err)
	}

	authr := identity.NewAuthenticator(st, nil, "", nil)

	// Backend stands in for the agent proxy: 200 + path echo.
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok " + r.URL.Path))
	})
	mw := controlplane.IdentityMiddleware(backend, authr, az, nil)
	srv := httptest.NewServer(mw)
	defer srv.Close()

	do := func(method, path, key string) int {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// alpha operator: read AND invoke its own agent a1.
	if c := do("GET", "/agents/a1/sessions", opKey.Plaintext); c != 200 {
		t.Errorf("alpha op read a1: %d want 200", c)
	}
	if c := do("POST", "/agents/a1/sessions", opKey.Plaintext); c != 200 {
		t.Errorf("alpha op invoke a1: %d want 200", c)
	}
	// alpha operator: 404 on beta's b1 (existence hidden cross-tenant).
	if c := do("GET", "/agents/b1/sessions", opKey.Plaintext); c != 404 {
		t.Errorf("alpha op read b1: %d want 404", c)
	}
	// alpha viewer: cannot invoke.
	if c := do("POST", "/agents/a1/sessions", viewKey.Plaintext); c != 403 {
		t.Errorf("alpha viewer invoke a1: %d want 403", c)
	}
	// no credential = 401.
	if c := do("GET", "/agents/a1/sessions", ""); c != 401 {
		t.Errorf("no cred: %d want 401", c)
	}
	// beta operator: can read AND invoke its OWN agent b1 (two-tenant symmetry).
	if c := do("GET", "/agents/b1/sessions", betaOpKey.Plaintext); c != 200 {
		t.Errorf("beta op read b1: %d want 200", c)
	}
	if c := do("POST", "/agents/b1/sessions", betaOpKey.Plaintext); c != 200 {
		t.Errorf("beta op invoke b1: %d want 200", c)
	}
	// beta operator: 404 on alpha's a1 (cross-tenant hidden, mirror direction).
	if c := do("GET", "/agents/a1/sessions", betaOpKey.Plaintext); c != 404 {
		t.Errorf("beta op read a1: %d want 404", c)
	}
	// revoked key = 401.
	if err := st.RevokeKey(ctx, "alpha", opKey.ID); err != nil {
		t.Fatal(err)
	}
	if c := do("GET", "/agents/a1/sessions", opKey.Plaintext); c != 401 {
		t.Errorf("revoked key: %d want 401", c)
	}
}
