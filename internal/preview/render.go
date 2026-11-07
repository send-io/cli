// Package preview renders a template folder (as `sendio templates pull`
// writes it) the way a send renders the template, and lints the result.
//
// The engine is the shared send.io/email-renderer module. The glue
// here mirrors the backend's foundation/templaterender: Render/RenderHandlebars for
// variables, fallbacks and type checks, SimpleFill for the "simple" engine
// and ApplyVersion for the parts and the hidden preview-text block. Where
// a send would fail (a missing variable, a value of the wrong type, a syntax
// error) Render returns the problem as a Warning of level "error" instead and
// renders what it can.
package preview

import (
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"

	"send.io/email-renderer"
)

// Engines (backend internal/templates).
const (
	EngineSimple     = "simple"
	EngineHandlebars = "handlebars"
)

// Variable is a declared template variable (meta.json "variables").
type Variable struct {
	Key           string `json:"key"`
	Type          string `json:"type"`
	FallbackValue any    `json:"fallback_value,omitempty"`
}

// Template is one template version's content.
type Template struct {
	Engine      string
	From        string
	Subject     string
	HTML        string
	Text        string
	PreviewText string
	ReplyTo     []string
	Variables   []Variable
}

// Warning is one finding about a render.
type Warning struct {
	Level   string `json:"level"` // error (a send would fail), warning, info
	Code    string `json:"code"`
	Message string `json:"message"`
	Part    string `json:"part,omitempty"`
}

// Result is what a send with the given data would contain.
type Result struct {
	Engine    string         `json:"engine"`
	From      string         `json:"from"`
	Subject   string         `json:"subject"`
	HTML      string         `json:"html"`
	Text      string         `json:"text"`
	ReplyTo   []string       `json:"reply_to,omitempty"`
	Variables map[string]any `json:"variables"`
	Warnings  []Warning      `json:"warnings"`
	HTMLBytes int            `json:"html_bytes"`
}

var (
	templateVarRe    = regexp.MustCompile(`\{\{\{?\s*([A-Za-z0-9_]+)\s*\}?\}\}`)
	templateTripleRe = regexp.MustCompile(`\{\{\{\s*([A-Za-z0-9_]+)\s*\}\}\}`)
	templateDoubleRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)
)

// keepPersonalTags is the backend's (templates.KeepPersonalTags): merge tags
// filled per contact later.
func keepPersonalTags(head string) bool {
	switch strings.ToLower(head) {
	case "contact", "unsubscribe_url", "resend_unsubscribe_url", "sendio_unsubscribe_url":
		return true
	}
	return false
}

type renderer struct {
	warn []Warning
	// heads are the top-level names the handlebars templates asked for.
	heads map[string]bool
}

func (r *renderer) add(level, code, part, format string, args ...any) {
	r.warn = append(r.warn, Warning{Level: level, Code: code, Part: part, Message: fmt.Sprintf(format, args...)})
}

func (r *renderer) engineErr(part string, err error) {
	switch e := err.(type) {
	case *tmpl.ParseError:
		r.add("error", "syntax_error", part, "Template syntax error in %s at line %d, column %d: %s", part, e.Line, e.Col, e.Msg)
	case *tmpl.RenderError:
		r.add("error", "render_error", part, "Template error in %s at line %d, column %d: %s", part, e.Line, e.Col, e.Msg)
	default:
		r.add("error", "render_error", part, "Template error in %s: %v", part, err)
	}
}

