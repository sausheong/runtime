package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: runtimectl <invoke|logs|deploy> [args]")
		os.Exit(2)
	}
	base := envOr("RUNTIME_CTL_URL", "http://localhost:8080")
	switch os.Args[1] {
	case "invoke":
		invoke(base, os.Args[2:])
	case "logs":
		logs(base, os.Args[2:])
	case "deploy":
		fmt.Println("deploy: M1 uses a single statically-configured agent 'default' (configured via runtimed env). Multi-agent deploy lands in M2.")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func invoke(base string, args []string) {
	msg := "hello"
	if len(args) > 0 {
		msg = args[0]
	}
	body, _ := json.Marshal(map[string]string{"message": msg})
	resp, err := http.Post(base+"/sessions", "application/json", bytes.NewReader(body))
	check(err)
	var out struct {
		SessionID string `json:"session_id"`
	}
	dec := json.NewDecoder(resp.Body)
	_ = dec.Decode(&out)
	resp.Body.Close()
	if out.SessionID == "" {
		fmt.Fprintln(os.Stderr, "error: no session id returned by control plane")
		os.Exit(1)
	}
	fmt.Println("session:", out.SessionID)
	stream(base, out.SessionID)
}

func logs(base string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: runtimectl logs <session-id>")
		os.Exit(2)
	}
	stream(base, args[0])
}

func stream(base, id string) {
	resp, err := http.Get(base + "/sessions/" + id + "/stream?since=0")
	check(err)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
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
