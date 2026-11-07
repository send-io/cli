package tmpl

// Converters from other providers' template languages to the Handlebars this
// package renders. Each is a pure function: it returns the converted
// template (which always parses), or a *ParseError with the line and column
// of the first construct it cannot convert, so a migration fails loudly
// instead of sending tags as text.
//
// Contact merge tags become send.io's per-contact tags ({{{contact.x|y}}},
// {{{unsubscribe_url}}}), which the engine leaves in place when the data has
// no "contact" key, for broadcast and automation personalization to fill.

import (
	"regexp"
	"strings"
)

var segRe = regexp.MustCompile(`^[A-Za-z0-9_$-]+$`)

// hbPath writes path segments as a Handlebars path ([..] for odd names).
func hbPath(segs []string) string {
	out := make([]string, len(segs))
	for i, s := range segs {
		if segRe.MatchString(s) && s != "this" {
			out[i] = s
		} else {
			out[i] = "[" + s + "]"
		}
	}
	return strings.Join(out, ".")
}

// hbString writes a Handlebars string literal; ok=false when s holds both
// quote characters (Handlebars literals have no escapes).
func hbString(s string) (string, bool) {
	if !strings.Contains(s, `"`) {
		return `"` + s + `"`, true
	}
	if !strings.Contains(s, "'") {
		return "'" + s + "'", true
	}
	return "", false
}

// contactTag writes {{{contact.key|fb}}} (raw) or {{contact.key|fb}}.
func contactTag(key, fb string, raw bool) (string, bool) {
	if strings.ContainsAny(fb, "{}|") {
		return "", false
	}
	inner := "contact." + key
	if fb != "" {
		inner += "|" + fb
	}
	if raw {
		return "{{{" + inner + "}}}", true
	}
	return "{{" + inner + "}}", true
}

