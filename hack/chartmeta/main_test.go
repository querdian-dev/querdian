package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const testChangelog = `# Changelog

## [Unreleased]

### Added

* Next feature.

## [0.2.0] - 2026-11-01

### Added

#### Proxy

* A [dashboard](docs/dashboard.md) with ` + "`ui.*`" + ` values,
  continued on a second line.
- Dash bullet with a link to [the issue](https://github.com/querdian-dev/querdian/issues/7).

### Security

* Escaped detected\_\* names are **no longer** leaked.

### Known limitations

* Not a change.

## [0.1.0] - unreleased

_Stub without entries._

[Unreleased]: https://github.com/querdian-dev/querdian/commits/main
[0.2.0]: https://github.com/querdian-dev/querdian/releases/tag/v0.2.0
`

func TestReleaseChanges(t *testing.T) {
	want020 := []change{
		{Kind: "added", Description: "A dashboard with ui.* values, continued on a second line."},
		{Kind: "added", Description: "Dash bullet with a link to the issue.", Links: []link{
			{Name: "the issue", URL: "https://github.com/querdian-dev/querdian/issues/7"},
		}},
		{Kind: "security", Description: "Escaped detected_* names are no longer leaked."},
	}
	wantUnreleased := []change{{Kind: "added", Description: "Next feature."}}
	for _, tc := range []struct {
		version string
		want    []change
		err     bool
	}{
		{version: "0.2.0", want: want020},
		{version: "0.2.0-rc.1", want: want020},        // falls back to its release section
		{version: "0.3.0-rc.1", want: wantUnreleased}, // no section yet
		{version: "0.1.0-rc.1", want: wantUnreleased}, // release section has no entries
		{version: "0.1.0", err: true},                 // a release needs its own entries
		{version: "0.3.0", err: true},
	} {
		got, err := releaseChanges(testChangelog, tc.version)
		if tc.err {
			if err == nil {
				t.Errorf("%s: want error, got %+v", tc.version, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.version, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.version, got, tc.want)
		}
	}
}

// The real CHANGELOG must stay parseable: a pre-release always takes [Unreleased] or its
// release section, and every entry has a kind and a description.
func TestRepositoryChangelog(t *testing.T) {
	b, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := releaseChanges(string(b), "0.0.0-rc.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if kinds[c.Kind] == "" || c.Description == "" || strings.ContainsAny(c.Description, "`\n") {
			t.Errorf("bad change %+v", c)
		}
	}
}

func TestAnnotate(t *testing.T) {
	chart, err := os.ReadFile("../../charts/querdian/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	changes := []change{{Kind: "security", Description: "Fixed: a leak.", Links: []link{{Name: "PR", URL: "https://example.com/1"}}}}
	images := []image{{Name: "querdian", Image: "ghcr.io/querdian-dev/querdian:1.2.3-rc.1"}}
	out, err := annotate(chart, "1.2.3-rc.1", changes, images)
	if err != nil {
		t.Fatal(err)
	}
	// Running it twice gives the same result (re-run of a failed release job on a dirty tree).
	again, err := annotate(out, "1.2.3-rc.1", changes, images)
	if err != nil || string(again) != string(out) {
		t.Fatalf("not idempotent (err %v):\n%s\n---\n%s", err, out, again)
	}

	var before, after struct {
		Name        string            `json:"name"`
		Version     string            `json:"version"`
		Annotations map[string]string `json:"annotations"`
	}
	if err := yaml.Unmarshal(chart, &before); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(out, &after); err != nil {
		t.Fatalf("invalid output: %v\n%s", err, out)
	}
	if after.Name != before.Name || after.Version != before.Version {
		t.Errorf("chart fields changed: %+v", after)
	}
	for k, v := range before.Annotations {
		if after.Annotations[k] != v {
			t.Errorf("static annotation %s changed to %q", k, after.Annotations[k])
		}
	}
	if a := after.Annotations; a["artifacthub.io/containsSecurityUpdates"] != "true" || a["artifacthub.io/prerelease"] != "true" {
		t.Errorf("flags: %v", a)
	}
	var gotChanges []change
	if err := yaml.UnmarshalStrict([]byte(after.Annotations["artifacthub.io/changes"]), &gotChanges); err != nil || !reflect.DeepEqual(gotChanges, changes) {
		t.Errorf("changes = %+v (%v)", gotChanges, err)
	}
	var gotImages []image
	if err := yaml.UnmarshalStrict([]byte(after.Annotations["helm.sh/images"]), &gotImages); err != nil || !reflect.DeepEqual(gotImages, images) {
		t.Errorf("images = %+v (%v)", gotImages, err)
	}
	if !strings.Contains(string(out), "# The release pipeline replaces version") {
		t.Error("comments were dropped")
	}

	out, err = annotate(chart, "1.2.3", []change{{Kind: "added", Description: "x"}}, images)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(out, &after); err != nil {
		t.Fatal(err)
	}
	if a := after.Annotations; a["artifacthub.io/containsSecurityUpdates"] != "false" || a["artifacthub.io/prerelease"] != "false" {
		t.Errorf("flags of a release without security entries: %v", a)
	}
}

// The default -image must be the repository the chart deploys.
func TestImageMatchesChartValues(t *testing.T) {
	b, err := os.ReadFile("../../charts/querdian/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Image struct {
			Repository string `json:"repository"`
		} `json:"image"`
	}
	if err := yaml.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if defaultImage != v.Image.Repository {
		t.Errorf("-image default %q, values.yaml image.repository %q", defaultImage, v.Image.Repository)
	}
}