// Render renders t with data the way POST /emails with this template and
// template.variables = data would.
func Render(t Template, data map[string]any) Result {
	r := &renderer{heads: map[string]bool{}}
	engine := t.Engine
	if engine == "" {
		engine = EngineSimple
	}
	provided := map[string]any{}
	for k, v := range data {
		provided[k] = v
	}
	content := t.From + t.Subject + t.HTML + t.Text + t.PreviewText + strings.Join(t.ReplyTo, ",")
	var fill func(part, value string, escapeHTML bool) string
	if engine == EngineHandlebars {
		for _, d := range t.Variables {
			value, ok := provided[d.Key]
			if !ok || value == nil {
				value = d.FallbackValue
			}
			if value == nil {
				if namedInTags(content, d.Key) {
					r.add("error", "missing_variable", "", "Missing template variable: %s (no value and no fallback; a send would fail)", d.Key)
				}
				continue
			}
			if !typeOK(d.Type, value) {
				r.add("error", "variable_type", "", "Variable must be a %s: %s", d.Type, d.Key)
			}
			provided[d.Key] = value
		}
		fill = func(part, value string, escapeHTML bool) string {
			if value == "" {
				return ""
			}
			out, err := tmpl.Render(value, provided, tmpl.Options{Escape: escapeHTML, Keep: func(head string) bool {
				r.heads[head] = true
				return keepPersonalTags(head)
			}})
			if err != nil {
				r.engineErr(part, err)
				return value
			}
			return out
		}
	} else {
		referenced := map[string]bool{}
		for _, m := range templateVarRe.FindAllStringSubmatch(content, -1) {
			referenced[m[1]] = true
		}
		declared := map[string]bool{}
		for _, d := range t.Variables {
			declared[d.Key] = true
			if !referenced[d.Key] {
				continue
			}
			value, ok := provided[d.Key]
			if !ok {
				value = d.FallbackValue
			}
			if value == nil {
				r.add("error", "missing_variable", "", "Missing template variable: %s (no value and no fallback; a send would fail)", d.Key)
				continue
			}
			if d.Type == "string" {
				if _, ok := value.(string); !ok {
					r.add("error", "variable_type", "", "Variable must be a string: %s", d.Key)
				}
			} else if _, ok := value.(float64); !ok {
				r.add("error", "variable_type", "", "Variable must be a number: %s", d.Key)
			}
			provided[d.Key] = value
		}
		for _, k := range sortedKeys(referenced) {
			if _, ok := provided[k]; !ok && !declared[k] && !keepPersonalTags(k) {
				r.add("warning", "undeclared_variable", "", "{{%s}} is not a declared variable and has no value here: it is sent as written", k)
			}
		}
		simple := simpleFill(provided)
		fill = func(_, value string, escapeHTML bool) string { return simple(value, escapeHTML) }
	}
	res := Result{Engine: engine, Variables: provided}
	res.From = fill("from", t.From, false)
	res.Subject = fill("subject", t.Subject, false)
	res.HTML = fill("html", t.HTML, true)
	res.Text = fill("text", t.Text, false)
	if pt := strings.TrimSpace(fill("preview_text", t.PreviewText, false)); pt != "" && res.HTML != "" {
		pre := `<div style="display:none;max-height:0;overflow:hidden;opacity:0;mso-hide:all">` + html.EscapeString(pt) + `</div>`
		h := res.HTML
		if i := strings.Index(strings.ToLower(h), "<body"); i >= 0 {
			if j := strings.Index(h[i:], ">"); j >= 0 {
				h = h[:i+j+1] + pre + h[i+j+1:]
			}
		} else {
			h = pre + h
		}
		res.HTML = h
	}
	for _, a := range t.ReplyTo {
		res.ReplyTo = append(res.ReplyTo, fill("reply_to", a, false))
	}
	if engine == EngineHandlebars {
		for _, h := range sortedKeys(r.heads) {
			if _, ok := provided[h]; ok || keepPersonalTags(h) || isHelper(h) || h == "root" || inTree(data, h) {
				continue
			}
			msg := "{{%s}} has no value in the data: it renders empty"
			for _, d := range t.Variables {
				if d.Key == h {
					msg = "{{%s}} is declared without a fallback and has no value in the data: it renders empty"
				}
			}
			r.add("warning", "no_value", "", msg, h)
		}
	}
	res.HTMLBytes = len(res.HTML)
	res.Warnings = append(r.warn, Lint(res)...)
	if res.Warnings == nil {
		res.Warnings = []Warning{}
	}
	return res
}

func typeOK(typ string, value any) bool {
	switch typ {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	}
	return false
}

// simpleFill is the backend's simple engine.
func simpleFill(provided map[string]any) func(value string, escapeHTML bool) string {
	return func(value string, escapeHTML bool) string {
		value = templateTripleRe.ReplaceAllStringFunc(value, func(tok string) string {
			name := templateTripleRe.FindStringSubmatch(tok)[1]
			if val, ok := provided[name]; ok {
				return fmt.Sprint(val)
			}
			return tok
		})
		return templateDoubleRe.ReplaceAllStringFunc(value, func(tok string) string {
			name := templateDoubleRe.FindStringSubmatch(tok)[1]
			val, ok := provided[name]
			if !ok {
				return tok
			}
			if escapeHTML {
				return html.EscapeString(fmt.Sprint(val))
			}
			return fmt.Sprint(val)
		})
	}
}

// namedInTags reports whether name appears as a word inside some {{ }} tag.
func namedInTags(content, name string) bool {
	// The name either opens the tag ({{name}}) or follows a non-name character
	// ({{ name }}, {{#if name}}, {{{name}}}); a dotted path like {{user.name}}
	// does not count.
	re := regexp.MustCompile(`\{\{(?:[^}]*[^A-Za-z0-9_@.])?` + regexp.QuoteMeta(name) + `(?:[^A-Za-z0-9_]|$)`)
	return re.MatchString(content)
}

var helpers = func() map[string]bool {
	m := map[string]bool{"else": true, "this": true}
	for _, h := range tmpl.HelperNames() {
		m[h] = true
	}
	return m
}()

func isHelper(s string) bool { return helpers[s] }

// inTree reports whether key names a field anywhere in v (a field of the
// items an {{#each}} walks, say), so it is not reported as missing.
func inTree(v any, key string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == key || inTree(e, key) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if inTree(e, key) {
				return true
			}
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
