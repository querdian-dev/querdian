// Package ci holds the tests of the scripts that the CI workflows run.
package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitEnv is the environment for git and for the script: without the configuration of the machine
// and without the GIT_* variables of the caller. A git hook, `git rebase --exec` and some editors
// set GIT_DIR and GIT_WORK_TREE; git would then work on that repository and not on the test
// repository.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
}

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
	cmd.Env = gitEnv()
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
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name, files, want string
	}{
		{"docs", "docs/operations.md\ndocs/research/traceql.md\n", docsOnly},
		{"root markdown", "README.md\nPRODUCT.md\nDESIGN.md\nSECURITY.md\n", docsOnly},
		{"impeccable", ".impeccable/surfaces/site-layouts-index-html.md\n.impeccable/design.json\n", docsOnly},
		{"templates", ".github/pull_request_template.md\n.github/ISSUE_TEMPLATE/bug_report.md\n", docsOnly},
		{"site", "site/layouts/index.html\n", siteOnly},
		{"site and docs", "site/docs-overview.md\ndocs/quickstart.md\n", siteOnly},
		{"docs image is read by a site test", "docs/img/flow.svg\n", siteOnly},
		{"docs image and docs", "docs/img/flow.svg\ndocs/quickstart.md\n", siteOnly},
		{"changelog is read by a test", "CHANGELOG.md\n", all},
		{"chart readme is packaged", "charts/querdian/README.md\n", all},
		{"markdown in a package can be embedded", "internal/ui/web/help.md\n", all},
		{"markdown in hack", "hack/quickstart/README.md\n", all},
		{"testdata markdown", "pkg/logql/testdata/x.md\n", all},
		{"root testdata markdown", "testdata/x.md\n", all},
		{"issue template config", ".github/ISSUE_TEMPLATE/config.yml\n", all},
		{"go code", "internal/proxy/proxy.go\n", all},
		{"go code and docs", "docs/operations.md\ninternal/proxy/proxy.go\n", all},
		{"workflow", ".github/workflows/ci.yml\n", all},
		{"script", "hack/ci/changes.sh\n", all},
		{"go.mod", "go.mod\n", all},
		{"root file without extension", "Makefile\n", all},
		{"quoted path", "\"docs/\\303\\244.md\"\n", all},
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

// Each case that the script cannot decide runs all steps.
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
		args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false"}, args...)
		cmd := exec.Command("git", args...) //nolint:gosec // arguments of the test
		cmd.Dir = dir
		cmd.Env = gitEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) string {
		t.Helper()
		git("add", "-A")
		git("commit", "-q", "-m", msg)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q", "-b", "main")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("README.md", "# Readme\n")
	base := commit("base")

	write("docs/a.md", "# A\n")
	docs := commit("docs")
	write("internal/b.go", "package internal\n")
	code := commit("code")

	if got := run(t, dir, "", "pull_request", base, docs); got != docsOnly {
		t.Errorf("docs commit: got %q", got)
	}
	if got := run(t, dir, "", "push", base, code); got != all {
		t.Errorf("docs and code commits: got %q", got)
	}
	if got := run(t, dir, "", "push", docs, docs); got != all {
		t.Errorf("no files: got %q", got)
	}

	// A rename of code into docs/ must count as a change of code: rename detection would give only
	// the new path.
	git("mv", "main.go", "docs/main.md")
	renamed := commit("rename")
	if got := run(t, dir, "", "pull_request", code, renamed); got != all {
		t.Errorf("rename of code into docs/: got %q", got)
	}

	// A push of a merge commit compares the earlier tip of main with the merge commit.
	git("checkout", "-q", "-b", "topic", code)
	write("docs/c.md", "# C\n")
	commit("topic docs")
	git("checkout", "-q", "main")
	git("reset", "-q", "--hard", code)
	write("README.md", "# Readme, changed\n")
	tip := commit("main docs")
	git("merge", "-q", "--no-ff", "-m", "merge", "topic")
	merge := git("rev-parse", "HEAD")
	if got := run(t, dir, "", "push", tip, merge); got != docsOnly {
		t.Errorf("merge of a docs branch: got %q", got)
	}
}
