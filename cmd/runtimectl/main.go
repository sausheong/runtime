package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/sausheong/runtime/conformance"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	base := envOr("RUNTIME_CTL_URL", "http://localhost:8080")
	cmd := os.Args[1]
	agent, rest := popAgentFlag(os.Args[2:])

	switch cmd {
	case "agents":
		listAgents(base)
	case "invoke":
		msg := "hello"
		if len(rest) > 0 {
			msg = rest[0]
		}
		invoke(base, resolveAgent(base, agent), msg)
	case "sessions":
		listSessions(base, resolveAgent(base, agent))
	case "logs":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: runtimectl logs --agent <id> <session-id>")
			os.Exit(2)
		}
		stream(base, resolveAgent(base, agent), rest[0])
	case "conformance":
		runConformance(base, resolveAgent(base, agent))
	case "admin":
		runAdmin(base, os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: runtimectl <agents|invoke|sessions|logs|conformance|admin> [--agent <id>] [args]")
	os.Exit(2)
}

// authdGet issues a GET request with a bearer token when RUNTIME_TOKEN is set.
func authdGet(url string) (*http.Response, error) {
	req, _ := http.NewRequest("GET", url, nil)
	addAuth(req)
	return http.DefaultClient.Do(req)
}

// authdPost issues a POST request with a bearer token when RUNTIME_TOKEN is set.
func authdPost(url, ctype string, body io.Reader) (*http.Response, error) {
	req, _ := http.NewRequest("POST", url, body)
	req.Header.Set("Content-Type", ctype)
	addAuth(req)
	return http.DefaultClient.Do(req)
}

