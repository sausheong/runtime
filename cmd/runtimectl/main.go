package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

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
		verbose := false
		for _, a := range rest {
			if a == "-v" {
				verbose = true
			} else {
				msg = a
			}
		}
		invoke(base, resolveAgent(base, agent), msg, verbose)
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
	case "register":
		runRegister(base, os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: runtimectl <agents|invoke [-v]|sessions|logs|conformance|admin|register> [--agent <id>] [args]")
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

func invoke(base, agent, msg string, verbose bool) {
	body, _ := json.Marshal(map[string]string{"message": msg})
	resp, err := authdPost(base+"/agents/"+agent+"/sessions", "application/json", bytes.NewReader(body))
	check(err)
	if verbose {
		fmt.Fprintln(os.Stderr, "request-id:", resp.Header.Get("X-Request-ID"))
	}
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
		tenant := flagValue(args[3:], "--tenant", "")
		mustAdminPost(base, "/admin/users", map[string]string{"subject": subject, "role": role, "tenant": tenant})
		fmt.Printf("user %s added (role=%s)\n", subject, role)
	case "user ls":
		fmt.Print(string(mustAdminGet(base, "/admin/users")))
	case "key create":
		// admin key create --role <r> [--label <l>]
		role := flagValue(args[2:], "--role", "viewer")
		label := flagValue(args[2:], "--label", "")
		tenant := flagValue(args[2:], "--tenant", "")
		out := mustAdminPost(base, "/admin/keys", map[string]string{"role": role, "label": label, "tenant": tenant})
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
	case "secret set":
		// admin secret set <name> <value> [--tenant t]
		if len(args) < 4 {
			adminUsage()
		}
		name, value := args[2], args[3]
		tenant := flagValue(args[4:], "--tenant", "")
		mustAdminPost(base, "/admin/secrets", map[string]string{"name": name, "value": value, "tenant": tenant})
		fmt.Printf("secret %s set\n", name)
	case "secret set-oauth2":
		// admin secret set-oauth2 --name n --token-url u --client-id c --client-secret s [--scope x ...] [--audience a] [--tenant t]
		name := flagValue(args[2:], "--name", "")
		if name == "" {
			fmt.Fprintln(os.Stderr, "secret set-oauth2 requires --name")
			os.Exit(2)
		}
		body := map[string]any{
			"name":          name,
			"type":          "oauth2_client_credentials",
			"token_url":     flagValue(args[2:], "--token-url", ""),
			"client_id":     flagValue(args[2:], "--client-id", ""),
			"client_secret": flagValue(args[2:], "--client-secret", ""),
			"scopes":        flagValues(args[2:], "--scope"),
			"audience":      flagValue(args[2:], "--audience", ""),
			"tenant":        flagValue(args[2:], "--tenant", ""),
		}
		mustAdminPostAny(base, "/admin/secrets", body)
		fmt.Printf("oauth2 credential %s set\n", name)
	case "secret set-obo":
		// admin secret set-obo --name n --token-url u --client-id c --client-secret s [--scope x ...] [--audience a] [--subject-token-type t] [--requested-token-type t] [--tenant t]
		name := flagValue(args[2:], "--name", "")
		if name == "" {
			fmt.Fprintln(os.Stderr, "secret set-obo requires --name")
			os.Exit(2)
		}
		body := map[string]any{
			"name":                 name,
			"type":                 "oauth2_obo",
			"token_url":            flagValue(args[2:], "--token-url", ""),
			"client_id":            flagValue(args[2:], "--client-id", ""),
			"client_secret":        flagValue(args[2:], "--client-secret", ""),
			"scopes":               flagValues(args[2:], "--scope"),
			"audience":             flagValue(args[2:], "--audience", ""),
			"subject_token_type":   flagValue(args[2:], "--subject-token-type", ""),
			"requested_token_type": flagValue(args[2:], "--requested-token-type", ""),
			"tenant":               flagValue(args[2:], "--tenant", ""),
		}
		mustAdminPostAny(base, "/admin/secrets", body)
		fmt.Printf("obo credential %s set\n", name)
	case "secret ls":
		fmt.Print(string(mustAdminGet(base, "/admin/secrets")))
	case "secret rm":
		// admin secret rm <name>
		if len(args) < 3 {
			adminUsage()
		}
		mustAdminDelete(base, "/admin/secrets/"+args[2])
		fmt.Printf("secret %s removed\n", args[2])
	case "secret rotate":
		// admin secret rotate [--tenant t]
		tenant := flagValue(args[2:], "--tenant", "")
		out := mustAdminPost(base, "/admin/secrets/rotate", map[string]string{"tenant": tenant})
		var stats []struct {
			Tenant  string `json:"tenant"`
			Total   int    `json:"total"`
			Rotated int    `json:"rotated"`
			Failed  int    `json:"failed"`
		}
		if err := json.Unmarshal(out, &stats); err != nil {
			fmt.Fprintf(os.Stderr, "rotate: bad response: %s\n", out)
			os.Exit(1)
		}
		failed := 0
		for _, s := range stats {
			fmt.Printf("tenant %s: total=%d rotated=%d failed=%d\n", s.Tenant, s.Total, s.Rotated, s.Failed)
			failed += s.Failed
		}
		if failed > 0 {
			os.Exit(1)
		}
	case "upstream add":
		// admin upstream add --name <n> --url <u> | --openapi <spec> [--base-url b] [--cred-secret s] [--cred-header h] [--tenant t]
		// Note: --operations is intentionally omitted for M1 (string-valued body); server treats absent operations as "all".
		name := flagValue(args[2:], "--name", "")
		if name == "" {
			adminUsage()
		}
		body := map[string]string{
			"name":        name,
			"url":         flagValue(args[2:], "--url", ""),
			"openapi":     flagValue(args[2:], "--openapi", ""),
			"base_url":    flagValue(args[2:], "--base-url", ""),
			"cred_secret": flagValue(args[2:], "--cred-secret", ""),
			"cred_header": flagValue(args[2:], "--cred-header", ""),
			"tenant":      flagValue(args[2:], "--tenant", ""),
		}
		out := mustAdminPost(base, "/admin/upstreams", body)
		var resp struct{ ID, Name string }
		if err := json.Unmarshal(out, &resp); err != nil || resp.ID == "" {
			fmt.Fprintf(os.Stderr, "upstream created but response malformed: %s\n", out)
			os.Exit(1)
		}
		fmt.Printf("upstream %s registered (id=%s)\n", resp.Name, resp.ID)
	case "upstream ls":
		fmt.Print(string(mustAdminGet(base, "/admin/upstreams")))
	case "upstream rm":
		if len(args) < 3 {
			adminUsage()
		}
		mustAdminDelete(base, "/admin/upstreams/"+args[2])
		fmt.Printf("upstream %s removed\n", args[2])
	case "agent add":
		// admin agent add --id <id> --url <u> [--name n] [--model m] [--cred-secret s] [--tenant t]
		id := flagValue(args[2:], "--id", "")
		url := flagValue(args[2:], "--url", "")
		if id == "" || url == "" {
			adminUsage()
		}
		body := map[string]string{
			"id":          id,
			"url":         url,
			"name":        flagValue(args[2:], "--name", id),
			"model":       flagValue(args[2:], "--model", ""),
			"auth_secret": flagValue(args[2:], "--cred-secret", ""),
			"tenant":      flagValue(args[2:], "--tenant", ""),
		}
		out := mustAdminPost(base, "/admin/agents", body)
		var resp struct{ ID string }
		if err := json.Unmarshal(out, &resp); err != nil || resp.ID == "" {
			fmt.Fprintf(os.Stderr, "agent registered but response malformed: %s\n", out)
			os.Exit(1)
		}
		fmt.Printf("agent %s registered\n", resp.ID)
	case "agent ls":
		fmt.Print(string(mustAdminGet(base, "/admin/agents")))
	case "agent rm":
		if len(args) < 3 {
			adminUsage()
		}
		mustAdminDelete(base, "/admin/agents/"+args[2])
		fmt.Printf("agent %s deregistered\n", args[2])
	case "agent enable":
		if len(args) < 3 {
			adminUsage()
		}
		mustAdminPost(base, "/admin/agents/"+args[2]+"/enable", map[string]string{})
		fmt.Printf("agent %s enabled\n", args[2])
	case "agent disable":
		if len(args) < 3 {
			adminUsage()
		}
		mustAdminPost(base, "/admin/agents/"+args[2]+"/disable", map[string]string{})
		fmt.Printf("agent %s disabled\n", args[2])
	case "agent restart":
		if len(args) < 3 {
			adminUsage()
		}
		mustAdminPost(base, "/admin/agents/"+args[2]+"/restart", map[string]string{})
		fmt.Printf("agent %s re-attached (health re-probed)\n", args[2])
	case "policy add":
		// admin policy add --name <n> --file <path.cedar> [--tenant t]
		// Policies are multi-line Cedar; the text is read from --file rather
		// than a flag literal.
		name := flagValue(args[2:], "--name", "")
		file := flagValue(args[2:], "--file", "")
		if name == "" || file == "" {
			adminUsage()
		}
		text, rerr := os.ReadFile(file)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "read policy file %q: %v\n", file, rerr)
			os.Exit(1)
		}
		body := map[string]string{
			"name":       name,
			"cedar_text": string(text),
			"tenant":     flagValue(args[2:], "--tenant", ""),
		}
		out := mustAdminPost(base, "/admin/policies", body)
		var resp struct{ Name string }
		if err := json.Unmarshal(out, &resp); err != nil || resp.Name == "" {
			fmt.Fprintf(os.Stderr, "policy created but response malformed: %s\n", out)
			os.Exit(1)
		}
		fmt.Printf("policy %s registered\n", resp.Name)
	case "policy ls":
		path := "/admin/policies"
		if t := flagValue(args[2:], "--tenant", ""); t != "" {
			path += "?tenant=" + t
		}
		fmt.Print(string(mustAdminGet(base, path)))
	case "policy rm":
		if len(args) < 3 {
			adminUsage()
		}
		path := "/admin/policies/" + args[2]
		if t := flagValue(args[3:], "--tenant", ""); t != "" {
			path += "?tenant=" + t
		}
		mustAdminDelete(base, path)
		fmt.Printf("policy %s removed\n", args[2])
	case "quota add":
		// admin quota add --tenant <t> --upstream <u> --rate <n>
		body := map[string]any{
			"tenant":       flagValue(args[2:], "--tenant", ""),
			"upstream":     flagValue(args[2:], "--upstream", ""),
			"rate_per_min": mustAtoi(flagValue(args[2:], "--rate", "")),
		}
		mustAdminPostAny(base, "/admin/quotas", body)
		fmt.Printf("quota %s/%s set\n", body["tenant"], body["upstream"])
	case "quota ls":
		fmt.Print(string(mustAdminGet(base, "/admin/quotas")))
	case "quota rm":
		// admin quota rm --tenant <t> --upstream <u>
		q := "?upstream=" + url.QueryEscape(flagValue(args[2:], "--upstream", ""))
		if tn := flagValue(args[2:], "--tenant", ""); tn != "" {
			q += "&tenant=" + url.QueryEscape(tn)
		}
		mustAdminDelete(base, "/admin/quotas"+q)
		fmt.Println("quota removed")
	case "eval set", "eval run", "eval runs", "eval results", "eval policy", "eval online-results":
		runEvalAdmin(base, args[1:])
	default:
		adminUsage()
	}
}

