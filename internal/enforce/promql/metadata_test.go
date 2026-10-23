package promql

import (
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
)

var (
	seriesEP = routes.Endpoint{Template: "/api/v1/series", Param: "match[]"}
	labelsEP = routes.Endpoint{Template: "/api/v1/labels", Param: "match[]"}
	valuesEP = routes.Endpoint{Template: "/api/v1/label/:name/values", Param: "match[]", PathParam: "job"}
)

func metaPlan(t *testing.T, ep routes.Endpoint, params url.Values, eff policy.Effective) (enforce.Plan, error) {
	t.Helper()
	return New().Plan(t.Context(), ep, params, eff)
}

func three() policy.Effective {
	return terms(
		[]policy.Matcher{eq("namespace", "a")},
		[]policy.Matcher{eq("namespace", "b"), {Name: "env", Op: v1.MatchNotEqual, Value: "dev"}},
		[]policy.Matcher{{Name: "namespace", Op: v1.MatchRegexMatch, Value: "c|d"}, eq(policy.TenantIDLabel, "t2")},
	)
}

func one() policy.Effective { return nsTerm() }

func TestMatchExpand(t *testing.T) {
	tests := []struct {
		name  string
		user  []string
		eff   policy.Effective
		want  []string
		check bool
	}{
		{"no user match, 1 term", nil, one(), []string{`{namespace="team-a"}`}, true},
		{"one user match, 1 term", []string{`up{job="x"}`}, one(), []string{`up{job="x",namespace="team-a"}`}, true},
		{"two user matches, 1 term", []string{`up`, `{job="x"}`}, one(),
			[]string{`up{namespace="team-a"}`, `{job="x",namespace="team-a"}`}, true},
		{"no user match, 3 terms", nil, three(), []string{
			`{namespace="a"}`,
			`{env!="dev",namespace="b"}`,
			`{__tenant_id__="t2",namespace=~"c|d"}`}, true},
		{"one user match, 3 terms", []string{`up{job="x"}`}, three(), []string{
			`up{job="x",namespace="a"}`,
			`up{env!="dev",job="x",namespace="b"}`,
			`up{__tenant_id__="t2",job="x",namespace=~"c|d"}`}, true},
		{"two user matches, 3 terms", []string{`up`, `down{a!="b"}`}, three(), []string{
			`up{namespace="a"}`,
			`up{env!="dev",namespace="b"}`,
			`up{__tenant_id__="t2",namespace=~"c|d"}`,
			`down{a!="b",namespace="a"}`,
			`down{a!="b",env!="dev",namespace="b"}`,
			`down{__tenant_id__="t2",a!="b",namespace=~"c|d"}`}, true},
		{"user matcher on the policy label stays ANDed", []string{`up{namespace="other"}`}, one(),
			[]string{`up{namespace="other",namespace="team-a"}`}, true},
		{"policy name matcher", []string{`up`}, terms([]policy.Matcher{{Name: "__name__", Op: v1.MatchRegexMatch, Value: "up|down"}}, []policy.Matcher{eq("ns", "a")}),
			[]string{`{__name__="up",__name__=~"up|down"}`, `up{ns="a"}`}, true},
		{"term without non-empty matcher", nil, terms([]policy.Matcher{{Name: "env", Op: v1.MatchNotEqual, Value: "dev"}}, []policy.Matcher{eq("a", "b")}),
			[]string{`{__name__=~".+",env!="dev"}`, `{a="b"}`}, true},
		{"duplicate selectors collapse", []string{`up`, `up`}, one(), []string{`up{namespace="team-a"}`}, true},
	}
	for _, ep := range []routes.Endpoint{seriesEP, labelsEP, valuesEP} {
		for _, tt := range tests {
			t.Run(ep.Template+"/"+tt.name, func(t *testing.T) {
				params := url.Values{"start": {"1"}, "end": {"2"}}
				if tt.user != nil {
					params["match[]"] = tt.user
				}
				p, err := metaPlan(t, ep, params, tt.eff)
				if err != nil {
					t.Fatal(err)
				}
				if p.Strategy != "match_expand" || p.Merge != enforce.MergeNone || len(p.Requests) != 1 {
					t.Fatalf("plan %+v", p)
				}
				got := p.Requests[0].Query["match[]"]
				// Compare as matcher sets: "up{a=1}" and `{__name__="up",a="1"}` are the same selector.
				if !slices.EqualFunc(got, tt.want, func(a, b string) bool { return canon(t, a) == canon(t, b) }) {
					t.Errorf("got  %q\nwant %q", got, tt.want)
				}
				for _, s := range got {
					if _, err := promParser.ParseMetricSelector(s); err != nil {
						t.Errorf("%s does not parse: %v", s, err)
					}
				}
				if p.Requests[0].Query.Get("start") != "1" || p.Requests[0].Query.Get("end") != "2" {
					t.Error("other parameters not copied")
				}
				if params.Get("match[]") != "" && !slices.Equal(params["match[]"], tt.user) {
					t.Error("input mutated")
				}
			})
		}
	}
}

