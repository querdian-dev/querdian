package logql

import (
	"context"
	"errors"
	"net/url"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
)

func tailEff(terms ...[]policy.Matcher) policy.Effective {
	var e policy.Effective
	for _, ms := range terms {
		e.Terms = append(e.Terms, policy.Term{Matchers: ms})
	}
	return e
}

func tailMatch(v string) []policy.Matcher {
	return []policy.Matcher{{Name: "ns", Op: v1.MatchEqual, Value: v}}
}

func TestPlanTail(t *testing.T) {
	ctx := context.Background()
	params := url.Values{"query": {`{app="x"}`}, "limit": {"5"}, "delay_for": {"2"}, "start": {"123"}}

	p, err := PlanTail(ctx, params, tailEff(tailMatch("a")))
	if err != nil {
		t.Fatal(err)
	}
	if p.Merge != enforce.MergeTailStreams || p.Strategy != "inject" || len(p.Requests) != 1 ||
		p.Requests[0].Query.Get("query") != `{app="x", ns="a"}` || p.Requests[0].Query.Get("start") != "123" {
		t.Errorf("single term: %+v", p)
	}

	p, err = PlanTail(ctx, params, tailEff(tailMatch("a"), tailMatch("b")))
	if err != nil {
		t.Fatal(err)
	}
	if p.Merge != enforce.MergeTailStreams || p.Strategy != "fanout" || len(p.Requests) != 2 ||
		p.Requests[1].Term != 1 || p.Requests[1].Query.Get("query") != `{app="x", ns="b"}` {
		t.Errorf("two terms: %+v", p)
	}

	p, err = PlanTail(ctx, params, policy.Effective{Unrestricted: true})
	if err != nil || p.Strategy != "passthrough" || p.Merge != enforce.MergeTailStreams {
		t.Errorf("unrestricted: %+v %v", p, err)
	}
}

func TestPlanTailRejects(t *testing.T) {
	ctx := context.Background()
	eff := tailEff(tailMatch("a"))
	cases := []struct {
		name   string
		params url.Values
		eff    policy.Effective
		kind   error
	}{
		{"missing query", url.Values{}, eff, enforce.ErrParse},
		{"bad query", url.Values{"query": {`{app=`}}, eff, enforce.ErrParse},
		{"metric", url.Values{"query": {`rate({app="x"}[1m])`}}, eff, enforce.ErrUnsupported},
		{"unknown param", url.Values{"query": {`{app="x"}`}, "direction": {"forward"}}, eff, enforce.ErrUnsupported},
		{"bad limit", url.Values{"query": {`{app="x"}`}, "limit": {"0"}}, eff, enforce.ErrParse},
		{"bad delay_for", url.Values{"query": {`{app="x"}`}, "delay_for": {"6"}}, eff, enforce.ErrParse},
		{"denied label", url.Values{"query": {`{app="x"} | secret="1"`}},
			policy.Effective{Terms: eff.Terms, DeniedLabels: []string{"secret"}}, enforce.ErrDeniedLabel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := PlanTail(ctx, c.params, c.eff)
			if !errors.Is(err, c.kind) {
				t.Fatalf("err = %v, want %v", err, c.kind)
			}
		})
	}
}