// runEvalAdmin dispatches `runtimectl admin eval <set|run|runs|results> ...`
// against the control plane's /admin/evals/* routes. args[0] is the first token
// after "eval" (e.g. "set", "run", "runs", "results").
func runEvalAdmin(base string, args []string) {
	if len(args) < 1 {
		adminUsage()
	}
	switch args[0] {
	case "set":
		if len(args) < 2 {
			adminUsage()
		}
		switch args[1] {
		case "add":
			// admin eval set add --name <n> --file <f> [--tenant <t>]
			name := flagValue(args[2:], "--name", "")
			file := flagValue(args[2:], "--file", "")
			if name == "" || file == "" {
				adminUsage()
			}
			raw, rerr := os.ReadFile(file)
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "read eval set file %q: %v\n", file, rerr)
				os.Exit(1)
			}
			cases, perr := parseEvalCases(raw)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "parse eval set file %q: %v\n", file, perr)
				os.Exit(1)
			}
			body := map[string]any{
				"name":   name,
				"tenant": flagValue(args[2:], "--tenant", ""),
				"cases":  cases,
			}
			mustAdminPostAny(base, "/admin/evals/sets", body)
			fmt.Printf("eval set %s stored (%d cases)\n", name, len(cases))
		case "ls":
			// admin eval set ls [--tenant <t>]
			path := "/admin/evals/sets"
			if t := flagValue(args[2:], "--tenant", ""); t != "" {
				path += "?tenant=" + url.QueryEscape(t)
			}
			fmt.Print(string(mustAdminGet(base, path)))
		case "rm":
			// admin eval set rm <name>
			if len(args) < 3 {
				adminUsage()
			}
			mustAdminDelete(base, "/admin/evals/sets/"+args[2])
			fmt.Printf("eval set %s removed\n", args[2])
		default:
			adminUsage()
		}
	case "run":
		// admin eval run <set> --agent <id> [--tenant <t>] [--wait]
		if len(args) < 2 {
			adminUsage()
		}
		set := args[1]
		agent := flagValue(args[2:], "--agent", "")
		if agent == "" {
			adminUsage()
		}
		body := map[string]any{
			"set":    set,
			"agent":  agent,
			"tenant": flagValue(args[2:], "--tenant", ""),
		}
		out := mustAdminPostAny(base, "/admin/evals/runs", body)
		var resp struct {
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal(out, &resp); err != nil || resp.RunID == "" {
			fmt.Fprintf(os.Stderr, "run started but response malformed: %s\n", out)
			os.Exit(1)
		}
		fmt.Printf("run %s started\n", resp.RunID)
		if hasFlag(args[2:], "--wait") {
			waitEvalRun(base, resp.RunID)
		}
	case "runs":
		// admin eval runs [--tenant <t>]
		path := "/admin/evals/runs"
		if t := flagValue(args[1:], "--tenant", ""); t != "" {
			path += "?tenant=" + url.QueryEscape(t)
		}
		fmt.Print(string(mustAdminGet(base, path)))
	case "results":
		// admin eval results <run-id>
		if len(args) < 2 {
			adminUsage()
		}
		fmt.Print(string(mustAdminGet(base, "/admin/evals/runs/"+args[1]+"/results")))
	case "policy":
		if len(args) < 2 {
			adminUsage()
		}
		switch args[1] {
		case "set":
			// admin eval policy set --agent <id> --rate <0-100> --file <f> [--tenant <t>]
			agent := flagValue(args[2:], "--agent", "")
			rate := flagValue(args[2:], "--rate", "")
			file := flagValue(args[2:], "--file", "")
			if agent == "" || rate == "" || file == "" {
				adminUsage()
			}
			raw, rerr := os.ReadFile(file)
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "read eval policy file %q: %v\n", file, rerr)
				os.Exit(1)
			}
			criteria, perr := parseEvalCriteria(raw)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "parse eval policy file %q: %v\n", file, perr)
				os.Exit(1)
			}
			body := map[string]any{
				"agent":       agent,
				"tenant":      flagValue(args[2:], "--tenant", ""),
				"sample_rate": mustAtoi(rate),
				"criteria":    criteria,
			}
			mustAdminPostAny(base, "/admin/evals/policy", body)
			fmt.Printf("eval policy for %s stored (rate %s%%, %d criteria)\n", agent, rate, len(criteria))
		case "ls":
			// admin eval policy ls [--tenant <t>]
			path := "/admin/evals/policy"
			if t := flagValue(args[2:], "--tenant", ""); t != "" {
				path += "?tenant=" + url.QueryEscape(t)
			}
			fmt.Print(string(mustAdminGet(base, path)))
		case "rm":
			// admin eval policy rm <agent>
			if len(args) < 3 {
				adminUsage()
			}
			mustAdminDelete(base, "/admin/evals/policy/"+args[2])
			fmt.Printf("eval policy for %s removed\n", args[2])
		default:
			adminUsage()
		}
	case "online-results":
		// admin eval online-results [--session <sid>] [--tenant <t>]
		q := ""
		if sid := flagValue(args[1:], "--session", ""); sid != "" {
			q = "?session=" + url.QueryEscape(sid)
		}
		if t := flagValue(args[1:], "--tenant", ""); t != "" {
			if q == "" {
				q = "?tenant=" + url.QueryEscape(t)
			} else {
				q += "&tenant=" + url.QueryEscape(t)
			}
		}
		fmt.Print(string(mustAdminGet(base, "/admin/evals/online-results"+q)))
	default:
		adminUsage()
	}
}

