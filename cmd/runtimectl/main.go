package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

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
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: runtimectl <agents|invoke|sessions|logs|conformance> [--agent <id>] [args]")
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
