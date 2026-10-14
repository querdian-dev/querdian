//go:build e2e

package loki

import (
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// safeLimit returns a limit close to want for which the entry at the cut and the one after it have
// different timestamps, so that the top-L result is unambiguous (the order of entries with equal
// timestamps is unspecified). Returns len(all) when the data is shorter than want.
func safeLimit(all []flat, want int, forward bool) int {
	ts := make([]int64, len(all))
	for i, e := range all {
		ts[i] = e.ts
	}
	sort.Slice(ts, func(i, j int) bool { return (ts[i] < ts[j]) == forward })
	for l := want; l < len(ts); l++ {
		if ts[l-1] != ts[l] {
			return l
		}
	}
	return len(ts)
}

func dirName(forward bool) string {
	if forward {
		return "forward"
	}
	return "backward"
}

func skipDenied(set termSet, q string) bool { return len(set.denied) > 0 && strings.Contains(q, "pod") }

func testEquivalence(t *testing.T, f *fixture) {
	checks, empty, rejected := 0, 0, 0
	for _, set := range termSets {
		t.Run(set.name, func(t *testing.T) {
			multi := len(f.eff[set.name].Terms) > 1
			user, oracle, ren := f.as(set), f.oracle(set), set.rename()

			// Log queries: both directions, a large limit and limits cutting the result (top-L of the union).
			for _, c := range logCorpus {
				if skipDenied(set, c.q) {
					continue
				}
				for _, forward := range []bool{false, true} {
					full := oracle.queryRange(c.q, url.Values{"direction": {dirName(forward)}})
					if full.Status != 200 {
						t.Errorf("oracle rejected %s: %d %.200s", c.q, full.Status, full.Body)
						continue
					}
					all := entries(t, full, ren)
					limits := []int{5000}
					if len(all) > 8 {
						limits = append(limits, safeLimit(all, 7, forward), safeLimit(all, len(all)/2, forward))
					}
					for _, lim := range limits {
						extra := url.Values{"direction": {dirName(forward)}, "limit": {itoa(lim)}}
						r := user.queryRange(c.q, extra)
						if r.Status != 200 {
							t.Errorf("%s %s limit=%d: status %d (proxy-originated=%v): %.300s", dirName(forward), c.q, lim, r.Status, fromProxy(r), r.Body)
							continue
						}
						checks++
						want := normalise(t, oracle.queryRange(c.q, extra), ren)
						got := normalise(t, r, ren)
						if d := diff(want, got); d != "" {
							t.Errorf("%s %s limit=%d: differs from the query over the granted data\n  %s", dirName(forward), c.q, lim, d)
							continue
						}
						checkOrder(t, r, forward)
						if len(want) == 0 {
							empty++
						}
					}
				}
			}

			// Metric queries, instant and range.
			for _, c := range metricCorpus {
				if skipDenied(set, c.q) {
					continue
				}
				for _, mode := range []string{"instant", "range"} {
					if c.instantOnly && mode == "range" {
						continue
					}
					call := func(a api) *framework.Response {
						if mode == "range" {
							return a.queryRange(c.q, nil)
						}
						return a.instant(c.q)
					}
					r := call(user)
					if c.rejectMulti && multi {
						rejected++
						if r.Status != 400 || !fromProxy(r) {
							t.Errorf("[%s] %s: want proxy 400 (unsupported), got %d %.300s", mode, c.q, r.Status, r.Body)
						}
						continue
					}
					if r.Status != 200 {
						t.Errorf("[%s] %s: status %d (proxy-originated=%v): %.300s", mode, c.q, r.Status, fromProxy(r), r.Body)
						continue
					}
					checks++
					want, got := normalise(t, call(oracle), ren), normalise(t, r, ren)
					if c.valuesOnly {
						want, got = valuesOnly(want), valuesOnly(got)
					}
					if c.absent && multi {
						want, got = distinct(want), distinct(got)
					}
					if d := diff(want, got); d != "" {
						t.Errorf("[%s] %s: differs from the query over the granted data\n  %s", mode, c.q, d)
						continue
					}
					if len(want) == 0 {
						empty++
					}
				}
			}
		})
	}
	t.Logf("%d comparisons (%d log + %d metric queries x %d term sets), %d with an empty expected result, %d documented rejections",
		checks, len(logCorpus), len(metricCorpus), len(termSets), empty, rejected)
}
