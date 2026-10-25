// Command chartmeta writes the release-dependent Artifact Hub annotations into the chart's
// Chart.yaml: the changelog of the version (artifacthub.io/changes, taken from CHANGELOG.md),
// artifacthub.io/containsSecurityUpdates, artifacthub.io/prerelease and the image list
// (helm.sh/images). The release workflow runs it before `helm package`; the static annotations
// live in Chart.yaml itself. See docs/releasing.md.
//
//	go run ./hack/chartmeta -version 0.1.0           # rewrites charts/querdian/Chart.yaml
//	go run ./hack/chartmeta -version 0.1.0 -out -    # prints the result instead
//
// The changes are the entries under the Keep a Changelog headings Added, Changed, Deprecated,
// Removed, Fixed and Security of the section "## [<version>]". A pre-release falls back to the
// section of its release version and then to "## [Unreleased]"; a release must have its own
// section with at least one entry, so a release with an unfinished CHANGELOG fails here.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// change is one entry of the artifacthub.io/changes annotation.
type change struct {
	Kind        string `yaml:"kind" json:"kind"`
	Description string `yaml:"description" json:"description"`
	Links       []link `yaml:"links,omitempty" json:"links,omitempty"`
}

type link struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
}

type image struct {
	Name  string `yaml:"name" json:"name"`
	Image string `yaml:"image" json:"image"`
}

// kinds maps Keep a Changelog headings to Artifact Hub change kinds. Other headings (for
// example "Known limitations") are not part of the Artifact Hub changelog.
var kinds = map[string]string{
	"added":      "added",
	"changed":    "changed",
	"deprecated": "deprecated",
	"removed":    "removed",
	"fixed":      "fixed",
	"security":   "security",
}

// defaultImage is the image repository the chart deploys (values.yaml image.repository).
const defaultImage = "ghcr.io/querdian-dev/querdian"

var (
	// Same rule as the release workflow's tag check, without the leading "v".
	semverRe   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	sectionRe  = regexp.MustCompile(`^## \[([^\]]+)\]`)
	mdLinkRe   = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	mdEscapeRe = regexp.MustCompile("\\\\([\\\\`*_{}\\[\\]()#+.!|-])")
	spaceRe    = regexp.MustCompile(`\s+`)
)

func main() {
	var (
		version   = flag.String("version", "", "chart version without the leading v (required)")
		chart     = flag.String("chart", "charts/querdian/Chart.yaml", "Chart.yaml to read")
		out       = flag.String("out", "", `file to write, "-" for stdout (default: -chart)`)
		changelog = flag.String("changelog", "CHANGELOG.md", "changelog to take the changes from")
		repo      = flag.String("image", defaultImage, "image repository of the release")
	)
	flag.Parse()
	if err := run(*version, *chart, *out, *changelog, *repo); err != nil {
		fmt.Fprintln(os.Stderr, "chartmeta:", err)
		os.Exit(1)
	}
}

func run(version, chartPath, outPath, changelogPath, repo string) error {
	if !semverRe.MatchString(version) {
		return fmt.Errorf("-version %q is not MAJOR.MINOR.PATCH[-pre]", version)
	}
	log, err := os.ReadFile(changelogPath) //nolint:gosec // path from the command line
	if err != nil {
		return err
	}
	changes, err := releaseChanges(string(log), version)
	if err != nil {
		return err
	}
	chart, err := os.ReadFile(chartPath) //nolint:gosec // path from the command line
	if err != nil {
		return err
	}
	res, err := annotate(chart, version, changes, []image{{Name: "querdian", Image: repo + ":" + version}})
	if err != nil {
		return fmt.Errorf("%s: %w", chartPath, err)
	}
	switch outPath {
	case "-":
		_, err = os.Stdout.Write(res)
		return err
	case "":
		outPath = chartPath
	}
	return os.WriteFile(outPath, res, 0o644) //nolint:gosec // Chart.yaml is not secret
}