func TestMatchExpandEveryExpansionRestricted(t *testing.T) {
	// Property: for any user selectors and terms, every emitted selector carries all matchers of
	// exactly one term, and the count is users x terms.
	eff := three()
	user := []string{`a`, `b{x="1"}`, `{y=~"z.*"}`}
	p, err := metaPlan(t, seriesEP, url.Values{"match[]": user}, eff)
	if err != nil {
		t.Fatal(err)
	}
	got := p.Requests[0].Query["match[]"]
	if len(got) != len(user)*len(eff.Terms) {
		t.Fatalf("got %d selectors", len(got))
	}
	for i, s := range got {
		ms, err := promParser.ParseMetricSelector(s)
		if err != nil {
			t.Fatal(err)
		}
		term := eff.Terms[i%len(eff.Terms)]
		for _, m := range term.Matchers {
			ok := false
			for _, lm := range ms {
				ok = ok || (lm.Name == m.Name && lm.Value == m.Value && lm.Type == matchType(m.Op))
			}
			if !ok {
				t.Errorf("%s lacks %s", s, m.Name)
			}
		}
	}
}

func TestMatchExpandUnrestrictedAndEmptyTerm(t *testing.T) {
	for name, eff := range map[string]policy.Effective{
		"unrestricted":      {Unrestricted: true},
		"empty term":        {Terms: []policy.Term{{}}},
		"empty among terms": terms([]policy.Matcher{eq("a", "b")}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			p, err := metaPlan(t, seriesEP, url.Values{"match[]": {`up`, `down{a="b"}`}}, eff)
			if err != nil {
				t.Fatal(err)
			}
			if got := p.Requests[0].Query["match[]"]; !slices.Equal(got, []string{`up`, `down{a="b"}`}) {
				t.Errorf("got %q", got)
			}
			// No user match[]: nothing needed, nothing sent.
			p, err = metaPlan(t, labelsEP, url.Values{}, eff)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := p.Requests[0].Query["match[]"]; ok {
				t.Error("match[] invented for an unrestricted policy")
			}
		})
	}
}

