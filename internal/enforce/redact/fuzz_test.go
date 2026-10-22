package redact

import (
	"encoding/json"
	"testing"

	"github.com/querdian-dev/querdian/internal/policy"
)

// FuzzRewriters feeds arbitrary upstream bodies to every rewriter: none may panic, and a successful
// rewrite must be valid JSON.
func FuzzRewriters(f *testing.F) {
	for _, s := range []string{
		`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"c":"1"},"values":[[1,"1"]]}]}}`,
		`{"status":"success","data":{"resultType":"streams","result":[{"stream":{"c":"1"},"values":[["1","l",{"parsed":{"c":"2"}}]]}]}}`,
		`{"status":"success","data":[{"seriesLabels":{"c":"1"},"exemplars":[{"labels":{"c":"1"}}]}]}`,
		`{"streams":[{"stream":{"c":"1"},"values":[["1","l"]]}],"dropped_entries":[{"labels":{"c":"1"}}]}`,
		`{"traces":[{"spanSets":[{"spans":[{"attributes":[{"key":"c","value":{"intValue":"1"}}]}]}]}]}`,
		`{"status":"error","error":"x"}`,
	} {
		f.Add([]byte(s))
	}
	rules := map[string]policy.Redaction{
		"c": placeholder, "span.c": hash, "ip": ip2, "rm": remove,
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		r := New(rules)
		for name, fn := range map[string]func([]byte) ([]byte, error){
			"query": r.Query, "exemplars": r.Exemplars, "tail": r.TailFrame, "tempo": r.Tempo,
		} {
			out, err := fn(body)
			if err == nil && !json.Valid(out) {
				t.Fatalf("%s produced invalid JSON for %q: %q", name, body, out)
			}
		}
	})
}
