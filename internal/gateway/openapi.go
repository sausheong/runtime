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
)

const (
	maxDescriptionLen = 1024
	// largeSpecWarn nudges operators toward an operations: filter when one
	// spec floods the catalog (spec §11).
	largeSpecWarn = 50
	// maxSchemaInlineDepth caps inlineSchema recursion as a backstop against
	// pathological nesting; exceeding it is treated like a cycle.
	maxSchemaInlineDepth = 30
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
	// Examples/defaults validation is disabled: invalid `example` values are
	// endemic in third-party specs and must not kill the whole upstream.
	if err := doc.Validate(vctx,
		openapi3.DisableExamplesValidation(),
		openapi3.DisableSchemaDefaultsValidation(),
	); err != nil {
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
			if name == "" {
				slog.Warn("gateway openapi: operation name unsanitizable; skipping",
					"server", server, "operation", method+" "+p)
				continue
			}
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
// An operationId that sanitizes to nothing (e.g. "!!!") falls back to the
// method_path slug; if that is also empty the caller must skip the operation.
func toolNameFor(opID, method, specPath string) string {
	if opID != "" {
		if n := sanitizeToolName(opID); n != "" {
			return n
		}
	}
	return sanitizeToolName(strings.ToLower(method) + "_" + specPath)
}

func sanitizeToolName(base string) string {
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
	// parameters OVERRIDE by (name, in) — not append. Merge into a map keyed
	// by name+"\x00"+in, item-level first, op-level overwriting, then iterate
	// in deterministic (sorted-key) order.
	merged := map[string]*openapi3.ParameterRef{}
	for _, pref := range item.Parameters {
		if pref.Value == nil {
			return restTool{}, fmt.Errorf("unresolved parameter ref")
		}
		merged[pref.Value.Name+"\x00"+pref.Value.In] = pref
	}
	for _, pref := range op.Parameters {
		if pref.Value == nil {
			return restTool{}, fmt.Errorf("unresolved parameter ref")
		}
		merged[pref.Value.Name+"\x00"+pref.Value.In] = pref
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := merged[k].Value
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
		switch {
		case mt != nil:
			schemaJSON, err := schemaToJSON(mt.Schema)
			if err != nil {
				return restTool{}, fmt.Errorf("request body: %w", err)
			}
			props["body"] = schemaJSON
			hasBody = true
			if op.RequestBody.Value.Required {
				required = append(required, "body")
			}
		case op.RequestBody.Value.Required:
			// Required non-JSON body: the operation is unusable without it.
			return restTool{}, fmt.Errorf("required request body has no application/json media type")
		default:
			// Optional non-JSON body: drop the body property, op usable bodyless.
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

	prose := strings.TrimSpace(strings.TrimSpace(op.Summary) + " " + strings.TrimSpace(op.Description))
	desc := method + " " + specPath
	if prose != "" {
		desc += " — " + prose
	}
	if len(desc) > maxDescriptionLen {
		desc = strings.ToValidUTF8(desc[:maxDescriptionLen], "")
	}

	return restTool{
		server: server, name: server + "__" + name,
		method: method, specPath: specPath, baseURL: baseURL,
		description: desc, schema: schemaBytes,
		pathParams: pathParams, queryParams: queryParams, headerParams: headerParams,
		hasBody: hasBody, requiredFields: required, client: client,
	}, nil
}

// schemaToJSON deep-inlines a (resolved) schema ref into plain JSON Schema
// with no "$ref" emission; nil schema ⇒ permissive {}.
func schemaToJSON(ref *openapi3.SchemaRef) (json.RawMessage, error) {
	v, err := inlineSchema(ref, map[*openapi3.Schema]bool{}, 0)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// inlineSchema walks the resolved openapi3.Schema graph and builds a
// map[string]any bottom-up, fully inlining nested component schemas.
// kin-openapi's MarshalJSON re-emits any nested component reference as a
// literal {"$ref": ...} (only the top level is expanded), which is useless in
// a tool input schema — so we never call MarshalJSON on ref-bearing fields and
// instead recurse into the typed structure. visited tracks the current
// ANCESTOR PATH only (marked on entry, unmarked on exit): sibling reuse of the
// same component is legal and common; only ancestor-path repetition is a
// genuine cycle and errors (the sole skip case, spec §5).
func inlineSchema(ref *openapi3.SchemaRef, visited map[*openapi3.Schema]bool, depth int) (any, error) {
	if ref == nil || ref.Value == nil {
		return map[string]any{}, nil // permissive
	}
	s := ref.Value
	if visited[s] {
		return nil, fmt.Errorf("cyclic schema reference")
	}
	if depth > maxSchemaInlineDepth {
		return nil, fmt.Errorf("schema nesting exceeds depth %d (treated as cyclic)", maxSchemaInlineDepth)
	}
	visited[s] = true
	defer delete(visited, s)

	out := map[string]any{}

	// Scalar / metadata fields useful for tool input schemas. Everything else
	// (discriminator, xml, externalDocs, example, readOnly, ...) is ignored.
	if s.Type != nil && len(*s.Type) > 0 {
		types := *s.Type
		if len(types) == 1 {
			out["type"] = types[0]
		} else {
			out["type"] = []string(types)
		}
	}
	if s.Description != "" {
		out["description"] = s.Description
	}
	if len(s.Enum) > 0 {
		out["enum"] = s.Enum
	}
	if s.Format != "" {
		out["format"] = s.Format
	}
	if len(s.Required) > 0 {
		out["required"] = s.Required
	}
	if s.Nullable {
		out["nullable"] = true
	}
	if s.Default != nil {
		out["default"] = s.Default
	}
	if s.Min != nil {
		out["minimum"] = *s.Min
	}
	if s.Max != nil {
		out["maximum"] = *s.Max
	}
	if s.MinLength != 0 {
		out["minLength"] = s.MinLength
	}
	if s.MaxLength != nil {
		out["maxLength"] = *s.MaxLength
	}
	if s.Pattern != "" {
		out["pattern"] = s.Pattern
	}

	// Ref-bearing fields: recurse.
	if len(s.Properties) > 0 {
		props := map[string]any{}
		for name, pref := range s.Properties {
			v, err := inlineSchema(pref, visited, depth+1)
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", name, err)
			}
			props[name] = v
		}
		out["properties"] = props
	}
	if s.Items != nil {
		v, err := inlineSchema(s.Items, visited, depth+1)
		if err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
		out["items"] = v
	}
	switch {
	case s.AdditionalProperties.Schema != nil:
		v, err := inlineSchema(s.AdditionalProperties.Schema, visited, depth+1)
		if err != nil {
			return nil, fmt.Errorf("additionalProperties: %w", err)
		}
		out["additionalProperties"] = v
	case s.AdditionalProperties.Has != nil:
		out["additionalProperties"] = *s.AdditionalProperties.Has
	}
	for key, refs := range map[string]openapi3.SchemaRefs{
		"oneOf": s.OneOf, "anyOf": s.AnyOf, "allOf": s.AllOf,
	} {
		if len(refs) == 0 {
			continue
		}
		list := make([]any, 0, len(refs))
		for i, sub := range refs {
			v, err := inlineSchema(sub, visited, depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s[%d]: %w", key, i, err)
			}
			list = append(list, v)
		}
		out[key] = list
	}

	return out, nil
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
