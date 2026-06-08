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
	// Two tenants, each owning one agent (a1→alpha, b1→beta).
	if err := st.CreateTenant(ctx, "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "beta", "Beta"); err != nil {
		t.Fatal(err)
	}
	az := identity.NewAuthorizer(map[string]string{"a1": "alpha", "b1": "beta"})

	// alpha operator key + alpha viewer key.
	opKey, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, opKey.ID, "alpha", opKey.Hash, identity.RoleOperator, "op"); err != nil {
		t.Fatal(err)
	}
	viewKey, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, viewKey.ID, "alpha", viewKey.Hash, identity.RoleViewer, "view"); err != nil {
		t.Fatal(err)
	}

	authr := identity.NewAuthenticator(st, nil, "", nil)

	// Backend stands in for the agent proxy: 200 + path echo.
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok " + r.URL.Path))
	})
	mw := controlplane.IdentityMiddleware(backend, authr, az)
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
	// revoked key = 401.
	if err := st.RevokeKey(ctx, "alpha", opKey.ID); err != nil {
		t.Fatal(err)
	}
	if c := do("GET", "/agents/a1/sessions", opKey.Plaintext); c != 401 {
		t.Errorf("revoked key: %d want 401", c)
	}
}
