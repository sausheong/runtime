package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/sausheong/harness/tool"
)

const (
	maxDescriptionLen = 1024
	// largeSpecWarn nudges operators toward an operations: filter when one
	// spec floods the catalog (spec §11).
	largeSpecWarn = 50
)

// generateTools parses an OpenAPI 3.x document and returns one restTool per
// selected operation, plus the resolved base URL (config override or spec
// servers[0]). client may be nil (tools get a default per-upstream client at
// dial; tests inject httptest clients).
func generateTools(server string, specBytes []byte, baseURL string, operations []string, client *http.Client) ([]restTool, string, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(specBytes)
	if err != nil {
		return nil, "", fmt.Errorf("openapi parse: %w", err)
	}
	vctx := loader.Context
	if vctx == nil {
		vctx = context.Background()
	}
	if err := doc.Validate(vctx); err != nil {
		return nil, "", fmt.Errorf("openapi validate: %w", err)
	}
	if baseURL == "" {
		if len(doc.Servers) > 0 && doc.Servers[0].URL != "" {
			baseURL = doc.Servers[0].URL
		} else {
			return nil, "", fmt.Errorf("openapi: no base_url configured and spec declares no servers[] entry")
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")

	var tools []restTool
	seen := map[string]bool{}
	// Deterministic order: sort paths, then fixed method order.
	paths := make([]string, 0, doc.Paths.Len())
	for p := range doc.Paths.Map() {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	methodOrder := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
	for _, p := range paths {
		item := doc.Paths.Value(p)
		ops := item.Operations() // map[method]*Operation, UPPERCASE keys
		for _, method := range methodOrder {
			op, ok := ops[method]
			if !ok {
				continue
			}
			if !operationSelected(operations, op.OperationID, method, p) {
				continue
			}
			name := toolNameFor(op.OperationID, method, p)
			if seen[name] {
				slog.Warn("gateway openapi: duplicate tool name after sanitization; skipping",
					"server", server, "operation", method+" "+p, "name", name)
				continue
			}
			t, err := buildRestTool(server, name, method, p, baseURL, item, op, client)
			if err != nil {
				slog.Warn("gateway openapi: skipping unmappable operation",
					"server", server, "operation", method+" "+p, "err", err)
				continue
			}
			seen[name] = true
			tools = append(tools, t)
		}
	}
	if len(tools) > largeSpecWarn {
		slog.Warn("gateway openapi: large tool catalog from one spec — consider an operations: filter",
			"server", server, "tools", len(tools))
	}
	if len(tools) == 0 {
		slog.Warn("gateway openapi: spec produced zero tools (filter too narrow or empty spec)",
			"server", server)
	}
	return tools, baseURL, nil
}

// operationSelected applies the operations allowlist (empty ⇒ all).
// Entries are bare operationIds or "METHOD /glob" (path.Match).
func operationSelected(allow []string, opID, method, specPath string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, a := range allow {
		m, glob, found := strings.Cut(a, " ")
		if !found {
			if a == opID && opID != "" {
				return true
			}
			continue
		}
		if m != method {
			continue
		}
		if ok, _ := path.Match(glob, specPath); ok {
			return true
		}
	}
	return false
}

// toolNameFor is operationId, else a slug of "<method>_<path>". "__" is the
// reserved gateway separator, so any run of underscores collapses to one.
func toolNameFor(opID, method, specPath string) string {
	base := opID
	if base == "" {
		base = strings.ToLower(method) + "_" + specPath
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	return strings.Trim(out, "_")
}

// buildRestTool maps one operation to a restTool: merged input schema
// (path/query/header_*/body) and request metadata for Execute.
func buildRestTool(server, name, method, specPath, baseURL string, item *openapi3.PathItem, op *openapi3.Operation, client *http.Client) (restTool, error) {
	props := map[string]json.RawMessage{}
	var required []string
	pathParams := map[string]bool{}
	queryParams := map[string]bool{}
	headerParams := map[string]string{} // schema property → wire header name

	// PathItem-level parameters apply to all its operations; operation-level
	// parameters override by (name, in). Merge both.
	all := append(append([]*openapi3.ParameterRef{}, item.Parameters...), op.Parameters...)
	for _, pref := range all {
		p := pref.Value
		if p == nil {
			return restTool{}, fmt.Errorf("unresolved parameter ref")
		}
		schemaJSON, err := schemaToJSON(p.Schema)
		if err != nil {
			return restTool{}, fmt.Errorf("parameter %s: %w", p.Name, err)
		}
		switch p.In {
		case "path":
			props[p.Name] = schemaJSON
			required = append(required, p.Name) // path params always required
			pathParams[p.Name] = true
		case "query":
			props[p.Name] = schemaJSON
			if p.Required {
				required = append(required, p.Name)
			}
			queryParams[p.Name] = true
		case "header":
			prop := "header_" + p.Name
			props[prop] = schemaJSON
			if p.Required {
				required = append(required, prop)
			}
			headerParams[prop] = p.Name
		default:
			// cookie etc.: skip the parameter, keep the operation
		}
	}

	hasBody := false
	if op.RequestBody != nil && op.RequestBody.Value != nil {
		mt := op.RequestBody.Value.Content.Get("application/json")
		if mt == nil {
			return restTool{}, fmt.Errorf("request body has no application/json media type")
		}
		schemaJSON, err := schemaToJSON(mt.Schema)
		if err != nil {
			return restTool{}, fmt.Errorf("request body: %w", err)
		}
		props["body"] = schemaJSON
		hasBody = true
		if op.RequestBody.Value.Required {
			required = append(required, "body")
		}
	}

	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	schemaBytes, err := json.Marshal(schema)
	if err != nil {
		return restTool{}, err
	}

	desc := method + " " + specPath + " — "
	prose := strings.TrimSpace(strings.TrimSpace(op.Summary) + " " + strings.TrimSpace(op.Description))
	desc += prose
	if len(desc) > maxDescriptionLen {
		desc = desc[:maxDescriptionLen]
	}

	return restTool{
		server: server, name: server + "__" + name,
		method: method, specPath: specPath, baseURL: baseURL,
		description: desc, schema: schemaBytes,
		pathParams: pathParams, queryParams: queryParams, headerParams: headerParams,
		hasBody: hasBody, requiredFields: required, client: client,
	}, nil
}

// schemaToJSON marshals a (resolved) schema ref; nil schema ⇒ permissive {}.
func schemaToJSON(ref *openapi3.SchemaRef) (json.RawMessage, error) {
	if ref == nil || ref.Value == nil {
		return json.RawMessage(`{}`), nil
	}
	return ref.Value.MarshalJSON()
}

// restTool is one generated REST operation exposed as a gateway tool.
type restTool struct {
	server, name, method, specPath, baseURL, description string
	schema                                               json.RawMessage
	pathParams, queryParams                              map[string]bool
	headerParams                                         map[string]string // schema prop → wire header
	hasBody                                              bool
	requiredFields                                       []string
	staticHeaders                                        map[string]string // config headers (set at dial)
	client                                               *http.Client
}

func (r restTool) Name() string                { return r.name }
func (r restTool) Description() string         { return r.description }
func (r restTool) Parameters() json.RawMessage { return r.schema }
func (r restTool) IsConcurrencySafe(json.RawMessage) bool {
	return r.method == "GET" || r.method == "HEAD"
}

func (r restTool) Execute(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	return tool.ToolResult{Error: "not wired"}, nil // Task 3
}