func finish(out string) (string, error) {
	if err := Check(out); err != nil {
		return "", err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Conditions shared by the Jinja-like languages (Mailjet, Brevo)
// ---------------------------------------------------------------------------

type condLexer struct {
	src  string
	pos  int // tag offset in src, for errors
	toks []string
	i    int
	ref  func(string) (string, error) // a variable reference to a Handlebars path
}

var condTokRe = regexp.MustCompile(`\s*("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|==|!=|>=|<=|>|<|\(|\)|[^\s()=!<>"']+)`)

func (c *condLexer) peek() string {
	if c.i < len(c.toks) {
		return c.toks[c.i]
	}
	return ""
}

func (c *condLexer) next() string {
	t := c.peek()
	c.i++
	return t
}

func (c *condLexer) fail(format string, args ...any) {
	panic(perr(c.src, c.pos, format, args...))
}

// parseCondition converts a condition to a Handlebars expression; a plain
// value comes back bare (x), anything else as a subexpression ((eq x 1)).
func parseCondition(src string, pos int, s string, ref func(string) (string, error)) (out string, err error) {
	c := &condLexer{src: src, pos: pos, ref: ref}
	rest := strings.TrimSpace(s)
	for rest != "" {
		m := condTokRe.FindStringSubmatchIndex(rest)
		if m == nil || m[0] != 0 {
			return "", perr(src, pos, "cannot read the condition %q", s)
		}
		c.toks = append(c.toks, rest[m[2]:m[3]])
		rest = strings.TrimSpace(rest[m[1]:])
	}
	if len(c.toks) == 0 {
		return "", perr(src, pos, "empty condition")
	}
	defer func() {
		if v := recover(); v != nil {
			if pe, ok := v.(*ParseError); ok {
				err = pe
				return
			}
			panic(v)
		}
	}()
	out = c.or()
	if c.i < len(c.toks) {
		c.fail("unexpected %q in condition %q", c.peek(), s)
	}
	return out, nil
}

func (c *condLexer) or() string {
	l := c.and()
	for strings.EqualFold(c.peek(), "or") || c.peek() == "||" {
		c.next()
		l = "(or " + l + " " + c.and() + ")"
	}
	return l
}

func (c *condLexer) and() string {
	l := c.not()
	for strings.EqualFold(c.peek(), "and") || c.peek() == "&&" {
		c.next()
		l = "(and " + l + " " + c.not() + ")"
	}
	return l
}

func (c *condLexer) not() string {
	if strings.EqualFold(c.peek(), "not") || c.peek() == "!" {
		c.next()
		return "(not " + c.not() + ")"
	}
	return c.cmp()
}

var condOps = map[string]string{"==": "eq", "!=": "ne", ">": "gt", "<": "lt", ">=": "gte", "<=": "lte"}

func (c *condLexer) cmp() string {
	l := c.operand()
	if op, ok := condOps[c.peek()]; ok {
		c.next()
		return "(" + op + " " + l + " " + c.operand() + ")"
	}
	return l
}

func (c *condLexer) operand() string {
	t := c.next()
	switch {
	case t == "":
		c.fail("incomplete condition")
	case t == "(":
		inner := c.or()
		if c.next() != ")" {
			c.fail("unclosed ( in condition")
		}
		return inner
	case t[0] == '"' || t[0] == '\'':
		v := t[1 : len(t)-1]
		v = strings.NewReplacer(`\"`, `"`, `\'`, "'", `\\`, `\`).Replace(v)
		lit, ok := hbString(v)
		if !ok {
			c.fail("the string %s holds both quote characters", t)
		}
		return lit
	case numberRe.MatchString(t):
		return t
	case t == "true" || t == "false" || t == "True" || t == "False":
		return strings.ToLower(t)
	case t == "null" || t == "None" || t == "none":
		return "null"
	}
	if _, isOp := condOps[t]; isOp || t == ")" {
		c.fail("unexpected %q in condition", t)
	}
	p, err := c.ref(t)
	if err != nil {
		panic(perr(c.src, c.pos, "%s", err.Error()))
	}
	return p
}

type convErr string

func (e convErr) Error() string { return string(e) }

// loopVar maps a reference to a declared loop variable (item.name), and
// Jinja's loop.index0 / loop.first / loop.last to @index / @first / @last.
func loopVar(ref string, loops []string) (string, bool, error) {
	segs := strings.Split(ref, ".")
	if segs[0] == "loop" && len(segs) == 2 {
		switch segs[1] {
		case "index0":
			return "@index", true, nil
		case "first":
			return "@first", true, nil
		case "last":
			return "@last", true, nil
		case "index":
			return "", true, convErr("loop.index (1-based) is not supported; use loop.index0 (0-based)")
		}
	}
	for _, l := range loops {
		if segs[0] == l {
			return hbPath(segs), true, nil
		}
	}
	return "", false, nil
}

// ---------------------------------------------------------------------------
// Jinja-like block tags ({% if %}, {% for %})
// ---------------------------------------------------------------------------

type jinjaDialect struct {
	name string
	// ref converts a variable reference in a condition or loop source.
	ref func(ref string, loops []string) (string, error)
	// output converts the inner text of a {{ }} tag.
	output func(inner string, loops []string) (string, error)
	// legacy converts a [[...]] tag (Mailjet); nil when the language has none.
	legacy func(inner string) (string, bool, error)
}

var forRe = regexp.MustCompile(`^for\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\s+(.+)$`)

func convertJinja(src string, d jinjaDialect) (string, error) {
	var out strings.Builder
	type open struct {
		kind string // if, for
		pos  int
		loop string
	}
	var stack []open
	loops := func() []string {
		var l []string
		for _, o := range stack {
			if o.loop != "" {
				l = append(l, o.loop)
			}
		}
		return l
	}
	i := 0
	for i < len(src) {
		j := i
		for j < len(src) {
			if strings.HasPrefix(src[j:], "{%") || strings.HasPrefix(src[j:], "{{") || (d.legacy != nil && strings.HasPrefix(src[j:], "[[")) {
				break
			}
			j++
		}
		out.WriteString(src[i:j])
		if j >= len(src) {
			break
		}
		pos := j
		switch {
		case strings.HasPrefix(src[j:], "{%"):
			end := strings.Index(src[j:], "%}")
			if end < 0 {
				return "", perr(src, pos, "unclosed {%% (missing %%})")
			}
			inner := src[j+2 : j+end]
			i = j + end + 2
			stripL, stripR := strings.HasPrefix(inner, "-"), strings.HasSuffix(inner, "-")
			inner = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(inner, "-"), "-"))
			l, r := "{{", "}}"
			if stripL {
				l = "{{~"
			}
			if stripR {
				r = "~}}"
			}
			kw := inner
			if k := strings.IndexAny(inner, " \t\r\n"); k >= 0 {
				kw = inner[:k]
			}
			rest := strings.TrimSpace(strings.TrimPrefix(inner, kw))
			ref := func(s string) (string, error) { return d.ref(s, loops()) }
			switch kw {
			case "if":
				cond, err := parseCondition(src, pos, rest, ref)
				if err != nil {
					return "", err
				}
				out.WriteString(l + "#if " + cond + r)
				stack = append(stack, open{kind: "if", pos: pos})
			case "elif", "elseif":
				if len(stack) == 0 || stack[len(stack)-1].kind != "if" {
					return "", perr(src, pos, "{%% %s %%} outside {%% if %%}", kw)
				}
				cond, err := parseCondition(src, pos, rest, ref)
				if err != nil {
					return "", err
				}
				out.WriteString(l + "else if " + cond + r)
			case "else":
				if len(stack) == 0 {
					return "", perr(src, pos, "{%% else %%} outside a block")
				}
				out.WriteString(l + "else" + r)
			case "endif":
				if len(stack) == 0 || stack[len(stack)-1].kind != "if" {
					return "", perr(src, pos, "{%% endif %%} without a matching {%% if %%}")
				}
				stack = stack[:len(stack)-1]
				out.WriteString(l + "/if" + r)
			case "for":
				m := forRe.FindStringSubmatch(inner)
				if m == nil {
					return "", perr(src, pos, "cannot read the loop %q; use {%% for item in list %%}", inner)
				}
				p, err := d.ref(strings.TrimSpace(m[2]), loops())
				if err != nil {
					return "", perr(src, pos, "%s", err.Error())
				}
				out.WriteString(l + "#each " + p + " as |" + m[1] + "|" + r)
				stack = append(stack, open{kind: "for", pos: pos, loop: m[1]})
			case "endfor":
				if len(stack) == 0 || stack[len(stack)-1].kind != "for" {
					return "", perr(src, pos, "{%% endfor %%} without a matching {%% for %%}")
				}
				stack = stack[:len(stack)-1]
				out.WriteString(l + "/each" + r)
			default:
				return "", perr(src, pos, "the %s template tag {%% %s %%} has no send.io equivalent", d.name, kw)
			}
		case strings.HasPrefix(src[j:], "[["):
			end := strings.Index(src[j:], "]]")
			if end < 0 {
				out.WriteString("[[")
				i = j + 2
				continue
			}
			inner := strings.TrimSpace(src[j+2 : j+end])
			conv, ok, err := d.legacy(inner)
			if err != nil {
				return "", perr(src, pos, "%s", err.Error())
			}
			if !ok {
				out.WriteString("[[")
				i = j + 2
				continue
			}
			out.WriteString(conv)
			i = j + end + 2
		default:
			end := strings.Index(src[j:], "}}")
			if end < 0 {
				return "", perr(src, pos, "unclosed {{ (missing }})")
			}
			i = j + end + 2
			inner := src[j+2 : j+end]
			raw := false
			if strings.HasPrefix(inner, "{") && strings.HasPrefix(src[i:], "}") {
				inner, raw = inner[1:], true
				i++
			}
			conv, err := d.output(strings.TrimSpace(inner), loops())
			if err != nil {
				return "", perr(src, pos, "%s", err.Error())
			}
			if raw && strings.HasPrefix(conv, "{{") && !strings.HasPrefix(conv, "{{{") {
				conv = "{" + conv + "}"
			}
			out.WriteString(conv)
		}
	}
	if len(stack) > 0 {
		o := stack[len(stack)-1]
		return "", perr(src, o.pos, "{%% %s %%} is never closed", o.kind)
	}
	return finish(out.String())
}

// filters applies Django/Jinja filters to a Handlebars expression.
func applyFilters(expr string, filters []string, name string) (string, bool, error) {
	raw := false
	for _, f := range filters {
		f = strings.TrimSpace(f)
		fname, arg := f, ""
		if k := strings.Index(f, ":"); k >= 0 {
			fname, arg = strings.TrimSpace(f[:k]), strings.TrimSpace(f[k+1:])
		}
		switch fname {
		case "default", "default_if_none":
			lit, err := literal(arg)
			if err != nil {
				return "", false, err
			}
			expr = "(default " + expr + " " + lit + ")"
		case "upper":
			expr = "(uppercase " + expr + ")"
		case "lower":
			expr = "(lowercase " + expr + ")"
		case "length":
			expr = "(length " + expr + ")"
		case "safe":
			raw = true
		default:
			return "", false, convErr("the " + name + " filter \"" + fname + "\" has no send.io equivalent (default, upper, lower, length and safe convert)")
		}
	}
	return expr, raw, nil
}

func literal(arg string) (string, error) {
	if arg == "" {
		return "", convErr("the default filter needs a value, as in default:\"text\"")
	}
	if numberRe.MatchString(arg) {
		return arg, nil
	}
	if (arg[0] == '"' || arg[0] == '\'') && len(arg) >= 2 && arg[len(arg)-1] == arg[0] {
		arg = arg[1 : len(arg)-1]
	}
	lit, ok := hbString(arg)
	if !ok {
		return "", convErr("the default value holds both quote characters")
	}
	return lit, nil
}

// splitFilters splits "a | f:"x|y" | g" at the pipes outside quotes.
func splitFilters(s string) []string {
	var parts []string
	var quote byte
	start := 0
	for k := 0; k < len(s); k++ {
		c := s[k]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '|':
			parts = append(parts, s[start:k])
			start = k + 1
		}
	}
	return append(parts, s[start:])
}

// unwrap turns a bare subexpression "(x)" into a tag body "x".
func unwrap(expr string) string {
	if strings.HasPrefix(expr, "(") && strings.HasSuffix(expr, ")") {
		return expr[1 : len(expr)-1]
	}
	return expr
}

// ---------------------------------------------------------------------------
// Mailjet
// ---------------------------------------------------------------------------

var (
	mjRefRe  = regexp.MustCompile(`^var:([A-Za-z0-9_.\-]+)$`)
	mjVarTag = regexp.MustCompile(`^(var|data)\s*:\s*([A-Za-z0-9_.\-]+)\s*(?::\s*("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s]*))?$`)
)

func mjContactKey(k string) string {
	k = strings.ToLower(k)
	switch k {
	case "firstname":
		return "first_name"
	case "lastname":
		return "last_name"
	}
	return k
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return strings.NewReplacer(`\"`, `"`, `\'`, "'", `\\`, `\`).Replace(s[1 : len(s)-1])
	}
	return s
}

// FromMailjet converts the Mailjet template language: {{var:x}},
// {{var:x:"default"}}, [[var:x]], {{data:x:"default"}} and [[data:x]]
// (contact properties), [[UNSUB_LINK_xx]], [[EMAIL_TO]], {% if %} / {% elseif %}
// / {% else %} / {% endif %} with comparisons, and, or, not, and
// {% for item in var:list %} ... {% endfor %}. Variables lose their var:
// prefix ({{var:order.id}} is {{order.id}}).
func FromMailjet(src string) (string, error) {
	mjVar := func(name, def string, raw bool) (string, error) {
		p := hbPath(strings.Split(name, "."))
		o, c := "{{", "}}"
		if raw {
			o, c = "{{{", "}}}"
		}
		if def == "" {
			return o + p + c, nil
		}
		lit, ok := hbString(unquote(def))
		if !ok {
			return "", convErr("the default value holds both quote characters")
		}
		return o + "default " + p + " " + lit + c, nil
	}
	conv := func(inner string, raw bool, loops []string) (string, error) {
		if m := mjVarTag.FindStringSubmatch(inner); m != nil {
			if m[1] == "data" {
				tag, ok := contactTag(mjContactKey(m[2]), unquote(m[3]), true)
				if !ok {
					return "", convErr("a contact property default cannot contain {, } or |")
				}
				return tag, nil
			}
			return mjVar(m[2], m[3], raw)
		}
		parts := splitFilters(inner)
		if p, ok, err := loopVar(strings.TrimSpace(parts[0]), loops); ok {
			if err != nil {
				return "", err
			}
			expr, safe, err := applyFilters(p, parts[1:], "Mailjet")
			if err != nil {
				return "", err
			}
			if raw || safe {
				return "{{{" + unwrap(expr) + "}}}", nil
			}
			return "{{" + unwrap(expr) + "}}", nil
		}
		return "", convErr("the Mailjet tag {{" + inner + "}} has no send.io equivalent; use {{var:name}}, {{var:name:\"default\"}} or {{data:property}}")
	}
	return convertJinja(src, jinjaDialect{
		name: "Mailjet",
		ref: func(ref string, loops []string) (string, error) {
			if m := mjRefRe.FindStringSubmatch(ref); m != nil {
				return hbPath(strings.Split(m[1], ".")), nil
			}
			if p, ok, err := loopVar(ref, loops); ok {
				return p, err
			}
			if strings.HasPrefix(ref, "data:") {
				return "", convErr("conditions and loops on contact properties (" + ref + ") are not supported; pass the value as a var")
			}
			return "", convErr("unknown Mailjet variable " + ref + "; use var:name")
		},
		output: func(inner string, loops []string) (string, error) { return conv(inner, false, loops) },
		legacy: func(inner string) (string, bool, error) {
			switch {
			case regexp.MustCompile(`^UNSUB_LINK(?:_[A-Za-z]{2}(?:_[A-Za-z]{2})?)?$`).MatchString(inner):
				return "{{{unsubscribe_url}}}", true, nil
			case inner == "EMAIL_TO":
				return "{{{contact.email}}}", true, nil
			case mjVarTag.MatchString(inner):
				s, err := conv(inner, true, nil)
				return s, true, err
			case regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_:]*$`).MatchString(inner):
				return "", false, convErr("the Mailjet tag [[" + inner + "]] has no send.io equivalent")
			}
			return "", false, nil
		},
	})
}

// ---------------------------------------------------------------------------
// Brevo
// ---------------------------------------------------------------------------

func brevoContactKey(k string) string {
	switch strings.ToUpper(k) {
	case "FIRSTNAME":
		return "first_name"
	case "LASTNAME":
		return "last_name"
	case "EMAIL":
		return "email"
	}
	return k
}

var brevoParamRef = regexp.MustCompile(`^params\.([A-Za-z0-9_.\-]+)$`)

// FromBrevo converts Brevo's template language: {{ params.x }} (the params.
// prefix is dropped: {{ params.order.id }} is {{order.id}}), filters
// default:"y", upper, lower, length and safe, {{ contact.FIRSTNAME }} and
// {{ unsubscribe }}, {% if %} / {% elif %} / {% else %} / {% endif %} with
// comparisons, and, or, not, and {% for item in params.list %} ... {% endfor %}.
func FromBrevo(src string) (string, error) {
	ref := func(r string, loops []string) (string, error) {
		if m := brevoParamRef.FindStringSubmatch(r); m != nil {
			return hbPath(strings.Split(m[1], ".")), nil
		}
		if p, ok, err := loopVar(r, loops); ok {
			return p, err
		}
		if strings.HasPrefix(r, "contact.") {
			return "", convErr("conditions and loops on contact attributes (" + r + ") are not supported; pass the value in params")
		}
		return "", convErr("unknown Brevo variable " + r + "; use params.NAME")
	}
	return convertJinja(src, jinjaDialect{
		name: "Brevo",
		ref:  ref,
		output: func(inner string, loops []string) (string, error) {
			parts := splitFilters(inner)
			head := strings.TrimSpace(parts[0])
			if head == "unsubscribe" || head == "mirror" {
				if head == "mirror" {
					return "", convErr("{{ mirror }} (the web version link) has no send.io equivalent")
				}
				return "{{{unsubscribe_url}}}", nil
			}
			if strings.HasPrefix(head, "contact.") {
				fb := ""
				for _, f := range parts[1:] {
					f = strings.TrimSpace(f)
					if !strings.HasPrefix(f, "default") {
						return "", convErr("only the default filter works on contact attributes")
					}
					fb = unquote(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(f, "default"), ":")))
				}
				tag, ok := contactTag(brevoContactKey(strings.TrimPrefix(head, "contact.")), fb, false)
				if !ok {
					return "", convErr("a contact attribute default cannot contain {, } or |")
				}
				return tag, nil
			}
			p, err := ref(head, loops)
			if err != nil {
				return "", err
			}
			expr, raw, err := applyFilters(p, parts[1:], "Brevo")
			if err != nil {
				return "", err
			}
			if raw {
				return "{{{" + unwrap(expr) + "}}}", nil
			}
			return "{{" + unwrap(expr) + "}}", nil
		},
	})
}

