// Package site tests build-content.sh, which generates the documentation versions of the website
// from the release tags and the working tree.
package site

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// version is one entry of the generated data/versions.yaml.
type version struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Ref     string `json:"ref"`
	Latest  bool   `json:"latest"`
	Release bool   `json:"release"`
	Nav     []struct {
		Title string   `json:"title"`
		Pages []string `json:"pages"`
	} `json:"nav"`
}

// testRepo is a temporary repository with a copy of build-content.sh at site/.
type testRepo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *testRepo {
	t.Helper()
	for _, tool := range []string{"git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	script, err := os.ReadFile("build-content.sh")
	if err != nil {
		t.Fatal(err)
	}
	r := &testRepo{t: t, dir: t.TempDir()}
	r.write("site/build-content.sh", string(script))
	r.git("init", "--quiet", "--initial-branch=main")
	return r
}

func (r *testRepo) write(name, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil { //nolint:gosec // a file of the test repository
		r.t.Fatal(err)
	}
}

func (r *testRepo) read(name string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, name)) //nolint:gosec // a file of the test repository
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *testRepo) exists(name string) bool {
	_, err := os.Stat(filepath.Join(r.dir, name))
	return err == nil
}

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

// git runs git in the repository, without the configuration and the hooks of the machine.
func (r *testRepo) git(args ...string) {
	r.t.Helper()
	args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)
	cmd := exec.Command("git", args...) //nolint:gosec // arguments of the test
	cmd.Dir = r.dir
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (r *testRepo) release(tag string) {
	r.t.Helper()
	r.git("add", "--all")
	r.git("commit", "--quiet", "--allow-empty", "-m", tag)
	r.git("tag", tag)
}

// build runs build-content.sh and returns its output and the generated version list.
func (r *testRepo) build() (string, []version, error) {
	r.t.Helper()
	cmd := exec.Command("bash", filepath.Join(r.dir, "site/build-content.sh")) //nolint:gosec // the script under test
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), nil, err
	}
	var versions []version
	if err := yaml.Unmarshal([]byte(r.read("site/data/versions.yaml")), &versions); err != nil {
		r.t.Fatalf("data/versions.yaml: %v", err)
	}
	return string(out), versions, nil
}

const (
	overview = "---\ntitle: Documentation\n---\n\nOverview.\n\n{{< docs-index >}}\n"
	navStart = "- title: Start\n  pages:\n    - quickstart\n"
)

func names(versions []version) []string {
	var out []string
	for _, v := range versions {
		out = append(out, v.Name+" "+v.Path)
	}
	return out
}

