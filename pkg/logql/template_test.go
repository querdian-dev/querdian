package logql

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"
)

// templateDeniedNames are the denied labels of the template tests.
var templateDeniedNames = []string{"secret", "customer_id"}

// templateDeniedQueries must all be rejected by [DeniedLabel] with
// templateDeniedNames. Each one can read a denied label or the whole label map,
// or does not parse.
var templateDeniedQueries = []string{
	// A "}}" in a string literal of the action.
	"{app=\"web\"} | line_format `{{ printf \"}}%v\" . }}`",
	// A character constant that holds a double quote.
	"{app=\"web\"} | line_format `{{ printf \"%c%v%c\" '\"' . '\"' }}`",
	// A character constant that holds a backtick.
	"{app=\"web\"} | line_format \"{{ printf \\\"%c%v%c\\\" '`' . '`' }}\"",
	// A label name that the template builds at run time.
	"{app=\"web\"} | label_format leak=`{{ printf \"%c%s%c\" '\"' (index . (print \"custo\" \"mer_id\")) '\"' }}`",
	// An escape in a template string hides the name.
	`{app="x"} | line_format "{{ print \"}}\" (index . \"s\\x65cret\") }}"`,
	// The same as an oracle in a metric query.
	"count_over_time({app=\"web\"} | line_format `{{ printf \"}}%v\" . }}` |= \"customer_id:42\" [5m])",
	"sum(count_over_time({app=\"web\"} | line_format `{{ printf \"%c%v%c\" '\"' . '\"' }}` |= \"customer_id:42\" [5m]))",
	// The control query.
	"{app=\"web\"} | line_format `{{ printf \"%v\" . }}`",
	// Fields that name a denied label.
	`{app="x"} | line_format "{{.secret}}"`,
	`{app="x"} | line_format "{{ .customer_id | upper }}"`,
	`{app="x"} | label_format a="{{ .app }}", b="{{ .secret }}"`,
	`{app="x"} | line_format "{{ if eq .secret \"1\" }}y{{ end }}"`,
	`{app="x"} | line_format "{{ if .app }}{{ else }}{{ .secret }}{{ end }}"`,
	// Dot in all forms.
	`{app="x"} | line_format "{{ . }}"`,
	`{app="x"} | line_format "{{- . -}}"`,
	`{app="x"} | line_format "{{ (.) }}"`,
	`{app="x"} | line_format "{{ . | printf \"%v\" }}"`,
	`{app="x"} | line_format "{{\"{{\"}}{{ . }}{{\"}}\"}}"`,
	`{app="x"} | line_format "{{/* }} */}}{{ . }}"`,
	`{app="x"} | line_format "{{ if .app }}{{ . }}{{ end }}"`,
	`{app="x"} | line_format "{{ if .app }}x{{ else }}{{ . }}{{ end }}"`,
	// Chained fields and chains.
	`{app="x"} | line_format "{{ .secret.x }}"`,
	`{app="x"} | line_format "{{ .app.x }}"`,
	`{app="x"} | line_format "{{ (.app).x }}"`,
	// Variables.
	`{app="x"} | line_format "{{ $ }}"`,
	`{app="x"} | line_format "{{ $.secret }}"`,
	`{app="x"} | line_format "{{ $x := . }}{{ $x }}"`,
	`{app="x"} | line_format "{{ $x := .app }}{{ $x }}"`,
	`{app="x"} | line_format "{{range $k, $v := .}}{{$k}}{{end}}"`,
	// range, with, template, define, block and else with.
	`{app="x"} | line_format "{{ range . }}x{{ end }}"`,
	`{app="x"} | line_format "{{ with . }}x{{ end }}"`,
	`{app="x"} | line_format "{{ with .app }}x{{ end }}"`,
	`{app="x"} | line_format "{{ if .app }}{{ else with . }}{{ . }}{{ end }}"`,
	`{app="x"} | line_format "{{ template \"x\" . }}"`,
	`{app="x"} | line_format "{{ define \"x\" }}{{ . }}{{ end }}"`,
	`{app="x"} | line_format "{{ block \"x\" . }}{{ . }}{{ end }}"`,
	// Functions outside the allow-list.
	`{app="x"} | label_format a="{{ index . \"secret\" }}"`,
	`{app="x"} | label_format a="{{ index . .app }}"`,
	`{app="x"} | line_format "{{ index .app 0 }}"`,
	`{app="x"} | line_format "{{ slice .app 1 }}"`,
	`{app="x"} | line_format "{{ call .app }}"`,
	`{app="x"} | line_format "{{ foo .app }}"`,
	`{app="x"} | line_format "{{ .app | index }}"`,
	// Control characters other than tab and newline, in raw and in
	// double-quoted strings.
	"{app=\"x\"} | line_format `{\r{ . }}`",
	"{app=\"x\"} | line_format `{{ .app }}\r`",
	"{app=\"x\"} | label_format a=`{\r{ . }}`",
	`{app="x"} | line_format "{\r{ . }}"`,
	`{app="x"} | label_format a="{\r{ . }}"`,
	`{app="x"} | line_format "{{ .app }}\r"`,
	`{app="x"} | line_format "{{ .app }}\x00"`,
	`{app="x"} | line_format "{{ .app }}\x7f"`,
	"{app=\"x\"} | line_format `{{ .app }}\u0085`",
	`{app="x"} | line_format "{{ .app }}\xff"`,
	// Templates that do not parse.
	`{app="x"} | line_format "{{ .app "`,
	`{app="x"} | line_format "{{ end }}"`,
	`{app="x"} | line_format "{{ printf \"%v }}"`,
	`{app="x"} | line_format "{{ break }}"`,
	`{app="x"} | label_format a="{{ if .app }}"`,
}