// releaseChanges returns the changes of version from a Keep a Changelog file.
func releaseChanges(changelog, version string) ([]change, error) {
	candidates := []string{version}
	if base, _, pre := strings.Cut(version, "-"); pre {
		candidates = append(candidates, base, "Unreleased")
	}
	sections := splitSections(changelog)
	for _, name := range candidates {
		if changes := parseChanges(sections[name]); len(changes) > 0 {
			return changes, nil
		}
	}
	return nil, fmt.Errorf("CHANGELOG has no entries under Added/Changed/Deprecated/Removed/Fixed/Security in a section ## [%s]",
		strings.Join(candidates, "] or ## ["))
}

// splitSections returns the lines of every "## [name]" section, keyed by name.
func splitSections(changelog string) map[string][]string {
	sections := map[string][]string{}
	var cur string
	for _, l := range strings.Split(strings.ReplaceAll(changelog, "\r\n", "\n"), "\n") {
		if m := sectionRe.FindStringSubmatch(l); m != nil {
			cur = m[1]
			continue
		}
		if strings.HasPrefix(l, "## ") {
			cur = ""
			continue
		}
		if cur != "" {
			sections[cur] = append(sections[cur], l)
		}
	}
	return sections
}

// parseChanges turns the top-level bullets under the known "###" headings into changes.
// "####" sub-headings only group entries and are dropped; continuation lines of a bullet are
// joined.
func parseChanges(lines []string) []change {
	var (
		out  []change
		kind string
		cur  []string
	)
	flush := func() {
		if cur != nil {
			desc, links := plain(strings.Join(cur, " "))
			out = append(out, change{Kind: kind, Description: desc, Links: links})
			cur = nil
		}
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "### "):
			flush()
			kind = kinds[strings.ToLower(strings.TrimSpace(l[4:]))]
		case strings.HasPrefix(l, "#"):
			flush()
		case kind == "":
		case strings.HasPrefix(l, "* "), strings.HasPrefix(l, "- "):
			flush()
			cur = []string{l[2:]}
		case cur != nil && strings.TrimSpace(l) != "" && (l[0] == ' ' || l[0] == '\t'):
			cur = append(cur, strings.TrimSpace(l))
		default:
			flush()
		}
	}
	flush()
	return out
}

// plain strips the Markdown that Artifact Hub would show literally. Absolute link targets are
// kept as links of the change; relative ones point into the repository and are dropped.
func plain(s string) (string, []link) {
	var links []link
	s = mdLinkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := mdLinkRe.FindStringSubmatch(m)
		if strings.HasPrefix(sub[2], "https://") {
			links = append(links, link{Name: sub[1], URL: sub[2]})
		}
		return sub[1]
	})
	s = strings.NewReplacer("`", "", "**", "").Replace(s)
	s = mdEscapeRe.ReplaceAllString(s, "$1")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " ")), links
}

// annotate sets the release annotations in Chart.yaml and keeps everything else, including
// comments and the static annotations.
func annotate(chart []byte, version string, changes []change, images []image) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(chart, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("not a YAML mapping")
	}
	ann := mappingValue(doc.Content[0], "annotations")
	security := false
	for _, c := range changes {
		security = security || c.Kind == "security"
	}
	for _, kv := range []struct {
		key string
		val any
	}{
		{"artifacthub.io/changes", changes},
		{"artifacthub.io/containsSecurityUpdates", fmt.Sprint(security)},
		{"artifacthub.io/prerelease", fmt.Sprint(strings.Contains(version, "-"))},
		{"helm.sh/images", images},
	} {
		if s, ok := kv.val.(string); ok {
			setScalar(ann, kv.key, s, 0)
			continue
		}
		b, err := yaml.Marshal(kv.val)
		if err != nil {
			return nil, err
		}
		setScalar(ann, kv.key, string(b), yaml.LiteralStyle)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// mappingValue returns the mapping under key in m, creating it if needed.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.MappingNode {
			return m.Content[i+1]
		}
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

// setScalar sets key in m to the string val, replacing an existing value.
func setScalar(m *yaml.Node, key, val string, style yaml.Style) {
	v := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val, Style: style}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}
