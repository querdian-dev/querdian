// Package ci holds the tests of the scripts that the CI workflows run.
package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run runs changes.sh with args in dir, with stdin as input, and returns its output.
func run(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	script, err := filepath.Abs("changes.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...) //nolint:gosec // the script under test
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("changes.sh %v: %v\n%s", args, err, out)
	}
	return string(out)
}

const (
	all      = "go=true\nsite=true\n"
	docsOnly = "go=false\nsite=false\n"
	siteOnly = "go=false\nsite=true\n"
	code     = "go=true\nsite=false\n"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name, files, want string
	}{
		{"docs", "docs/operations.md\ndocs/research/traceql.md\n", docsOnly},
		{"docs image is read by a site test", "docs/img/flow.svg\n", siteOnly},
		{"root markdown", "README.md\nPRODUCT.md\nDESIGN.md\nSECURITY.md\n", docsOnly},
		{"impeccable", ".impeccable/surfaces/site-layouts-index-html.md\n", docsOnly},
		{"markdown in hack", "hack/quickstart/README.md\n", docsOnly},
		{"pull request template", ".github/pull_request_template.md\n", docsOnly},
		{"site", "site/layouts/index.html\n", siteOnly},
		{"site and docs", "site/docs-overview.md\ndocs/quickstart.md\n", siteOnly},
		{"changelog is read by a test", "CHANGELOG.md\n", code},
		{"chart readme is packaged", "charts/querdian/README.md\n", code},
		{"testdata markdown", "hack/research/testdata/x.md\n", code},
		{"go code", "internal/proxy/proxy.go\n", code},
		{"go code and docs", "docs/operations.md\ninternal/proxy/proxy.go\n", code},
		{"workflow", ".github/workflows/ci.yml\n", code},
		{"go.mod", "go.mod\n", code},
		{"site and code", "site/layouts/index.html\ncmd/querdian/main.go\n", all},
		{"no files", "", all},
		{"empty lines only", "\n\n", all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(t, ".", tc.files, "classify"); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Each case that the script cannot decide runs every job.
func TestUndecidedRunsAll(t *testing.T) {
	dir := t.TempDir() // not a git repository, so each diff fails
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"schedule", []string{"schedule", "abc", "def"}},
		{"workflow_dispatch", []string{"workflow_dispatch", "", ""}},
		{"no event", nil},
		{"new branch", []string{"push", "0000000000000000000000000000000000000000", "abc"}},
		{"no base", []string{"pull_request", "", "abc"}},
		{"diff fails", []string{"push", "abc", "def"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(t, dir, "", tc.args...); got != all {
				t.Fatalf("got %q, want %q", got, all)
			}
		})
	}
}

// The diff mode compares base...head of a real repository.
func TestDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...) //nolint:gosec // fixed arguments of the test
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("main.go")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")
	write("docs/a.md")
	git("add", ".")
	git("commit", "-q", "-m", "docs")
	docs := git("rev-parse", "HEAD")
	write("internal/b.go")
	git("add", ".")
	git("commit", "-q", "-m", "code")
	head := git("rev-parse", "HEAD")

	if got := run(t, dir, "", "pull_request", base, docs); got != docsOnly {
		t.Fatalf("docs commit: got %q", got)
	}
	if got := run(t, dir, "", "push", base, head); got != code {
		t.Fatalf("docs and code commits: got %q", got)
	}
	if got := run(t, dir, "", "push", docs, docs); got != all {
		t.Fatalf("no files: got %q", got)
	}
}