// templateTestTreeNames are tree names that a template engine can give to a
// line_format or label_format template. The check must not depend on them.
var templateTestTreeNames = []string{templateTreeNames[0], templateTreeNames[1], "label_format", "line", "label", "t"}

// templateDefineCases returns templates with a define action for each name of
// templateTestTreeNames, with an empty and with a non-empty main body. The
// check must reject all of them.
func templateDefineCases() []string {
	var out []string
	for _, name := range templateTestTreeNames {
		q := strconv.Quote(name)
		out = append(out,
			"{{define "+q+"}}{{ . }}{{end}}",
			"{{define "+q+"}}{{ .secret }}{{end}}",
			"{{ define "+q+" }}{{ .app }}{{ end }}",
			"x{{define "+q+"}}{{ . }}{{end}}",
			"{{ .app }}{{define "+q+"}}{{ .secret }}{{end}}",
			"{{define "+q+"}}{{ . }}{{end}}{{ .app }}",
			"{{block "+q+" .}}{{ .app }}{{end}}",
			"{{ .app }}{{define "+q+"}}{{end}}",
			"{{define "+q+"}}{{end}}",
			"{{define "+q+"}}{{end}}{{ .app }}",
		)
	}
	return out
}

func TestTemplateCheckDefine(t *testing.T) {
	denied := map[string]struct{}{"secret": {}, "customer_id": {}}
	for _, tmpl := range templateDefineCases() {
		if templateCheck(tmpl, denied) == "" {
			t.Errorf("templateCheck(%q) accepted a define", tmpl)
		}
		for _, stage := range []string{"line_format ", "label_format a="} {
			q := `{app="x"} | ` + stage + "`" + tmpl + "`"
			if _, found, err := DeniedLabel(mustParse(t, q), templateDeniedNames); err != nil || !found {
				t.Errorf("DeniedLabel(%s) = %v,%v want denied", q, found, err)
			}
		}
	}
}

// goTemplateBuiltins returns the names in the built-in function map of the
// Go text/template package. It reads the source of the Go toolchain that runs
// the test.
func goTemplateBuiltins(t *testing.T) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-f", "{{.Dir}}", "text/template").Output()
	if err != nil {
		t.Fatalf("go list text/template: %v", err)
	}
	file := filepath.Join(strings.TrimSpace(string(out)), "funcs.go")
	f, err := goparser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var names []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "builtins" || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if kv, ok := n.(*ast.KeyValueExpr); ok {
				if lit, ok := kv.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					names = append(names, name)
				}
			}
			return true
		})
	}
	if len(names) == 0 {
		t.Fatalf("no built-in functions found in %s", file)
	}
	return names
}

