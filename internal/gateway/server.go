package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

	// Index enables search mode (?mode=search): nil ⇒ search mode is
	// unavailable and requests for it are rejected with 400.
	Index *Index

	mu sync.Mutex
	// cache maps mode-qualified view key → server, rebuilt when the Manager's
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

// viewMode is the consumption mode of a gateway session.
type viewMode string

const (
	modeFull   viewMode = "full"
	modeSearch viewMode = "search"
)

// modeFromRequest parses ?mode=; absent/empty ⇒ full. Unknown values are an
// error (HTTP 400 at the edge, before session creation).
func modeFromRequest(r *http.Request) (viewMode, error) {
	switch r.URL.Query().Get("mode") {
	case "", "full":
		return modeFull, nil
	case "search":
		return modeSearch, nil
	default:
		return "", fmt.Errorf("unknown mode %q (want full|search)", r.URL.Query().Get("mode"))
	}
}

// principalView computes the principal-view base key and tenant filter for a
// principal. Unscoped ("") for open mode and superusers. A non-superuser
// principal with an empty TenantID gets an impossible view (sees nothing)
// rather than falling through to the unscoped view — "" doubles as the
// see-everything filter in Manager.ToolsFor, and no legitimate principal
// has an empty tenant (only the bootstrap superuser does).
func principalView(p identity.Principal, ok bool) (key, tenant string) {
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

// viewKey is the mode-qualified cache key: principal-view base + "|" + mode.
// The same principal may hold full and search sessions concurrently; only
// the base part identifies the principal's view (per-call re-checks compare
// the base, not the mode).
func viewKey(p identity.Principal, ok bool, mode viewMode) (key, tenant string) {
	base, tenant := principalView(p, ok)
	return base + "|" + string(mode), tenant
}

// viewBase strips the trailing "|<mode>" from a mode-qualified view key.
// The mode is always the LAST pipe-delimited segment ("full" or "search");
// the base may itself contain pipes (tenant IDs are free strings), so a
// first-pipe cut would truncate it — see the cross-tenant replay regression
// test.
func viewBase(key string) string {
	if i := strings.LastIndex(key, "|"); i >= 0 {
		return key[:i]
	}
	return key
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
		mode, _ := modeFromRequest(r) // junk already rejected in the wrapper
		return h.serverFor(p, ok, mode)
	}, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.PrincipalFor == nil {
			http.Error(w, "gateway not wired", http.StatusServiceUnavailable)
			return
		}
		mode, err := modeFromRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if mode == modeSearch && h.Index == nil {
			http.Error(w, "search mode requires embeddings (RUNTIME_EMBED_MODEL)", http.StatusBadRequest)
			return
		}
		mcp.ServeHTTP(w, r)
	})
}

// serverFor returns the cached SDK server for the principal's mode-qualified
// view, rebuilding when the manager generation has moved. Both modes register
// the full visible catalog as callable tools; search mode additionally adds
// search_tools and a list-filtering middleware so tools/list exposes only
// search_tools (the catalog stays callable but unlisted).
func (h *Handler) serverFor(p identity.Principal, ok bool, mode viewMode) *sdk.Server {
	key, tenant := viewKey(p, ok, mode)
	gen := h.m.Generation()
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, hit := h.cache[key]; hit && c.gen == gen {
		return c.srv
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "runtime-gateway", Version: "m2"}, nil)
	for _, t := range h.m.ToolsFor(tenant) {
		srv.AddTool(&sdk.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: json.RawMessage(t.Parameters()),
		}, h.toolHandler(key, t, h.m.ForwardsTenant(t.Name())))
	}
	if mode == modeSearch {
		srv.AddTool(searchToolDef(), h.searchHandler(key, tenant))
		srv.AddReceivingMiddleware(listOnlySearchTools)
	}
	h.cache[key] = &cachedServer{gen: gen, srv: srv}
	return srv
}

