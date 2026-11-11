package main

import (
	"reflect"
	"testing"
	"testing/fstest"
)

func page(body string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("<!doctype html><html><head><title>t</title></head><body>" + body + "</body></html>")}
}

func TestCheck(t *testing.T) {
	const base = "https://querdian.dev/"
	site := fstest.MapFS{
		"index.html": page(`<a href="/docs/">docs</a> <a href=/docs/quickstart/>minified</a>
			<a href="/docs/multicluster/">main only</a> <a href="https://example.com/x">external</a>
			<a href="//example.com/x">other host</a> <a href="mailto:a@example.com">mail</a>
			<a href="#top">fragment</a> <a href="/docs/quickstart/#steps">page and fragment</a>
			<a href="/docs">directory without slash</a> <a href="https://querdian.dev/docs/main/">absolute</a>
			<a href="https://querdian.dev/docs/gone/">absolute, broken</a>
			<link rel="stylesheet" href="/css/main.css"><script src="/js/absent.js"></script>
			<script src="/css/main.css" data-index="/docs/search-index.json"></script>
			<script src="/css/main.css" data-index="/docs/main/search-index.json"></script>`),
		"css/main.css":                      {Data: []byte("body{}")},
		"docs/index.html":                   page(`<a href="quickstart/">relative</a> <a href="../">up</a> <a href="absent/">relative, broken</a>`),
		"docs/quickstart/index.html":        page(`<a href="../main/">sibling</a> <a href="/../etc/passwd">outside</a>`),
		"docs/main/index.html":              page(`<a href="/docs/main/multicluster/">ok</a>`),
		"docs/main/multicluster/index.html": page(``),
		// Hugo alias pages: a redirect to the page that replaced the address.
		"docs/v0.1.2/index.html":      {Data: []byte(`<!doctype html><html lang=en><head><title>x</title><link rel=canonical href=https://querdian.dev/docs/><meta charset=utf-8><meta http-equiv=refresh content="0; url=https://querdian.dev/docs/"></head></html>`)},
		"docs/v0.1.1/gone/index.html": {Data: []byte(`<!doctype html><html><head><meta http-equiv="refresh" content="0; url=https://querdian.dev/docs/gone/"></head></html>`)},
		"docs/search-index.json":      {Data: []byte(`[{"u":"/docs/absent/"}]`)},
	}

	broken, pages, err := check(site, base)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 7 {
		t.Errorf("pages = %d, want 7", pages)
	}
	want := []string{
		"docs/index.html: absent/",
		"docs/quickstart/index.html: /../etc/passwd",
		"docs/v0.1.1/gone/index.html: https://querdian.dev/docs/gone/",
		"index.html: /docs/main/search-index.json",
		"index.html: /docs/multicluster/",
		"index.html: /js/absent.js",
		"index.html: https://querdian.dev/docs/gone/",
	}
	if !reflect.DeepEqual(broken, want) {
		t.Errorf("broken links:\n got %q\nwant %q", broken, want)
	}
}

func TestCheckWithoutBase(t *testing.T) {
	site := fstest.MapFS{"index.html": page(`<a href="https://querdian.dev/docs/gone/">not internal without -base</a>`)}
	broken, _, err := check(site, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(broken) != 0 {
		t.Errorf("broken = %q, want none", broken)
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		page, link string
		target     string
		internal   bool
	}{
		{"index.html", "/", "index.html", true},
		{"index.html", "/docs/", "docs/index.html", true},
		{"index.html", "/docs", "docs", true},
		{"index.html", "/favicon.svg", "favicon.svg", true},
		{"docs/quickstart/index.html", "../", "docs/index.html", true},
		{"docs/quickstart/index.html", "../policy-guide/#rules", "docs/policy-guide/index.html", true},
		{"docs/quickstart/index.html", "img.png", "docs/quickstart/img.png", true},
		{"docs/index.html", "/docs/?q=1", "docs/index.html", true},
		{"index.html", "https://querdian.dev/docs/main/", "docs/main/index.html", true},
		{"index.html", "https://querdian.dev", "", false},
		{"index.html", "https://github.com/querdian-dev/querdian", "", false},
		{"index.html", "#main", "", false},
		{"index.html", "", "", false},
		{"index.html", "http://[::1", "", true},
	}
	for _, tt := range tests {
		target, internal := resolve(tt.page, tt.link, "https://querdian.dev/")
		if target != tt.target || internal != tt.internal {
			t.Errorf("resolve(%q, %q) = %q, %v, want %q, %v", tt.page, tt.link, target, internal, tt.target, tt.internal)
		}
	}
}