// TestTemplateBuiltinsKnown fails when Go text/template gets a built-in
// function that is not in templateFuncs or templateRejectedFuncs. Decide for
// each new built-in if it agrees with the two facts in templateFuncs.
func TestTemplateBuiltinsKnown(t *testing.T) {
	builtins := goTemplateBuiltins(t)
	for _, name := range builtins {
		_, ok := templateFuncs[name]
		_, rej := templateRejectedFuncs[name]
		if ok == rej {
			t.Errorf("Go template built-in %q must be in exactly one of templateFuncs and templateRejectedFuncs", name)
		}
	}
	for name := range templateRejectedFuncs {
		if !strings.Contains(" "+strings.Join(builtins, " ")+" ", " "+name+" ") {
			t.Errorf("rejected function %q is not a Go built-in", name)
		}
	}
	if templateCheck("{{ index .app 0 }}", nil) == "" || templateCheck("{{ len .app }}", nil) != "" {
		t.Error("the allow-list and the reject list do not apply")
	}
}

// templateAllowedQueries must all be accepted by [DeniedLabel] with
// templateDeniedNames.
var templateAllowedQueries = []string{
	`{app="x"} | line_format "{{.app}}"`,
	"{app=\"x\"} | line_format `{{ .app }}\n{{ .env }}\t{{ .level }}`",
	`{app="x"} | line_format "{{ .app }}\n{{ .env }}\t"`,
	`{app="x"} | line_format "{{ .app | upper }} {{ .env }}"`,
	`{app="x"} | line_format "literal . text and { braces }"`,
	`{app="x"} | line_format "{{ \".\" }}"`,
	`{app="x"} | line_format "{{\"{{\"}}"`,
	`{app="x"} | line_format "{{- .app -}}"`,
	`{app="x"} | line_format "{{ (.app) }}"`,
	`{app="x"} | line_format "{{/* . */}}{{ .app }}"`,
	`{app="x"} | line_format "the secret is not a label"`,
	`{app="x"} | line_format "{{ .secretive }} {{ .Secret }}"`,
	`{app="x"} | line_format "{{ __line__ }} {{ __timestamp__ }}"`,
	`{app="x"} | line_format "{{ printf \"%s-%d\" .app 3 }}"`,
	`{app="x"} | line_format "{{ if eq .level \"error\" }}E{{ else if .msg }}{{ .msg }}{{ else }}-{{ end }}"`,
	`{app="x"} | line_format "{{ .app | ToUpper | trunc 3 }} {{ add 1 2 }} {{ true }} {{ printf \"%v\" nil }}"`,
	`{app="x"} | label_format lv="{{.level}}", a=app`,
	"{app=\"x\"} | logfmt | line_format `{{ .level }}: {{ .msg }}`",
	`sum(count_over_time({app="x"} | line_format "{{ .msg }}" |= "secret" [5m]))`,
}

func TestDeniedLabelTemplates(t *testing.T) {
	for _, q := range templateDeniedQueries {
		got, found, err := DeniedLabel(mustParse(t, q), templateDeniedNames)
		if err != nil || !found {
			t.Errorf("DeniedLabel(%s) = %q,%v,%v want denied", q, got, found, err)
		}
	}
	for _, q := range templateAllowedQueries {
		got, found, err := DeniedLabel(mustParse(t, q), templateDeniedNames)
		if err != nil || found {
			t.Errorf("DeniedLabel(%s) = %q,%v,%v want allowed", q, got, found, err)
		}
	}
	// The result names the denied label, the template class, or the parse failure.
	for q, want := range map[string]string{
		`{app="x"} | line_format "{{ .customer_id }}"`: "customer_id",
		`{app="x"} | line_format "{{ . }}"`:            TemplateAllLabels,
		`{app="x"} | line_format "{{ index .app 0 }}"`: TemplateAllLabels,
		`{app="x"} | line_format "{{ .app "`:           TemplateInvalid,
	} {
		if got, _, _ := DeniedLabel(mustParse(t, q), templateDeniedNames); got != want {
			t.Errorf("DeniedLabel(%s) = %q want %q", q, got, want)
		}
	}
	// The lexer rejects a literal CR in a double-quoted string.
	if _, err := Parse("{app=\"x\"} | line_format \"{\r{ . }}\""); err == nil {
		t.Error("literal CR in a double-quoted string must not parse")
	}
	// Without denied labels templates are not checked.
	if _, found, _ := DeniedLabel(mustParse(t, `{app="x"} | line_format "{{ . }}"`), nil); found {
		t.Error("no denied labels must not match")
	}
}

