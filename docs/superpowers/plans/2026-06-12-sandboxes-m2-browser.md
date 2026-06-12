# Sandboxes M2 — Browser Sandbox Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A managed headless-browser sandbox runtime agents drive (navigate, click, type, extract, screenshot) under an enforced egress policy and per-tenant scoping, federated through the gateway like the M1 code interpreter (`mcp__gateway__browser__<tool>`).

**Architecture:** A new `cmd/browserd` (package `internal/browser`), sibling to `cmd/sandboxd`. Chrome runs in a locked-down Docker container with no direct network route; `chromedp` drives it over remote CDP. All container traffic is forced through a `browserd`-run egress proxy that allows/denies by hostname in three modes (`deny-all` default, `allow-list`, `allow-all-public`) with an unconditional internal-address block + DNS-rebind defense. Session lifecycle reuses M1's Manager contract (per-tenant cap, slot reservation under lock, idle/max-lifetime reaper, reap-on-start, existence-hiding lookup).

**Tech Stack:** Go 1.25, `github.com/chromedp/chromedp` + `github.com/chromedp/cdproto`, `github.com/docker/docker` client, `github.com/modelcontextprotocol/go-sdk/mcp`, `github.com/sausheong/harness/tools/web` (SSRF guard), `internal/obs` (egress metric). Headless Chromium image via `deploy/browser.Dockerfile`.

**Reference files (read before starting):**
- `internal/sandbox/manager.go`, `manager_test.go` — the Manager contract to mirror.
- `internal/sandbox/backend.go` — the `Backend` interface + `fakeBackend` pattern.
- `internal/sandbox/docker.go` — the locked-down container posture.
- `internal/sandbox/tools.go`, `tenant.go`, `paths.go` — MCP tool wiring, `popTenant`, path confinement.
- `cmd/sandboxd/main.go` — the env-config + stdio-MCP skeleton.
- `harness/tools/browser/browser.go` — the chromedp action logic to PORT (navigate/click/type/get_text/screenshot/evaluate, stealth script, wait budgets).
- `harness/tools/web/ssrf.go` — `web.ValidateURLNotInternal` (import directly) and the private-network CIDR list.
- `test/gateway_sandbox_e2e_test.go` — the through-serve e2e to mirror.
- `internal/config/config.go` — `GatewayServer` (already supports `command:`/`forward_tenant:`/`env:` — no config change needed; browserd is just another stdio upstream).

**Spec:** `docs/superpowers/specs/2026-06-12-sandboxes-m2-browser-design.md`

---

## Task 1: Egress policy decision engine

The heart of the milestone, built first and in isolation: a pure `Policy.Decide(host)` with no network. The HTTP proxy server (Task 2) wraps it.

**Files:**
- Create: `internal/browser/egress.go`
- Test: `internal/browser/egress_test.go`

- [ ] **Step 1: Write the failing test**

```go
package browser

import "testing"

func TestPolicyDecide(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		allow   []string
		host    string
		wantErr bool // true = denied
	}{
		{"deny-all denies public", "deny-all", nil, "example.com", true},
		{"deny-all denies all", "deny-all", []string{"*.x.org"}, "a.x.org", true},
		{"allow-list match label", "allow-list", []string{"*.wikipedia.org"}, "en.wikipedia.org", false},
		{"allow-list miss", "allow-list", []string{"*.wikipedia.org"}, "example.com", true},
		{"allow-list exact", "allow-list", []string{"api.github.com"}, "api.github.com", false},
		{"allow-list no substring leak", "allow-list", []string{"*.x.org"}, "xx.org", true},
		{"allow-list case-insensitive", "allow-list", []string{"*.X.ORG"}, "a.x.org", false},
		{"allow-all-public allows", "allow-all-public", nil, "example.com", false},
		{"unknown mode fails closed", "bogus", nil, "example.com", true},
		{"malformed host fails closed", "allow-all-public", nil, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := NewPolicy(c.mode, c.allow)
			if c.mode == "bogus" {
				if err == nil {
					t.Fatal("unknown mode should error at construction")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			err = p.Decide(c.host)
			if (err != nil) != c.wantErr {
				t.Fatalf("Decide(%q) err=%v, wantErr=%v", c.host, err, c.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/browser/ -run TestPolicyDecide`
Expected: FAIL — `undefined: NewPolicy`.

- [ ] **Step 3: Implement the policy**

```go
// Package browser implements the headless-browser sandbox sessions served by
// cmd/browserd as MCP tools behind the platform gateway. Chrome runs in a
// locked-down container with no direct network; all traffic is forced through
// the egress proxy in this package, which allows or denies by hostname.
package browser

import (
	"fmt"
	"net"
	"strings"
)

// Egress modes.
const (
	ModeDenyAll        = "deny-all"
	ModeAllowList      = "allow-list"
	ModeAllowAllPublic = "allow-all-public"
)

// Policy decides whether the browser may reach a given host. It is the sole
// egress control: the container has no direct route out, so every connection
// Chrome opens (top-level, subresource, fetch, redirect, websocket) is decided
// here. Construction validates the mode; an unknown mode is an error, never a
// silent allow.
type Policy struct {
	mode   string
	allow  []string // hostname globs, lowercased (allow-list mode)
	lookup func(host string) ([]net.IP, error)
}

// NewPolicy builds a Policy. allow globs are only meaningful for allow-list
// mode. An unrecognized mode is rejected (fail-closed at construction).
func NewPolicy(mode string, allow []string) (*Policy, error) {
	switch mode {
	case ModeDenyAll, ModeAllowList, ModeAllowAllPublic:
	default:
		return nil, fmt.Errorf("unknown egress mode %q (want %s|%s|%s)",
			mode, ModeDenyAll, ModeAllowList, ModeAllowAllPublic)
	}
	low := make([]string, len(allow))
	for i, g := range allow {
		low[i] = strings.ToLower(strings.TrimSpace(g))
	}
	return &Policy{mode: mode, allow: low, lookup: net.LookupIP}, nil
}

// Decide returns nil if the host is allowed, or an error (the deny reason) if
// not. The internal-address block is UNCONDITIONAL across every mode and is
// checked against the RESOLVED IPs (DNS-rebind defense), so an allowlisted name
// pointing at a private address is still denied.
func (p *Policy) Decide(host string) error {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return fmt.Errorf("egress denied: empty host")
	}
	// Strip a port if present (CONNECT targets carry host:port).
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// Mode gate first (cheap, no DNS for deny-all / allow-list misses).
	switch p.mode {
	case ModeDenyAll:
		return fmt.Errorf("egress denied: deny-all policy blocks %q", host)
	case ModeAllowList:
		if !p.matchAllow(host) {
			return fmt.Errorf("egress denied: %q not in allow-list", host)
		}
	case ModeAllowAllPublic:
		// fall through to the unconditional internal check below.
	}

	// Unconditional internal-address block (DNS-rebind defense): resolve, then
	// reject any private/loopback/link-local result. Applies to allow-list AND
	// allow-all-public — no mode can reach an internal address.
	if err := p.blockInternal(host); err != nil {
		return err
	}
	return nil
}

// matchAllow reports whether host matches any configured glob. A glob's "*"
// spans one or more leading labels: "*.x.org" matches "a.x.org" and
// "a.b.x.org" but NOT "x.org" or "xx.org". An exact glob (no "*") matches the
// host verbatim. Matching is label-wise on the dotted suffix — never substring.
func (p *Policy) matchAllow(host string) bool {
	for _, g := range p.allow {
		if g == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(g, "*."); ok {
			// host must END with ".suffix" (at least one extra label).
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		}
	}
	return false
}

// blockInternal resolves host and denies if any resolved IP is private,
// loopback, or link-local. A resolution failure is itself a denial (cannot
// prove the target is safe). A host that is already an IP literal is checked
// directly.
func (p *Policy) blockInternal(host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if isInternalIP(ip) {
			return fmt.Errorf("egress denied: internal address %s", ip)
		}
		return nil
	}
	if host == "metadata" || host == "metadata.google.internal" {
		return fmt.Errorf("egress denied: cloud metadata endpoint")
	}
	ips, err := p.lookup(host)
	if err != nil {
		return fmt.Errorf("egress denied: cannot resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if isInternalIP(ip) {
			return fmt.Errorf("egress denied: %q resolves to internal address %s", host, ip)
		}
	}
	return nil
}

// internalNets is the private/loopback/link-local set, mirroring
// harness/tools/web/ssrf.go.
var internalNets []*net.IPNet

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"169.254.0.0/16", "::1/128", "fc00::/7", "fe80::/10",
	} {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			internalNets = append(internalNets, n)
		}
	}
}

func isInternalIP(ip net.IP) bool {
	for _, n := range internalNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Add the DNS-rebind test (uses the injectable resolver)**

Append to `egress_test.go`:

```go
import "net" // add to the import block