// addAuth attaches a bearer token from RUNTIME_TOKEN to req when present.
func addAuth(req *http.Request) {
	if tok := os.Getenv("RUNTIME_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// cliT adapts conformance.TestingT to stdout, tracking failure.
type cliT struct{ failed bool }

func (c *cliT) Errorf(f string, a ...any) { c.failed = true; fmt.Printf("FAIL: "+f+"\n", a...) }
func (c *cliT) Fatalf(f string, a ...any) { c.failed = true; fmt.Printf("FATAL: "+f+"\n", a...) }
func (c *cliT) Logf(f string, a ...any)   { fmt.Printf("ok: "+f+"\n", a...) }

func runConformance(base, agent string) {
	t := &cliT{}
	conformance.Run(t, base+"/agents/"+agent)
	if t.failed {
		fmt.Fprintln(os.Stderr, "conformance: FAILED")
		os.Exit(1)
	}
	fmt.Println("conformance: PASSED")
}

// popAgentFlag extracts "--agent <id>" from args, returning the id and the rest.
func popAgentFlag(args []string) (string, []string) {
	var agent string
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--agent" && i+1 < len(args) {
			agent = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return agent, rest
}

// resolveAgent returns the explicit --agent, or the sole agent if exactly one
// is registered, else errors.
func resolveAgent(base, agent string) string {
	if agent != "" {
		return agent
	}
	infos := fetchAgents(base)
	if len(infos) == 1 {
		return infos[0].ID
	}
	fmt.Fprintf(os.Stderr, "error: --agent required (%d agents registered)\n", len(infos))
	os.Exit(2)
	return ""
}

type agentInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

func fetchAgents(base string) []agentInfo {
	resp, err := authdGet(base + "/agents")
	check(err)
	defer resp.Body.Close()
	var infos []agentInfo
	_ = json.NewDecoder(resp.Body).Decode(&infos)
	return infos
}

func listAgents(base string) {
	for _, a := range fetchAgents(base) {
		fmt.Printf("%s\t%s\t%s\n", a.ID, a.Name, a.Model)
	}
}

func invoke(base, agent, msg string) {
	body, _ := json.Marshal(map[string]string{"message": msg})
	resp, err := authdPost(base+"/agents/"+agent+"/sessions", "application/json", bytes.NewReader(body))
	check(err)
	var out struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.SessionID == "" {
		fmt.Fprintln(os.Stderr, "error: no session id returned")
		os.Exit(1)
	}
	fmt.Println("session:", out.SessionID)
	stream(base, agent, out.SessionID)
}

func listSessions(base, agent string) {
	resp, err := authdGet(base + "/agents/" + agent + "/sessions")
	check(err)
	defer resp.Body.Close()
	var rows []struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		TurnCount int    `json:"turn_count"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rows)
	for _, s := range rows {
		fmt.Printf("%s\t%s\tturns=%d\n", s.ID, s.Status, s.TurnCount)
	}
}

func stream(base, agent, id string) {
	resp, err := authdGet(base + "/agents/" + agent + "/sessions/" + id + "/stream?since=0")
	check(err)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			fmt.Println(line)
		}
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// runAdmin dispatches `runtimectl admin <tenant|user|key> ...`.
func runAdmin(base string, args []string) {
	if len(args) < 2 {
		adminUsage()
	}
	switch args[0] + " " + args[1] {
	case "tenant create":
		// admin tenant create <id> [--name <name>]
		if len(args) < 3 {
			adminUsage()
		}
		id := args[2]
		name := flagValue(args[3:], "--name", id)
		mustAdminPost(base, "/admin/tenants", map[string]string{"id": id, "name": name})
		fmt.Printf("tenant %s created\n", id)
	case "user add":
		// admin user add <subject> --role <r>  (tenant = caller's, server-side)
		if len(args) < 3 {
			adminUsage()
		}
		subject := args[2]
		role := flagValue(args[3:], "--role", "viewer")
		mustAdminPost(base, "/admin/users", map[string]string{"subject": subject, "role": role})
		fmt.Printf("user %s added (role=%s)\n", subject, role)
	case "user ls":
		fmt.Print(string(mustAdminGet(base, "/admin/users")))
	case "key create":
		// admin key create --role <r> [--label <l>]
		role := flagValue(args[2:], "--role", "viewer")
		label := flagValue(args[2:], "--label", "")
		out := mustAdminPost(base, "/admin/keys", map[string]string{"role": role, "label": label})
		var resp struct{ ID, Plaintext string }
		if err := json.Unmarshal(out, &resp); err != nil || resp.Plaintext == "" {
			fmt.Fprintf(os.Stderr, "key created but plaintext missing from response: %s\n", out)
			os.Exit(1)
		}
		fmt.Printf("%s\n(store this now — shown once)\n", resp.Plaintext)
	case "key ls":
		fmt.Print(string(mustAdminGet(base, "/admin/keys")))
	case "key revoke":
		// admin key revoke <id>
		if len(args) < 3 {
			adminUsage()
		}
		id := args[2]
		mustAdminDelete(base, "/admin/keys/"+id)
		fmt.Printf("key %s revoked\n", id)
	default:
		adminUsage()
	}
}

func adminUsage() {
	fmt.Fprintln(os.Stderr, "usage: runtimectl admin <tenant create <id> [--name n]|user add <subject> --role r|user ls|key create --role r [--label l]|key ls|key revoke <id>>")
	os.Exit(2)
}

// flagValue returns the value following name in args, or def.
func flagValue(args []string, name, def string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return def
}

func adminPost(base, path string, body map[string]string) ([]byte, error) {
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	addAuth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("admin %s: %s: %s", path, resp.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func mustAdminPost(base, path string, body map[string]string) []byte {
	out, err := adminPost(base, path, body)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return out
}

func mustAdminGet(base, path string) []byte {
	resp, err := authdGet(base + path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "admin %s: %s: %s\n", path, resp.Status, strings.TrimSpace(string(out)))
		os.Exit(1)
	}
	return out
}

func mustAdminDelete(base, path string) {
	req, _ := http.NewRequest("DELETE", base+path, nil)
	addAuth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "admin %s: %s: %s\n", path, resp.Status, strings.TrimSpace(string(out)))
		os.Exit(1)
	}
}
