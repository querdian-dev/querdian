//go:build envtest

package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

var k8s client.Client

func TestMain(m *testing.M) {
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	if err := v1.AddToScheme(scheme.Scheme); err != nil {
		panic(err)
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme.Scheme}); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func caps(name string, subj v1.Subjects, rules ...v1.ClusterRule) *v1.ClusterAccessPolicy {
	return &v1.ClusterAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{GenerateName: name + "-"},
		Spec:       v1.ClusterAccessPolicySpec{Subjects: subj, Rules: rules},
	}
}

func eq(n, v string) v1.Matcher { return v1.Matcher{Name: n, Op: v1.MatchEqual, Value: v} }

func TestClusterAccessPolicyValidation(t *testing.T) {
	grp := v1.Subjects{Groups: []string{"g"}, Users: []string{}}
	tests := []struct {
		name    string
		obj     *v1.ClusterAccessPolicy
		wantErr string // empty = accepted
	}{
		{"unrestricted", caps("a", grp, v1.ClusterRule{Backends: []string{"metrics"}, Tenants: []string{"t1"}, Unrestricted: true}), ""},
		{"matchers", caps("b", grp, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "namespace", Op: v1.MatchRegexMatch, Value: "a|b"}}, Labels: &v1.LabelFilter{Deny: []string{"customer_id"}}}), ""},
		{"empty subjects", caps("c", v1.Subjects{Groups: []string{}, Users: []string{}}, v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true}), "at least one group or user required"},
		{"unrestricted with matchers", caps("d", grp, v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true, Matchers: []v1.Matcher{eq("a", "b")}}), "set either unrestricted or matchers"},
		{"neither", caps("e", grp, v1.ClusterRule{Backends: []string{"metrics"}}), "set either unrestricted or matchers"},
		{"invalid op", caps("f", grp, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "a", Op: "Like", Value: "b"}}}), "Unsupported value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { check(t, k8s.Create(context.Background(), tc.obj), tc.wantErr) })
	}
}

func TestAccessPolicyValidation(t *testing.T) {
	mk := func(subj v1.Subjects, rules ...v1.Rule) *v1.AccessPolicy {
		return &v1.AccessPolicy{ObjectMeta: metav1.ObjectMeta{GenerateName: "ap-", Namespace: "default"}, Spec: v1.AccessPolicySpec{Subjects: subj, Rules: rules}}
	}
	// PLAN §4.5 example.
	ok := mk(v1.Subjects{Groups: []string{"order-service-team"}, Users: []string{"alice@example.com"}},
		v1.Rule{Backends: []string{"metrics", "logs"}, Matchers: []v1.Matcher{{Name: "service", Op: v1.MatchNotEqual, Value: ""}}},
		v1.Rule{Backends: []string{"traces"}})
	check(t, k8s.Create(context.Background(), ok), "")
	check(t, k8s.Create(context.Background(), mk(v1.Subjects{Groups: []string{}, Users: []string{}}, v1.Rule{Backends: []string{"metrics"}})), "at least one group or user required")
	check(t, k8s.Create(context.Background(), mk(v1.Subjects{Users: []string{"u"}})), "spec.rules")
}

func check(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("expected accepted, got %v", err)
	case want != "" && err == nil:
		t.Fatalf("expected rejection containing %q, got none", want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Fatalf("expected %q in error, got %v", want, err)
	}
}
