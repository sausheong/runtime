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
// cycle; runtimed wires it to controlplane.PrincipalFromContext. A false
// return means open mode: full visibility, calls allowed.
type Handler struct {
	m            *Manager
	PrincipalFor func(ctx context.Context) (identity.Principal, bool)

	mu    sync.Mutex
	cache map[string]*cachedServer // tenant view key → server
}

type cachedServer struct {
	gen uint64
	srv *sdk.Server
}

// NewHandler builds a Handler over m. PrincipalFor defaults to "no principal"
// (open mode) until wired.
func NewHandler(m *Manager) *Handler {
	return &Handler{
		m:            m,
		PrincipalFor: func(context.Context) (identity.Principal, bool) { return identity.Principal{}, false },
		cache:        map[string]*cachedServer{},
	}
}

// viewKey computes the cache key and the tenant filter for a principal.
// Unscoped ("" tenant filter) for open mode and superusers.
func viewKey(p identity.Principal, ok bool) (key, tenant string) {
	if !ok || p.Superuser {
		return "*", ""
	}
	return "t:" + p.TenantID, p.TenantID
}

// HTTP returns the Streamable HTTP handler for /gateway/mcp.
func (h *Handler) HTTP() http.Handler {
	return sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		p, ok := h.PrincipalFor(r.Context())
		return h.serverFor(p, ok)
	}, nil)
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
		}, h.toolHandler(t))
	}
	h.cache[key] = &cachedServer{gen: gen, srv: srv}
	return srv
}

// toolHandler adapts one harness tool.Tool to an SDK ToolHandler, enforcing
// the role gate: when a principal is present, calling requires ≥ operator
// (viewers may list but not call).
//
// The gate reads the principal from the tool handler's ctx. With Streamable
// HTTP the SDK derives the handler ctx from the POST request that delivered
// the tools/call, so a per-request identity middleware upstream of HTTP()
// is honored here — confirmed by TestServerViewerCannotCall passing via
// this ctx path (no serverFor-time fallback needed).
func (h *Handler) toolHandler(t tool.Tool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if p, ok := h.PrincipalFor(ctx); ok && !p.Superuser && p.Role == identity.RoleViewer {
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
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	tenant := ""
	if p, ok := h.PrincipalFor(r.Context()); ok {
		if p.Role == identity.RoleViewer && !p.Superuser {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !p.Superuser {
			tenant = p.TenantID
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.m.Status(tenant))
}
