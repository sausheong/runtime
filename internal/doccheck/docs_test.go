package doccheck_test

import (
	"bufio"
	"fmt"
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

type workflowDocument struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Steps []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Name            string `yaml:"name"`
	Run             string `yaml:"run"`
	Uses            string `yaml:"uses"`
	ContinueOnError bool   `yaml:"continue-on-error"`
}

var releaseValidationCommands = []string{
	"make check",
	"make security-scan",
	"make test-integration",
	"go test -race",
	"pytest contrib/shims/python/tests",
	"make helm-lint",
	"bash deploy/charts/runtime/test.sh",
	"shellcheck",
	"docker compose -f deploy/compose/docker-compose.yml config --quiet",
	"docker compose -f deploy/gcp/control-plane/docker-compose.yml config --quiet",
}

var publicationCommands = []string{
	"docker push",
	"helm push",
	"cosign sign",
	"cosign attest",
	"gh release create",
}

var racePackages = []string{
	"./internal/eval",
	"./internal/browser",
	"./internal/memory",
	"./internal/httplimit",
	"./cmd/runtimed",
}

func stepRuns(step workflowStep, command string) bool {
	for _, line := range strings.Split(step.Run, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, command) {
			return true
		}
	}
	return false
}

func validateReleasePublicationGates(data []byte) error {
	var workflow workflowDocument
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return fmt.Errorf("parse release workflow: %w", err)
	}
	publish, ok := workflow.Jobs["publish"]
	if !ok {
		return fmt.Errorf("release workflow has no publish job")
	}
	firstPublication := -1
	for i, step := range publish.Steps {
		for _, command := range publicationCommands {
			if stepRuns(step, command) {
				firstPublication = i
				break
			}
		}
		if firstPublication >= 0 {
			break
		}
	}
	if firstPublication < 0 {
		return fmt.Errorf("publish job has no publication operation")
	}
	for _, required := range releaseValidationCommands {
		found := false
		for i, step := range publish.Steps {
			if i >= firstPublication {
				break
			}
			if step.ContinueOnError {
				continue
			}
			if stepRuns(step, required) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("publish job lacks blocking pre-publication gate %q", required)
		}
	}
	for i, step := range publish.Steps[:firstPublication] {
		if stepRuns(step, "go test -race") {
			for _, pkg := range racePackages {
				if !stepRuns(step, pkg) {
					return fmt.Errorf("race gate at step %d omits %s", i, pkg)
				}
			}
		}
	}
	return nil
}

func validateCIHelmLint(data []byte) error {
	var workflow workflowDocument
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return fmt.Errorf("parse CI workflow: %w", err)
	}
	job, ok := workflow.Jobs["helm"]
	if !ok {
		return fmt.Errorf("CI workflow has no helm job")
	}
	for _, step := range job.Steps {
		if !step.ContinueOnError && stepRuns(step, "make helm-lint") {
			return nil
		}
	}
	return fmt.Errorf("CI helm job lacks blocking make helm-lint")
}

func validateCIRaceGate(data []byte) error {
	var workflow workflowDocument
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return fmt.Errorf("parse CI workflow: %w", err)
	}
	job, ok := workflow.Jobs["unit"]
	if !ok {
		return fmt.Errorf("CI workflow has no unit job")
	}
	for i, step := range job.Steps {
		if step.ContinueOnError || !stepRuns(step, "go test -race") {
			continue
		}
		for _, pkg := range racePackages {
			if !stepRuns(step, pkg) {
				return fmt.Errorf("CI race gate at step %d omits %s", i, pkg)
			}
		}
		return nil
	}
	return fmt.Errorf("CI unit job lacks blocking race gate")
}

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
	if err := validateReleasePublicationGates(data); err != nil {
		t.Error(err)
	}
	for _, required := range []string{
		"${{ github.ref_name }}",
		"--build-arg VERSION=",
		"--build-arg REVISION=",
		"org.opencontainers.image.version",
		"org.opencontainers.image.revision",
		"deploy/sandbox.Dockerfile",
		"deploy/browser.Dockerfile",
		"deploy/compose/embedder/Dockerfile",
		"grype runtime:release-validation --fail-on high",
		"--only-fixed",
		"docker push",
		"syft ",
		"cosign sign ",
		"cosign attest ",
		"helm push",
	} {
		if !strings.Contains(string(data), required) {
			t.Errorf("release workflow missing %q", required)
		}
	}
	ciData, err := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCIHelmLint(ciData); err != nil {
		t.Error(err)
	}
	if err := validateCIRaceGate(ciData); err != nil {
		t.Error(err)
	}
	if !strings.Contains(string(ciData), "grype runtime:ci --fail-on high --only-fixed") {
		t.Error("CI does not enforce the actionable high-severity image gate")
	}
}

