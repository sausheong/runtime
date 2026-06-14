// Command v1-probe exercises the MCP-session-dependent pillars of the v1.0
// turnkey stack for deploy/compose/v1-proof.sh: it connects to /gateway/mcp as
// an authed MCP client and (a) lists federated tools, (b) calls the REST-adapter
// tool, (c) runs code in a sandbox. Bash handles the curl-able assertions; this
// handles the ones that need an MCP session handshake. Exits non-zero on any
// failure with a one-line reason on stderr.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "v1-probe: "+format+"\n", a...)
	os.Exit(1)
}

func parseFlags(args []string) (base, key string) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--base":
			i++
			if i < len(args) {
				base = args[i]
			}
		case "--key":
			i++
			if i < len(args) {
				key = args[i]
			}
		}
	}
	if base == "" || key == "" {
		die("usage: v1-probe <list|call-rest|sandbox> --base <url> --key <bearer>")
	}
	return base, key
}

func connect(base, key string) *sdk.ClientSession {
	cli := sdk.NewClient(&sdk.Implementation{Name: "v1-probe", Version: "v1"}, nil)
	sess, err := cli.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint:   strings.TrimRight(base, "/") + "/gateway/mcp",
		HTTPClient: &http.Client{Transport: bearerRT{token: key}},
	}, nil)
	if err != nil {
		die("connect %s/gateway/mcp: %v", base, err)
	}
	return sess
}

func connectWhenFederated(base, key string, want ...string) *sdk.ClientSession {
	deadline := time.Now().Add(30 * time.Second)
	for {
		sess := connect(base, key)
		lt, err := sess.ListTools(context.Background(), &sdk.ListToolsParams{})
		if err != nil {
			die("list tools: %v", err)
		}
		seen := map[string]bool{}
		for _, tool := range lt.Tools {
			seen[tool.Name] = true
		}
		all := true
		for _, w := range want {
			all = all && seen[w]
		}
		if all {
			return sess
		}
		_ = sess.Close()
		if time.Now().After(deadline) {
			names := make([]string, 0, len(lt.Tools))
			for _, tool := range lt.Tools {
				names = append(names, tool.Name)
			}
			die("tools %v never federated; saw: %s", want, strings.Join(names, ","))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// contentText extracts the human-readable text from a tool result's content
// parts (an error result carries its message as TextContent), so failures show
// the real reason instead of pointer addresses.
func contentText(content []sdk.Content) string {
	var parts []string
	for _, c := range content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	if len(parts) == 0 {
		return "(no text content)"
	}
	return strings.Join(parts, " | ")
}

func callJSON(sess *sdk.ClientSession, name string, args map[string]any, out any) {
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		die("call %s: %v", name, err)
	}
	if res.IsError {
		die("call %s errored: %s", name, contentText(res.Content))
	}
	if len(res.Content) != 1 {
		die("call %s: want 1 content part, got %d", name, len(res.Content))
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		die("call %s: want TextContent, got %T", name, res.Content[0])
	}
	if out != nil {
		if err := json.Unmarshal([]byte(tc.Text), out); err != nil {
			die("call %s: decode %q: %v", name, tc.Text, err)
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		die("usage: v1-probe <list|call-rest|sandbox> --base <url> --key <bearer>")
	}
	cmd := os.Args[1]
	base, key := parseFlags(os.Args[2:])

	switch cmd {
	case "list":
		sess := connectWhenFederated(base, key, "orders__listOrders", "sandbox__execute_code")
		defer sess.Close()
		fmt.Println("OK: federated orders__listOrders (REST) + sandbox__execute_code (MCP)")

	case "call-rest":
		sess := connectWhenFederated(base, key, "orders__listOrders")
		defer sess.Close()
		var raw json.RawMessage
		callJSON(sess, "orders__listOrders", map[string]any{}, &raw)
		if len(raw) == 0 || string(raw) == "null" {
			die("orders__listOrders returned empty")
		}
		fmt.Printf("OK: orders__listOrders returned %d bytes\n", len(raw))

	case "sandbox":
		sess := connectWhenFederated(base, key, "sandbox__create_sandbox", "sandbox__execute_code")
		defer sess.Close()
		var created struct {
			SandboxID string `json:"sandbox_id"`
		}
		callJSON(sess, "sandbox__create_sandbox", map[string]any{}, &created)
		if !strings.HasPrefix(created.SandboxID, "sbx-") {
			die("create_sandbox: bad id %q", created.SandboxID)
		}
		var exec struct {
			Stdout   string `json:"stdout"`
			ExitCode int    `json:"exit_code"`
		}
		callJSON(sess, "sandbox__execute_code",
			map[string]any{"sandbox_id": created.SandboxID, "code": "print(6*7)"}, &exec)
		if !strings.Contains(exec.Stdout, "42") {
			die("execute_code stdout=%q exit=%d, want 42", exec.Stdout, exec.ExitCode)
		}
		fmt.Printf("OK: sandbox executed code → stdout has 42 (exit %d)\n", exec.ExitCode)

	default:
		die("unknown subcommand %q", cmd)
	}
}
