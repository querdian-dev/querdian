package logql

import (
	"net/url"
	"testing"

	"github.com/querdian-dev/querdian/internal/policy"
)

func TestSeriesResponseFilterWiring(t *testing.T) {
	params := url.Values{"match[]": {`{app="a"}`}}
	denied := policy.Effective{Unrestricted: true, DeniedLabels: []string{"secret"}}
	p, err := planSeries(params, denied)
	if err != nil {
		t.Fatal(err)
	}
	if p.ResponseFilter == nil || p.ResponseFilterMaxBytes <= 0 {
		t.Fatal("series with denied labels must carry a response filter")
	}
	out, err := p.ResponseFilter([]byte(`{"status":"success","data":[{"app":"a","secret":"s1"},{"app":"a","secret":"s2"}]}`))
	if err != nil || string(out) != `{"data":[{"app":"a"}],"status":"success"}` {
		t.Fatalf("got %s (%v)", out, err)
	}
	if _, err := p.ResponseFilter([]byte(`not json`)); err == nil {
		t.Fatal("malformed body must fail closed")
	}

	plain, err := planSeries(params, policy.Effective{Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if plain.ResponseFilter != nil {
		t.Fatal("no denied labels, no filter")
	}
}