func TestMatchExpandErrors(t *testing.T) {
	denied := one()
	denied.DeniedLabels = []string{"customer_id", "secret"}
	valuesOf := func(name string) routes.Endpoint {
		return routes.Endpoint{Template: "/api/v1/label/:name/values", Param: "match[]", PathParam: name}
	}
	tests := []struct {
		name   string
		ep     routes.Endpoint
		params url.Values
		eff    policy.Effective
		want   *enforce.Error
	}{
		{"parse error", seriesEP, url.Values{"match[]": {`up{`}}, one(), enforce.ErrParse},
		{"expression not selector", seriesEP, url.Values{"match[]": {`rate(up[5m])`}}, one(), enforce.ErrParse},
		{"no terms", seriesEP, url.Values{"match[]": {`up`}}, policy.Effective{}, enforce.ErrUnsupported},
		{"denied in user match", seriesEP, url.Values{"match[]": {`up{secret="x"}`}}, denied, enforce.ErrDeniedLabel},
		{"denied in second match", labelsEP, url.Values{"match[]": {`up`, `{secret=~".+"}`}}, denied, enforce.ErrDeniedLabel},
		{"denied label values", valuesOf("secret"), url.Values{}, denied, enforce.ErrDeniedLabel},
		// Prometheus 3 decodes U__ names on label/:name/values (U__customer__id is customer_id).
		{"denied label values, U__ escaped", valuesOf("U__secret"), url.Values{}, denied, enforce.ErrDeniedLabel},
		{"denied label values, U__ escaped underscore", valuesOf("U__customer__id"), url.Values{}, denied, enforce.ErrDeniedLabel},
		{"too many", seriesEP, url.Values{"match[]": manySelectors(100)}, three(), enforce.ErrTooComplex},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := metaPlan(t, tt.ep, tt.params, tt.eff); !errors.Is(err, tt.want) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
		})
	}
	// Non-denied lookalikes pass.
	if _, err := metaPlan(t, labelsEP, url.Values{"match[]": {`up{secrets="x"}`}}, denied); err != nil {
		t.Error(err)
	}
	for _, name := range []string{"U__job", "U__secret_2e_x", "U__customer_id"} {
		if _, err := metaPlan(t, valuesOf(name), url.Values{}, denied); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// canon returns the sorted matchers of a selector as one string.
func canon(t *testing.T, sel string) string {
	t.Helper()
	ms, err := promParser.ParseMetricSelector(sel)
	if err != nil {
		t.Fatalf("%s: %v", sel, err)
	}
	var parts []string
	for _, m := range ms {
		parts = append(parts, m.String())
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

func manySelectors(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = `m` + strings.Repeat("x", i)
	}
	return out
}

func TestSeriesResponseFilterWiring(t *testing.T) {
	eff := one()
	eff.DeniedLabels = []string{"secret"}
	p, err := metaPlan(t, seriesEP, url.Values{}, eff)
	if err != nil {
		t.Fatal(err)
	}
	if p.ResponseFilter == nil || p.ResponseFilterMaxBytes != MaxLabelsResponseBytes {
		t.Fatalf("series filter not wired: %v %d", p.ResponseFilter != nil, p.ResponseFilterMaxBytes)
	}
	// Without denied labels the series plan stays a plain stream.
	if q, _ := metaPlan(t, seriesEP, url.Values{}, one()); q.ResponseFilter != nil {
		t.Error("filter set without denied labels")
	}
	// Query results are deliberately not filtered.
	if q, _ := plan(`up`, eff); q.ResponseFilter != nil {
		t.Error("query plans must not filter results")
	}
}

func TestFilterSeriesResponse(t *testing.T) {
	tests := []struct {
		name, in, want string
		denied         []string
		wantErr        bool
	}{
		{"removes denied labels", `{"status":"success","data":[{"__name__":"up","job":"a","secret":"s1"},{"__name__":"up","job":"b"}]}`,
			`{"data":[{"__name__":"up","job":"a"},{"__name__":"up","job":"b"}],"status":"success"}`, []string{"secret"}, false},
		{"duplicates collapse", `{"status":"success","data":[{"job":"a","secret":"1"},{"job":"a","secret":"2"},{"job":"b","secret":"1"}]}`,
			`{"data":[{"job":"a"},{"job":"b"}],"status":"success"}`, []string{"secret"}, false},
		{"several denied", `{"status":"success","data":[{"a":"1","b":"2","c":"3"}]}`,
			`{"data":[{"c":"3"}],"status":"success"}`, []string{"a", "b"}, false},
		{"all labels denied leaves an empty set", `{"status":"success","data":[{"a":"1"}]}`,
			`{"data":[{}],"status":"success"}`, []string{"a"}, false},
		{"empty data", `{"status":"success","data":[]}`, `{"data":[],"status":"success"}`, []string{"a"}, false},
		{"other members preserved", `{"status":"success","data":[{"a":"1"}],"warnings":["w"]}`,
			`{"data":[{}],"status":"success","warnings":["w"]}`, []string{"a"}, false},
		{"html is not escaped", `{"status":"success","data":[{"a":"<b>"}]}`, `{"data":[{"a":"<b>"}],"status":"success"}`, []string{"x"}, false},
		{"upstream error body untouched", `{"status":"error","errorType":"bad_data","error":"x"}`,
			`{"status":"error","errorType":"bad_data","error":"x"}`, []string{"a"}, false},
		{"success without data fails", `{"status":"success"}`, ``, []string{"a"}, true},
		{"data not label sets", `{"status":"success","data":["a"]}`, ``, []string{"a"}, true},
		{"not json", `nope`, ``, []string{"a"}, true},
		{"not an object", `[1]`, ``, []string{"a"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FilterSeriesResponse([]byte(tt.in), tt.denied)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %s", got)
				}
				return
			}
			if err != nil || string(got) != tt.want {
				t.Fatalf("got %s (%v), want %s", got, err, tt.want)
			}
		})
	}
	if _, err := FilterSeriesResponse(make([]byte, MaxLabelsResponseBytes+1), []string{"a"}); err == nil {
		t.Error("oversized body must fail")
	}
}