func TestBuildContentVersions(t *testing.T) {
	r := newRepo(t)

	// v0.1.0 and v0.1.1: one release line. The overview page is at its first location.
	r.write("docs/quickstart.md", "# Quickstart\n\nBody of 0.1.0.\n")
	r.write("docs/legacy.md", "# Legacy\n\nOnly in 0.1.\n")
	r.write("site/data/nav.yaml", navStart+"    - legacy\n")
	r.write("site/content/docs/_index.md", overview)
	r.release("v0.1.0")
	r.write("docs/quickstart.md", "# Quickstart\n\nBody of 0.1.1.\n")
	r.release("v0.1.1")

	// v0.2.0: a new page, a title override and the overview page at its current location. The
	// release candidate and the tag that is not a version are not published.
	r.write("docs/quickstart.md", "# Quickstart\n\nBody of 0.2.0.\n")
	r.write("docs/security/threat-model.md", "# Threat \"model\"\n\nThreats.\n")
	r.write("docs/guide.md", "# Guide\n\n# Not a title\n")
	r.write("site/data/nav.yaml", navStart+"    - guide | The guide\n- title: Security\n  pages:\n    - security/threat-model\n")
	r.git("mv", "site/content/docs/_index.md", "site/docs-overview.md")
	r.release("v0.2.0-rc.1")
	r.release("v0.2.0")
	r.git("tag", "vnext")

	// The working tree has a page that is not in a release, without a commit.
	r.write("docs/quickstart.md", "# Quickstart\n\nBody of main.\n")
	r.write("docs/next.md", "# Next\n\nUnreleased.\n")
	r.write("site/data/nav.yaml", navStart+"    - next\n- title: Security\n  pages:\n    - security/threat-model\n")
	// A file of an earlier build must not stay.
	r.write("site/content/docs/v0.0.9/stale.md", "stale")

	out, versions, err := r.build()
	if err != nil {
		t.Fatalf("build-content.sh: %v\n%s", err, out)
	}

	if got, want := names(versions), []string{"v0.2.0 /docs", "v0.1.1 /docs/v0.1.1", "main /docs/main"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("versions = %q, want %q", got, want)
	}
	latest, older, main := versions[0], versions[1], versions[2]
	if !latest.Latest || !latest.Release || latest.Ref != "v0.2.0" {
		t.Errorf("latest = %+v", latest)
	}
	if older.Latest || !older.Release || older.Ref != "v0.1.1" {
		t.Errorf("older = %+v", older)
	}
	if main.Latest || main.Release || main.Ref != "main" {
		t.Errorf("main = %+v", main)
	}
	if got, want := older.Nav[0].Pages, []string{"quickstart", "legacy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("navigation of v0.1.1 = %q, want %q", got, want)
	}
	if got, want := latest.Nav[0].Pages, []string{"quickstart", "guide | The guide"}; !reflect.DeepEqual(got, want) {
		t.Errorf("navigation of v0.2.0 = %q, want %q", got, want)
	}
	if got, want := main.Nav[0].Pages, []string{"quickstart", "next"}; !reflect.DeepEqual(got, want) {
		t.Errorf("navigation of main = %q, want %q", got, want)
	}

	// Each version has the text of its own tag.
	for file, body := range map[string]string{
		"site/content/docs/quickstart.md":        "Body of 0.2.0.",
		"site/content/docs/v0.1.1/quickstart.md": "Body of 0.1.1.",
		"site/content/docs/main/quickstart.md":   "Body of main.",
		"site/content/docs/main/next.md":         "Unreleased.",
	} {
		if got := r.read(file); !strings.Contains(got, body) {
			t.Errorf("%s does not contain %q:\n%s", file, body, got)
		}
	}
	for _, file := range []string{
		"site/content/docs/next.md",     // not in the latest release
		"site/content/docs/v0.1.0",      // an earlier patch of a line
		"site/content/docs/v0.2.0",      // the latest release is at /docs/
		"site/content/docs/v0.2.0-rc.1", // a pre-release
		"site/content/docs/vnext",
		"site/content/docs/v0.0.9",
		"site/content/docs/main/guide.md", // not in the navigation of main
		"site/content/docs/legacy.md",     // removed in v0.2.0
	} {
		if r.exists(file) {
			t.Errorf("%s must not exist", file)
		}
	}

	// Front matter: title, order, version and the other addresses of the version.
	if got, want := r.read("site/content/docs/quickstart.md"),
		"---\ntitle: \"Quickstart\"\nweight: 10\nsource: docs/quickstart.md\nversion: \"v0.2.0\"\naliases: [\"/docs/v0.2.0/quickstart/\"]\n---\n\nBody of 0.2.0.\n"; got != want {
		t.Errorf("latest quickstart:\n got %q\nwant %q", got, want)
	}
	if got, want := r.read("site/content/docs/guide.md"),
		"---\ntitle: \"The guide\"\nweight: 20\nsource: docs/guide.md\nversion: \"v0.2.0\"\naliases: [\"/docs/v0.2.0/guide/\"]\n---\n\n# Not a title\n"; got != want {
		t.Errorf("guide:\n got %q\nwant %q", got, want)
	}
	if got, want := r.read("site/content/docs/security/threat-model.md"),
		"---\ntitle: \"Threat \\\"model\\\"\"\nweight: 30\nsource: docs/security/threat-model.md\nversion: \"v0.2.0\"\naliases: [\"/docs/v0.2.0/security/threat-model/\"]\n---\n\nThreats.\n"; got != want {
		t.Errorf("threat model:\n got %q\nwant %q", got, want)
	}
	if got, want := r.read("site/content/docs/v0.1.1/quickstart.md"),
		"---\ntitle: \"Quickstart\"\nweight: 10\nsource: docs/quickstart.md\nversion: \"v0.1.1\"\naliases: [\"/docs/v0.1.0/quickstart/\"]\nsitemap: {disable: true}\n---\n\nBody of 0.1.1.\n"; got != want {
		t.Errorf("older quickstart:\n got %q\nwant %q", got, want)
	}
	if got := r.read("site/content/docs/main/quickstart.md"); strings.Contains(got, "aliases") {
		t.Errorf("main must not have aliases:\n%s", got)
	}

	// Sitemap: a page that the latest release also has is not listed for the other versions. A page
	// that the latest release does not have stays in the sitemap, and so do all pages of the latest
	// release.
	const noSitemap = "\nsitemap: {disable: true}\n"
	for _, file := range []string{
		"site/content/docs/main/quickstart.md",
		"site/content/docs/main/security/threat-model.md", // a nested page
		"site/content/docs/v0.1.1/quickstart.md",
	} {
		if got := r.read(file); !strings.Contains(got, noSitemap) {
			t.Errorf("%s must not be in the sitemap:\n%s", file, got)
		}
	}
	for _, file := range []string{
		"site/content/docs/main/next.md",     // only on main
		"site/content/docs/v0.1.1/legacy.md", // only in the older release line
		"site/content/docs/quickstart.md",
		"site/content/docs/guide.md",
		"site/content/docs/_index.md",
	} {
		if got := r.read(file); strings.Contains(got, "sitemap") {
			t.Errorf("%s must be in the sitemap:\n%s", file, got)
		}
	}

	// Overview pages, from both locations of the source.
	if got, want := r.read("site/content/docs/_index.md"),
		"---\nversion: \"v0.2.0\"\naliases: [\"/docs/v0.2.0/\"]\ntitle: Documentation\n---\n\nOverview.\n\n{{< docs-index >}}\n"; got != want {
		t.Errorf("latest overview:\n got %q\nwant %q", got, want)
	}
	if got, want := r.read("site/content/docs/v0.1.1/_index.md"),
		"---\nversion: \"v0.1.1\"\naliases: [\"/docs/v0.1.0/\"]\nsitemap: {disable: true}\ntitle: Documentation\n---\n\nOverview.\n\n{{< docs-index >}}\n"; got != want {
		t.Errorf("older overview:\n got %q\nwant %q", got, want)
	}
	if got, want := r.read("site/content/docs/main/_index.md"),
		"---\nversion: \"main\"\nsitemap: {disable: true}\ntitle: Documentation\n---\n\nOverview.\n\n{{< docs-index >}}\n"; got != want {
		t.Errorf("main overview:\n got %q\nwant %q", got, want)
	}
}