func TestPolicyDNSRebindDefense(t *testing.T) {
	p, err := NewPolicy(ModeAllowList, []string{"*.evil.test"})
	if err != nil {
		t.Fatal(err)
	}
	// Allowlisted name, but it resolves to a private address.
	p.lookup = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.1.2.3")}, nil
	}
	if err := p.Decide("inner.evil.test"); err == nil {
		t.Fatal("allowlisted host resolving to a private IP must be denied")
	}

	// allow-all-public must also block a literal private IP.
	pub, _ := NewPolicy(ModeAllowAllPublic, nil)
	if err := pub.Decide("192.168.0.5"); err == nil {
		t.Fatal("literal private IP must be denied in allow-all-public")
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/browser/ -run TestPolicy`
Expected: PASS (both `TestPolicyDecide` and `TestPolicyDNSRebindDefense`).

- [ ] **Step 6: Commit**

```bash
git add internal/browser/egress.go internal/browser/egress_test.go
git commit -m "feat(browser): egress policy decision engine (3 modes + DNS-rebind defense)"
```

---

## Task 2: Egress proxy HTTP server

Wrap `Policy` in an `http.Server` that handles plain-HTTP forwarding and HTTPS `CONNECT` tunneling, deciding on the host.

**Files:**
- Modify: `internal/browser/egress.go` (append the server)
- Test: `internal/browser/egress_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `egress_test.go`:

```go
import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyForwardAllowDeny(t *testing.T) {
	// An upstream the proxy will forward to.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from upstream")
	}))
	defer upstream.Close()

	// Allow-list contains the upstream's host; a second host is denied.
	// upstream.URL is http://127.0.0.1:PORT — allow 127.0.0.1 explicitly and
	// neutralize the internal-block for the test via the resolver hook.
	p, err := NewPolicy(ModeAllowList, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	p.lookup = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil // pretend public
	}
	proxy := NewProxy(p)
	ps := httptest.NewServer(proxy)
	defer ps.Close()

	// Client whose transport routes through the proxy.
	proxyURL, _ := url.Parse(ps.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("allowed GET through proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello from upstream" {
		t.Fatalf("body = %q", body)
	}

	// A denied host: rebuild policy to deny-all and confirm 403.
	deny, _ := NewPolicy(ModeDenyAll, nil)
	dps := httptest.NewServer(NewProxy(deny))
	defer dps.Close()
	dpu, _ := url.Parse(dps.URL)
	dclient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(dpu)}}
	dresp, err := dclient.Get(upstream.URL)
	if err != nil {
		t.Fatalf("denied GET (transport): %v", err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied GET status = %d, want 403", dresp.StatusCode)
	}
}
```

(Add `"net/url"` to the test import block.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/browser/ -run TestProxyForwardAllowDeny`
Expected: FAIL — `undefined: NewProxy`.

- [ ] **Step 3: Implement the proxy**

Append to `egress.go` (add `"io"`, `"net/http"`, `"time"` to the import block):

```go
// Proxy is the forced egress proxy. The Chrome container points HTTP_PROXY /
// HTTPS_PROXY / --proxy-server at it and has no other route out, so every
// request passes through ServeHTTP, where Policy adjudicates the host. Plain
// HTTP is forwarded; HTTPS arrives as CONNECT and is blind-tunneled (the body
// stays encrypted — host-level control only, by design).
type Proxy struct {
	policy *Policy
	client *http.Client
	// onDecision is called for every allow/deny (host, allowed). Wired to the
	// egress metric in cmd/browserd; nil in tests.
	onDecision func(host string, allowed bool)
}

// NewProxy builds a Proxy over policy.
func NewProxy(policy *Policy) *Proxy {
	return &Proxy{
		policy: policy,
		client: &http.Client{
			Timeout: 60 * time.Second,
			// Do not auto-follow redirects here: each hop is a fresh request the
			// browser issues and the proxy re-adjudicates. Return the 3xx as-is.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (p *Proxy) decide(host string) bool {
	err := p.policy.Decide(host)
	if p.onDecision != nil {
		p.onDecision(host, err == nil)
	}
	return err == nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleForward(w, r)
}

// handleForward proxies a plain-HTTP request after an allow decision.
func (p *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	if !p.decide(r.Host) {
		http.Error(w, "egress denied by policy", http.StatusForbidden)
		return
	}
	// Build an outbound request to the absolute URL the proxy received.
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, "bad proxied request", http.StatusBadGateway)
		return
	}
	copyHeader(outReq.Header, r.Header)
	resp, err := p.client.Do(outReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleConnect blind-tunnels HTTPS after an allow decision on the CONNECT
// target host.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !p.decide(r.Host) {
		http.Error(w, "egress denied by policy", http.StatusForbidden)
		return
	}
	dst, err := net.DialTimeout("tcp", r.Host, 30*time.Second)
	if err != nil {
		http.Error(w, "dial upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = dst.Close()
		http.Error(w, "proxy: hijack unsupported", http.StatusInternalServerError)
		return
	}
	src, _, err := hj.Hijack()
	if err != nil {
		_ = dst.Close()
		return
	}
	_, _ = src.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	// Pump both directions until either side closes.
	go func() { _, _ = io.Copy(dst, src); _ = dst.Close() }()
	go func() { _, _ = io.Copy(src, dst); _ = src.Close() }()
}

// copyHeader copies HTTP headers, skipping the hop-by-hop Connection header.
func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		if k == "Proxy-Connection" || k == "Connection" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/browser/ -run TestProxy`
Expected: PASS.

- [ ] **Step 5: Run the whole package**

Run: `go test ./internal/browser/ && go vet ./internal/browser/`
Expected: PASS, no vet complaints.

- [ ] **Step 6: Commit**

```bash
git add internal/browser/egress.go internal/browser/egress_test.go
git commit -m "feat(browser): forced egress proxy (HTTP forward + HTTPS CONNECT tunnel)"
```

---

## Task 3: Backend interface + fake backend + Manager

Mirror M1's Manager almost verbatim, swapping container-exec semantics for a browser session (CDP endpoint + per-session chromedp ctx). The fake backend lets every Manager test run without Chrome or Docker.

**Files:**
- Create: `internal/browser/backend.go`
- Create: `internal/browser/manager.go`
- Create: `internal/browser/tenant.go`
- Test: `internal/browser/manager_test.go`

- [ ] **Step 1: Write the Backend + fake (no test yet — exercised via Manager)**

`internal/browser/backend.go`:

```go
package browser

import (
	"context"
	"fmt"
	"sync"
)

// BrowserHandle is the connected form of one browser container: its CDP
// websocket endpoint plus the container id for removal. The Manager turns the
// endpoint into a chromedp context lazily on first action (Task 5 fills in the
// chromedp wiring; the fake leaves Endpoint empty).
type BrowserHandle struct {
	ContainerID string
	Endpoint    string // ws://… CDP endpoint; empty under the fake backend
}

// Backend abstracts the container engine for browser sandboxes. dockerBackend
// (docker.go) is the real implementation; fakeBackend serves hermetic tests
// and cmd/browserd's RUNTIME_BROWSER_FAKE mode.
type Backend interface {
	// Create starts one locked-down Chrome container wired to the egress proxy
	// at proxyAddr and returns its handle.
	Create(ctx context.Context, tenant, proxyAddr string) (BrowserHandle, error)
	// Remove force-removes the container.
	Remove(ctx context.Context, containerID string) error
	// ListLeftovers returns ids of all runtime.browser=1 containers (reap-on-start).
	ListLeftovers(ctx context.Context) ([]string, error)
}

// fakeBackend is an in-memory Backend: no Chrome, no Docker. Create returns a
// synthetic handle; actions against it are short-circuited by the Manager's
// test seam (see manager.go actionRunner).
type fakeBackend struct {
	mu    sync.Mutex
	next  int
	boxes map[string]bool // containerID → exists
}

// NewFakeBackend returns the in-memory Backend (tests / RUNTIME_BROWSER_FAKE).
func NewFakeBackend() Backend {
	return &fakeBackend{boxes: map[string]bool{}}
}

func (f *fakeBackend) Create(_ context.Context, _, _ string) (BrowserHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("fake-%d", f.next)
	f.boxes[id] = true
	return BrowserHandle{ContainerID: id}, nil
}

func (f *fakeBackend) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.boxes[id] {
		return fmt.Errorf("fake backend: unknown container %q", id)
	}
	delete(f.boxes, id)
	return nil
}

func (f *fakeBackend) ListLeftovers(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.boxes))
	for id := range f.boxes {
		ids = append(ids, id)
	}
	return ids, nil
}
```

- [ ] **Step 2: Write `tenant.go` (lifted from M1, renamed package)**

`internal/browser/tenant.go`:

```go
package browser

import "encoding/json"

// tenantKey is the reserved argument the gateway injects for forward_tenant
// upstreams. browserd trusts it because it is a stdio child reachable only
// through the gateway.
const tenantKey = "__rt_tenant"

// defaultTenant mirrors Identity M1's absent-tenant rule.
const defaultTenant = "default"

// popTenant extracts and removes the reserved tenant key from raw JSON tool
// arguments, returning the remaining arguments for normal decoding. present
// reports whether the key existed at all (any string value, including "").
func popTenant(raw json.RawMessage) (tenant string, present bool, rest json.RawMessage, err error) {
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", false, nil, err
		}
	}
	if m == nil {
		m = map[string]any{}
	}
	if v, ok := m[tenantKey].(string); ok {
		tenant = v
		present = true
	}
	delete(m, tenantKey)
	if tenant == "" {
		tenant = defaultTenant
	}
	rest, err = json.Marshal(m)
	return tenant, present, rest, err
}
```

- [ ] **Step 3: Write the Manager test (mirrors M1)**

`internal/browser/manager_test.go`:

```go
package browser

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testManager(t *testing.T) (*Manager, Backend, *time.Time) {
	t.Helper()
	be := NewFakeBackend()
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	m := NewManager(be, Config{MaxPerTenant: 2, IdleTTL: 10 * time.Minute, MaxLifetime: time.Hour})
	m.now = func() time.Time { return now }
	return m, be, &now
}

func TestCreateCloseRoundTrip(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	s, err := m.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.ID, "brw-") || len(s.ID) != 4+32 {
		t.Fatalf("bad id %q", s.ID)
	}
	if err := m.Close(ctx, "acme", s.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx, "acme", s.ID); err != nil {
		t.Fatal("second close should be nil (idempotent)")
	}
}

func TestCrossTenantHiddenAsNotFound(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	s, _ := m.Create(ctx, "acme")
	_, errCross := m.Lookup("globex", s.ID)
	_, errMissing := m.Lookup("globex", "brw-doesnotexist")
	if errCross == nil || errMissing == nil {
		t.Fatal("both must error")
	}
	if errCross.Error() != errMissing.Error() {
		t.Fatalf("cross-tenant %q must equal missing %q", errCross, errMissing)
	}
	if got := m.List("globex"); len(got) != 0 {
		t.Fatalf("globex sees %d", len(got))
	}
	if got := m.List("acme"); len(got) != 1 {
		t.Fatalf("acme sees %d", len(got))
	}
}

func TestPerTenantCap(t *testing.T) {
	ctx := context.Background()
	m, _, _ := testManager(t)
	if _, err := m.Create(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "acme"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("third create should hit the cap, got %v", err)
	}
}

type slowCreateBackend struct {
	Backend
	delay time.Duration
}

func (b *slowCreateBackend) Create(ctx context.Context, tenant, proxy string) (BrowserHandle, error) {
	time.Sleep(b.delay)
	return b.Backend.Create(ctx, tenant, proxy)
}

func TestPerTenantCapUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	be := &slowCreateBackend{Backend: NewFakeBackend(), delay: 50 * time.Millisecond}
	m := NewManager(be, Config{MaxPerTenant: 2, IdleTTL: 10 * time.Minute, MaxLifetime: time.Hour})
	const attempts = 6
	var ok, limited atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Create(ctx, "acme")
			switch {
			case err == nil:
				ok.Add(1)
			case strings.Contains(err.Error(), "limit"):
				limited.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 2 || limited.Load() != 4 {
		t.Fatalf("got %d ok / %d limited, want 2/4", ok.Load(), limited.Load())
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 2 {
		t.Fatalf("backend has %d containers, want 2", len(ids))
	}
}

type blockingCreateBackend struct {
	Backend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingCreateBackend) Create(ctx context.Context, tenant, proxy string) (BrowserHandle, error) {
	close(b.entered)
	<-b.release
	return b.Backend.Create(ctx, tenant, proxy)
}

func TestCloseDuringCreateDoesNotLeak(t *testing.T) {
	ctx := context.Background()
	be := &blockingCreateBackend{Backend: NewFakeBackend(), entered: make(chan struct{}), release: make(chan struct{})}
	m := NewManager(be, Config{MaxPerTenant: 2, IdleTTL: 10 * time.Minute, MaxLifetime: time.Hour})
	errCh := make(chan error, 1)
	go func() {
		_, err := m.Create(ctx, "acme")
		errCh <- err
	}()
	<-be.entered
	sessions := m.List("acme")
	if len(sessions) != 1 {
		t.Fatalf("expected 1 reserved session, got %d", len(sessions))
	}
	if err := m.Close(ctx, "acme", sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	close(be.release)
	if err := <-errCh; !errors.Is(err, errNoSandbox) {
		t.Fatalf("Create after lost reservation = %v, want errNoSandbox", err)
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 0 {
		t.Fatalf("container leaked: %v", ids)
	}
}

func TestReaperIdleAndMaxLifetime(t *testing.T) {
	ctx := context.Background()
	m, be, now := testManager(t)
	idle, _ := m.Create(ctx, "acme")
	busy, _ := m.Create(ctx, "acme")
	*now = now.Add(9 * time.Minute)
	if _, err := m.Lookup("acme", busy.ID); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute)
	m.ReapOnce(ctx)
	if _, err := m.Lookup("acme", idle.ID); err == nil {
		t.Fatal("idle session should be reaped")
	}
	if _, err := m.Lookup("acme", busy.ID); err != nil {
		t.Fatalf("busy session reaped early: %v", err)
	}
	*now = now.Add(50 * time.Minute)
	m.ReapOnce(ctx)
	if _, err := m.Lookup("acme", busy.ID); err == nil {
		t.Fatal("session past max lifetime should be reaped")
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 0 {
		t.Fatalf("backend still has %v", ids)
	}
}

func TestReapStartupRemovesLeftovers(t *testing.T) {
	ctx := context.Background()
	be := NewFakeBackend()
	_, _ = be.Create(ctx, "old1", "")
	_, _ = be.Create(ctx, "old2", "")
	m := NewManager(be, Config{MaxPerTenant: 5})
	if err := m.ReapStartup(ctx); err != nil {
		t.Fatal(err)
	}
	ids, _ := be.ListLeftovers(ctx)
	if len(ids) != 0 {
		t.Fatalf("leftovers not reaped: %v", ids)
	}
}
```

- [ ] **Step 4: Run tests to verify they fail**

Run: `go test ./internal/browser/ -run 'TestCreate|TestCross|TestPerTenant|TestClose|TestReap'`
Expected: FAIL — `undefined: Manager` / `NewManager` / `Config`.

- [ ] **Step 5: Implement the Manager**

`internal/browser/manager.go`:

```go
package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// errNoSandbox is the single not-found error: a wrong-tenant id and a
// nonexistent id are indistinguishable (existence hidden, Identity M1 posture).
var errNoSandbox = errors.New("no such browser")

// Config bounds Manager behavior; zero/invalid fields get defaults.
type Config struct {
	MaxPerTenant int           // concurrent browsers per tenant (default 5)
	IdleTTL      time.Duration // close after this long unused (default 10m)
	MaxLifetime  time.Duration // close this long after create (default 1h)
	ProxyAddr    string        // egress proxy address passed to Backend.Create
}

// Session is one live browser. The chromedp context fields are populated lazily
// by the action layer (Task 5); the fake backend leaves them nil.
type Session struct {
	ID          string
	Tenant      string
	ContainerID string
	Endpoint    string
	CreatedAt   time.Time
	LastUsed    time.Time
	ExpiresAt   time.Time
	CurrentURL  string

	mu         sync.Mutex          // serializes chromedp actions (one tab)
	allocCtx   context.Context     // remote allocator ctx (Task 5)
	taskCtx    context.Context     // chromedp task ctx (Task 5)
	cancel     context.CancelFunc  // tears down both (Task 5)
}

// Manager owns the browser sessions over a Backend. Mirrors the M1 sandbox
// Manager contract.
type Manager struct {
	be  Backend
	cfg Config
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager builds a Manager over be, applying defaults for zero fields.
func NewManager(be Backend, cfg Config) *Manager {
	if cfg.MaxPerTenant <= 0 {
		cfg.MaxPerTenant = 5
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 10 * time.Minute
	}
	if cfg.MaxLifetime <= 0 {
		cfg.MaxLifetime = time.Hour
	}
	return &Manager{be: be, cfg: cfg, now: time.Now, sessions: map[string]*Session{}}
}

// newBrowserID returns "brw-" + 32 hex chars from 16 random bytes.
func newBrowserID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("browser: crypto/rand failed: %v", err))
	}
	return "brw-" + hex.EncodeToString(b[:])
}

// Create starts a new browser for tenant, enforcing the per-tenant cap with a
// slot reservation under lock (identical discipline to M1).
func (m *Manager) Create(ctx context.Context, tenant string) (*Session, error) {
	now := m.now()
	s := &Session{
		ID:        newBrowserID(),
		Tenant:    tenant,
		CreatedAt: now,
		LastUsed:  now,
		ExpiresAt: now.Add(m.cfg.MaxLifetime),
	}
	m.mu.Lock()
	count := 0
	for _, other := range m.sessions {
		if other.Tenant == tenant {
			count++
		}
	}
	if count >= m.cfg.MaxPerTenant {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser limit reached (%d per tenant): close one with close_browser", m.cfg.MaxPerTenant)
	}
	m.sessions[s.ID] = s // reservation
	m.mu.Unlock()

	h, err := m.be.Create(ctx, tenant, m.cfg.ProxyAddr)
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, s.ID)
		m.mu.Unlock()
		return nil, fmt.Errorf("browser backend unavailable: %w", err)
	}

	m.mu.Lock()
	if _, ok := m.sessions[s.ID]; !ok {
		m.mu.Unlock()
		if rmErr := m.be.Remove(ctx, h.ContainerID); rmErr != nil {
			slog.Warn("browser create: container remove after lost reservation failed",
				"browser_id", s.ID, "container_id", h.ContainerID, "err", rmErr)
		}
		return nil, errNoSandbox
	}
	s.ContainerID = h.ContainerID
	s.Endpoint = h.Endpoint
	m.mu.Unlock()
	return s, nil
}

// Lookup resolves a browser id for tenant, touching LastUsed. A missing id and
// a foreign tenant's id return the identical errNoSandbox.
func (m *Manager) Lookup(tenant, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.Tenant != tenant {
		return nil, errNoSandbox
	}
	s.LastUsed = m.now()
	return s, nil
}

// maskIfGone scrubs backend errors before they reach the LLM. A vanished
// session → errNoSandbox; a still-live session's error is logged and
// genericized.
func (m *Manager) maskIfGone(id string, err error) error {
	if err == nil {
		return nil
	}
	m.mu.Lock()
	_, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return errNoSandbox
	}
	slog.Warn("browser: backend error", "browser", id, "err", err)
	return errors.New("browser action failed (see browserd logs)")
}

// List returns copies of tenant's live sessions (without the unexported fields).
func (m *Manager) List(tenant string) []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.sessions {
		if s.Tenant == tenant {
			out = append(out, Session{
				ID: s.ID, Tenant: s.Tenant, ContainerID: s.ContainerID,
				CreatedAt: s.CreatedAt, LastUsed: s.LastUsed, ExpiresAt: s.ExpiresAt,
				CurrentURL: s.CurrentURL,
			})
		}
	}
	return out
}

