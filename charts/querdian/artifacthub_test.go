package chart

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/querdian-dev/querdian/api/v1alpha1"
)

// chartAnnotations returns the annotations of Chart.yaml.
func chartAnnotations(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c.Annotations
}

func TestArtifactHubAnnotations(t *testing.T) {
	a := chartAnnotations(t)
	// https://artifacthub.io/docs/topics/annotations/helm/
	if !slices.Contains([]string{"ai-machine-learning", "database", "integration-delivery", "monitoring-logging",
		"networking", "security", "storage", "streaming-messaging"}, a["artifacthub.io/category"]) {
		t.Errorf("artifacthub.io/category %q is not an Artifact Hub category", a["artifacthub.io/category"])
	}
	if a["artifacthub.io/license"] != "Apache-2.0" {
		t.Errorf("artifacthub.io/license = %q", a["artifacthub.io/license"])
	}
	var links []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := yaml.UnmarshalStrict([]byte(a["artifacthub.io/links"]), &links); err != nil || len(links) == 0 {
		t.Errorf("artifacthub.io/links: %v (%d links)", err, len(links))
	}
	for _, l := range links {
		if l.Name == "" || !strings.HasPrefix(l.URL, "https://") {
			t.Errorf("bad link %+v", l)
		}
	}
	// Set per release by hack/chartmeta; a static value would be wrong for every other version.
	for _, k := range []string{"artifacthub.io/changes", "artifacthub.io/containsSecurityUpdates",
		"artifacthub.io/prerelease", "helm.sh/images"} {
		if _, ok := a[k]; ok {
			t.Errorf("%s must not be set in the checked-in Chart.yaml", k)
		}
	}
}

// artifacthub.io/crds lists exactly the CRDs in crds/, and every CRD has an example in
// artifacthub.io/crdsExamples that decodes strictly into the API types.
func TestArtifactHubCRDs(t *testing.T) {
	a := chartAnnotations(t)
	type entry struct {
		Kind        string `json:"kind"`
		Version     string `json:"version"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Description string `json:"description"`
	}
	var listed []entry
	if err := yaml.UnmarshalStrict([]byte(a["artifacthub.io/crds"]), &listed); err != nil {
		t.Fatalf("artifacthub.io/crds: %v", err)
	}

	files, _ := filepath.Glob("crds/*.yaml")
	var want []entry
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // test fixture path
		if err != nil {
			t.Fatal(err)
		}
		var crd struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Names struct {
					Kind string `json:"kind"`
				} `json:"names"`
				Versions []struct {
					Name   string `json:"name"`
					Served bool   `json:"served"`
				} `json:"versions"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(b, &crd); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, v := range crd.Spec.Versions {
			if v.Served {
				want = append(want, entry{Kind: crd.Spec.Names.Kind, Version: v.Name, Name: crd.Metadata.Name})
			}
		}
	}
	if len(listed) != len(want) {
		t.Errorf("artifacthub.io/crds lists %d CRD versions, crds/ serves %d", len(listed), len(want))
	}
	for _, w := range want {
		i := slices.IndexFunc(listed, func(e entry) bool { return e.Kind == w.Kind && e.Version == w.Version && e.Name == w.Name })
		if i < 0 {
			t.Errorf("artifacthub.io/crds lacks %+v", w)
		} else if listed[i].DisplayName == "" || listed[i].Description == "" {
			t.Errorf("artifacthub.io/crds entry %s has no displayName or description", w.Kind)
		}
	}

	var examples []map[string]any
	if err := yaml.Unmarshal([]byte(a["artifacthub.io/crdsExamples"]), &examples); err != nil {
		t.Fatalf("artifacthub.io/crdsExamples: %v", err)
	}
	seen := map[string]bool{}
	for _, ex := range examples {
		b, err := yaml.Marshal(ex)
		if err != nil {
			t.Fatal(err)
		}
		var obj any
		switch ex["kind"] {
		case "ClusterAccessPolicy":
			obj = &v1alpha1.ClusterAccessPolicy{}
		case "AccessPolicy":
			obj = &v1alpha1.AccessPolicy{}
		default:
			t.Errorf("example of unknown kind %v", ex["kind"])
			continue
		}
		if ex["apiVersion"] != v1alpha1.GroupVersion.String() {
			t.Errorf("example %v has apiVersion %v", ex["kind"], ex["apiVersion"])
		}
		if err := yaml.UnmarshalStrict(b, obj); err != nil {
			t.Errorf("example %v does not match the API types: %v", ex["kind"], err)
		}
		seen[ex["kind"].(string)] = true
	}
	for _, w := range want {
		if !seen[w.Kind] {
			t.Errorf("artifacthub.io/crdsExamples has no %s", w.Kind)
		}
	}
}

// The repository metadata file pushed by the release workflow (tag artifacthub.io) only uses
// the fields Artifact Hub knows.
func TestArtifactHubRepositoryMetadata(t *testing.T) {
	b, err := os.ReadFile("../artifacthub-repo.yml")
	if err != nil {
		t.Fatal(err)
	}
	var md struct {
		RepositoryID string `json:"repositoryID"`
		Owners       []struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"owners"`
		Ignore []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"ignore"`
	}
	if err := yaml.UnmarshalStrict(b, &md); err != nil {
		t.Fatal(err)
	}
	if len(md.Owners) == 0 {
		t.Error("no owners: nobody can claim the repository")
	}
	for _, o := range md.Owners {
		if o.Name == "" || !strings.Contains(o.Email, "@") {
			t.Errorf("bad owner %+v", o)
		}
	}
}