func TestReleaseImagesAreDigestDeployableAndOptionalImagesConstrained(t *testing.T) {
	root := repositoryRoot(t)
	values, err := os.ReadFile(filepath.Join(root, "deploy/charts/runtime/values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	helpers, err := os.ReadFile(filepath.Join(root, "deploy/charts/runtime/templates/_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(values), "digest:") ||
		!strings.Contains(string(helpers), `printf "%s@%s" .Values.image.repository .Values.image.digest`) {
		t.Fatal("Helm chart cannot consume an immutable image digest")
	}
	for _, path := range []string{
		"deploy/Dockerfile",
		"deploy/sandbox.Dockerfile",
		"deploy/browser.Dockerfile",
		"deploy/compose/embedder/Dockerfile",
		"deploy/gcp/agent-python/Dockerfile",
		"deploy/gcp/agent-claude/Dockerfile",
		"deploy/gcp/agent-food-label/Dockerfile",
	} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && strings.EqualFold(fields[0], "FROM") &&
				fields[1] != "scratch" && !strings.Contains(fields[1], "@sha256:") {
				t.Errorf("%s line %d base image is not digest-pinned: %q", path, i+1, line)
			}
			if strings.Contains(line, "COPY --from=") {
				from := strings.SplitN(strings.SplitN(line, "COPY --from=", 2)[1], " ", 2)[0]
				if strings.Contains(from, "/") && !strings.Contains(from, "@sha256:") {
					t.Errorf("%s line %d external copy image is not digest-pinned: %q", path, i+1, line)
				}
			}
		}
	}
	for _, path := range []string{
		"deploy/sandbox-requirements.txt",
		"deploy/compose/embedder/requirements.txt",
	} {
		requirements, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(strings.TrimSpace(string(requirements)), "\n") {
			if !strings.Contains(line, "==") {
				t.Errorf("%s line %d is not exactly pinned: %q", path, i+1, line)
			}
		}
	}
}

func TestReleaseWorkflowGateValidatorRejectsMutations(t *testing.T) {
	step := func(command string, continueOnError bool) string {
		extra := ""
		if continueOnError {
			extra = "\n        continue-on-error: true"
		}
		return fmt.Sprintf("\n      - run: %q%s", command, extra)
	}
	valid := "jobs:\n  publish:\n    steps:"
	for _, command := range releaseValidationCommands {
		if command == "go test -race" {
			command += " ./controlplane " + strings.Join(racePackages, " ")
		}
		valid += step(command, false)
	}
	valid += step("docker push image", false)
	if err := validateReleasePublicationGates([]byte(valid)); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}

	tests := map[string]string{
		"omitted": strings.Replace(valid,
			step("make helm-lint", false), "", 1),
		"after publication": strings.Replace(
			strings.Replace(valid, step("make helm-lint", false), "", 1),
			step("docker push image", false),
			step("docker push image", false)+step("make helm-lint", false), 1),
		"unrelated job": strings.Replace(
			strings.Replace(valid, step("make helm-lint", false), "", 1),
			"jobs:", "jobs:\n  unrelated:\n    steps:"+step("make helm-lint", false), 1),
		"comment only": strings.Replace(valid,
			step("make helm-lint", false),
			"\n      - run: |\n          # make helm-lint\n          echo skipped", 1),
		"continue on error": strings.Replace(valid,
			step("make helm-lint", false), step("make helm-lint", true), 1),
	}
	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateReleasePublicationGates([]byte(fixture)); err == nil {
				t.Fatal("mutated workflow was accepted")
			}
		})
	}
}

func TestSecurityScanCoversEveryShippedBinaryAndFailsClosed(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(data)
	for _, required := range []string{
		"BINS := agentd browserd runtimectl runtimed sandboxd v1-probe",
		"SECURITY_BINS := $(BINS)",
		"security-scan:",
		"-scan=package ./...",
		"set -eu",
		"-mode=binary",
	} {
		if !strings.Contains(makefile, required) {
			t.Errorf("security scan is missing fail-closed requirement %q", required)
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

func TestDeploymentProfilesExposeRetentionAndConcurrencyControls(t *testing.T) {
	root := repositoryRoot(t)
	requiredControl := []string{
		"RUNTIME_SESSION_RETENTION",
		"RUNTIME_SESSION_RETENTION_BATCH",
		"RUNTIME_SESSION_RETENTION_DRY_RUN",
		"RUNTIME_EVAL_RETENTION",
		"RUNTIME_MAX_REQUESTS",
		"RUNTIME_MAX_STREAMS",
	}
	for _, name := range []string{
		"deploy/docker-compose.full.yml",
		"deploy/compose/docker-compose.yml",
		"deploy/secured/docker-compose.yml",
		"deploy/gcp/control-plane/docker-compose.yml",
		"deploy/charts/runtime/templates/deployment.yaml",
	} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, variable := range requiredControl {
			if !strings.Contains(string(data), variable) {
				t.Errorf("%s does not expose %s", name, variable)
			}
		}
	}

	requiredAgent := []string{
		"RUNTIME_AGENT_MAX_REQUESTS",
		"RUNTIME_AGENT_MAX_STREAMS",
		"RUNTIME_MEMORY_RETENTION_DRY_RUN",
		"RUNTIME_MEMORY_RETENTION_EPISODE",
		"RUNTIME_MEMORY_RETENTION_FACT",
		"RUNTIME_MEMORY_RETENTION_SUMMARY",
	}
	for _, name := range []string{
		"deploy/compose/docker-compose.yml",
		"deploy/secured/docker-compose.yml",
		"deploy/gcp/agent-go/docker-compose.yml",
		"deploy/charts/runtime/templates/deployment.yaml",
		"deploy/charts/runtime/templates/agent-statefulset.yaml",
	} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, variable := range requiredAgent {
			if !strings.Contains(string(data), variable) {
				t.Errorf("%s does not expose %s", name, variable)
			}
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