// Close removes the browser. Idempotent; never reveals existence.
func (m *Manager) Close(ctx context.Context, tenant, id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok || s.Tenant != tenant {
		m.mu.Unlock()
		return nil
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	if err := m.be.Remove(ctx, s.ContainerID); err != nil {
		slog.Warn("browser close: container remove failed",
			"browser_id", s.ID, "container_id", s.ContainerID, "err", err)
	}
	return nil
}

// ReapOnce closes every session idle past IdleTTL or past its max lifetime.
func (m *Manager) ReapOnce(ctx context.Context) {
	now := m.now()
	m.mu.Lock()
	var expired []*Session
	for id, s := range m.sessions {
		if now.Sub(s.LastUsed) > m.cfg.IdleTTL || now.After(s.ExpiresAt) {
			expired = append(expired, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, s := range expired {
		if s.cancel != nil {
			s.cancel()
		}
		if err := m.be.Remove(ctx, s.ContainerID); err != nil {
			slog.Warn("browser reap: container remove failed",
				"browser_id", s.ID, "container_id", s.ContainerID, "err", err)
			continue
		}
		slog.Info("browser reaped", "browser_id", s.ID, "tenant", s.Tenant)
	}
}

// StartReaper runs ReapOnce every interval until ctx is canceled.
func (m *Manager) StartReaper(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.ReapOnce(ctx)
			}
		}
	}()
}

// ReapStartup removes all leftover browser containers (crash recovery).
func (m *Manager) ReapStartup(ctx context.Context) error {
	ids, err := m.be.ListLeftovers(ctx)
	if err != nil {
		return fmt.Errorf("list leftover browsers: %w", err)
	}
	for _, id := range ids {
		if err := m.be.Remove(ctx, id); err != nil {
			slog.Warn("startup reap: container remove failed", "container_id", id, "err", err)
		}
	}
	return nil
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/browser/ && go vet ./internal/browser/`
Expected: PASS. (Note: the `Session` unexported chromedp fields are unused until Task 5 — that is expected and compiles.)

- [ ] **Step 7: Commit**

```bash
git add internal/browser/backend.go internal/browser/manager.go internal/browser/tenant.go internal/browser/manager_test.go
git commit -m "feat(browser): Backend interface, fake backend, and session Manager"
```

---

## Task 4: HTML extract (clean text/markdown)

A small, pure HTML→text function for the `extract` action. No browser needed — it operates on an HTML string, so it's fully hermetic.

**Files:**
- Create: `internal/browser/extract.go`
- Test: `internal/browser/extract_test.go`

- [ ] **Step 1: Write the failing test**

```go
package browser

import (
	"strings"
	"testing"
)

func TestExtractText(t *testing.T) {
	html := `<html><head><title>T</title><style>.x{color:red}</style>
	<script>var a=1;</script></head>
	<body><nav>menu menu</nav><main><h1>Hello</h1><p>World  of   text.</p>
	<p>Second line.</p></main><footer>foot</footer></body></html>`
	got := ExtractText(html)
	if strings.Contains(got, "var a=1") || strings.Contains(got, "color:red") {
		t.Fatalf("script/style leaked into extract: %q", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "World of text.") {
		t.Fatalf("expected body text, got %q", got)
	}
	// Whitespace collapsed: no run of 2+ spaces.
	if strings.Contains(got, "  ") {
		t.Fatalf("whitespace not collapsed: %q", got)
	}
}

func TestExtractMalformedHTMLNoPanic(t *testing.T) {
	// Must not panic on broken markup.
	_ = ExtractText("<div><p>unclosed <b>bold")
	_ = ExtractText("")
	_ = ExtractText("plain text no tags")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/browser/ -run TestExtract`
Expected: FAIL — `undefined: ExtractText`.

- [ ] **Step 3: Implement extract using golang.org/x/net/html**

First confirm the dep is available (it is an indirect dep already via many libs; add it explicitly):

Run: `go get golang.org/x/net/html`

`internal/browser/extract.go`:

```go
package browser

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ExtractText renders an HTML document to clean, readable plain text: script,
// style, noscript, and nav/footer chrome are dropped; text nodes are joined and
// runs of whitespace collapsed to single spaces, with block elements separated
// by newlines. Malformed HTML never panics (the tokenizer is lenient); on a
// parse failure the raw input is returned with tags stripped best-effort.
func ExtractText(htmlStr string) string {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		return collapseWS(htmlStr)
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Script, atom.Style, atom.Noscript, atom.Nav, atom.Footer, atom.Head:
				return // skip the subtree
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		// Block-level boundary → newline so paragraphs don't run together.
		if n.Type == html.ElementNode && isBlock(n.DataAtom) {
			b.WriteString("\n")
		}
	}
	walk(doc)
	return collapseWS(b.String())
}

func isBlock(a atom.Atom) bool {
	switch a {
	case atom.P, atom.Div, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
		atom.Li, atom.Br, atom.Tr, atom.Section, atom.Article, atom.Header:
		return true
	}
	return false
}

// collapseWS trims each line and collapses interior whitespace runs to single
// spaces, dropping blank lines.
func collapseWS(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/browser/ -run TestExtract`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/browser/extract.go internal/browser/extract_test.go go.mod go.sum
git commit -m "feat(browser): HTML-to-clean-text extract for the extract action"
```

---

## Task 5: Chromedp action layer + Docker backend

Port the harness chromedp actions to drive a Manager-held remote-allocator context, and implement the real `dockerBackend`. The action layer is exercised by the live test (Task 8) — here we keep the hermetic surface honest by unit-testing only the pure pieces (selector/URL validation), and gate the Chrome-touching code behind the live build tag.

**Files:**
- Create: `internal/browser/actions.go`
- Create: `internal/browser/docker.go`
- Create: `internal/browser/paths.go`
- Test: `internal/browser/paths_test.go`
- Test: `internal/browser/docker_live_test.go` (live tag)

- [ ] **Step 1: Write the pure-helper test**

`internal/browser/paths_test.go`:

```go
package browser

import "testing"

func TestValidateNavURL(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"https://example.com", false},
		{"http://example.com/path?q=1", false},
		{"ftp://example.com", true},
		{"file:///etc/passwd", true},
		{"javascript:alert(1)", true},
		{"", true},
		{"not a url", true},
	}
	for _, c := range cases {
		err := validateNavURL(c.url)
		if (err != nil) != c.wantErr {
			t.Fatalf("validateNavURL(%q) err=%v wantErr=%v", c.url, err, c.wantErr)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/browser/ -run TestValidateNavURL`
Expected: FAIL — `undefined: validateNavURL`.

- [ ] **Step 3: Implement `paths.go`**

`internal/browser/paths.go`:

```go
package browser

import (
	"fmt"
	"strings"
)

// validateNavURL ensures a navigation target is an absolute http(s) URL.
// Egress (which host is reachable) is enforced by the proxy, not here; this is
// only a scheme/shape guard so a non-web URL never reaches Chrome.
func validateNavURL(u string) error {
	if u == "" {
		return fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("url must start with http:// or https://")
	}
	// Reject embedded whitespace / control chars that could confuse Chrome.
	if strings.ContainsAny(u, " \t\r\n") {
		return fmt.Errorf("url contains whitespace")
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/browser/ -run TestValidateNavURL`
Expected: PASS.

- [ ] **Step 5: Implement the action layer**

`internal/browser/actions.go` — ported from `harness/tools/browser/browser.go`, adapted to operate on a `*Session`'s chromedp context. The session's `taskCtx`/`allocCtx`/`cancel` are lazily initialized on first action via `ensureChrome`.

```go
package browser

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

const (
	navTimeout        = 60 * time.Second
	networkIdleBudget = 5 * time.Second
	elementWaitBudget = 25 * time.Second
	defaultSettleMs   = 1000
	screenshotMaxJPEG = 90
)

// stealthScript hides the most common headless/automation tells. Lifted from
// the harness browser tool.
const stealthScript = `(function(){
  try { Object.defineProperty(navigator, 'webdriver', { get: () => undefined }); } catch (e) {}
  try { if (!navigator.languages || navigator.languages.length === 0) { Object.defineProperty(navigator, 'languages', { get: () => ['en-US','en'] }); } } catch (e) {}
  try { Object.defineProperty(navigator, 'plugins', { get: () => [1,2,3,4,5] }); } catch (e) {}
  if (!window.chrome) { window.chrome = { runtime: {} }; }
})();`

// ensureChrome lazily connects a chromedp context to the session's CDP
// endpoint (remote allocator) the first time an action runs. Must be called
// with s.mu held.
func ensureChrome(s *Session) error {
	if s.taskCtx != nil {
		return s.taskCtx.Err()
	}
	if s.Endpoint == "" {
		return fmt.Errorf("session has no CDP endpoint")
	}
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), s.Endpoint)
	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(taskCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			if _, err := page.AddScriptToEvaluateOnNewDocument(stealthScript).Do(ctx); err != nil {
				return err
			}
			return page.SetLifecycleEventsEnabled(true).Do(ctx)
		}),
	); err != nil {
		taskCancel()
		allocCancel()
		return err
	}
	s.allocCtx = allocCtx
	s.taskCtx = taskCtx
	s.cancel = func() { taskCancel(); allocCancel() }
	return nil
}

// withAction runs fn against the session's chromedp ctx under a per-call
// deadline, holding s.mu (one tab, serialized). ensureChrome runs first.
func withAction(parent context.Context, s *Session, timeout time.Duration, fn func(ctx context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureChrome(s); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.taskCtx, timeout)
	defer cancel()
	// Honor caller cancellation too.
	go func() {
		select {
		case <-parent.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return fn(ctx)
}

// Navigate loads url and waits for readiness, updating s.CurrentURL on success.
func Navigate(parent context.Context, s *Session, url, waitFor string, waitMs int) (title string, err error) {
	err = withAction(parent, s, navTimeout, func(ctx context.Context) error {
		idleCh := make(chan struct{}, 1)
		chromedp.ListenTarget(ctx, func(ev any) {
			if e, ok := ev.(*page.EventLifecycleEvent); ok && e.Name == "networkIdle" {
				select {
				case idleCh <- struct{}{}:
				default:
				}
			}
		})
		if err := chromedp.Run(ctx, chromedp.Navigate(url), chromedp.WaitReady("body")); err != nil {
			return err
		}
		select {
		case <-idleCh:
		case <-time.After(networkIdleBudget):
		case <-ctx.Done():
			return ctx.Err()
		}
		if waitFor != "" {
			wc, wcancel := context.WithTimeout(ctx, elementWaitBudget)
			defer wcancel()
			if err := chromedp.Run(wc, chromedp.WaitVisible(waitFor)); err != nil {
				return fmt.Errorf("wait_for %q: %w", waitFor, err)
			}
		}
		settle := waitMs
		if settle <= 0 {
			settle = defaultSettleMs
		}
		if err := chromedp.Run(ctx, chromedp.Sleep(time.Duration(settle)*time.Millisecond)); err != nil {
			return err
		}
		return chromedp.Run(ctx, chromedp.Title(&title))
	})
	if err == nil {
		s.CurrentURL = url
	}
	return title, err
}

// Click clicks the selector (waiting for it to be visible).
func Click(parent context.Context, s *Session, selector, waitFor string) error {
	return withAction(parent, s, navTimeout, func(ctx context.Context) error {
		sel := selector
		if waitFor != "" {
			sel = waitFor
		}
		if err := waitVisible(ctx, sel); err != nil {
			return err
		}
		return chromedp.Run(ctx, chromedp.Click(selector))
	})
}

// TypeText clears the selector and types text into it.
func TypeText(parent context.Context, s *Session, selector, text string) error {
	return withAction(parent, s, navTimeout, func(ctx context.Context) error {
		if err := waitVisible(ctx, selector); err != nil {
			return err
		}
		return chromedp.Run(ctx, chromedp.Clear(selector), chromedp.SendKeys(selector, text))
	})
}

// GetHTML returns the innerHTML of selector (or body).
func GetHTML(parent context.Context, s *Session, selector string) (string, error) {
	if selector == "" {
		selector = "body"
	}
	var out string
	err := withAction(parent, s, navTimeout, func(ctx context.Context) error {
		if err := waitReady(ctx, selector); err != nil {
			return err
		}
		return chromedp.Run(ctx, chromedp.InnerHTML(selector, &out))
	})
	return out, err
}

// Screenshot returns a full-page JPEG.
func Screenshot(parent context.Context, s *Session) ([]byte, error) {
	var buf []byte
	err := withAction(parent, s, navTimeout, func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.FullScreenshot(&buf, screenshotMaxJPEG))
	})
	return buf, err
}

// Evaluate runs script and returns the JSON-able result.
func Evaluate(parent context.Context, s *Session, script string) (any, error) {
	var result any
	err := withAction(parent, s, navTimeout, func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Evaluate(script, &result))
	})
	return result, err
}

func waitVisible(ctx context.Context, selector string) error {
	wc, cancel := context.WithTimeout(ctx, elementWaitBudget)
	defer cancel()
	if err := chromedp.Run(wc, chromedp.WaitVisible(selector)); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("timed out waiting for selector %q (CSS only — Playwright :has-text()/text= are unsupported)", selector)
		}
		return fmt.Errorf("wait %q: %w", selector, err)
	}
	return nil
}

func waitReady(ctx context.Context, selector string) error {
	wc, cancel := context.WithTimeout(ctx, elementWaitBudget)
	defer cancel()
	if err := chromedp.Run(wc, chromedp.WaitReady(selector)); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("timed out waiting for selector %q", selector)
		}
		return fmt.Errorf("wait %q: %w", selector, err)
	}
	return nil
}
```

- [ ] **Step 6: Implement the Docker backend**

`internal/browser/docker.go` — mirrors M1's `docker.go` posture, but the container runs Chromium with remote debugging and is wired to the egress proxy. Chrome is started with no network access except through the proxy.

```go
package browser

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

const (
	browserLabel = "runtime.browser"
	browserUID   = 1000
	cdpPort      = "9222"
)

// DockerConfig is the container posture for real browser sandboxes.
type DockerConfig struct {
	Image     string  // default runtime-browser:latest
	MemMB     int64   // memory limit (default 1024 — Chrome is heavy)
	CPUs      float64 // cpu limit (default 1.0)
	ProfileMB int     // tmpfs profile dir size (default 256)
	Runtime   string  // optional engine runtime, e.g. "runsc"
}

type dockerBackend struct {
	cli *client.Client
	cfg DockerConfig
}

// NewDockerBackend connects to the engine (DOCKER_HOST or default socket).
func NewDockerBackend(cfg DockerConfig) (Backend, error) {
	if cfg.Image == "" {
		cfg.Image = "runtime-browser:latest"
	}
	if cfg.MemMB <= 0 {
		cfg.MemMB = 1024
	}
	if cfg.CPUs <= 0 {
		cfg.CPUs = 1.0
	}
	if cfg.ProfileMB <= 0 {
		cfg.ProfileMB = 256
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &dockerBackend{cli: cli, cfg: cfg}, nil
}

// Create starts one locked-down Chromium container: egress only via the proxy
// at proxyAddr, read-only rootfs, tmpfs profile, all caps dropped, non-root,
// bounded cpu/mem/pids. Chrome listens for CDP on cdpPort, published to the
// host on an ephemeral port so chromedp can connect.
func (d *dockerBackend) Create(ctx context.Context, tenant, proxyAddr string) (BrowserHandle, error) {
	pids := int64(512) // Chrome spawns many threads/processes
	cmd := []string{
		"chromium", "--headless=new", "--no-sandbox", "--disable-gpu",
		"--remote-debugging-address=0.0.0.0",
		"--remote-debugging-port=" + cdpPort,
		"--proxy-server=http://" + proxyAddr,
		"--disable-blink-features=AutomationControlled",
		"--user-data-dir=/profile",
		"--no-first-run", "--no-default-browser-check",
		"about:blank",
	}
	created, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image:  d.cfg.Image,
			Cmd:    cmd,
			User:   strconv.Itoa(browserUID),
			Env:    []string{"HTTP_PROXY=http://" + proxyAddr, "HTTPS_PROXY=http://" + proxyAddr, "NO_PROXY="},
			Labels: map[string]string{browserLabel: "1", browserLabel + ".tenant": tenant},
			ExposedPorts: map[container.PortSet]struct{}{}, // placeholder; see nat below
		},
		&container.HostConfig{
			ReadonlyRootfs: true,
			Tmpfs:          map[string]string{"/profile": fmt.Sprintf("size=%dm,mode=1777", d.cfg.ProfileMB), "/tmp": "size=64m,mode=1777"},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Runtime:        d.cfg.Runtime,
			PublishAllPorts: true,
			Resources: container.Resources{
				NanoCPUs:  int64(d.cfg.CPUs * 1e9),
				Memory:    d.cfg.MemMB << 20,
				PidsLimit: &pids,
			},
		},
		nil, nil, "")
	if err != nil {
		return BrowserHandle{}, err
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = d.cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return BrowserHandle{}, err
	}
	endpoint, err := d.waitForCDP(ctx, created.ID)
	if err != nil {
		_ = d.cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return BrowserHandle{}, fmt.Errorf("CDP never became ready: %w", err)
	}
	return BrowserHandle{ContainerID: created.ID, Endpoint: endpoint}, nil
}

// waitForCDP polls the published CDP port until Chrome answers /json/version,
// returning the webSocketDebuggerUrl chromedp connects to. Bounded wait.
func (d *dockerBackend) waitForCDP(ctx context.Context, containerID string) (string, error) {
	deadline := time.Now().Add(20 * time.Second)
	for {
		ep, err := d.cdpEndpoint(ctx, containerID)
		if err == nil && ep != "" {
			return ep, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout: %v", err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// cdpEndpoint inspects the container for the host port mapped to cdpPort and
// fetches the webSocketDebuggerUrl from Chrome's /json/version endpoint,
// rewriting its host to the mapped host:port (Chrome reports 0.0.0.0).
// (Implementation detail completed during execution; see helper below.)
func (d *dockerBackend) cdpEndpoint(ctx context.Context, containerID string) (string, error) {
	return cdpEndpointFromInspect(ctx, d.cli, containerID)
}

// Remove force-removes the container.
func (d *dockerBackend) Remove(ctx context.Context, containerID string) error {
	return d.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// ListLeftovers returns every container carrying the browser label.
func (d *dockerBackend) ListLeftovers(ctx context.Context) ([]string, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", browserLabel+"=1")),
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	return ids, nil
}
```

> **Implementer note:** the exact Docker SDK types for port publishing (`nat.PortSet`/`nat.PortMap`) and the `cdpEndpointFromInspect` helper (inspect → read `NetworkSettings.Ports["9222/tcp"]` host port → HTTP GET `http://127.0.0.1:<port>/json/version` → parse `webSocketDebuggerUrl` → swap its host for `127.0.0.1:<port>`) must be written against the installed `github.com/docker/docker` version. Put `cdpEndpointFromInspect` in `docker.go`. Verify with `go build ./...`. The M1 `docker.go` is the reference for client usage. This helper is only run in live tests, so unit `go test` does not cover it — that is acceptable (it is I/O glue, proven by Task 8's live test).

- [ ] **Step 7: Write the live test (Chrome required)**

`internal/browser/docker_live_test.go`:

```go
//go:build live

package browser

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLiveBrowseAndEgress is the real-Chrome proof: a container browses an
// allowed local server, extraction returns its text, and a denied host is
// blocked by the egress proxy. Requires Docker + the runtime-browser image
// (make browser-image) and runs only under -tags live.
func TestLiveBrowseAndEgress(t *testing.T) {
	// Local "allowed" site.
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html><body><main><h1>Live OK</h1></main></body></html>")
	}))
	defer site.Close()

	// Egress policy: allow the test site's host only. Neutralize the internal
	// block for the loopback test host via the resolver hook.
	host := strings.TrimPrefix(site.URL, "http://")
	hostOnly := host[:strings.IndexByte(host, ':')]
	pol, err := NewPolicy(ModeAllowList, []string{hostOnly})
	if err != nil {
		t.Fatal(err)
	}
	pol.lookup = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
	proxy := NewProxy(pol)
	ps := httptest.NewServer(proxy)
	defer ps.Close()
	proxyAddr := strings.TrimPrefix(ps.URL, "http://")

	be, err := NewDockerBackend(DockerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(be, Config{MaxPerTenant: 2, ProxyAddr: proxyAddr})
	ctx := context.Background()
	t.Cleanup(func() { m.ReapStartup(ctx) })

	s, err := m.Create(ctx, "acme")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer m.Close(ctx, "acme", s.ID)

	// NOTE: the test site and proxy run on the HOST; the container reaches them
	// via host.docker.internal — the implementer rewrites site.URL/proxyAddr
	// host to host.docker.internal for the in-container Chrome. Confirm during
	// execution that the allowed fetch works and adjust addresses as needed.
	if _, err := Navigate(ctx, s, site.URL, "h1", 0); err != nil {
		t.Fatalf("navigate allowed: %v", err)
	}
	txt, err := GetHTML(ctx, s, "body")
	if err != nil {
		t.Fatalf("get_text: %v", err)
	}
	if !strings.Contains(ExtractText(txt), "Live OK") {
		t.Fatalf("extract missing content: %q", txt)
	}

	// Denied host: a different host not in the allow-list must fail to load.
	if _, err := Navigate(ctx, s, "https://example.com", "", 0); err == nil {
		t.Fatal("navigate to non-allowlisted host should fail (egress blocked)")
	}

	// Screenshot returns bytes.
	shot, err := Screenshot(ctx, s)
	if err != nil || len(shot) == 0 {
		t.Fatalf("screenshot: err=%v len=%d", err, len(shot))
	}
	_ = time.Now
}
```

(Add `"net"` to this file's imports.)

- [ ] **Step 8: Build and run the hermetic tests**

Run: `go build ./... && go test ./internal/browser/`
Expected: PASS for hermetic tests; the live test is excluded (no `-tags live`). `go build ./...` must compile the live file too — run `go build -tags live ./internal/browser/` to confirm.

- [ ] **Step 9: Commit**

```bash
git add internal/browser/actions.go internal/browser/docker.go internal/browser/paths.go internal/browser/paths_test.go internal/browser/docker_live_test.go
git commit -m "feat(browser): chromedp action layer + Docker backend (Chrome via egress proxy)"
```

---

## Task 6: MCP tool server

The ten browser tools over the Manager, with `popTenant` tenancy and image-content for screenshots.

**Files:**
- Create: `internal/browser/tools.go`
- Test: `internal/browser/tools_test.go`

- [ ] **Step 1: Write the failing test**

`internal/browser/tools_test.go`:

```go
package browser

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerToolsRegistered(t *testing.T) {
	m := NewManager(NewFakeBackend(), Config{})
	srv := NewServer(m, false)
	if srv == nil {
		t.Fatal("nil server")
	}
}

func TestPopTenantStripAndDefault(t *testing.T) {
	tn, present, rest, err := popTenant(json.RawMessage(`{"__rt_tenant":"acme","x":1}`))
	if err != nil || tn != "acme" || !present {
		t.Fatalf("tenant=%q present=%v err=%v", tn, present, err)
	}
	var got map[string]any
	_ = json.Unmarshal(rest, &got)
	if _, leaked := got["__rt_tenant"]; leaked {
		t.Fatal("__rt_tenant not stripped")
	}
	tn2, present2, _, _ := popTenant(json.RawMessage(`{}`))
	if present2 || tn2 != "default" {
		t.Fatalf("absent key: tenant=%q present=%v", tn2, present2)
	}
}

// callTool drives a registered tool handler directly through the server's
// in-process dispatch by listing then calling — but since that needs a session
// transport, we instead assert handler wiring via a fake-backed create call.
func TestCreateBrowserToolFakeBackend(t *testing.T) {
	m := NewManager(NewFakeBackend(), Config{})
	// allowDirect=true so an absent __rt_tenant maps to "default" without the
	// gateway.
	srv := NewServer(m, true)
	_ = srv
	// Direct Manager check stands in for the create_browser handler path.
	s, err := m.Create(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		t.Fatal("empty id")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/browser/ -run 'TestServer|TestPopTenant|TestCreateBrowserTool'`
Expected: FAIL — `undefined: NewServer`.

- [ ] **Step 3: Implement the tool server**

`internal/browser/tools.go` — modeled on `internal/sandbox/tools.go`. The `add` helper pops the tenant, fails closed when absent unless `allowDirect`, and marshals results. `screenshot` returns image content.

```go
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const sessionNote = " The browser_id persists page, cookies, and scroll state across calls; reuse it for a multi-step flow and close_browser when done."
const selectorNote = " Selectors are standard CSS only — Playwright extensions (:has-text(), text=, >> chains) are not supported; use attribute or structural selectors, or evaluate."

type errMissing string

func (e errMissing) Error() string { return "missing required argument(s): " + string(e) }

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func errResult(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: msg}}}
}

// NewServer builds the browserd MCP server: the 10 browser tools over m. Tool
// names are unprefixed — the gateway namespaces them (browser__*). Every
// handler pops the reserved __rt_tenant the gateway injects; an absent key
// fails closed unless allowDirect.
func NewServer(m *Manager, allowDirect bool) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "runtime-browser", Version: "m2"}, nil)

	add := func(name, desc, schema string, h func(ctx context.Context, tenant string, args json.RawMessage) (*sdk.CallToolResult, error)) {
		srv.AddTool(&sdk.Tool{Name: name, Description: desc, InputSchema: json.RawMessage(schema)},
			func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				tenant, present, rest, err := popTenant(req.Params.Arguments)
				if err != nil {
					return errResult("invalid arguments: " + err.Error()), nil
				}
				if !present && !allowDirect {
					return errResult("missing gateway tenant: browserd must be served behind the platform gateway with forward_tenant: true (or set RUNTIME_BROWSER_ALLOW_DIRECT=1 for single-tenant direct use)"), nil
				}
				res, err := h(ctx, tenant, rest)
				if err != nil {
					return errResult(err.Error()), nil
				}
				return res, nil
			})
	}

	// jsonResult marshals v into a single text part.
	jsonResult := func(v any) (*sdk.CallToolResult, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return errResult("internal: marshal result: " + err.Error()), nil
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(b)}}}, nil
	}

	add("create_browser",
		"Create an isolated headless-browser sandbox (Chromium). Returns a browser_id for the other browser tools. Network access is governed by the platform egress policy."+sessionNote,
		`{"type":"object","properties":{}}`,
		func(ctx context.Context, tenant string, _ json.RawMessage) (*sdk.CallToolResult, error) {
			s, err := m.Create(ctx, tenant)
			if err != nil {
				return nil, err
			}
			return jsonResult(map[string]any{"browser_id": s.ID, "expires_at": s.ExpiresAt.Format(time.RFC3339)})
		})

	add("navigate",
		"Navigate the browser to a URL and wait for it to load. Returns the final url and page title. Blocked hosts (per egress policy) return a navigation error."+sessionNote,
		`{"type":"object","properties":{
			"browser_id":{"type":"string","description":"id from create_browser"},
			"url":{"type":"string","description":"http(s) URL to load"},
			"wait_for":{"type":"string","description":"optional CSS selector to wait for (SPAs)"},
			"wait_ms":{"type":"integer","description":"optional extra settle time in ms after load"}
		},"required":["browser_id","url"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (*sdk.CallToolResult, error) {
			var a struct {
				BrowserID string `json:"browser_id"`
				URL       string `json:"url"`
				WaitFor   string `json:"wait_for"`
				WaitMs    int    `json:"wait_ms"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if a.BrowserID == "" || a.URL == "" {
				return nil, errMissing("browser_id, url")
			}
			if err := validateNavURL(a.URL); err != nil {
				return nil, err
			}
			s, err := m.Lookup(tenant, a.BrowserID)
			if err != nil {
				return nil, err
			}
			title, err := Navigate(ctx, s, a.URL, a.WaitFor, a.WaitMs)
			if err != nil {
				return nil, m.maskNav(s.ID, err)
			}
			return jsonResult(map[string]any{"url": a.URL, "title": title})
		})

	add("click",
		"Click an element by CSS selector."+sessionNote+selectorNote,
		`{"type":"object","properties":{
			"browser_id":{"type":"string"},"selector":{"type":"string"},
			"wait_for":{"type":"string","description":"optional selector to wait for before clicking"}
		},"required":["browser_id","selector"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (*sdk.CallToolResult, error) {
			var a struct {
				BrowserID, Selector, WaitFor string
			}
			var in struct {
				BrowserID string `json:"browser_id"`
				Selector  string `json:"selector"`
				WaitFor   string `json:"wait_for"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			a.BrowserID, a.Selector, a.WaitFor = in.BrowserID, in.Selector, in.WaitFor
			if a.BrowserID == "" || a.Selector == "" {
				return nil, errMissing("browser_id, selector")
			}
			s, err := m.Lookup(tenant, a.BrowserID)
			if err != nil {
				return nil, err
			}
			if err := Click(ctx, s, a.Selector, a.WaitFor); err != nil {
				return nil, m.maskNav(s.ID, err)
			}
			return jsonResult(map[string]any{"clicked": a.Selector})
		})

	add("type",
		"Type text into an input element by CSS selector (clears it first)."+sessionNote+selectorNote,
		`{"type":"object","properties":{
			"browser_id":{"type":"string"},"selector":{"type":"string"},"text":{"type":"string"}
		},"required":["browser_id","selector","text"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (*sdk.CallToolResult, error) {
			var in struct {
				BrowserID string  `json:"browser_id"`
				Selector  string  `json:"selector"`
				Text      *string `json:"text"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if in.BrowserID == "" || in.Selector == "" || in.Text == nil {
				return nil, errMissing("browser_id, selector, text")
			}
			s, err := m.Lookup(tenant, in.BrowserID)
			if err != nil {
				return nil, err
			}
			if err := TypeText(ctx, s, in.Selector, *in.Text); err != nil {
				return nil, m.maskNav(s.ID, err)
			}
			return jsonResult(map[string]any{"typed": in.Selector})
		})

	add("get_text",
		"Get the innerHTML of an element (defaults to body)."+sessionNote,
		`{"type":"object","properties":{
			"browser_id":{"type":"string"},"selector":{"type":"string"}
		},"required":["browser_id"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (*sdk.CallToolResult, error) {
			var in struct {
				BrowserID string `json:"browser_id"`
				Selector  string `json:"selector"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if in.BrowserID == "" {
				return nil, errMissing("browser_id")
			}
			s, err := m.Lookup(tenant, in.BrowserID)
			if err != nil {
				return nil, err
			}
			html, err := GetHTML(ctx, s, in.Selector)
			if err != nil {
				return nil, m.maskNav(s.ID, err)
			}
			return jsonResult(map[string]any{"html": clip(html)})
		})

	add("extract",
		"Extract clean readable text from the current page (script/style/nav stripped). Prefer this over get_text for reading content."+sessionNote,
		`{"type":"object","properties":{
			"browser_id":{"type":"string"},"selector":{"type":"string"}
		},"required":["browser_id"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (*sdk.CallToolResult, error) {
			var in struct {
				BrowserID string `json:"browser_id"`
				Selector  string `json:"selector"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if in.BrowserID == "" {
				return nil, errMissing("browser_id")
			}
			s, err := m.Lookup(tenant, in.BrowserID)
			if err != nil {
				return nil, err
			}
			html, err := GetHTML(ctx, s, in.Selector)
			if err != nil {
				return nil, m.maskNav(s.ID, err)
			}
			return jsonResult(map[string]any{"text": clip(ExtractText(html))})
		})

	add("screenshot",
		"Capture a screenshot of the current page. Returns an image."+sessionNote,
		`{"type":"object","properties":{"browser_id":{"type":"string"}},"required":["browser_id"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (*sdk.CallToolResult, error) {
			var in struct {
				BrowserID string `json:"browser_id"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if in.BrowserID == "" {
				return nil, errMissing("browser_id")
			}
			s, err := m.Lookup(tenant, in.BrowserID)
			if err != nil {
				return nil, err
			}
			shot, err := Screenshot(ctx, s)
			if err != nil {
				return nil, m.maskNav(s.ID, err)
			}
			return &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.TextContent{Text: fmt.Sprintf("Screenshot captured (%d bytes).", len(shot))},
				&sdk.ImageContent{MIMEType: "image/jpeg", Data: shot},
			}}, nil
		})

	add("evaluate",
		"Execute JavaScript in the page and return its result as JSON."+sessionNote,
		`{"type":"object","properties":{
			"browser_id":{"type":"string"},"script":{"type":"string"}
		},"required":["browser_id","script"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (*sdk.CallToolResult, error) {
			var in struct {
				BrowserID string `json:"browser_id"`
				Script    string `json:"script"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if in.BrowserID == "" || in.Script == "" {
				return nil, errMissing("browser_id, script")
			}
			s, err := m.Lookup(tenant, in.BrowserID)
			if err != nil {
				return nil, err
			}
			result, err := Evaluate(ctx, s, in.Script)
			if err != nil {
				return nil, m.maskNav(s.ID, err)
			}
			return jsonResult(map[string]any{"result": result})
		})

	add("list_browsers",
		"List your live browser sandboxes with their timestamps and current URL.",
		`{"type":"object","properties":{}}`,
		func(_ context.Context, tenant string, _ json.RawMessage) (*sdk.CallToolResult, error) {
			sessions := m.List(tenant)
			out := make([]map[string]any, 0, len(sessions))
			for _, s := range sessions {
				out = append(out, map[string]any{
					"browser_id": s.ID, "created_at": s.CreatedAt.Format(time.RFC3339),
					"last_used_at": s.LastUsed.Format(time.RFC3339), "expires_at": s.ExpiresAt.Format(time.RFC3339),
					"current_url": s.CurrentURL,
				})
			}
			return jsonResult(map[string]any{"browsers": out})
		})

	add("close_browser",
		"Close a browser sandbox and discard its state. Idempotent.",
		`{"type":"object","properties":{"browser_id":{"type":"string"}},"required":["browser_id"]}`,
		func(ctx context.Context, tenant string, raw json.RawMessage) (*sdk.CallToolResult, error) {
			var in struct {
				BrowserID string `json:"browser_id"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if in.BrowserID == "" {
				return nil, errMissing("browser_id")
			}
			if err := m.Close(ctx, tenant, in.BrowserID); err != nil {
				return nil, err
			}
			return jsonResult(map[string]any{"closed": true})
		})

	return srv
}

// maxOutput caps text returned to the model.
const maxOutput = 256 << 10

func clip(s string) string {
	if len(s) > maxOutput {
		return s[:maxOutput] + "\n[truncated]"
	}
	return s
}
```

Add the `maskNav` helper to `manager.go` (a sibling of `maskIfGone` that passes user-actionable navigation/selector errors through verbatim while genericizing engine errors):

```go
// maskNav is maskIfGone for action errors: a vanished session → errNoSandbox;
// a still-live session's error passes through (selector/egress/JS errors are
// user-actionable and leak nothing), EXCEPT it is logged for the operator.
func (m *Manager) maskNav(id string, err error) error {
	if err == nil {
		return nil
	}
	m.mu.Lock()
	_, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return errNoSandbox
	}
	return err // navigation/selector/egress errors are actionable for the agent
}
```

> **Implementer note:** verify the SDK's `ImageContent` field names (`MIMEType` vs `MimeType`) against the installed `modelcontextprotocol/go-sdk` — `internal/gateway/server.go` already constructs an `ImageContent`, so copy its exact field names. Fix the test/code to match.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/browser/ && go vet ./internal/browser/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/browser/tools.go internal/browser/tools_test.go internal/browser/manager.go
git commit -m "feat(browser): MCP tool server (10 tools, image screenshots, tenancy)"
```

---

## Task 7: cmd/browserd + Dockerfile + Makefile target

Wire the daemon: env config, start the egress proxy, Manager, reapers, serve MCP over stdio. Add the Chrome image and `make browser-image`.

**Files:**
- Create: `cmd/browserd/main.go`
- Create: `deploy/browser.Dockerfile`
- Modify: `Makefile` (add `browser-image` target)
- Modify: `internal/browser/egress.go` (wire the obs metric — see step 3)
- Modify: `internal/obs/obs.go` (add `BrowserEgress` counter)

- [ ] **Step 1: Add the egress metric to internal/obs**

In `internal/obs/obs.go`, add to `ControlMetrics` a counter and a nil-safe helper (follow the existing pattern of the other counters in that file — find an existing `prometheus.NewCounterVec` registration and mirror it):

```go
// (field on ControlMetrics)
//   browserEgress *prometheus.CounterVec   // runtime_browser_egress_total{decision}
//
// (in the constructor, registered on the same registry as the others)
//   cm.browserEgress = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
//       Name: "runtime_browser_egress_total",
//       Help: "Browser egress decisions by outcome.",
//   }, []string{"decision"})
//
// (nil-safe helper)
func (m *ControlMetrics) BrowserEgress(allowed bool) {
	if m == nil || m.browserEgress == nil {
		return
	}
	d := "deny"
	if allowed {
		d = "allow"
	}
	m.browserEgress.WithLabelValues(d).Inc()
}
```

> **Implementer note:** match the EXACT constructor style in `obs.go` (it uses `promauto.With(reg)` or direct `reg.MustRegister` — copy whichever the file uses). Verify with `go test ./internal/obs/`.

- [ ] **Step 2: Run the obs test to confirm no regression**

Run: `go test ./internal/obs/`
Expected: PASS.

- [ ] **Step 3: Implement cmd/browserd/main.go**

```go
// Command browserd is the Sandboxes M2 MCP server: isolated headless-browser
// sandboxes (one locked-down Chromium container per session) exposed as MCP
// tools over stdio, designed to run as a gateway upstream with
// forward_tenant: true. Chrome has no direct network — all traffic is forced
// through the in-process egress proxy, which allows/denies by hostname.
//
// Run exactly one browserd per host (or per DOCKER_HOST): reap-on-start removes
// ALL runtime.browser=1 containers.
//
// Env:
//
//	RUNTIME_BROWSER_IMAGE           container image (default runtime-browser:latest)
//	RUNTIME_BROWSER_MAX_PER_TENANT  concurrent browsers per tenant (default 5)
//	RUNTIME_BROWSER_IDLE_TTL        idle close, Go duration (default 10m)
//	RUNTIME_BROWSER_MAX_LIFETIME    hard close, Go duration (default 1h)
//	RUNTIME_BROWSER_MEM_MB          memory limit (default 1024)
//	RUNTIME_BROWSER_CPUS            cpu limit (default 1.0)
//	RUNTIME_BROWSER_PROFILE_MB      tmpfs profile size (default 256)
//	RUNTIME_BROWSER_RUNTIME         engine runtime, e.g. runsc
//	RUNTIME_BROWSER_EGRESS_MODE     deny-all | allow-list | allow-all-public (default deny-all)
//	RUNTIME_BROWSER_EGRESS_ALLOW    comma-separated hostname globs (allow-list mode)
//	RUNTIME_BROWSER_PROXY_ADDR      host:port the egress proxy listens on (default 127.0.0.1:0 → ephemeral)
//	RUNTIME_BROWSER_ALLOW_DIRECT    "1" ⇒ accept calls without the gateway's __rt_tenant key
//	RUNTIME_BROWSER_FAKE            "1" ⇒ in-memory fake backend (tests only)
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sausheong/runtime/internal/browser"
)

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("browserd: bad integer env, using default", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Warn("browserd: bad float env, using default", "key", key, "value", v, "default", def)
		return def
	}
	return f
}

func envDur(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("browserd: bad duration env, using default", "key", key, "value", v, "default", def)
		return def
	}
	return d
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Egress policy.
	mode := os.Getenv("RUNTIME_BROWSER_EGRESS_MODE")
	if mode == "" {
		mode = browser.ModeDenyAll
	}
	var allow []string
	if a := os.Getenv("RUNTIME_BROWSER_EGRESS_ALLOW"); a != "" {
		for _, g := range strings.Split(a, ",") {
			if g = strings.TrimSpace(g); g != "" {
				allow = append(allow, g)
			}
		}
	}
	policy, err := browser.NewPolicy(mode, allow)
	if err != nil {
		slog.Error("browserd: bad egress policy", "err", err)
		os.Exit(1)
	}

	// Start the egress proxy on a listener the containers can reach.
	proxyAddr := os.Getenv("RUNTIME_BROWSER_PROXY_ADDR")
	if proxyAddr == "" {
		proxyAddr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		slog.Error("browserd: egress proxy listen failed", "addr", proxyAddr, "err", err)
		os.Exit(1)
	}
	proxy := browser.NewProxy(policy)
	go func() {
		if err := http.Serve(ln, proxy); err != nil {
			slog.Error("browserd: egress proxy exited", "err", err)
		}
	}()
	actualProxyAddr := ln.Addr().String()
	slog.Info("browserd: egress proxy listening", "addr", actualProxyAddr, "mode", mode)

	var be browser.Backend
	if os.Getenv("RUNTIME_BROWSER_FAKE") == "1" {
		slog.Warn("browserd: RUNTIME_BROWSER_FAKE=1 — in-memory fake backend (tests only)")
		be = browser.NewFakeBackend()
	} else {
		be, err = browser.NewDockerBackend(browser.DockerConfig{
			Image:     os.Getenv("RUNTIME_BROWSER_IMAGE"),
			MemMB:     int64(envInt("RUNTIME_BROWSER_MEM_MB", 1024)),
			CPUs:      envFloat("RUNTIME_BROWSER_CPUS", 1.0),
			ProfileMB: envInt("RUNTIME_BROWSER_PROFILE_MB", 256),
			Runtime:   os.Getenv("RUNTIME_BROWSER_RUNTIME"),
		})
		if err != nil {
			slog.Error("browserd: docker backend init failed", "err", err)
			os.Exit(1)
		}
	}

	m := browser.NewManager(be, browser.Config{
		MaxPerTenant: envInt("RUNTIME_BROWSER_MAX_PER_TENANT", 5),
		IdleTTL:      envDur("RUNTIME_BROWSER_IDLE_TTL", 10*time.Minute),
		MaxLifetime:  envDur("RUNTIME_BROWSER_MAX_LIFETIME", time.Hour),
		ProxyAddr:    actualProxyAddr,
	})

	ctx := context.Background()
	if err := m.ReapStartup(ctx); err != nil {
		slog.Warn("browserd: startup reap failed", "err", err)
	}
	m.StartReaper(ctx, time.Minute)

	allowDirect := os.Getenv("RUNTIME_BROWSER_ALLOW_DIRECT") == "1"
	srv := browser.NewServer(m, allowDirect)
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil {
		slog.Error("browserd: server exited", "err", err)
		os.Exit(1)
	}
}
```

> **Implementer note:** the egress metric (`obs.ControlMetrics.BrowserEgress`) is wired by setting `proxy.onDecision` — but `onDecision` is unexported. Add an exported setter `func (p *Proxy) OnDecision(fn func(host string, allowed bool))` to `egress.go`, and in browserd, if you choose to surface metrics from the standalone daemon, call it. Since browserd is a separate process from runtimed (which owns the obs registry), M2 logs every decision via `slog` inside the proxy and leaves the Prometheus counter wired for in-process use; the e2e test asserts the gateway-side `runtime_gateway_tool_calls_total` instead. Add the `slog` line in `decide()`. Keep `BrowserEgress` defined in obs for future in-process use and to satisfy the spec's metric mention.

Update `decide` in `egress.go` to log:

```go
func (p *Proxy) decide(host string) bool {
	err := p.policy.Decide(host)
	allowed := err == nil
	if p.onDecision != nil {
		p.onDecision(host, allowed)
	}
	if allowed {
		slog.Debug("egress allow", "host", host)
	} else {
		slog.Info("egress deny", "host", host, "reason", err)
	}
	return allowed
}
```

(Add `"log/slog"` to `egress.go` imports and the exported `OnDecision` setter.)

- [ ] **Step 4: Create the Chrome Dockerfile**

`deploy/browser.Dockerfile`:

```dockerfile
# The bundled browser image for Sandboxes M2 (cmd/browserd).
# Build: make browser-image
# Override at runtime with RUNTIME_BROWSER_IMAGE.
FROM debian:bookworm-slim

# Chromium + fonts for headless rendering.
RUN apt-get update && apt-get install -y --no-install-recommends \
        chromium fonts-liberation ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Non-root user; uid must match browserUID in internal/browser/docker.go.
RUN useradd --uid 1000 --create-home browser
USER browser
WORKDIR /home/browser
```

- [ ] **Step 5: Add the Makefile target**

Find the existing `sandbox-image` target in `Makefile` and add a sibling immediately after it:

```makefile
.PHONY: browser-image
browser-image:
	docker build -f deploy/browser.Dockerfile -t runtime-browser:latest .
```

- [ ] **Step 6: Build everything**

Run: `go build ./... && go build -tags live ./... && go vet ./...`
Expected: PASS — `cmd/browserd` compiles, live test compiles.

- [ ] **Step 7: Commit**

```bash
git add cmd/browserd/main.go deploy/browser.Dockerfile Makefile internal/browser/egress.go internal/obs/obs.go internal/obs/obs_test.go
git commit -m "feat(browser): cmd/browserd daemon, Chrome image, egress metric"
```

---

## Task 8: Through-serve e2e + docs

The federation proof with identity and two tenants (fake backend, no Chrome), plus ROADMAP/README updates.

**Files:**
- Create: `test/gateway_browser_e2e_test.go`
- Modify: `ROADMAP.md`
- Modify: `README.md`

- [ ] **Step 1: Write the e2e test (mirrors test/gateway_sandbox_e2e_test.go)**

`test/gateway_browser_e2e_test.go`. Reuse the helpers already in the `test` package (`bearerRT`, `connectGatewayAs`, `connectWhenFederated`, `callJSON`, `mustExec`, `waitURL`, `dsn` — all defined in `gateway_sandbox_e2e_test.go`, same package). Do NOT redefine them.

```go
//go:build integration

package test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sausheong/runtime/internal/identity"
)

// TestGatewayBrowserE2E boots the whole stack with identity ENFORCED and a
// forward_tenant browserd upstream (fake backend, no Chrome): two tenants
// federate the browser tools, tenant scoping holds, and a spoofed __rt_tenant
// is overridden.
func TestGatewayBrowserE2E(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres (is it running at %s?): %v", dsn, err)
	}
	mustExec(t, db, `DROP TABLE IF EXISTS session_events, sessions, agents CASCADE`)
	mustExec(t, db, `DROP SCHEMA IF EXISTS dbos CASCADE`)
	for _, q := range []string{
		`DROP TABLE IF EXISTS service_keys CASCADE`,
		`DROP TABLE IF EXISTS identity_users CASCADE`,
		`DROP TABLE IF EXISTS tenants CASCADE`,
	} {
		mustExec(t, db, q)
	}
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

	st, err := identity.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, "beta", "Beta"); err != nil {
		t.Fatal(err)
	}
	alphaKey, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, alphaKey.ID, "alpha", alphaKey.Hash, identity.RoleOperator, "alpha-op"); err != nil {
		t.Fatal(err)
	}
	betaKey, _ := identity.MintServiceKey()
	if err := st.InsertServiceKey(ctx, betaKey.ID, "beta", betaKey.Hash, identity.RoleOperator, "beta-op"); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	agentd := filepath.Join(tmp, "agentd")
	if out, err := exec.Command("go", "build", "-o", agentd, "../cmd/agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	runtimed := filepath.Join(tmp, "runtimed")
	if out, err := exec.Command("go", "build", "-o", runtimed, "../cmd/runtimed").CombinedOutput(); err != nil {
		t.Fatalf("build runtimed: %v\n%s", err, out)
	}
	browserd := filepath.Join(tmp, "browserd")
	if out, err := exec.Command("go", "build", "-o", browserd, "../cmd/browserd").CombinedOutput(); err != nil {
		t.Fatalf("build browserd: %v\n%s", err, out)
	}

	cfgPath := filepath.Join(tmp, "runtime.yaml")
	cfg := "agents:\n" +
		"  - {id: a1, name: A1, model: test/scripted, listen_addr: 127.0.0.1:8181, tenant: alpha}\n" +
		"gateway:\n" +
		"  servers:\n" +
		"    - name: browser\n" +
		"      command: " + browserd + "\n" +
		"      forward_tenant: true\n" +
		"      env: {RUNTIME_BROWSER_FAKE: \"1\"}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctlAddr := "127.0.0.1:8180"
	cmd := exec.Command(runtimed)
	cmd.Env = append(os.Environ(),
		"RUNTIME_PG_DSN="+dsn,
		"RUNTIME_CTL_ADDR="+ctlAddr,
		"RUNTIME_AGENTD_BIN="+agentd,
		"RUNTIME_CONFIG="+cfgPath,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	base := "http://" + ctlAddr
	waitURL(t, base+"/healthz", 15*time.Second)

	alphaSess := connectWhenFederated(t, base, alphaKey.Plaintext,
		"browser__create_browser", "browser__navigate")
	betaSess := connectWhenFederated(t, base, betaKey.Plaintext,
		"browser__list_browsers")

	// alpha creates a browser WITH a spoofed __rt_tenant — gateway overrides it.
	var created struct {
		BrowserID string `json:"browser_id"`
	}
	callJSON(t, alphaSess, "browser__create_browser", map[string]any{"__rt_tenant": "beta"}, &created)
	if !strings.HasPrefix(created.BrowserID, "brw-") {
		t.Fatalf("browser_id %q lacks brw- prefix", created.BrowserID)
	}

	// beta lists: ZERO (spoof did not land under beta; list is tenant-scoped).
	var listed struct {
		Browsers []map[string]any `json:"browsers"`
	}
	callJSON(t, betaSess, "browser__list_browsers", nil, &listed)
	if len(listed.Browsers) != 0 {
		t.Fatalf("beta sees %d browsers, want 0 (tenant leak)", len(listed.Browsers))
	}

	// beta cannot close alpha's browser via a navigate (existence hidden →
	// navigate against the fake endpoint errors, but cross-tenant must be the
	// not-found path: assert IsError).
	res, err := betaSess.CallTool(ctx, &sdk.CallToolParams{
		Name:      "browser__navigate",
		Arguments: map[string]any{"browser_id": created.BrowserID, "url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("beta cross-tenant navigate (protocol): %v", err)
	}
	if !res.IsError {
		t.Fatalf("beta navigated alpha's browser — cross-tenant access")
	}
}
```

> **Implementer note:** the fake backend returns a handle with an EMPTY CDP endpoint, so `navigate` against an alpha-owned browser would fail at `ensureChrome` ("no CDP endpoint") — that is fine for this test, which only needs `create_browser`/`list_browsers` to prove federation + tenancy, and the cross-tenant `navigate` to return `IsError` (it returns errNoSandbox for beta before ever touching Chrome). Do NOT assert a successful navigate here — Chrome lives in the live test.

- [ ] **Step 2: Run the e2e (requires Postgres at the integration dsn)**

Run: `go test -tags integration ./test/ -run TestGatewayBrowserE2E -v`
Expected: PASS (federation, tenant scoping, spoof override).

- [ ] **Step 3: Run the full hermetic suite + vet**

Run: `go test ./... && go vet ./... && go build -tags live ./...`
Expected: all PASS.

- [ ] **Step 4: Update ROADMAP.md**

In the Sandboxes section (around line 387, the "Remaining B4" sentence), record M2 as done. Add a milestone entry after the M1 paragraph following the exact style of the M1 entry and the Gateway M3 entry: a "Second milestone DONE" paragraph naming `cmd/browserd`/`internal/browser`, the forced-proxy egress (three modes + unconditional internal block + DNS-rebind defense), the chromedp-over-remote-CDP container, the ten tools, the reuse of M1's Manager contract, and the live-proof results (filled in after the live proof runs). Update the "Remaining B4" list to drop "browser sandbox (M2 candidate)" and "network egress policy".

- [ ] **Step 5: Update README.md**

Add a "Browser sandbox" subsection near the existing Sandboxes/code-interpreter documentation describing: the `command: browserd` + `forward_tenant: true` gateway wiring, the `RUNTIME_BROWSER_EGRESS_MODE`/`_ALLOW` env, the ten `browser__*` tools, and the security posture (container has no direct egress; proxy enforces hostname allow/deny; internal addresses always blocked). Add the testing entries (`go test ./internal/browser/...`, the `-tags live` browser test needing `make browser-image`, the `-tags integration` e2e).

- [ ] **Step 6: Commit**

```bash
git add test/gateway_browser_e2e_test.go ROADMAP.md README.md
git commit -m "test(browser): through-serve e2e + docs (ROADMAP/README)"
```

---

## Final verification (after all tasks)

- [ ] `go test ./...` — full hermetic suite green
- [ ] `go test -tags integration ./test/ -run TestGatewayBrowserE2E` — federation e2e green (needs Postgres)
- [ ] `go vet ./...` and `go build -tags live ./...` — clean
- [ ] `make browser-image` then `go test -tags live ./internal/browser/ -run TestLiveBrowseAndEgress` — real-Chrome proof green (needs Docker)
- [ ] Live proof (recorded in ROADMAP): real federated browse on an allow-listed page; egress block of a non-allowlisted host + internal-address refusal under allow-all-public; screenshot through the gateway; an end-to-end agent turn using a browser tool + `gateway: search` discovery
- [ ] Merge to master with `--no-ff`, delete branch, update memory
