package logql

import (
	"net/url"
	"slices"
	"strings"

	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
)

// Route templates of the summed endpoints.
const (
	tmplIndexStats       = "/loki/api/v1/index/stats"
	tmplIndexVolume      = "/loki/api/v1/index/volume"
	tmplIndexVolumeRange = "/loki/api/v1/index/volume_range"
	tmplPatterns         = "/loki/api/v1/patterns"
)

// planSum handles index/stats, index/volume, index/volume_range and patterns. Like the string-set endpoints the
// log selector in "query" is optional here (an omitted one is replaced by the policy selector) and each policy
// term gets its own request; the responses are combined with MergeSumCounts. The merge is exact when
// Effective.Disjoint holds, otherwise it is an estimate and the server marks it (fanout.NewSumCountsMerger).
//
// targetLabels (index/volume*) names labels and is checked against the denied labels. With denied labels the
// merge is used even for one request, because it removes denied labels from the reported series.
func planSum(params url.Values, eff policy.Effective) (enforce.Plan, error) {
	if v := params.Get("targetLabels"); v != "" {
		for _, l := range strings.Split(v, ",") {
			l = strings.TrimSpace(l)
			if slices.Contains(eff.DeniedLabels, l) {
				return enforce.Plan{}, enforce.Errorf(enforce.ErrDeniedLabel, "label %s denied by policy", l)
			}
		}
	}
	branches, strategy, err := optionalQueryBranches(params, "query", eff)
	if err != nil {
		return enforce.Plan{}, err
	}
	return branchPlan(params, "query", branches, strategy, enforce.MergeSumCounts, len(eff.DeniedLabels) > 0), nil
}