// templateFuncsTestFor returns stub functions for every name of
// [templateFuncs] that is not a Go built-in, and for index, slice and call.
// Each stub prints all its arguments, so a value that reaches a function shows
// in the output. printf is replaced with a version that refuses large widths,
// so that the fuzz target does not allocate much memory.
func templateFuncsTestFor() template.FuncMap {
	stub := func(args ...any) string { return fmt.Sprint(args...) }
	fm := template.FuncMap{}
	builtin := map[string]bool{
		"and": true, "or": true, "not": true, "len": true, "print": true, "printf": true,
		"println": true, "html": true, "js": true, "urlquery": true, "eq": true, "ne": true,
		"lt": true, "le": true, "gt": true, "ge": true,
	}
	for name := range templateFuncs {
		if !builtin[name] {
			fm[name] = stub
		}
	}
	for name := range templateRejectedFuncs {
		fm[name] = stub
	}
	fm["printf"] = func(format string, args ...any) (string, error) {
		if strings.ContainsAny(format, "*[") || len(format) > 256 {
			return "", fmt.Errorf("format not allowed in test")
		}
		digits := 0
		for _, r := range format {
			if r >= '0' && r <= '9' {
				digits++
				if digits > 3 {
					return "", fmt.Errorf("width not allowed in test")
				}
			} else {
				digits = 0
			}
		}
		return fmt.Sprintf(format, args...), nil
	}
	return fm
}

// runTemplate executes tmpl over labels with real text/template. It fails the
// test if the execution does not stop within a time limit.
func runTemplate(t *testing.T, name, tmpl string, labels map[string]string) (string, error) {
	t.Helper()
	tp, err := template.New(name).Option("missingkey=zero").Funcs(templateFuncsTestFor()).Parse(tmpl)
	if err != nil {
		return "", err
	}
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		var b strings.Builder
		err := tp.Execute(&b, labels)
		done <- result{b.String(), err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(5 * time.Second):
		t.Fatalf("template %q did not stop", tmpl)
		return "", nil
	}
}

// FuzzTemplateDenied: when [templateCheck] accepts a template, real
// text/template must never print the canary value of a denied label. The test
// executes the template two times with different canary values under the
// denied keys. The output and the error must be the same in the two runs.
// It does this for the tree name of the check and for other tree names.
func FuzzTemplateDenied(f *testing.F) {
	for _, list := range [][]string{templateDeniedQueries, templateAllowedQueries} {
		for _, q := range list {
			parsed, err := Parse(q)
			if err != nil {
				f.Fatalf("Parse(%q): %v", q, err)
			}
			toks, err := Lex(parsed.Source)
			if err != nil {
				f.Fatal(err)
			}
			for _, tk := range toks {
				if tk.Kind == String || tk.Kind == RawString {
					f.Add(tk.Value)
				}
			}
		}
	}
	for _, tmpl := range templateDefineCases() {
		f.Add(tmpl)
	}
	denied := map[string]struct{}{}
	for _, d := range templateDeniedNames {
		denied[d] = struct{}{}
	}
	const canaryA, canaryB = "c4n4ryAq7", "c4n4ryBz3"
	labels := func(canary string) map[string]string {
		return map[string]string{
			"app": "web", "env": "prod", "level": "error", "msg": "hello",
			"secret": canary, "customer_id": canary + "-42",
		}
	}
	f.Fuzz(func(t *testing.T, tmpl string) {
		if len(tmpl) > 4096 || templateCheck(tmpl, denied) != "" {
			return
		}
		for _, name := range []string{templateTreeNames[0], "line", "label"} {
			outA, errA := runTemplate(t, name, tmpl, labels(canaryA))
			outB, errB := runTemplate(t, name, tmpl, labels(canaryB))
			// The canaries differ, so equal results show that no denied
			// value reaches the output or an error. A canary in the output
			// that the template builds from its own text is the same in
			// both results.
			if outA != outB || errString(errA) != errString(errB) {
				t.Fatalf("template %q (name %q) output depends on a denied label: %q / %q (%v / %v)", tmpl, name, outA, outB, errA, errB)
			}
			if strings.Contains(outA, canaryA) && !strings.Contains(tmpl, canaryA) && !strings.Contains(outB, canaryA) {
				t.Fatalf("template %q (name %q) prints the canary: %q", tmpl, name, outA)
			}
		}
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
