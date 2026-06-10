package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sausheong/harness/tool"

	"github.com/sausheong/runtime/internal/identity"
)

// Handler serves the federated tool set over MCP Streamable HTTP plus the
// operator status endpoint. Per-tenant SDK servers are cached and rebuilt
// when the Manager's generation moves.
//
// PrincipalFor extracts the authenticated principal from a request context.
// It is injected (rather than importing controlplane) to avoid an import
// cycle. Wiring states:
//   - nil (the zero state): NOT WIRED — HTTP() and Status fail loud with 503
//     so a wiring omission can never silently serve everything open;
//   - OpenMode: identity is intentionally off — unauthenticated full
//     visibility, calls allowed;
//   - wired (runtimed sets controlplane.PrincipalFromContext): per-principal
//     tenant views and role gates.
type Handler struct {
	m            *Manager
	PrincipalFor func(ctx context.Context) (identity.Principal, bool)

	mu sync.Mutex
	// cache maps tenant view key → server, rebuilt when the Manager's
	// generation moves. Replacement semantics: existing MCP sessions keep
	// the old server (and thus the old view) until they reconnect; stale
	// tools on the old server fail per-call with IsError. Replaced servers
	// are plain in-memory objects — upstream connections belong to the
	// Manager — so dropping them leaks nothing.
	cache map[string]*cachedServer
}

// OpenMode is the explicit opt-in for running the gateway without identity:
// set h.PrincipalFor = gateway.OpenMode when identity is off. It reports no
// principal, which means unauthenticated full visibility and calls allowed.
var OpenMode = func(context.Context) (identity.Principal, bool) { return identity.Principal{}, false }

type cachedServer struct {
	gen uint64
	srv *sdk.Server
}

// NewHandler builds a Handler over m. PrincipalFor is left nil (NOT WIRED):
// HTTP() and Status serve 503 until it is set to gateway.OpenMode or a real
// principal extractor.
func NewHandler(m *Manager) *Handler {
	return &Handler{
		m:     m,
		cache: map[string]*cachedServer{},
	}
}

// viewKey computes the cache key and tenant filter for a principal.
// Unscoped ("") for open mode and superusers. A non-superuser principal
// with an empty TenantID gets an impossible view (sees nothing) rather
// than falling through to the unscoped view — "" doubles as the
// see-everything filter in Manager.ToolsFor, and no legitimate principal
// has an empty tenant (only the bootstrap superuser does).
func viewKey(p identity.Principal, ok bool) (key, tenant string) {
	if !ok || p.Superuser {
		return "*", ""
	}
	if p.TenantID == "" {
		// Prefix-free sentinel key: real tenant keys all start with "t:" and
		// the unscoped key is "*", so "!none" cannot collide with any
		// principal-derived key. The noneTenant filter matches no upstream.
		return "!none", noneTenant
	}
	return "t:" + p.TenantID, p.TenantID
}

// HTTP returns the Streamable HTTP handler for /gateway/mcp. Call it once
// and mount the result; each call creates an independent session namespace
// (sessions established against one handler are unknown to another).
//
// The nil-PrincipalFor (not wired) check runs at request time, not here, so
// runtimed's wiring order does not matter.
func (h *Handler) HTTP() http.Handler {
	mcp := sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		p, ok := h.PrincipalFor(r.Context())
		return h.serverFor(p, ok)
	}, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.PrincipalFor == nil {
			http.Error(w, "gateway not wired", http.StatusServiceUnavailable)
			return
		}
		mcp.ServeHTTP(w, r)
	})
}

// serverFor returns the cached SDK server for the principal's view,
// rebuilding when the manager generation has moved.
func (h *Handler) serverFor(p identity.Principal, ok bool) *sdk.Server {
	key, tenant := viewKey(p, ok)
	gen := h.m.Generation()
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, hit := h.cache[key]; hit && c.gen == gen {
		return c.srv
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "runtime-gateway", Version: "m1"}, nil)
	for _, t := range h.m.ToolsFor(tenant) {
		srv.AddTool(&sdk.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: json.RawMessage(t.Parameters()),
		}, h.toolHandler(key, t))
	}
	h.cache[key] = &cachedServer{gen: gen, srv: srv}
	return srv
}

// toolHandler adapts one harness tool.Tool to an SDK ToolHandler. Two
// per-call gates run on the live request principal (MCP sessions are not
// principal-bound: later POSTs bypass getServer, so a session ID replayed
// by a different principal would otherwise inherit the creator's view):
//  1. view check — the caller's view key must match the view this server
//     was built for;
//  2. role gate — viewers cannot call tools (requires ≥ operator).
//
// The gates read the principal from the tool handler's ctx. With Streamable
// HTTP the SDK derives the handler ctx from the POST request that delivered
// the tools/call, so a per-request identity middleware upstream of HTTP()
// is honored here — confirmed by TestServerViewerCannotCall passing via
// this ctx path (no serverFor-time fallback needed).
func (h *Handler) toolHandler(builtFor string, t tool.Tool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		p, ok := h.PrincipalFor(ctx)
		if key, _ := viewKey(p, ok); key != builtFor {
			return errResult("forbidden: session does not belong to this principal's view"), nil
		}
		if ok && !p.Superuser && p.Role == identity.RoleViewer {
			return errResult("forbidden: role viewer cannot call tools (requires operator)"), nil
		}
		res, err := t.Execute(ctx, req.Params.Arguments)
		if err != nil {
			return errResult(err.Error()), nil
		}
		if res.Error != "" {
			return errResult(res.Error), nil
		}
		out := &sdk.CallToolResult{}
		// Emit the text part when there is output, or when there are no
		// images either — Content must never be empty.
		if res.Output != "" || len(res.Images) == 0 {
			out.Content = append(out.Content, &sdk.TextContent{Text: res.Output})
		}
		for _, img := range res.Images {
			out.Content = append(out.Content, &sdk.ImageContent{
				MIMEType: img.MimeType, Data: img.Data,
			})
		}
		return out, nil
	}
}

func errResult(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: msg}},
	}
}

// Status serves GET /gateway/status: per-upstream state, tenant-scoped.
// Requires role ≥ operator when identity is on (open mode: allowed).
// 503s when PrincipalFor is nil (gateway not wired).
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	if h.PrincipalFor == nil {
		http.Error(w, "gateway not wired", http.StatusServiceUnavailable)
		return
	}
	tenant := ""
	if p, ok := h.PrincipalFor(r.Context()); ok {
		if p.Role == identity.RoleViewer && !p.Superuser {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, tenant = viewKey(p, ok)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.m.Status(tenant))
}
