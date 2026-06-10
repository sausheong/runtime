// Package sandbox implements the Docker-backed code-interpreter sessions
// served by cmd/sandboxd as MCP tools behind the platform gateway.
package sandbox

import (
	"fmt"
	"path"
	"strings"
)

// workspace is the only writable directory in a sandbox container (a tmpfs).
const workspace = "/workspace"

// confinePath resolves a user-supplied path strictly under /workspace.
// Relative paths are joined to /workspace; absolute paths must already be
// inside it. Anything escaping after cleaning (.., absolute elsewhere,
// /workspaceX prefix tricks) is rejected. Path strings are never shell-
// interpolated: file I/O goes through argv-only execs (dd/head), never a
// shell string.
func confinePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is empty")
	}
	if !strings.HasPrefix(p, "/") {
		p = workspace + "/" + p
	}
	clean := path.Clean(p)
	if clean != workspace && !strings.HasPrefix(clean, workspace+"/") {
		return "", fmt.Errorf("path %q is outside %s", p, workspace)
	}
	return clean, nil
}