// A patch release of an older line replaces the published patch of that line only.
func TestBuildContentPatchOfOlderLine(t *testing.T) {
	r := newRepo(t)
	r.write("docs/quickstart.md", "# Quickstart\n\nBody.\n")
	r.write("site/data/nav.yaml", navStart)
	r.write("site/docs-overview.md", overview)
	for _, tag := range []string{"v0.9.0", "v0.10.0", "v0.9.1", "v1.0.0", "v0.10.1", "v1.0.1"} {
		r.release(tag)
	}
	out, versions, err := r.build()
	if err != nil {
		t.Fatalf("build-content.sh: %v\n%s", err, out)
	}
	want := []string{"v1.0.1 /docs", "v0.10.1 /docs/v0.10.1", "v0.9.1 /docs/v0.9.1", "main /docs/main"}
	if got := names(versions); !reflect.DeepEqual(got, want) {
		t.Errorf("versions = %q, want %q", got, want)
	}
	// The latest release answers at its tag and at the earlier patch of its line.
	if got := r.read("site/content/docs/quickstart.md"); !strings.Contains(got, "aliases: [\"/docs/v1.0.1/quickstart/\", \"/docs/v1.0.0/quickstart/\"]\n") {
		t.Errorf("v1.0.1 quickstart:\n%s", got)
	}
	if got := r.read("site/content/docs/_index.md"); !strings.Contains(got, "aliases: [\"/docs/v1.0.1/\", \"/docs/v1.0.0/\"]\n") {
		t.Errorf("v1.0.1 overview:\n%s", got)
	}
	if got := r.read("site/content/docs/v0.10.1/quickstart.md"); !strings.Contains(got, "aliases: [\"/docs/v0.10.0/quickstart/\"]\n") {
		t.Errorf("v0.10.1 quickstart:\n%s", got)
	}
}

// Without release tags the working tree is the only version, at /docs/.
func TestBuildContentWithoutReleases(t *testing.T) {
	r := newRepo(t)
	r.write("docs/quickstart.md", "# Quickstart\n\nBody.\n")
	r.write("site/data/nav.yaml", navStart)
	r.write("site/docs-overview.md", overview)
	r.release("v0.1.0-rc.1")

	out, versions, err := r.build()
	if err != nil {
		t.Fatalf("build-content.sh: %v\n%s", err, out)
	}
	if got, want := names(versions), []string{"main /docs"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("versions = %q, want %q", got, want)
	}
	if !versions[0].Latest || versions[0].Release {
		t.Errorf("main = %+v", versions[0])
	}
	if !r.exists("site/content/docs/quickstart.md") || r.exists("site/content/docs/main") {
		t.Error("the working tree must be at /docs/ only")
	}
	for _, file := range []string{"site/content/docs/quickstart.md", "site/content/docs/_index.md"} {
		if got := r.read(file); strings.Contains(got, "sitemap") {
			t.Errorf("%s must be in the sitemap:\n%s", file, got)
		}
	}
}