// ---------------------------------------------------------------------------
// Mailchimp (Mandrill merge language "mailchimp")
// ---------------------------------------------------------------------------

var (
	mcTagRe  = regexp.MustCompile(`\*\|(.*?)\|\*`)
	mcNameRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	mcCondRe = regexp.MustCompile(`^\s*([A-Za-z0-9_]+)\s*(?:(=|!=|<>|>=|<=|>|<)\s*(.*?))?\s*$`)
)

// FromMailchimp converts Mailchimp merge tags: *|NAME|* and *|HTML:NAME|*
// (both inserted as-is, {{{NAME}}}, as Mandrill does), *|IF:NAME|*,
// *|IF:NAME=value|* (and != > < >= <=), *|IFNOT:NAME|*, *|ELSEIF:...|*,
// *|ELSE:|* and *|END:IF|*. Other system tags (*|UNSUB|* aside, which becomes
// the unsubscribe URL) must be translated before converting. Names keep their
// case; Mandrill callers should pass variables in the case the template uses.
func FromMailchimp(src string) (string, error) {
	var out strings.Builder
	type open struct {
		kind string
		pos  int
	}
	var stack []open
	last := 0
	for _, m := range mcTagRe.FindAllStringSubmatchIndex(src, -1) {
		out.WriteString(src[last:m[0]])
		last = m[1]
		pos := m[0]
		tag := strings.TrimSpace(src[m[2]:m[3]])
		upper := strings.ToUpper(tag)
		kw, arg := upper, ""
		if k := strings.Index(tag, ":"); k >= 0 {
			kw, arg = strings.ToUpper(strings.TrimSpace(tag[:k])), tag[k+1:]
		}
		cond := func() (string, error) {
			c := mcCondRe.FindStringSubmatch(arg)
			if c == nil {
				return "", perr(src, pos, "cannot read the merge condition *|%s|*", tag)
			}
			if c[2] == "" {
				return c[1], nil
			}
			op := map[string]string{"=": "eq", "!=": "ne", "<>": "ne", ">": "gt", "<": "lt", ">=": "gte", "<=": "lte"}[c[2]]
			v := unquote(strings.TrimSpace(c[3]))
			lit := v
			if !numberRe.MatchString(v) {
				var ok bool
				if lit, ok = hbString(v); !ok {
					return "", perr(src, pos, "the value in *|%s|* holds both quote characters", tag)
				}
			}
			return "(" + op + " " + c[1] + " " + lit + ")", nil
		}
		switch {
		case kw == "IF" || kw == "IFNOT":
			c, err := cond()
			if err != nil {
				return "", err
			}
			if kw == "IF" {
				out.WriteString("{{#if " + c + "}}")
				stack = append(stack, open{"if", pos})
			} else {
				out.WriteString("{{#unless " + c + "}}")
				stack = append(stack, open{"unless", pos})
			}
		case kw == "ELSEIF":
			if len(stack) == 0 {
				return "", perr(src, pos, "*|ELSEIF:|* outside *|IF:|*")
			}
			c, err := cond()
			if err != nil {
				return "", err
			}
			out.WriteString("{{else if " + c + "}}")
		case kw == "ELSE":
			if len(stack) == 0 {
				return "", perr(src, pos, "*|ELSE:|* outside *|IF:|*")
			}
			out.WriteString("{{else}}")
		case kw == "END" && strings.EqualFold(strings.TrimSpace(arg), "IF"):
			if len(stack) == 0 {
				return "", perr(src, pos, "*|END:IF|* without a matching *|IF:|*")
			}
			out.WriteString("{{/" + stack[len(stack)-1].kind + "}}")
			stack = stack[:len(stack)-1]
		case kw == "HTML" && mcNameRe.MatchString(strings.TrimSpace(arg)):
			out.WriteString("{{{" + strings.TrimSpace(arg) + "}}}")
		case upper == "UNSUB":
			out.WriteString("{{{unsubscribe_url}}}")
		case mcNameRe.MatchString(tag):
			out.WriteString("{{{" + tag + "}}}")
		default:
			return "", perr(src, pos, "the merge tag *|%s|* has no send.io equivalent", tag)
		}
	}
	out.WriteString(src[last:])
	if len(stack) > 0 {
		return "", perr(src, stack[len(stack)-1].pos, "*|IF:|* is never closed (missing *|END:IF|*)")
	}
	return finish(out.String())
}

