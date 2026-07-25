package doccheck_test

import (
	"bufio"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var markdownLink = regexp.MustCompile(`\[[^]]*]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func trackedMarkdown(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "ls-files", "*.md")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("tracked Markdown inventory requires a Git checkout: %v", err)
	}
	files := strings.Fields(string(out))
	seen := make(map[string]bool, len(files))
	for _, name := range files {
		seen[name] = true
	}
	// Planning documents are required acceptance artefacts and must be checked
	// even in a local worktree before they have been staged.
	for _, name := range []string{
		"docs/planning/runtime-remediation-issues.md",
		"docs/planning/runtime-remediation-plan.md",
	} {
		if !seen[name] {
			files = append(files, name)
		}
	}
	return files
}

func TestTrackedMarkdownStructure(t *testing.T) {
	root := repositoryRoot(t)
	files := trackedMarkdown(t, root)
	if len(files) == 0 {
		t.Fatal("no tracked Markdown files found")
	}

	for _, name := range files {
		name := name
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, filepath.FromSlash(name))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			checkFences(t, data)
			checkLocalLinks(t, root, path, data)
		})
	}
}

func TestRequiredDocumentationAndPlanningRegister(t *testing.T) {
	root := repositoryRoot(t)
	required := []string{
		"README.md",
		"SECURITY.md",
		"CONTRIBUTING.md",
		"CHANGELOG.md",
		"RELEASING.md",
		"documentation-map.md",
		"docs/planning/runtime-remediation-issues.md",
		"docs/planning/runtime-remediation-plan.md",
	}
	for _, name := range required {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Errorf("required documentation %s: %v", name, err)
		}
	}
}

func TestChartREADMEVersionMatchesMetadata(t *testing.T) {
	root := repositoryRoot(t)
	chartData, err := os.ReadFile(filepath.Join(root, "deploy/charts/runtime/Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Version    string `yaml:"version"`
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal(chartData, &chart); err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(filepath.Join(root, "deploy/charts/runtime/README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	if !strings.Contains(text, "**Chart version:** "+chart.Version) {
		t.Errorf("Helm README does not report chart version %s", chart.Version)
	}
	if !strings.Contains(text, "**App version:** "+chart.AppVersion) {
		t.Errorf("Helm README does not report app version %s", chart.AppVersion)
	}
}

func TestReleaseWorkflowIsValidAndPinned(t *testing.T) {
	root := repositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("release workflow YAML: %v", err)
	}
	action := regexp.MustCompile(`(?m)^\s*-\s+uses:\s+\S+@([0-9a-f]{40})\s*(?:#.*)?$`)
	usesLines := regexp.MustCompile(`(?m)^\s*-\s+uses:\s+.*$`).FindAll(data, -1)
	if len(usesLines) == 0 {
		t.Fatal("release workflow has no actions")
	}
	if got := len(action.FindAll(data, -1)); got != len(usesLines) {
		t.Errorf("release workflow has unpinned action references: %d actions, %d commit-pinned", len(usesLines), got)
	}
	for _, required := range []string{
		"${{ github.ref_name }}",
		"docker push",
		"syft ",
		"cosign sign ",
		"cosign attest ",
		"helm push",
		"make test-integration",
		"go test -race",
		"./internal/eval",
		"pytest contrib/shims/python/tests",
		"bash deploy/charts/runtime/test.sh",
		"docker compose -f deploy/compose/docker-compose.yml config --quiet",
		"shellcheck ",
	} {
		if !strings.Contains(string(data), required) {
			t.Errorf("release workflow missing %q", required)
		}
	}
}

func TestDeploymentAgentDatabaseCredentialsFailClosed(t *testing.T) {
	root := repositoryRoot(t)
	gcpCompose, err := os.ReadFile(filepath.Join(root, "deploy/gcp/control-plane/docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	composeText := string(gcpCompose)
	if strings.Contains(composeText, "runtime-agent-change-me") {
		t.Fatal("GCP Compose must not provide a predictable agent database password")
	}
	if got := strings.Count(composeText, "${RUNTIME_AGENT_DB_PASSWORD:?set RUNTIME_AGENT_DB_PASSWORD}"); got != 2 {
		t.Fatalf("GCP Compose has %d fail-closed agent password references, want 2", got)
	}

	envExample, err := os.ReadFile(filepath.Join(root, "deploy/gcp/control-plane/.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envExample), "RUNTIME_AGENT_DB_PASSWORD=") {
		t.Fatal("GCP environment example omits RUNTIME_AGENT_DB_PASSWORD")
	}

	for _, workflow := range []string{".github/workflows/ci.yml", ".github/workflows/release.yml"} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(workflow)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(data), "RUNTIME_AGENT_DB_PASSWORD:") < 2 {
			t.Errorf("%s does not configure the agent database password for both Compose gates", workflow)
		}
	}
}

func checkFences(t *testing.T, data []byte) {
	t.Helper()
	var backticks, tildes int
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "```"):
			backticks++
		case strings.HasPrefix(line, "~~~"):
			tildes++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if backticks%2 != 0 {
		t.Errorf("unbalanced backtick fences: %d fence lines", backticks)
	}
	if tildes%2 != 0 {
		t.Errorf("unbalanced tilde fences: %d fence lines", tildes)
	}
}

func checkLocalLinks(t *testing.T, root, source string, data []byte) {
	t.Helper()
	for _, match := range markdownLink.FindAllSubmatch(data, -1) {
		raw := strings.Trim(string(match[1]), "<>")
		if strings.HasPrefix(raw, "#") || hasExternalScheme(raw) {
			continue
		}
		target := strings.SplitN(raw, "#", 2)[0]
		if target == "" {
			continue
		}
		decoded, err := url.PathUnescape(target)
		if err != nil {
			t.Errorf("invalid link escape %q: %v", raw, err)
			continue
		}
		resolved := filepath.Clean(filepath.Join(filepath.Dir(source), filepath.FromSlash(decoded)))
		if rel, err := filepath.Rel(root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("local link escapes repository: %q", raw)
			continue
		}
		if _, err := os.Stat(resolved); err != nil {
			t.Errorf("broken local link %q: %v", raw, err)
		}
	}
}

func hasExternalScheme(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme != ""
}