func TestLabelsResponseFilterWiring(t *testing.T) {
	eff := one()
	eff.DeniedLabels = []string{"secret", "pii"}
	for _, tt := range []struct {
		ep   routes.Endpoint
		want bool
	}{{labelsEP, true}, {valuesEP, false}} {
		p, err := metaPlan(t, tt.ep, url.Values{}, eff)
		if err != nil {
			t.Fatal(err)
		}
		if (p.ResponseFilter != nil) != tt.want {
			t.Errorf("%s: ResponseFilter set = %v", tt.ep.Template, p.ResponseFilter != nil)
		}
		if tt.want {
			if p.ResponseFilterMaxBytes != MaxLabelsResponseBytes {
				t.Errorf("cap %d", p.ResponseFilterMaxBytes)
			}
			out, err := p.ResponseFilter([]byte(`{"status":"success","data":["a","secret","b"]}`))
			if err != nil || string(out) != `{"data":["a","b"],"status":"success"}` {
				t.Errorf("out %s err %v", out, err)
			}
		}
	}
	// No denied labels: no filter, and the plan equals the unfiltered one.
	p, _ := metaPlan(t, labelsEP, url.Values{}, one())
	if p.ResponseFilter != nil || p.ResponseFilterMaxBytes != 0 {
		t.Error("filter set without denied labels")
	}
	// Other enforcers' plans keep the zero value.
	q, _ := plan(`up`, one())
	if q.ResponseFilter != nil {
		t.Error("query plan has a response filter")
	}
}

func TestFilterLabelsResponse(t *testing.T) {
	tests := []struct {
		name, in, want string
		denied         []string
		err            bool
	}{
		{"removes denied", `{"status":"success","data":["__name__","job","secret"]}`, `{"data":["__name__","job"],"status":"success"}`, []string{"secret"}, false},
		{"keeps all", `{"status":"success","data":["a","b"]}`, `{"data":["a","b"],"status":"success"}`, []string{"x"}, false},
		{"all removed gives empty array", `{"status":"success","data":["a"]}`, `{"data":[],"status":"success"}`, []string{"a"}, false},
		{"empty data", `{"status":"success","data":[]}`, `{"data":[],"status":"success"}`, []string{"a"}, false},
		{"preserves envelope", `{"status":"success","data":["a","b"],"warnings":["w<>&"],"infos":["i"]}`, `{"data":["b"],"infos":["i"],"status":"success","warnings":["w<>&"]}`, []string{"a"}, false},
		{"no denied", `{"status":"success","data":["a"]}`, `{"data":["a"],"status":"success"}`, nil, false},
		{"upstream error unchanged", `{"status":"error","errorType":"bad_data","error":"x"}`, `{"status":"error","errorType":"bad_data","error":"x"}`, []string{"a"}, false},
		{"success without data", `{"status":"success"}`, ``, []string{"a"}, true},
		{"not json", `<html>`, ``, []string{"a"}, true},
		{"not an object", `["a"]`, ``, []string{"a"}, true},
		{"data not strings", `{"status":"success","data":[1,2]}`, ``, []string{"a"}, true},
		{"data not array", `{"status":"success","data":{"a":1}}`, ``, []string{"a"}, true},
		{"empty body", ``, ``, []string{"a"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FilterLabelsResponse([]byte(tt.in), tt.denied)
			if (err != nil) != tt.err {
				t.Fatalf("err = %v", err)
			}
			if err == nil && string(got) != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestFilterLabelsResponseCap(t *testing.T) {
	big := make([]byte, MaxLabelsResponseBytes+1)
	if _, err := FilterLabelsResponse(big, []string{"a"}); err == nil {
		t.Error("oversized body accepted")
	}
	// A body at the cap is still processed.
	prefix := `{"status":"success","data":["`
	suffix := `"]}`
	name := strings.Repeat("n", MaxLabelsResponseBytes-len(prefix)-len(suffix))
	out, err := FilterLabelsResponse([]byte(prefix+name+suffix), []string{"a"})
	if err != nil || len(out) == 0 {
		t.Errorf("body at the cap: %v", err)
	}
}

func FuzzFilterLabelsResponse(f *testing.F) {
	f.Add(`{"status":"success","data":["a","secret"]}`)
	f.Add(`{"data":null}`)
	f.Add(`[]`)
	f.Fuzz(func(t *testing.T, in string) {
		out, err := FilterLabelsResponse([]byte(in), []string{"secret"})
		if err != nil {
			return
		}
		if strings.Contains(string(out), `"secret"`) && strings.Contains(in, `"data"`) {
			// The name may legitimately appear outside "data" (e.g. in warnings); check data only.
			var env struct {
				Data []string `json:"data"`
			}
			if json.Unmarshal(out, &env) == nil && slices.Contains(env.Data, "secret") {
				t.Fatalf("denied label survived: %s", out)
			}
		}
	})
}