// A source tree without a repository has no releases: the working tree is the only version.
func TestBuildContentWithoutRepository(t *testing.T) {
	r := newRepo(t)
	r.write("docs/quickstart.md", "# Quickstart\n\nBody.\n")
	r.write("site/data/nav.yaml", navStart)
	r.write("site/docs-overview.md", overview)
	if err := os.RemoveAll(filepath.Join(r.dir, ".git")); err != nil {
		t.Fatal(err)
	}
	out, versions, err := r.build()
	if err != nil {
		t.Fatalf("build-content.sh: %v\n%s", err, out)
	}
	if got, want := names(versions), []string{"main /docs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("versions = %q, want %q", got, want)
	}
}

// The GIT_* variables of the caller must not send the git commands of the test to a different
// repository.
func TestBuildContentIgnoresGitEnvironment(t *testing.T) {
	other := newRepo(t)
	other.write("file", "content")
	other.release("v9.9.9")
	head := func() string {
		cmd := exec.Command("git", "--git-dir", filepath.Join(other.dir, ".git"), "rev-parse", "HEAD", "refs/tags/v9.9.9") //nolint:gosec // a test repository
		cmd.Env = gitEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git rev-parse: %v\n%s", err, out)
		}
		return string(out)
	}
	before := head()

	t.Setenv("GIT_DIR", filepath.Join(other.dir, ".git"))
	t.Setenv("GIT_WORK_TREE", other.dir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other.dir, ".git", "index"))

	r := newRepo(t)
	r.write("docs/quickstart.md", "# Quickstart\n\nBody.\n")
	r.write("site/data/nav.yaml", navStart)
	r.write("site/docs-overview.md", overview)
	r.release("v0.1.0")
	out, versions, err := r.build()
	if err != nil {
		t.Fatalf("build-content.sh: %v\n%s", err, out)
	}
	// v9.9.9 of the other repository is not a version of this one.
	if got, want := names(versions), []string{"v0.1.0 /docs", "main /docs/main"}; !reflect.DeepEqual(got, want) {
		t.Errorf("versions = %q, want %q", got, want)
	}
	if after := head(); after != before {
		t.Errorf("the repository of GIT_DIR changed:\nbefore %s\nafter  %s", before, after)
	}
	if other.exists("docs") || other.exists("site/data") {
		t.Error("the test wrote into the repository of GIT_DIR")
	}
}

func TestBuildContentRejects(t *testing.T) {
	tests := []struct {
		name  string
		setup func(r *testRepo)
		want  string
	}{
		{"page of the navigation without a file", func(r *testRepo) {
			r.write("site/data/nav.yaml", navStart+"    - absent\n")
		}, "main: missing docs/absent.md"},
		{"page without a title", func(r *testRepo) {
			r.write("docs/quickstart.md", "No heading.\n")
		}, "main: docs/quickstart.md has no H1"},
		{"no navigation", func(r *testRepo) {
			if err := os.Remove(filepath.Join(r.dir, "site/data/nav.yaml")); err != nil {
				r.t.Fatal(err)
			}
		}, "main: missing site/data/nav.yaml"},
		{"no overview page", func(r *testRepo) {
			if err := os.Remove(filepath.Join(r.dir, "site/docs-overview.md")); err != nil {
				r.t.Fatal(err)
			}
		}, "main: missing the docs overview page"},
		{"overview page without front matter", func(r *testRepo) {
			r.write("site/docs-overview.md", "Overview.\n")
		}, "main: the docs overview page has no front matter"},
		{"release with a page of the navigation without a file", func(r *testRepo) {
			r.write("site/data/nav.yaml", navStart+"    - absent\n")
			r.release("v0.1.0")
			r.write("site/data/nav.yaml", navStart)
		}, "v0.1.0: missing docs/absent.md"},
		{"release without a navigation", func(r *testRepo) {
			r.git("add", "docs", "site/docs-overview.md")
			r.git("commit", "--quiet", "-m", "no navigation")
			r.git("tag", "v0.1.0")
		}, "site/data/nav.yaml"},
		{"repository that git cannot read", func(r *testRepo) {
			r.release("v0.1.0")
			r.write(".git/HEAD", "not a ref\n")
		}, "fatal:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepo(t)
			r.write("docs/quickstart.md", "# Quickstart\n\nBody.\n")
			r.write("site/data/nav.yaml", navStart)
			r.write("site/docs-overview.md", overview)
			tt.setup(r)
			out, _, err := r.build()
			if err == nil {
				t.Fatalf("build-content.sh passed, want an error with %q\n%s", tt.want, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output does not contain %q:\n%s", tt.want, out)
			}
		})
	}
}