// toolHandler adapts one harness tool.Tool to an SDK ToolHandler. Two
// per-call gates run on the live request principal (MCP sessions are not
// principal-bound: later POSTs bypass getServer, so a session ID replayed
// by a different principal would otherwise inherit the creator's view):
//  1. view check — the caller's PRINCIPAL VIEW (base part of the
//     mode-qualified key, before "|") must match the view this server was
//     built for. The mode is deliberately excluded: it belongs to the
//     session's server, and the same principal may use full and search
//     sessions concurrently;
//  2. role gate — viewers cannot call tools (requires ≥ operator).
//
// The gates read the principal from the tool handler's ctx. With Streamable
// HTTP the SDK derives the handler ctx from the POST request that delivered
// the tools/call, so a per-request identity middleware upstream of HTTP()
// is honored here — confirmed by TestServerViewerCannotCall passing via
// this ctx path (no serverFor-time fallback needed).
func (h *Handler) toolHandler(builtFor string, t tool.Tool, forwardTenant bool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		p, ok := h.PrincipalFor(ctx)
		callerBase, _ := principalView(p, ok)
		builtBase := viewBase(builtFor)
		if callerBase != builtBase {
			return errResult("forbidden: session does not belong to this principal's view"), nil
		}
		if ok && !p.Superuser && p.Role == identity.RoleViewer {
			return errResult("forbidden: role viewer cannot call tools (requires operator)"), nil
		}
		args := req.Params.Arguments
		if forwardTenant {
			injected, err := injectTenant(args, p, ok)
			if err != nil {
				return errResult("invalid arguments: " + err.Error()), nil
			}
			args = injected
		}
		res, err := t.Execute(ctx, args)
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

// injectTenant strips any caller-supplied __rt_tenant from raw JSON arguments
// and sets the authenticated principal's tenant. Open mode and superusers
// inject "" (the upstream maps it to its default-tenant rule). The agent can
// therefore never choose its own tenant.
func injectTenant(raw json.RawMessage, p identity.Principal, ok bool) (json.RawMessage, error) {
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
	}
	tenant := ""
	if ok && !p.Superuser {
		tenant = p.TenantID
	}
	m["__rt_tenant"] = tenant
	return json.Marshal(m)
}

// searchToolDef describes the search_tools tool. The name cannot collide
// with upstream tools: their names always contain "__" (server__tool).
func searchToolDef() *sdk.Tool {
	return &sdk.Tool{
		Name: "search_tools",
		Description: "Search the tool catalog by describing what you want to do. " +
			"Returns matching tools (name, description, input schema) ranked by relevance; " +
			"call any returned tool directly by name.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "natural-language description of the capability you need"},
				"k": {"type": "integer", "description": "max results (default 5, cap 20)"}
			},
			"required": ["query"]
		}`),
	}
}

// searchHandler serves search_tools for one view. Viewers MAY search
// (search is a read, like tools/list); the call gate still protects the
// result tools themselves. Principal-view re-check matches toolHandler's.
func (h *Handler) searchHandler(builtFor, tenant string) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		p, ok := h.PrincipalFor(ctx)
		callerBase, _ := principalView(p, ok)
		builtBase := viewBase(builtFor)
		if callerBase != builtBase {
			return errResult("forbidden: session does not belong to this principal's view"), nil
		}
		if h.Index == nil {
			// Unreachable via HTTP() (search mode 400s without an Index);
			// cheap insurance for any future direct serverFor caller.
			return errResult("search unavailable: no embedding index configured"), nil
		}
		var in struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil || in.Query == "" {
			return errResult(`search_tools requires {"query": string}`), nil
		}
		ms, err := h.Index.Search(ctx, h.m.ToolsFor(tenant), in.Query, in.K)
		if err != nil {
			return errResult("search temporarily unavailable: " + err.Error()), nil
		}
		if ms == nil {
			ms = []Match{}
		}
		b, _ := json.Marshal(ms)
		text := string(b)
		if len(ms) == 0 {
			text += "\nNo tools matched; try a broader query."
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}, nil
	}
}

// listOnlySearchTools is receiving middleware that rewrites tools/list
// results to expose only search_tools — the catalog stays callable but
// unlisted (the point of search mode). Locked by unit test so an SDK
// upgrade that changes the method name or result type fails loudly.
func listOnlySearchTools(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil || method != "tools/list" {
			return res, err
		}
		lt, ok := res.(*sdk.ListToolsResult)
		if !ok {
			return res, err
		}
		filtered := &sdk.ListToolsResult{Tools: []*sdk.Tool{}}
		for _, t := range lt.Tools {
			if t.Name == "search_tools" {
				filtered.Tools = append(filtered.Tools, t)
			}
		}
		return filtered, nil
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
		_, tenant = principalView(p, ok)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.m.Status(tenant))
}
