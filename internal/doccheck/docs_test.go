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
	return strings.Fields(string(out))
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
