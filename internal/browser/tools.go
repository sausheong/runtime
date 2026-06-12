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

// maxOutput caps text returned to the model.
const maxOutput = 256 << 10

func clip(s string) string {
	if len(s) > maxOutput {
		return s[:maxOutput] + "\n[truncated]"
	}
	return s
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
			var in struct {
				BrowserID string `json:"browser_id"`
				Selector  string `json:"selector"`
				WaitFor   string `json:"wait_for"`
			}
			if err := decode(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if in.BrowserID == "" || in.Selector == "" {
				return nil, errMissing("browser_id, selector")
			}
			s, err := m.Lookup(tenant, in.BrowserID)
			if err != nil {
				return nil, err
			}
			if err := Click(ctx, s, in.Selector, in.WaitFor); err != nil {
				return nil, m.maskNav(s.ID, err)
			}
			return jsonResult(map[string]any{"clicked": in.Selector})
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
		"Get the innerHTML of an element (defaults to body)."+sessionNote+selectorNote,
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
		"Extract clean readable text from the current page (script/style/nav stripped). Prefer this over get_text for reading content."+sessionNote+selectorNote,
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
			for i := range sessions {
				s := &sessions[i]
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
