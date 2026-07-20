//go:build integration

package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/gateway"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/rheader"
)

// oboExchangeGrantType is the RFC 8693 token-exchange grant_type URN the fake
// IdP asserts on the inbound exchange request. It mirrors the unexported
// gateway.oboGrantType (kept as a literal here so this out-of-package test does
// not depend on an internal symbol).
const oboExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"

// oboIdPSrv is a fake RFC 8693 token-exchange endpoint (the tenant's IdP). Each
// hit asserts the grant_type is token-exchange and that a non-empty
// subject_token was presented, then mints an access token DERIVED from the
// subject_token ("obo-<subject_token>") so the caller's identity is provably
// carried through the exchange (and distinct callers observe distinct tokens).
// It records the last grant_type + subject_token it saw and counts hits, so the
// test can prove the exchange (not the raw JWT, not an agent key) reached the
// upstream.
type oboIdPSrv struct {
	*httptest.Server
	hits atomic.Int32

	mu               sync.Mutex
	lastGrantType    string
	lastSubjectToken string
}

func newOBOIdPSrv(t *testing.T) *oboIdPSrv {
	t.Helper()
	idp := &oboIdPSrv{}
	idp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idp.hits.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		gt := r.Form.Get("grant_type")
		subj := r.Form.Get("subject_token")
		idp.mu.Lock()
		idp.lastGrantType, idp.lastSubjectToken = gt, subj
		idp.mu.Unlock()
		if gt != oboExchangeGrantType || subj == "" {
			// Fail the exchange if the gateway did not present a token-exchange
			// grant with a subject_token — proves the gateway sends both.
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      "obo-" + subj,
			"token_type":        "Bearer",
			"expires_in":        3600,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		})
	}))
	t.Cleanup(idp.Close)
	return idp
}

func (idp *oboIdPSrv) seen() (grantType, subjectToken string) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.lastGrantType, idp.lastSubjectToken
}

// oboFakeVerifier is an identity.OIDCVerifier: "good.jwt"→subject "alice";
// everything else errors (fail-closed source). Lets the in-process gateway
// re-verify a forwarded caller JWT without a live IdP.
type oboFakeVerifier struct{}

func (oboFakeVerifier) Verify(_ context.Context, raw string) (string, error) {
	if raw == "good.jwt" {
		return "alice", nil
	}
	return "", errorsNew("bad token")
}

// oboFakeUsers is a gateway.UserTenantSource: alice→tenant acme (exactly one
// row, so verifyCallerAssertion's tenant-bind succeeds against an acme agent).
type oboFakeUsers struct{}

func (oboFakeUsers) UsersBySubject(_ context.Context, sub string) ([]identity.UserRow, error) {
	if sub == "alice" {
		return []identity.UserRow{{TenantID: "acme", Subject: "alice", Role: identity.RoleOperator}}, nil
	}
	return nil, nil
}

// oboAssertionRT stamps X-Runtime-Assertion on every outbound request (empty
// value ⇒ no header — the "no assertion" case). Mirrors the M2a client
// RoundTripper that forwards the caller JWT over the wire.
type oboAssertionRT struct {
	value string
}