// parseEvalCriteria parses an online-eval-policy criteria list from either a
// wrapper object (`{"criteria":[...]}`) or a bare JSON array (`[...]`).
func parseEvalCriteria(raw []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty file")
	}
	if trimmed[0] == '{' {
		var wrap struct {
			Criteria []json.RawMessage `json:"criteria"`
		}
		if err := json.Unmarshal(trimmed, &wrap); err != nil {
			return nil, err
		}
		return wrap.Criteria, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

// parseEvalCases parses a golden-set case list from either a wrapper object
// (`{"cases":[...]}`) or a bare JSON array (`[...]`).
func parseEvalCases(raw []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty file")
	}
	if trimmed[0] == '{' {
		var wrap struct {
			Cases []json.RawMessage `json:"cases"`
		}
		if err := json.Unmarshal(trimmed, &wrap); err != nil {
			return nil, err
		}
		return wrap.Cases, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

// hasFlag reports whether name appears as a bare (boolean) flag in args.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

// waitEvalRun polls a run every second until it reaches a terminal status, then
// prints the run aggregate and a per-case results summary.
func waitEvalRun(base, id string) {
	for {
		out := mustAdminGet(base, "/admin/evals/runs/"+id)
		var run struct {
			Status string  `json:"status"`
			Total  int     `json:"total"`
			Passed int     `json:"passed"`
			Failed int     `json:"failed"`
			Score  float64 `json:"score"`
			Error  string  `json:"error"`
		}
		if err := json.Unmarshal(out, &run); err != nil {
			fmt.Fprintf(os.Stderr, "poll run %s: bad response: %s\n", id, out)
			os.Exit(1)
		}
		if run.Status == "completed" || run.Status == "error" {
			fmt.Printf("status=%s total=%d passed=%d failed=%d score=%.3f\n",
				run.Status, run.Total, run.Passed, run.Failed, run.Score)
			if run.Error != "" {
				fmt.Printf("error: %s\n", run.Error)
			}
			printEvalResults(base, id)
			if run.Status == "error" || run.Failed > 0 {
				os.Exit(1)
			}
			return
		}
		time.Sleep(time.Second)
	}
}

// printEvalResults prints a one-line-per-case summary for a finished run.
func printEvalResults(base, id string) {
	out := mustAdminGet(base, "/admin/evals/runs/"+id+"/results")
	var results []struct {
		CaseIndex int    `json:"case_index"`
		Scorer    string `json:"scorer"`
		Passed    bool   `json:"passed"`
		Detail    string `json:"detail"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		fmt.Fprintf(os.Stderr, "results %s: bad response: %s\n", id, out)
		os.Exit(1)
	}
	for _, r := range results {
		verdict := "PASS"
		if !r.Passed {
			verdict = "FAIL"
		}
		line := fmt.Sprintf("  case %d [%s] %s", r.CaseIndex, r.Scorer, verdict)
		if r.Detail != "" {
			line += ": " + r.Detail
		}
		fmt.Println(line)
	}
}

// mustAtoi parses s as an int or exits with a usage-style error.
func mustAtoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid integer %q\n", s)
		os.Exit(2)
	}
	return n
}

func adminUsage() {
	fmt.Fprintln(os.Stderr, "usage: runtimectl admin <tenant create <id> [--name n]|user add <subject> --role r [--tenant t]|user ls|key create --role r [--label l] [--tenant t]|key ls|key revoke <id>|secret set <name> <value> [--tenant t]|secret set-oauth2 --name n --token-url u --client-id c --client-secret s [--scope x] [--audience a] [--tenant t]|secret set-obo --name n --token-url u --client-id c --client-secret s [--scope x] [--audience a] [--subject-token-type t] [--requested-token-type t] [--tenant t]|secret ls|secret rm <name>|secret rotate [--tenant t]|upstream add --name n (--url u|--openapi spec) [--base-url b] [--cred-secret s] [--cred-header h] [--tenant t]|upstream ls|upstream rm <id>|agent add --id i --url u [--name n] [--model m] [--cred-secret s] [--tenant t]|agent ls|agent rm <id>|agent enable <id>|agent disable <id>|agent restart <id>|policy add --name n --file p.cedar [--tenant t]|policy ls [--tenant t]|policy rm <name> [--tenant t]|quota add --tenant t --upstream u --rate n|quota ls|quota rm --upstream u [--tenant t]|eval set add --name n --file f [--tenant t]|eval set ls [--tenant t]|eval set rm <name>|eval run <set> --agent id [--tenant t] [--wait]|eval runs [--tenant t]|eval results <run-id>|eval policy set --agent id --rate 0-100 --file f [--tenant t]|eval policy ls [--tenant t]|eval policy rm <agent>|eval online-results [--session sid] [--tenant t]>")
	os.Exit(2)
}

// runRegister dispatches `runtimectl register <mint|list|revoke> ...`, calling
// the control plane's /admin/register-tokens routes (mirrors `admin key ...`).
func runRegister(base string, args []string) {
	if len(args) < 1 {
		registerUsage()
	}
	switch args[0] {
	case "mint":
		// register mint --agent <id>
		agent := flagValue(args[1:], "--agent", "")
		if agent == "" {
			registerUsage()
		}
		out := mustAdminPost(base, "/admin/register-tokens", map[string]string{"agent": agent})
		var resp struct{ ID, Plaintext string }
		if err := json.Unmarshal(out, &resp); err != nil || resp.Plaintext == "" {
			fmt.Fprintf(os.Stderr, "token created but plaintext missing: %s\n", out)
			os.Exit(1)
		}
		fmt.Printf("%s\n(store this now — shown once; set it as RUNTIME_REGISTRATION_TOKEN on agent %s)\n", resp.Plaintext, agent)
	case "list", "ls":
		fmt.Print(string(mustAdminGet(base, "/admin/register-tokens")))
	case "revoke":
		// register revoke <token-id>
		if len(args) < 2 {
			registerUsage()
		}
		mustAdminDelete(base, "/admin/register-tokens/"+args[1])
		fmt.Printf("registration token %s revoked\n", args[1])
	default:
		registerUsage()
	}
}

func registerUsage() {
	fmt.Fprintln(os.Stderr, "usage: runtimectl register <mint --agent <id>|list|revoke <token-id>>")
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

// flagValues collects the value following every occurrence of name in args, so
// a repeated flag (e.g. --scope a --scope b) yields ["a","b"]. Returns nil when
// the flag is absent.
func flagValues(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == name {
			out = append(out, args[i+1])
		}
	}
	return out
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

// mustAdminPostAny is mustAdminPost for a body with non-string values (e.g. the
// int rate_per_min in a quota). Same auth + error-exit semantics.
func mustAdminPostAny(base, path string, body map[string]any) []byte {
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	addAuth(req)
	resp, err := http.DefaultClient.Do(req)
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
