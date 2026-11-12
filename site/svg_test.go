package site

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The website puts each SVG file of docs/img into the page itself (layouts/_default/_markup/
// render-image.html), so that the text of a drawing uses the fonts of the site. A drawing is then
// a part of the page: a script would run, and a style rule would apply to the full page. GitHub
// shows the same file as an image and hides these parts, so a review does not see them. This
// check permits only drawing elements and keeps the styles and ids inside the drawing.

var svgElements = map[string]bool{
	"svg": true, "title": true, "desc": true, "style": true, "defs": true, "marker": true,
	"pattern": true, "g": true, "rect": true, "path": true, "line": true, "polyline": true,
	"polygon": true, "circle": true, "ellipse": true, "text": true, "tspan": true,
}

var svgRootClass = regexp.MustCompile(`^qd-[a-z0-9]+$`)

// checkSVG returns the reasons why a drawing must not go into a page.
func checkSVG(data []byte) []string {
	var bad []string
	fail := func(format string, args ...string) {
		for _, a := range args {
			format = strings.Replace(format, "%s", a, 1)
		}
		bad = append(bad, format)
	}
	if bytes.Contains(data, []byte("<!DOCTYPE")) || bytes.Contains(data, []byte("<!ENTITY")) {
		fail("a DOCTYPE or an entity declaration")
	}

	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		root, prefix string
		ids          = map[string]bool{}
		refs         []string
		inStyle      bool
		style        strings.Builder
		depth        int
		hasTitle     bool
	)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return append(bad, "not well-formed XML: "+err.Error())
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			name := t.Name.Local
			if !svgElements[name] {
				fail("the element <%s>", name)
			}
			if depth == 1 {
				if name != "svg" {
					fail("the root element <%s>", name)
				}
				attrs := map[string]string{}
				for _, a := range t.Attr {
					attrs[a.Name.Local] = a.Value
				}
				for _, c := range strings.Fields(attrs["class"]) {
					if c != "qd-diagram" && svgRootClass.MatchString(c) {
						root = c
					}
				}
				if root == "" {
					fail("the root element has no class of the form qd-<name>")
				}
				prefix = root + "-"
				if attrs["role"] != "img" {
					fail(`the root element has no role="img"`)
				}
				if attrs["aria-labelledby"] == "" {
					fail("the root element has no aria-labelledby")
				}
				refs = append(refs, strings.Fields(attrs["aria-labelledby"])...)
				refs = append(refs, strings.Fields(attrs["aria-describedby"])...)
			}
			if name == "title" && depth == 2 {
				hasTitle = true
			}
			inStyle = name == "style"
			for _, a := range t.Attr {
				key, val := a.Name.Local, a.Value
				lower := strings.ToLower(val)
				switch {
				case strings.HasPrefix(strings.ToLower(key), "on"):
					fail("the event attribute %s", key)
				case key == "href" || key == "src":
					fail("the attribute %s", key)
				case strings.Contains(lower, "javascript:") || strings.Contains(lower, "data:") || strings.Contains(lower, "@import") || strings.Contains(lower, "expression("):
					fail("the value of the attribute %s", key)
				}
				for _, u := range urlRefs(val) {
					if !strings.HasPrefix(u, "#") {
						fail("the external address %s in the attribute %s", u, key)
					} else {
						refs = append(refs, u[1:])
					}
				}
				if key == "id" {
					ids[val] = true
					if prefix != "" && !strings.HasPrefix(val, prefix) {
						fail("the id %s does not start with %s", val, prefix)
					}
				}
			}
		case xml.EndElement:
			depth--
			inStyle = false
		case xml.CharData:
			if inStyle {
				style.Write(t)
			}
		case xml.ProcInst:
			if t.Target != "xml" {
				fail("the processing instruction %s", t.Target)
			}
		case xml.Directive:
			fail("a directive")
		}
	}
	if !hasTitle {
		bad = append(bad, "no <title> element")
	}
	for _, r := range refs {
		if !ids[r] {
			fail("a reference to the id %s, which does not exist", r)
		}
	}
	bad = append(bad, checkSVGStyle(style.String(), root)...)
	return bad
}

