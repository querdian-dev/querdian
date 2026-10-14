//go:build e2e

package prometheus

import (
	"strconv"
	"strings"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

// qcase is one query of the equivalence corpus. The placeholders are replaced per use:
//
//	{{S}}  selector without own matchers: "" for the user's query, "{term matchers}" for a per-term query
//	{{M}}  trailing matchers inside braces: "" resp. ",term matchers"
//
// so the same template yields the query the user sends and the per-term queries the test computes the
// expectation from, independent of the enforcer under test.
type qcase struct {
	q string
	// pointwise: every output series depends on a single input series only, so the result over the
	// union of terms equals the union of the per-term results (checked in addition to the reference).
	pointwise bool
	// valuesOnly: label sets of the result legitimately differ (absent(): Prometheus infers labels from
	// equality matchers of a plain selector, PLAN §7.2); compare the values only.
	valuesOnly bool
	// rejectMulti: documented to be rejected (HTTP 400, unsupported) under a multi-term policy.
	rejectMulti bool
	// instantOnly: the expression type is not valid for query_range (range vector result).
	instantOnly bool
}

var corpus = []qcase{
	{q: `up{{S}}`, pointwise: true},
	{q: `http_requests_total{{S}}`, pointwise: true},
	{q: `http_requests_total{code="500"{{M}}}`, pointwise: true},
	{q: `http_requests_total{code=~"2.."{{M}}}`, pointwise: true},
	{q: `{__name__=~"queue.*"{{M}}}`, pointwise: true},
	{q: `rate(http_requests_total{{S}}[5m])`, pointwise: true},
	{q: `increase(http_requests_total{code="200"{{M}}}[10m])`, pointwise: true},
	{q: `irate(http_requests_total{{S}}[2m])`, pointwise: true},
	{q: `delta(temperature{{S}}[10m])`, pointwise: true},
	{q: `deriv(temperature{{S}}[10m])`, pointwise: true},
	{q: `predict_linear(temperature{{S}}[10m], 300)`, pointwise: true},
	{q: `resets(resets_total{{S}}[30m])`, pointwise: true},
	{q: `changes(temperature{{S}}[10m])`, pointwise: true},
	{q: `max_over_time(temperature{{S}}[10m])`, pointwise: true},
	{q: `quantile_over_time(0.9, temperature{{S}}[10m])`, pointwise: true},
	{q: `http_requests_total{{S}} offset 5m`, pointwise: true},
	{q: `rate(http_requests_total{{S}}[5m] offset 5m)`, pointwise: true},
	{q: `sum_over_time(queue_depth{{S}}[5m] @ start())`, pointwise: true},
	{q: `avg_over_time(temperature{{S}}[10m:1m])`, pointwise: true},
	{q: `max_over_time(rate(http_requests_total{{S}}[2m])[10m:1m])`, pointwise: true},
	{q: `label_replace(up{{S}}, "svc", "$1", "job", "(.*)")`, pointwise: true},
	{q: `label_join(up{{S}}, "x", "-", "namespace", "job")`, pointwise: true},
	{q: `clamp_max(temperature{{S}}, 20)`, pointwise: true},
	{q: `round(temperature{{S}} / 3)`, pointwise: true},
	{q: `queue_depth{{S}} > bool 5`, pointwise: true},
	{q: `timestamp(up{{S}})`, pointwise: true},
	{q: `sort_desc(http_requests_total{{S}})`, pointwise: true},
	{q: `sum(http_requests_total{{S}})`},
	{q: `sum by (namespace) (rate(http_requests_total{{S}}[5m]))`},
	{q: `sum without (pod) (http_requests_total{{S}})`},
	{q: `sum by (job, code) (http_requests_total{{S}})`},
	{q: `avg(temperature{{S}})`},
	{q: `max by (env) (temperature{{S}})`},
	{q: `stddev(temperature{{S}})`},
	{q: `quantile(0.5, temperature{{S}})`},
	{q: `count(up{{S}})`},
	{q: `count by (namespace) (up{{S}} == 1)`},
	{q: `group by (namespace) (up{{S}})`},
	{q: `topk(2, http_requests_total{{S}})`},
	{q: `bottomk(1, temperature{{S}})`},
	{q: `count_values("v", queue_depth{{S}})`},
	{q: `count({__name__=~".+"{{M}}})`},
	{q: `count by (__name__) ({__name__=~".+"{{M}}})`},
	{q: `scalar(sum(queue_depth{{S}}))`},
	{q: `histogram_quantile(0.9, sum by (le) (rate(request_duration_seconds_bucket{{S}}[5m])))`},
	{q: `histogram_quantile(0.5, sum by (le, namespace) (rate(request_duration_seconds_bucket{{S}}[5m])))`},
	{q: `rate(request_duration_seconds_sum{{S}}[5m]) / rate(request_duration_seconds_count{{S}}[5m])`},
	{q: `http_requests_total{code="200"{{M}}} / ignoring(code) http_requests_total{code="500"{{M}}}`},
	{q: `http_requests_total{code="200"{{M}}} + on(namespace, job) http_requests_total{code="500"{{M}}}`},
	{q: `queue_depth{{S}} / on(namespace) group_left sum by (namespace) (queue_depth{{S}})`},
	{q: `queue_depth{{S}} and on(namespace) up{job="api"{{M}}}`},
	{q: `queue_depth{{S}} unless on(namespace, job) up{job="api"{{M}}}`},
	{q: `queue_depth{{S}} or vector(0)`},
	{q: `absent(up{job="api"{{M}}})`, valuesOnly: true},
	{q: `absent(nonexistent_metric{job="x"{{M}}})`, valuesOnly: true},
	{q: `absent_over_time(up{job="api"{{M}}}[5m])`, valuesOnly: true},
	{q: `last_over_time(up{{S}}[5m])`, pointwise: true},
	{q: `count_over_time(queue_depth{{S}}[5m])`, pointwise: true},
	{q: `abs(delta(temperature{{S}}[5m]))`, pointwise: true},
	{q: `http_requests_total{{S}} @ end()`, pointwise: true},
	{q: `topk by (namespace) (1, http_requests_total{{S}})`},
	{q: `quantile by (job) (0.5, queue_depth{{S}})`},
	{q: `count(count by (job) (up{{S}}))`},
	{q: `queue_depth{{S}} > scalar(avg(queue_depth{{S}}))`},
	{q: `sum(rate(http_requests_total{{S}}[5m])) / scalar(count(up{{S}}))`},
	{q: `up{{S}} * on(namespace) group_left(job) queue_depth{job="api"{{M}}}`},
	{q: `max by (namespace) (max_over_time(rate(http_requests_total{{S}}[1m])[5m:30s]))`},
	{q: `label_replace(sum by (namespace) (up{{S}}), "ns", "$1", "namespace", "(.*)")`},
	{q: `avg_over_time(temperature{{S}}[10m]) > avg(temperature{{S}})`},
	{q: `vector(1)`},
	{q: `time()`},
	{q: `http_requests_total{{S}}[5m]`, rejectMulti: true, instantOnly: true},
	{q: `quantile_over_time(scalar(sum(queue_depth{{S}})), temperature{{S}}[10m])`, rejectMulti: true},
}

// termSet is a named set of policy terms (one ClusterRule each).
type termSet struct {
	name  string
	group string
	terms [][]v1.Matcher
}

func eq(n, v string) v1.Matcher  { return v1.Matcher{Name: n, Op: v1.MatchEqual, Value: v} }
func ne(n, v string) v1.Matcher  { return v1.Matcher{Name: n, Op: v1.MatchNotEqual, Value: v} }
func re(n, v string) v1.Matcher  { return v1.Matcher{Name: n, Op: v1.MatchRegexMatch, Value: v} }
func nre(n, v string) v1.Matcher { return v1.Matcher{Name: n, Op: v1.MatchRegexNoMatch, Value: v} }

var termSets = []termSet{
	{name: "single", terms: [][]v1.Matcher{{eq("namespace", "ns-a")}}},
	{name: "two-namespaces", terms: [][]v1.Matcher{{eq("namespace", "ns-a")}, {eq("namespace", "ns-b")}}},
	{name: "overlapping", terms: [][]v1.Matcher{{eq("namespace", "ns-a")}, {eq("job", "api")}}},
	{name: "three-terms", terms: [][]v1.Matcher{
		{eq("namespace", "ns-b"), eq("job", "worker")},
		{re("namespace", "ns-a|ns-c"), eq("code", "500")},
		{eq("namespace", "ns-c"), eq("env", "prod")},
	}},
	{name: "negative", terms: [][]v1.Matcher{{ne("namespace", "ns-c")}, {eq("__name__", "cluster_info")}}},
	{name: "name-single", terms: [][]v1.Matcher{{re("__name__", "up|queue_depth|temperature")}}},
	{name: "mixed-ops", terms: [][]v1.Matcher{{re("namespace", "ns-.*"), nre("job", "worker")}, {re("pod", "worker-ns-c.*")}}},
}

func init() {
	for i := range termSets {
		termSets[i].group = "ts-" + strconv.Itoa(i)
	}
}

// matcherText renders a policy matcher as PromQL.
func matcherText(m v1.Matcher) string {
	op := map[v1.MatchOp]string{v1.MatchEqual: "=", v1.MatchNotEqual: "!=", v1.MatchRegexMatch: "=~", v1.MatchRegexNoMatch: "!~"}[m.Op]
	return m.Name + op + strconv.Quote(m.Value)
}

func termText(term []v1.Matcher) string {
	parts := make([]string, len(term))
	for i, m := range term {
		parts[i] = matcherText(m)
	}
	return strings.Join(parts, ",")
}

// userForm is the query as the user sends it.
func (c qcase) userForm() string {
	return strings.NewReplacer("{{S}}", "", "{{M}}", "").Replace(c.q)
}

// termForm is the query restricted to one term, written by the test itself (no enforcer involved).
func (c qcase) termForm(term []v1.Matcher) string {
	txt := termText(term)
	return strings.NewReplacer("{{S}}", "{"+txt+"}", "{{M}}", ","+txt).Replace(c.q)
}
