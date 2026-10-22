package policy

import (
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

func TestSnapshotPoliciesAndSubjects(t *testing.T) {
	s := snap(t, []v1.ClusterAccessPolicy{
		capol("p", v1.Subjects{Groups: []string{"g1", "g2"}, Users: []string{"alice"}},
			v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{ns("a")}, Labels: &v1.LabelFilter{Deny: []string{"secret"}, Redact: []v1.RedactRule{{Name: "c", Strategy: v1.RedactHash}}}}),
		capol("bad", v1.Subjects{Groups: []string{"g3"}}, v1.ClusterRule{Backends: []string{"nope"}, Matchers: []v1.Matcher{ns("a")}}),
	}, nil, nil)

	pols := s.Policies()
	if len(pols) != 2 || pols[0].ID != "ClusterAccessPolicy/bad" || pols[1].ID != "ClusterAccessPolicy/p" {
		t.Fatalf("unexpected policies: %+v", pols)
	}
	if len(pols[0].Rules) != 0 || pols[0].Result.Ready {
		t.Fatalf("invalid policy must have no rules and not be ready: %+v", pols[0])
	}
	p := pols[1]
	if strings.Join(p.Groups, ",") != "g1,g2" || strings.Join(p.Users, ",") != "alice" {
		t.Fatalf("subjects: %+v", p)
	}
	if len(p.Rules) != 1 || p.Rules[0].Backend != "metrics" || strings.Join(p.Rules[0].DeniedLabels, ",") != "c,secret" ||
		p.Rules[0].Redactions["c"] != (Redaction{Strategy: v1.RedactHash}) {
		t.Fatalf("rules: %+v", p.Rules)
	}

	users, groups := s.Subjects()
	if strings.Join(users, ",") != "alice" || strings.Join(groups, ",") != "g1,g2" {
		t.Fatalf("subjects: %v %v", users, groups)
	}
}