var (
	svgURL     = regexp.MustCompile(`url\(\s*['"]?([^'")]*)['"]?\s*\)`)
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

func urlRefs(s string) []string {
	var out []string
	for _, m := range svgURL.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// checkSVGStyle makes sure that each rule of the style element applies only inside the drawing.
func checkSVGStyle(css, root string) []string {
	var bad []string
	css = cssComment.ReplaceAllString(css, "")
	lower := strings.ToLower(css)
	if strings.Contains(css, "@") {
		bad = append(bad, "an at-rule in the style element")
	}
	if len(urlRefs(css)) > 0 || strings.Contains(lower, "expression(") || strings.Contains(lower, "javascript:") {
		bad = append(bad, "an address or an expression in the style element")
	}
	for css = strings.TrimSpace(css); css != ""; css = strings.TrimSpace(css) {
		open, end := strings.Index(css, "{"), strings.Index(css, "}")
		if open < 0 || end < open {
			return append(bad, "a style rule that does not parse")
		}
		if strings.Contains(css[open+1:end], "{") {
			return append(bad, "a nested style rule")
		}
		for _, sel := range strings.Split(css[:open], ",") {
			sel = strings.TrimSpace(sel)
			if root == "" || (sel != "."+root && !strings.HasPrefix(sel, "."+root+" ")) {
				bad = append(bad, "the selector "+sel+" does not start with the root class of the drawing")
			}
		}
		css = css[end+1:]
	}
	return bad
}

func TestDocsDrawings(t *testing.T) {
	files, err := filepath.Glob("../docs/img/*.svg")
	if err != nil {
		t.Fatal(err)
	}
	prefixes := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // a file of the repository
		if err != nil {
			t.Fatal(err)
		}
		for _, reason := range checkSVG(data) {
			t.Errorf("%s: %s", f, reason)
		}
		// Two drawings can be in one page: each needs a root class of its own.
		if m := regexp.MustCompile(`class="[^"]*\b(qd-[a-z0-9]+)"`).FindSubmatch(data); m != nil {
			if other, ok := prefixes[string(m[1])]; ok {
				t.Errorf("%s and %s use the same root class %s", f, other, m[1])
			}
			prefixes[string(m[1])] = f
		}
	}
}

func TestCheckSVG(t *testing.T) {
	const ok = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10" class="qd-diagram qd-x" role="img" aria-labelledby="qd-x-title" aria-describedby="qd-x-desc">
  <title id="qd-x-title">T</title><desc id="qd-x-desc">D</desc>
  <style>/* c */ .qd-x text { fill: #101418; } .qd-x .qd-h, .qd-x .qd-s { font-size: 12px; }</style>
  <defs><marker id="qd-x-arrow"><path d="M0 0L1 1"/></marker><pattern id="qd-x-hatch"><rect width="1" height="1"/></pattern></defs>
  <rect width="10" height="10" fill="url(#qd-x-hatch)"/><path d="M0 0H5" marker-end="url(#qd-x-arrow)"/><text class="qd-h" x="1" y="1" style="font-size:14px">a</text>
</svg>`
	if got := checkSVG([]byte(ok)); len(got) != 0 {
		t.Fatalf("a correct drawing is rejected: %q", got)
	}
	tests := []struct {
		name, old, new, want string
	}{
		{"script", `<rect width="10"`, `<script>alert(1)</script><rect width="10"`, "the element <script>"},
		{"foreignObject", `<rect width="10"`, `<foreignObject/><rect width="10"`, "the element <foreignObject>"},
		{"image", `<rect width="10"`, `<image/><rect width="10"`, "the element <image>"},
		{"link", `<text class="qd-h"`, `<a href="https://example.com"/><text class="qd-h"`, "the element <a>"},
		{"use with an external file", `<rect width="10"`, `<use href="other.svg#x"/><rect width="10"`, "the element <use>"},
		{"event attribute", `<rect width="10"`, `<rect onload="alert(1)" width="10"`, "the event attribute onload"},
		{"external address in an attribute", `fill="url(#qd-x-hatch)"`, `fill="url(https://example.com/x.svg#p)"`, "the external address"},
		{"external address in an inline style", `style="font-size:14px"`, `style="fill:url('//example.com/x')"`, "the external address"},
		{"javascript address", `style="font-size:14px"`, `style="background:javascript:alert(1)"`, "the value of the attribute style"},
		{"import in the style element", `/* c */`, `@import "https://example.com/x.css";`, "an at-rule"},
		{"address in the style element", `fill: #101418;`, `fill: url(https://example.com/p);`, "an address or an expression"},
		{"selector for the full page", `.qd-x text {`, `text {`, "the selector text does not start with the root class"},
		{"selector that only has the root class as a prefix", `.qd-x text {`, `.qd-xy text {`, "the selector .qd-xy text does not start"},
		{"selector list with a rule for the full page", `.qd-x .qd-h, .qd-x .qd-s {`, `.qd-x .qd-h, body {`, "the selector body does not start"},
		{"media rule", `/* c */`, `@media (min-width: 1px) { body { display: none } }`, "an at-rule"},
		{"id without the prefix", `id="qd-x-arrow"`, `id="arrow"`, "the id arrow does not start with qd-x-"},
		{"reference to an id that does not exist", `url(#qd-x-arrow)`, `url(#qd-x-absent)`, "a reference to the id qd-x-absent"},
		{"no role", ` role="img"`, ``, `no role="img"`},
		{"no title", `<title id="qd-x-title">T</title>`, ``, "no <title> element"},
		{"no root class", `class="qd-diagram qd-x"`, `class="qd-diagram"`, "no class of the form qd-<name>"},
		{"entity", `<svg `, `<!DOCTYPE svg [<!ENTITY x "y">]><svg `, "a DOCTYPE or an entity declaration"},
		{"not XML", `</svg>`, ``, "not well-formed XML"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(ok, tt.old) {
				t.Fatalf("the test drawing does not contain %q", tt.old)
			}
			got := strings.Join(checkSVG([]byte(strings.Replace(ok, tt.old, tt.new, 1))), "\n")
			if !strings.Contains(got, tt.want) {
				t.Errorf("reasons %q do not contain %q", got, tt.want)
			}
		})
	}
}