func (rt oboAssertionRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if rt.value != "" {
		r = r.Clone(r.Context())
		r.Header.Set(rheader.Assertion, rt.value)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// oboStack is the in-process gateway assembled over a REAL DB-backed broker and
// a REAL OpenAPI upstream, with FAKE assertion verify/tenant-bind wired so a
// forwarded caller JWT can land without a live IdP. This is the only way to
// exercise the full OBO path end-to-end: the production child wires a real
// identity.OIDCVerifier (which cannot mint "good.jwt"→alice), so the in-process
// handler substitutes the fake verifier while keeping every OTHER hop real —
// the broker seals/opens the OBO config in Postgres, the OBOManager performs the
// RFC 8693 exchange, and the REST adapter injects the exchanged token into a
// live outbound HTTP call.
type oboStack struct {
	srvURL string
	up     *oauth2UpstreamSrv
	cm     *obs.ControlMetrics
}

// newOBOStack resets the DB, creates tenant acme, seals an OBO credential
// (pointing at idp) via the broker, and boots the in-process gateway with a
// single openapi upstream referencing that credential. It returns once the
// upstream's orders__whoami tool has federated.
func newOBOStack(t *testing.T, ctx context.Context, db *sql.DB, idp *oboIdPSrv) *oboStack {
	t.Helper()
	oauth2ResetDB(t, db)
	t.Cleanup(func() {
		cdb, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cdb.Close()
		for _, q := range oauth2DropQueries {
			_, _ = cdb.Exec(q)
		}
	})

	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}

	// Seal the OBO credential in Postgres via a real broker. TokenURL points at
	// the fake IdP; ClientID/ClientSecret authenticate the gateway AS the
	// exchange client (never leave the broker). Any key works — the same broker
	// instance backs the OBOManager, so it opens what it sealed.
	key, _ := newSecretKey(t)
	broker := inProcessBroker(t, ctx, db, key)
	if err := broker.SetOBO(ctx, "acme", "orders_obo", identity.OBOConfig{
		TokenURL:     idp.URL,
		ClientID:     "gateway-client",
		ClientSecret: "s3cr3t",
		Scopes:       []string{"orders.read"},
	}); err != nil {
		t.Fatalf("seed obo credential: %v", err)
	}

	up := newOAuth2UpstreamSrv(t) // serves the spec + echoes Authorization

	cm := obs.NewControlMetrics()
	oboMgr := gateway.NewOBOManager(context.Background(), broker)
	m := gateway.NewManager([]config.GatewayServer{{
		Name:       "orders",
		OpenAPI:    up.URL + "/openapi.yaml",
		BaseURL:    up.URL,
		Tenants:    []string{"acme"},
		CredSecret: "orders_obo",
		CredHeader: "Authorization",
	}}, gateway.WithOBO(oboMgr))
	m.Metrics = cm
	m.Start(ctx)
	t.Cleanup(m.Close)

	h := gateway.NewHandler(m)
	h.OBO = oboMgr
	h.Assertion = oboFakeVerifier{}
	h.Users = oboFakeUsers{}
	h.Metrics = cm
	// The acme agent principal: operator (≥operator can call tools), same tenant
	// as the upstream + the resolved caller — so verifyCallerAssertion binds.
	h.PrincipalFor = func(context.Context) (identity.Principal, bool) {
		return identity.Principal{TenantID: "acme", Subject: "svk-agent", Role: identity.RoleOperator, Kind: identity.KindServiceKey}, true
	}

	srv := httptest.NewServer(h.HTTP())
	t.Cleanup(srv.Close)

	stk := &oboStack{srvURL: srv.URL, up: up, cm: cm}
	// Wait for the openapi upstream to federate (async dial fetches the spec).
	// An MCP session pins its SDK server view at creation, so a session opened
	// before the dial completes keeps the empty view forever — poll with FRESH
	// sessions until the tool appears (mirrors connectWhenFederated).
	stk.waitFederated(t, "orders__whoami")
	return stk
}

// connect dials an SDK client at the in-process gateway with headerVal stamped
// as X-Runtime-Assertion on every request (empty ⇒ no header).
func (s *oboStack) connect(t *testing.T, headerVal string) *sdk.ClientSession {
	t.Helper()
	cli := sdk.NewClient(&sdk.Implementation{Name: "obo-e2e", Version: "v0"}, nil)
	sess, err := cli.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint:   s.srvURL,
		HTTPClient: &http.Client{Transport: oboAssertionRT{value: headerVal}},
	}, nil)
	if err != nil {
		t.Fatalf("connect obo gateway: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// scrape renders the control registry (fail-closed credential-error series live
// here) through the fan-out handler, mirroring the sibling oauth2 test's read of
// /metrics but in-process.
func (s *oboStack) scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	obs.FanoutHandler(s.cm, func() []obs.ScrapeTarget { return nil }).
		ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

// waitFederated polls with FRESH sessions until want appears in the tool list
// (the upstream dial is async, and an MCP session pins its view at creation — a
// session opened before the dial completes never sees the tool). Mirrors
// connectWhenFederated.
func (s *oboStack) waitFederated(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sess := s.connect(t, "good.jwt")
		lt, err := sess.ListTools(context.Background(), &sdk.ListToolsParams{})
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		found := false
		for _, tl := range lt.Tools {
			if tl.Name == want {
				found = true
				break
			}
		}
		_ = sess.Close()
		if found {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("tool %q never federated", want)
}

// errorsNew avoids importing errors just for one sentinel in the fake verifier.
func errorsNew(msg string) error { return &oboErr{msg} }

type oboErr struct{ s string }

func (e *oboErr) Error() string { return e.s }

// TestOBOExchangeEndToEnd is the P2.1 M2b headline proof: an OpenAPI upstream
// referencing an OBO credential receives a token EXCHANGED (RFC 8693) for the
// caller's forwarded JWT — never the caller's raw JWT, never an agent key.
func TestOBOExchangeEndToEnd(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable (%s): %v", dsn, err)
	}

	// (1) Happy path: a forwarded caller JWT is re-verified + tenant-bound, gate
	// #5 exchanges it at the IdP, and the upstream receives the EXCHANGED token.
	t.Run("happy_path_upstream_gets_exchanged_token", func(t *testing.T) {
		idp := newOBOIdPSrv(t)
		stk := newOBOStack(t, ctx, db, idp)

		// A fresh session so the (now-federated) view is pinned WITH the tool, and
		// the RT stamps X-Runtime-Assertion: good.jwt on every request.
		sess := stk.connect(t, "good.jwt")
		auth := whoamiAuth(t, sess)

		// (a) The upstream must have received the EXCHANGED on-behalf-of token —
		// not the raw caller JWT, not any agent key/static header.
		if auth != "Bearer obo-good.jwt" {
			t.Fatalf("upstream Authorization = %q, want %q (exchanged OBO token)", auth, "Bearer obo-good.jwt")
		}
		if strings.Contains(auth, "good.jwt") && !strings.HasPrefix(auth, "Bearer obo-") {
			t.Fatalf("upstream must not receive the RAW caller JWT: %q", auth)
		}
		// (b) The IdP exchange endpoint saw a token-exchange grant carrying the
		// caller's JWT as subject_token.
		if got := idp.hits.Load(); got < 1 {
			t.Fatalf("IdP exchange endpoint hits = %d, want ≥1", got)
		}
		gt, subj := idp.seen()
		if gt != oboExchangeGrantType {
			t.Fatalf("IdP grant_type = %q, want %q", gt, oboExchangeGrantType)
		}
		if subj != "good.jwt" {
			t.Fatalf("IdP subject_token = %q, want the forwarded caller JWT %q", subj, "good.jwt")
		}
	})

	// (2) Fail-closed: NO forwarded assertion ⇒ the call is rejected, the
	// upstream is NEVER dispatched to, and the credential-error metric fires.
	t.Run("fail_closed_without_assertion", func(t *testing.T) {
		idp := newOBOIdPSrv(t)
		stk := newOBOStack(t, ctx, db, idp)

		sess := stk.connect(t, "") // no X-Runtime-Assertion header
		res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "orders__whoami", Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("call transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("call must fail closed without a caller assertion: %+v", res.Content)
		}
		tc, ok := res.Content[0].(*sdk.TextContent)
		if !ok || !strings.Contains(tc.Text, "credential unavailable") {
			t.Fatalf("reject must cite \"credential unavailable\", got %+v", res.Content[0])
		}
		if got := stk.up.hits.Load(); got != 0 {
			t.Fatalf("upstream received %d requests, want 0 (never dispatch uncredentialed)", got)
		}
		if got := idp.hits.Load(); got != 0 {
			t.Fatalf("IdP exchange hits = %d, want 0 (no assertion ⇒ never exchange)", got)
		}
		body := stk.scrape(t)
		want := `runtime_gateway_credential_errors_total{server="orders",tenant="acme"}`
		if !strings.Contains(body, want) {
			t.Fatalf("scrape missing credential-error series %q:\n%s", want, body)
		}
	})

	// (3) OpenAPI-only: an OBO cred on a non-openapi (url:) upstream is refused at
	// registration with a 4xx mentioning "openapi" (mirrors the oauth2 sibling).
	// Uses a real child runtimed — the registration gate is independent of the
	// assertion channel, so no fake verifier is needed here.
	t.Run("openapi_only_refuses_non_openapi", func(t *testing.T) {
		oauth2ResetDB(t, db)
		t.Cleanup(func() {
			cdb, cerr := sql.Open("pgx", dsn)
			if cerr != nil {
				return
			}
			defer cdb.Close()
			for _, q := range oauth2DropQueries {
				_, _ = cdb.Exec(q)
			}
		})

		key, keyB64 := newSecretKey(t)

		st, serr := identity.NewStore(ctx, db)
		if serr != nil {
			t.Fatal(serr)
		}
		if err := st.CreateTenant(ctx, "acme", "Acme"); err != nil {
			t.Fatal(err)
		}
		acmeAdmin, _ := identity.MintServiceKey()
		if err := st.InsertServiceKey(ctx, acmeAdmin.ID, "acme", acmeAdmin.Hash, identity.RoleAdmin, "acme-admin"); err != nil {
			t.Fatal(err)
		}

		idp := newOBOIdPSrv(t)
		// Seed the OBO cred so the registration-time credType lookup resolves to
		// obo (an unknown cred would be skipped, not rejected).
		broker := inProcessBroker(t, ctx, db, key)
		if err := broker.SetOBO(ctx, "acme", "orders_obo", identity.OBOConfig{
			TokenURL:     idp.URL,
			ClientID:     "gateway-client",
			ClientSecret: "s3cr3t",
		}); err != nil {
			t.Fatalf("seed obo credential: %v", err)
		}

		tmp := t.TempDir()
		cfgPath := filepath.Join(tmp, "runtime.yaml")
		cfg := "agents:\n" +
			"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8487, tenant: acme}\n"
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}

		base := bootRuntimed(t, tmp, cfgPath, "127.0.0.1:8486", keyB64)

		// Register a url: (MCP-over-HTTP) upstream naming the OBO cred → rejected
		// 4xx mentioning "openapi".
		body, _ := json.Marshal(map[string]any{
			"name":        "badmcp",
			"url":         "http://127.0.0.1:9/mcp",
			"cred_secret": "orders_obo",
			"cred_header": "Authorization",
			"tenant":      "acme",
		})
		resp := authReq(t, "POST", base+"/admin/upstreams", acmeAdmin.Plaintext, strings.NewReader(string(body)))
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		rb := string(raw)
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Fatalf("url:+obo registration must be a 4xx, got %d body=%s", resp.StatusCode, rb)
		}
		if !strings.Contains(rb, "openapi") {
			t.Fatalf("rejection must mention \"openapi\", got: %s", rb)
		}
	})
}