// ---------------------------------------------------------------------------
// Postmark Mustachio
// ---------------------------------------------------------------------------

// FromMustachio converts Postmark's Mustachio: values, dotted paths, ../,
// comments, {{#each}}, {{#Key}} sections (kept: Handlebars sections behave
// the same on objects, lists and booleans), {{^Key}} inverted sections
// (become {{#unless Key}}), {{#if}}, {{#unless}} and {{#with}}. Partials and
// layouts ({{> name}}, {{{@content}}}) fail.
//
// One difference remains: inside a section Mustachio also looks names up in
// the enclosing scopes, while Handlebars only reads the current one, so a
// root-level value used inside {{#each}} must be written {{../name}} or
// {{@root.name}}.
func FromMustachio(src string) (string, error) {
	var out strings.Builder
	type open struct {
		name, close string
		pos         int
	}
	var stack []open
	i := 0
	for i < len(src) {
		j := strings.Index(src[i:], "{{")
		if j < 0 {
			out.WriteString(src[i:])
			break
		}
		j += i
		out.WriteString(src[i:j])
		pos := j
		closer := "}}"
		if strings.HasPrefix(src[j:], "{{{") {
			closer = "}}}"
		}
		end := strings.Index(src[j:], closer)
		if end < 0 {
			return "", perr(src, pos, "unclosed tag (missing %s)", closer)
		}
		tag := src[j : j+end+len(closer)]
		i = j + end + len(closer)
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(tag, "{{"), "{"), "}}"))
		if closer == "}}}" {
			inner = strings.TrimSpace(strings.TrimSuffix(inner, "}"))
			if strings.HasPrefix(inner, "@content") {
				return "", perr(src, pos, "Postmark layouts ({{{@content}}}) are not supported; inline the layout")
			}
			out.WriteString(tag)
			continue
		}
		if inner == "" {
			return "", perr(src, pos, "empty tag")
		}
		switch inner[0] {
		case '>':
			return "", perr(src, pos, "Postmark partials ({{> name}}) are not supported; inline the content")
		case '!', '&':
			out.WriteString(tag)
		case '=':
			return "", perr(src, pos, "changing delimiters ({{= =}}) is not supported")
		case '#':
			body := strings.TrimSpace(inner[1:])
			fields := strings.Fields(body)
			if len(fields) == 0 {
				return "", perr(src, pos, "a section needs a name")
			}
			switch fields[0] {
			case "each", "if", "unless", "with":
				if len(fields) != 2 {
					return "", perr(src, pos, "{{#%s}} needs one value", fields[0])
				}
				stack = append(stack, open{name: fields[0], close: fields[0], pos: pos})
			default:
				if len(fields) != 1 {
					return "", perr(src, pos, "helpers are not part of Mustachio: {{%s}}", inner)
				}
				stack = append(stack, open{name: fields[0], close: fields[0], pos: pos})
			}
			out.WriteString("{{#" + body + "}}")
		case '^':
			name := strings.TrimSpace(inner[1:])
			if name == "" || strings.ContainsAny(name, " \t") {
				return "", perr(src, pos, "an inverted section needs one name")
			}
			stack = append(stack, open{name: name, close: "unless", pos: pos})
			out.WriteString("{{#unless " + name + "}}")
		case '/':
			name := strings.TrimSpace(inner[1:])
			if len(stack) == 0 {
				return "", perr(src, pos, "{{/%s}} closes a section that was never opened", name)
			}
			o := stack[len(stack)-1]
			if o.name != name {
				return "", perr(src, pos, "{{/%s}} does not match {{#%s}}", name, o.name)
			}
			stack = stack[:len(stack)-1]
			out.WriteString("{{/" + o.close + "}}")
		default:
			if inner == "else" || strings.HasPrefix(inner, "else ") {
				return "", perr(src, pos, "{{else}} is not part of Mustachio; use {{^name}}")
			}
			if strings.ContainsAny(inner, " \t") {
				return "", perr(src, pos, "helpers are not part of Mustachio: {{%s}}", inner)
			}
			out.WriteString(tag)
		}
	}
	if len(stack) > 0 {
		o := stack[len(stack)-1]
		return "", perr(src, o.pos, "{{#%s}} is never closed", o.name)
	}
	return finish(out.String())
}
