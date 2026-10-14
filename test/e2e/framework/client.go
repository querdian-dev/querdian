package framework

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

// Client issues HTTP requests against a base URL (the proxy as a user, or an upstream directly).
type Client struct {
	// Base is prepended to every path.
	Base string
	// Token, when set, is sent as a bearer token.
	Token string

	t    testing.TB
	http *http.Client
}

// NewClient returns a client for an upstream (or any URL) without credentials.
func NewClient(t testing.TB, base string) *Client {
	return &Client{Base: base, t: t, http: &http.Client{Timeout: 2 * time.Minute}}
}

// Response is a fully read HTTP response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON decodes the body into v and fails the test on error.
func (r *Response) JSON(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %q: %v", r.Body, err)
	}
}

// Get sends GET path?q.
func (c *Client) Get(path string, q url.Values) *Response {
	c.t.Helper()
	u := c.Base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil) //nolint:noctx // test-only
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do(req)
}

// PostForm sends POST path with an application/x-www-form-urlencoded body.
func (c *Client) PostForm(path string, form url.Values) *Response {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.Base+path, strings.NewReader(form.Encode())) //nolint:noctx // test-only
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req)
}

// Do sends a custom request (the bearer token is added when the client has one).
func (c *Client) Do(req *http.Request) *Response { c.t.Helper(); return c.do(req) }

func (c *Client) do(req *http.Request) *Response {
	c.t.Helper()
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http.Do(req) //nolint:gosec // test-only client, URL chosen by the test
	if err != nil {
		c.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// --- CRD builders --------------------------------------------------------------------------------

// Eq is an Equal matcher.
func Eq(name, value string) v1.Matcher {
	return v1.Matcher{Name: name, Op: v1.MatchEqual, Value: value}
}

// Ne is a NotEqual matcher.
func Ne(name, value string) v1.Matcher {
	return v1.Matcher{Name: name, Op: v1.MatchNotEqual, Value: value}
}

// Re is a RegexMatch matcher.
func Re(name, value string) v1.Matcher {
	return v1.Matcher{Name: name, Op: v1.MatchRegexMatch, Value: value}
}

// NRe is a RegexNoMatch matcher.
func NRe(name, value string) v1.Matcher {
	return v1.Matcher{Name: name, Op: v1.MatchRegexNoMatch, Value: value}
}

// Group builds Subjects for groups.
func Group(groups ...string) v1.Subjects { return v1.Subjects{Groups: groups, Users: []string{}} }

// User builds Subjects for users.
func User(users ...string) v1.Subjects { return v1.Subjects{Groups: []string{}, Users: users} }

// ClusterPolicy builds a ClusterAccessPolicy.
func ClusterPolicy(name string, subj v1.Subjects, rules ...v1.ClusterRule) v1.ClusterAccessPolicy {
	p := v1.ClusterAccessPolicy{Spec: v1.ClusterAccessPolicySpec{Subjects: subj, Rules: rules}}
	p.Name = name
	return p
}

// Rule builds a restricted ClusterRule for one backend; each rule is one DNF term.
func Rule(backend string, matchers ...v1.Matcher) v1.ClusterRule {
	return v1.ClusterRule{Backends: []string{backend}, Matchers: matchers}
}

// Unrestricted builds an unrestricted ClusterRule for one backend.
func Unrestricted(backend string) v1.ClusterRule {
	return v1.ClusterRule{Backends: []string{backend}, Unrestricted: true}
}

// Deny returns r with labels.deny set.
func Deny(r v1.ClusterRule, labels ...string) v1.ClusterRule {
	r.Labels = &v1.LabelFilter{Deny: labels}
	return r
}

// NamespacedPolicy builds an AccessPolicy with one rule per backend list entry.
func NamespacedPolicy(namespace, name string, subj v1.Subjects, rules ...v1.Rule) v1.AccessPolicy {
	p := v1.AccessPolicy{Spec: v1.AccessPolicySpec{Subjects: subj, Rules: rules}}
	p.Name, p.Namespace = name, namespace
	return p
}
