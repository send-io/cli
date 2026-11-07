// Package tmpl is send.io's Handlebars-compatible template engine.
//
// It renders the subset of Handlebars that email providers' template
// languages use (SendGrid dynamic templates, Mandrill and Mailgun handlebars,
// Postmark Mustachio, and Mailjet, Brevo and Mailchimp logic after
// conversion):
//
//	{{path}}  {{{path}}}  {{&path}}         escaped and raw output
//	a.b.c  a.[0]  this  .  ../x  @root.x    paths
//	@index @key @first @last                loop data
//	{{#if}} {{else}} {{else if}} {{/if}}    conditionals (and #unless)
//	{{#each}} (arrays and objects)          loops, with block params (as |item i|)
//	{{#with}}                               context change
//	{{#section}} {{^section}}               Mustache-style sections
//	{{!-- comment --}} {{! comment }}       comments
//	{{~ ~}}                                 whitespace control
//	(helper args)                           subexpressions
//
// Helpers: eq ne lt gt lte gte (and SendGrid's equals notEquals lessThan
// greaterThan), and or not, length, lookup, formatDate, default, uppercase,
// lowercase and insert. A value helper used as a block ({{#eq a b}}...)
// renders its body when the result is truthy.
//
// Parsing is strict: malformed templates, unknown helpers, wrong argument
// counts, partials and decorators fail with a *ParseError carrying the line
// and column. Missing values render as empty strings, as in Handlebars.
//
// One send.io extension keeps the simple engine's merge tags working:
// {{path|fallback}} and {{{path|fallback}}} render the fallback when the value
// is missing or empty.
package tmpl

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ParseError is a malformed template; Line and Col are 1-based and point at
// the start of the offending tag.
type ParseError struct {
	Line, Col int
	Msg       string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Col, e.Msg)
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

type tokKind int

const (
	tText     tokKind = iota
	tEscaped          // {{x}}
	tRaw              // {{{x}}} or {{&x}}
	tOpen             // {{#x}}
	tInverted         // {{^x}}
	tClose            // {{/x}}
	tElse             // {{else}}, {{else x}}, {{^}}
	tComment          // {{! }} {{!-- --}}
)

type token struct {
	kind        tokKind
	text        string // tText: the text; others: the inner content (sigil removed)
	orig        string // the whole tag as written
	pos         int    // byte offset of the tag in the source
	stripL      bool   // {{~
	stripR      bool   // ~}}
	line, col   int
	standalone  bool
	blockParams []string
}

func lineCol(src string, pos int) (int, int) {
	line := 1 + strings.Count(src[:pos], "\n")
	col := pos - strings.LastIndex(src[:pos], "\n")
	return line, col
}

func perr(src string, pos int, format string, args ...any) *ParseError {
	l, c := lineCol(src, pos)
	return &ParseError{Line: l, Col: c, Msg: fmt.Sprintf(format, args...)}
}

// lex splits src into text and tags.
func lex(src string) ([]token, error) {
	var toks []token
	text := strings.Builder{}
	textStart := 0
	flush := func() {
		if text.Len() > 0 {
			toks = append(toks, token{kind: tText, text: text.String(), pos: textStart})
			text.Reset()
		}
	}
	i := 0
	for i < len(src) {
		j := strings.Index(src[i:], "{{")
		if j < 0 {
			if text.Len() == 0 {
				textStart = i
			}
			text.WriteString(src[i:])
			break
		}
		j += i
		// \{{ is a literal {{ (the backslash is dropped); \\{{ is a
		// literal backslash before a real tag.
		if j > 0 && src[j-1] == '\\' && !(j > 1 && src[j-2] == '\\') {
			if text.Len() == 0 {
				textStart = i
			}
			text.WriteString(src[i : j-1])
			text.WriteString("{{")
			i = j + 2
			continue
		}
		if text.Len() == 0 {
			textStart = i
		}
		if j > 1 && src[j-1] == '\\' && src[j-2] == '\\' {
			text.WriteString(src[i : j-1])
		} else {
			text.WriteString(src[i:j])
		}
		flush()
		tok, end, err := lexTag(src, j)
		if err != nil {
			return nil, err
		}
		toks = append(toks, tok)
		i = end
		textStart = i
	}
	flush()
	return toks, nil
}

var blockParamsRe = regexp.MustCompile(`\s+as\s+\|\s*([^|]*?)\s*\|\s*$`)

// lexTag reads the tag starting at src[start:] ("{{").
func lexTag(src string, start int) (token, int, error) {
	t := token{pos: start}
	t.line, t.col = lineCol(src, start)
	p := start + 2
	if p < len(src) && src[p] == '~' {
		t.stripL = true
		p++
	}
	// Comments may contain }} when written {{!-- --}}.
	if strings.HasPrefix(src[p:], "!--") {
		for k := p + 3; ; {
			idx := strings.Index(src[k:], "--")
			if idx < 0 {
				return t, 0, perr(src, start, "unclosed comment: {{!-- needs a matching --}}")
			}
			q := k + idx + 2
			if strings.HasPrefix(src[q:], "}}") {
				t.kind, t.text, t.orig = tComment, src[p+3:q-2], src[start:q+2]
				return t, q + 2, nil
			}
			if strings.HasPrefix(src[q:], "~}}") {
				t.kind, t.text, t.orig, t.stripR = tComment, src[p+3:q-2], src[start:q+3], true
				return t, q + 3, nil
			}
			k = k + idx + 1
		}
	}
	if strings.HasPrefix(src[p:], "!") {
		end := strings.Index(src[p:], "}}")
		if end < 0 {
			return t, 0, perr(src, start, "unclosed comment: {{! needs a matching }}")
		}
		q := p + end
		inner := src[p+1 : q]
		if strings.HasSuffix(inner, "~") {
			inner, t.stripR = inner[:len(inner)-1], true
		}
		t.kind, t.text, t.orig = tComment, inner, src[start:q+2]
		return t, q + 2, nil
	}
	triple := p < len(src) && src[p] == '{'
	if triple {
		p++
	}
	closer := "}}"
	if triple {
		closer = "}}}"
	}
	// Find the closer outside string literals.
	q := -1
	var quote byte
	for k := p; k < len(src); k++ {
		c := src[k]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if strings.HasPrefix(src[k:], closer) {
			q = k
			break
		}
		if triple && strings.HasPrefix(src[k:], "}~}}") {
			q, closer, t.stripR = k, "}~}}", true
			break
		}
		if triple && strings.HasPrefix(src[k:], "}}") && !strings.HasPrefix(src[k:], "}}}") {
			return t, 0, perr(src, start, "{{{ must be closed with }}}")
		}
	}
	if q < 0 {
		if quote != 0 {
			return t, 0, perr(src, start, "unterminated string literal in tag")
		}
		return t, 0, perr(src, start, "unclosed tag: %s needs a matching %s", src[start:p], closer)
	}
	inner := src[p:q]
	if strings.HasSuffix(inner, "~") {
		inner, t.stripR = inner[:len(inner)-1], true
	}
	t.orig = src[start : q+len(closer)]
	end := q + len(closer)
	if triple {
		t.kind, t.text = tRaw, inner
		if strings.TrimSpace(inner) == "" {
			return t, 0, perr(src, start, "empty tag")
		}
		return t, end, nil
	}
	trimmed := strings.TrimSpace(inner)
	if trimmed == "" {
		return t, 0, perr(src, start, "empty tag")
	}
	switch trimmed[0] {
	case '#':
		rest := strings.TrimSpace(trimmed[1:])
		if strings.HasPrefix(rest, ">") {
			return t, 0, perr(src, start, "partial blocks ({{#> ...}}) are not supported")
		}
		if strings.HasPrefix(rest, "*") {
			return t, 0, perr(src, start, "decorators ({{#* ...}}) are not supported")
		}
		t.kind, t.text = tOpen, rest
	case '^':
		rest := strings.TrimSpace(trimmed[1:])
		if rest == "" {
			t.kind = tElse
		} else {
			t.kind, t.text = tInverted, rest
		}
	case '/':
		t.kind, t.text = tClose, strings.TrimSpace(trimmed[1:])
		if t.text == "" {
			return t, 0, perr(src, start, "closing tag needs a name")
		}
	case '>':
		return t, 0, perr(src, start, "partials ({{> name}}) are not supported; inline the content")
	case '*':
		return t, 0, perr(src, start, "decorators ({{* ...}}) are not supported")
	case '&':
		t.kind, t.text = tRaw, strings.TrimSpace(trimmed[1:])
		if t.text == "" {
			return t, 0, perr(src, start, "empty tag")
		}
	default:
		if trimmed == "else" {
			t.kind = tElse
		} else if strings.HasPrefix(trimmed, "else ") || strings.HasPrefix(trimmed, "else\t") || strings.HasPrefix(trimmed, "else\n") {
			t.kind, t.text = tElse, strings.TrimSpace(trimmed[4:])
		} else {
			t.kind, t.text = tEscaped, trimmed
		}
	}
	if t.kind == tOpen || t.kind == tInverted || (t.kind == tElse && t.text != "") {
		if m := blockParamsRe.FindStringSubmatchIndex(t.text); m != nil {
			names := strings.Fields(t.text[m[2]:m[3]])
			if len(names) == 0 {
				return t, 0, perr(src, start, "empty block parameters (as | |)")
			}
			for _, n := range names {
				if !identRe.MatchString(n) {
					return t, 0, perr(src, start, "invalid block parameter %q", n)
				}
			}
			t.blockParams = names
			t.text = strings.TrimSpace(t.text[:m[0]])
		}
	}
	return t, end, nil
}

var identRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$-]*$`)

// ---------------------------------------------------------------------------
// Whitespace: standalone lines and ~
// ---------------------------------------------------------------------------

func isBlockish(k tokKind) bool {
	return k == tOpen || k == tInverted || k == tClose || k == tElse || k == tComment
}

// whitespace applies Handlebars' standalone-line rule (a block tag or
// comment alone on its line takes the line with it) and ~ stripping.
func whitespace(toks []token) {
	for i := range toks {
		t := &toks[i]
		if !isBlockish(t.kind) {
			continue
		}
		// Before: start of template, or text whose last line is blank.
		before := false
		if i == 0 {
			before = true
		} else if toks[i-1].kind == tText {
			s := toks[i-1].text
			last := s[strings.LastIndex(s, "\n")+1:]
			if strings.TrimLeft(last, " \t") == "" && (strings.Contains(s, "\n") || i == 1) {
				before = true
			}
		}
		if !before {
			continue
		}
		after := false
		if i == len(toks)-1 {
			after = true
		} else if toks[i+1].kind == tText {
			s := strings.TrimLeft(toks[i+1].text, " \t")
			if s == "" && i+1 == len(toks)-1 || strings.HasPrefix(s, "\n") || strings.HasPrefix(s, "\r\n") {
				after = true
			}
		}
		if after {
			t.standalone = true
		}
	}
	for i := range toks {
		t := toks[i]
		if !t.standalone {
			continue
		}
		if i > 0 && toks[i-1].kind == tText {
			s := toks[i-1].text
			toks[i-1].text = s[:strings.LastIndex(s, "\n")+1]
		}
		if i+1 < len(toks) && toks[i+1].kind == tText {
			s := strings.TrimLeft(toks[i+1].text, " \t")
			if strings.HasPrefix(s, "\r\n") {
				s = s[2:]
			} else if strings.HasPrefix(s, "\n") {
				s = s[1:]
			}
			toks[i+1].text = s
		}
	}
	for i := range toks {
		if toks[i].kind == tText {
			continue
		}
		if toks[i].stripL && i > 0 && toks[i-1].kind == tText {
			toks[i-1].text = strings.TrimRight(toks[i-1].text, " \t\r\n")
		}
		if toks[i].stripR && i+1 < len(toks) && toks[i+1].kind == tText {
			toks[i+1].text = strings.TrimLeft(toks[i+1].text, " \t\r\n")
		}
	}
}

// ---------------------------------------------------------------------------
// Expressions
// ---------------------------------------------------------------------------

type exprKind int

const (
	ePath exprKind = iota
	eString
	eNumber
	eBool
	eNull
	eSub
)

type pathExpr struct {
	data  bool // @index, @root.x
	depth int  // ../
	this  bool // this. / ./ prefix (no block-param or helper lookup)
	parts []string
	orig  string
}

type expr struct {
	kind exprKind
	path pathExpr
	str  string
	num  float64
	b    bool
	sub  *call
}

type hashPair struct {
	key string
	val expr
}

type call struct {
	head   expr
	params []expr
	hash   []hashPair
}

// simpleName is the helper name a head could be: a bare identifier.
func (c *call) simpleName() string {
	if c.head.kind != ePath || c.head.path.data || c.head.path.depth > 0 || c.head.path.this || len(c.head.path.parts) != 1 {
		return ""
	}
	return c.head.path.parts[0]
}

type exprLexer struct {
	src string // the whole template (for error positions)
	s   string
	i   int
	tag int // offset of the tag
}

func (l *exprLexer) fail(format string, args ...any) {
	panic(perr(l.src, l.tag, format, args...))
}

func (l *exprLexer) skipSpace() {
	for l.i < len(l.s) && strings.ContainsRune(" \t\r\n", rune(l.s[l.i])) {
		l.i++
	}
}

// word reads a path or literal token; brackets [..] may hold anything but ].
func (l *exprLexer) word() string {
	start := l.i
	for l.i < len(l.s) {
		c := l.s[l.i]
		if c == '[' {
			end := strings.IndexByte(l.s[l.i:], ']')
			if end < 0 {
				l.fail("unclosed [ in path")
			}
			l.i += end + 1
			continue
		}
		if strings.ContainsRune(" \t\r\n()=\"'|", rune(c)) {
			break
		}
		l.i++
	}
	return l.s[start:l.i]
}

var numberRe = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

func (l *exprLexer) operand() expr {
	l.skipSpace()
	if l.i >= len(l.s) {
		l.fail("expected an expression")
	}
	c := l.s[l.i]
	switch {
	case c == '(':
		l.i++
		sub := l.call(true)
		l.skipSpace()
		if l.i >= len(l.s) || l.s[l.i] != ')' {
			l.fail("unclosed ( in subexpression")
		}
		l.i++
		return expr{kind: eSub, sub: sub}
	case c == '"' || c == '\'':
		end := strings.IndexByte(l.s[l.i+1:], c)
		if end < 0 {
			l.fail("unterminated string literal")
		}
		v := l.s[l.i+1 : l.i+1+end]
		l.i += end + 2
		return expr{kind: eString, str: v}
	case c == ')':
		l.fail("unexpected )")
	case c == '=':
		l.fail("unexpected =")
	case c == '|':
		l.fail("unexpected |")
	}
	w := l.word()
	if w == "" {
		l.fail("unexpected character %q", string(c))
	}
	switch w {
	case "true", "false":
		return expr{kind: eBool, b: w == "true"}
	case "null", "undefined":
		return expr{kind: eNull}
	}
	if numberRe.MatchString(w) {
		n, _ := strconv.ParseFloat(w, 64)
		return expr{kind: eNumber, num: n}
	}
	return expr{kind: ePath, path: l.parsePath(w)}
}

func (l *exprLexer) parsePath(w string) pathExpr {
	p := pathExpr{orig: w}
	s := w
	if strings.HasPrefix(s, "@") {
		p.data = true
		s = s[1:]
	}
	for s == ".." || strings.HasPrefix(s, "../") {
		p.depth++
		s = strings.TrimPrefix(s[2:], "/")
	}
	switch {
	case s == "." || s == "this":
		p.this, s = true, ""
	case strings.HasPrefix(s, "./"):
		p.this, s = true, s[2:]
	case strings.HasPrefix(s, "this.") || strings.HasPrefix(s, "this/"):
		p.this, s = true, s[5:]
	}
	if s == "" {
		if !p.this && p.depth == 0 {
			l.fail("invalid path %q", w)
		}
		if p.data {
			l.fail("invalid data path %q", w)
		}
		return p
	}
	cur := strings.Builder{}
	lit := false
	flush := func() {
		seg := cur.String()
		if !lit && (seg == "" || seg == ".." || seg == "." || seg == "this") {
			l.fail("invalid path %q", w)
		}
		p.parts = append(p.parts, seg)
		cur.Reset()
		lit = false
	}
	for k := 0; k < len(s); k++ {
		c := s[k]
		switch c {
		case '[':
			end := strings.IndexByte(s[k:], ']')
			if end < 0 {
				l.fail("unclosed [ in path %q", w)
			}
			cur.WriteString(s[k+1 : k+end])
			lit = true
			k += end
		case '.', '/':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return p
}

// call reads "head param... key=value...".
func (l *exprLexer) call(sub bool) *call {
	c := &call{head: l.operand()}
	if c.head.kind != ePath && c.head.kind != eSub {
		if sub || len(l.s[l.i:]) > 0 && strings.TrimSpace(l.s[l.i:]) != "" {
			l.fail("a helper name must come first, not a literal")
		}
	}
	for {
		l.skipSpace()
		if l.i >= len(l.s) || l.s[l.i] == ')' {
			return c
		}
		// key=value?
		save := l.i
		if l.s[l.i] != '(' && l.s[l.i] != '"' && l.s[l.i] != '\'' {
			w := l.word()
			l.skipSpace()
			if l.i < len(l.s) && l.s[l.i] == '=' && identRe.MatchString(w) {
				l.i++
				c.hash = append(c.hash, hashPair{key: w, val: l.operand()})
				continue
			}
			l.i = save
		}
		if len(c.hash) > 0 {
			l.fail("positional arguments must come before key=value arguments")
		}
		c.params = append(c.params, l.operand())
	}
}

func parseCall(src string, tagPos int, s string) (c *call, err error) {
	defer func() {
		if v := recover(); v != nil {
			if pe, ok := v.(*ParseError); ok {
				err = pe
				return
			}
			panic(v)
		}
	}()
	l := &exprLexer{src: src, s: s, tag: tagPos}
	c = l.call(false)
	l.skipSpace()
	if l.i < len(l.s) {
		l.fail("unexpected %q", l.s[l.i:])
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Tree
// ---------------------------------------------------------------------------

type node interface{}

type textNode struct{ s string }

type mustacheNode struct {
	c      *call
	escape bool
	orig   string
	line   int
	col    int
	merge  bool // {{path|fallback}}
	path   pathExpr
	fb     string
}

type blockNode struct {
	c           *call
	name        string // as written, matched by the closing tag
	inverted    bool   // {{^x}}
	blockParams []string
	body        []node
	inverse     []node
	hasElse     bool
	line, col   int
}

// Template is a parsed template; Render is safe for concurrent use.
type Template struct {
	root []node
}

var mergeRe = regexp.MustCompile(`^\s*([A-Za-z0-9_.]+)\s*\|([^}]*)$`)

// Parse parses src strictly.
func Parse(src string) (*Template, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	whitespace(toks)
	type frame struct {
		b       *blockNode
		chained bool
	}
	root := &blockNode{}
	stack := []frame{{b: root}}
	appendNode := func(n node) {
		f := stack[len(stack)-1].b
		if f.hasElse {
			f.inverse = append(f.inverse, n)
		} else {
			f.body = append(f.body, n)
		}
	}
	for _, t := range toks {
		switch t.kind {
		case tText:
			if t.text != "" {
				appendNode(&textNode{s: t.text})
			}
		case tComment:
		case tEscaped, tRaw:
			n := &mustacheNode{escape: t.kind == tEscaped, orig: t.orig, line: t.line, col: t.col}
			if m := mergeRe.FindStringSubmatch(t.text); m != nil {
				n.merge = true
				l := &exprLexer{src: src, tag: t.pos}
				var pe error
				func() {
					defer func() {
						if v := recover(); v != nil {
							if e, ok := v.(*ParseError); ok {
								pe = e
								return
							}
							panic(v)
						}
					}()
					n.path = l.parsePath(m[1])
				}()
				if pe != nil {
					return nil, pe
				}
				n.fb = strings.TrimSpace(m[2])
				appendNode(n)
				continue
			}
			c, err := parseCall(src, t.pos, t.text)
			if err != nil {
				return nil, err
			}
			if err := checkCall(src, t.pos, c, false); err != nil {
				return nil, err
			}
			n.c = c
			appendNode(n)
		case tOpen, tInverted:
			c, err := parseCall(src, t.pos, t.text)
			if err != nil {
				return nil, err
			}
			if c.head.kind != ePath {
				return nil, perr(src, t.pos, "a block needs a helper or path name")
			}
			if t.kind == tInverted && (len(c.params) > 0 || len(c.hash) > 0) {
				return nil, perr(src, t.pos, "an inverted section {{^name}} takes no arguments")
			}
			if err := checkCall(src, t.pos, c, true); err != nil {
				return nil, err
			}
			b := &blockNode{c: c, name: c.head.path.orig, inverted: t.kind == tInverted, blockParams: t.blockParams, line: t.line, col: t.col}
			appendNode(b)
			stack = append(stack, frame{b: b})
		case tElse:
			if len(stack) == 1 {
				return nil, perr(src, t.pos, "{{else}} outside a block")
			}
			f := stack[len(stack)-1].b
			if f.hasElse {
				return nil, perr(src, t.pos, "a block can only have one {{else}}")
			}
			f.hasElse = true
			if t.text != "" {
				c, err := parseCall(src, t.pos, t.text)
				if err != nil {
					return nil, err
				}
				if c.head.kind != ePath {
					return nil, perr(src, t.pos, "{{else ...}} needs a helper name")
				}
				if err := checkCall(src, t.pos, c, true); err != nil {
					return nil, err
				}
				b := &blockNode{c: c, name: c.head.path.orig, blockParams: t.blockParams, line: t.line, col: t.col}
				f.inverse = append(f.inverse, b)
				stack = append(stack, frame{b: b, chained: true})
			}
		case tClose:
			for len(stack) > 1 && stack[len(stack)-1].chained {
				stack = stack[:len(stack)-1]
			}
			if len(stack) == 1 {
				return nil, perr(src, t.pos, "{{/%s}} closes a block that was never opened", t.text)
			}
			f := stack[len(stack)-1].b
			if f.name != t.text {
				return nil, perr(src, t.pos, "{{/%s}} does not match {{#%s}} opened at line %d, column %d", t.text, f.name, f.line, f.col)
			}
			stack = stack[:len(stack)-1]
		}
	}
	for len(stack) > 1 && stack[len(stack)-1].chained {
		stack = stack[:len(stack)-1]
	}
	if len(stack) > 1 {
		f := stack[len(stack)-1].b
		return nil, &ParseError{Line: f.line, Col: f.col, Msg: fmt.Sprintf("{{#%s}} is never closed (missing {{/%s}})", f.name, f.name)}
	}
	return &Template{root: root.body}, nil
}

// Check reports whether src parses.
func Check(src string) error {
	_, err := Parse(src)
	return err
}

// checkCall validates helper names and argument counts at parse time.
func checkCall(src string, pos int, c *call, block bool) error {
	if c.head.kind == eSub {
		return perr(src, pos, "a tag cannot start with a subexpression")
	}
	name := c.simpleName()
	if block {
		if n, ok := blockHelpers[name]; ok {
			if len(c.params) != n {
				return perr(src, pos, "#%s needs exactly %d argument(s), got %d", name, n, len(c.params))
			}
			return checkArgs(src, pos, c)
		}
	}
	if len(c.params) == 0 && len(c.hash) == 0 {
		return nil // a path (or a section)
	}
	if err := checkHelper(src, pos, c, block); err != nil {
		return err
	}
	return checkArgs(src, pos, c)
}

func checkHelper(src string, pos int, c *call, block bool) error {
	name := c.simpleName()
	h, ok := helpers[name]
	if !ok {
		what := "helper"
		if block {
			what = "block helper"
		}
		if c.head.kind == ePath {
			return perr(src, pos, "unknown %s %q", what, c.head.path.orig)
		}
		return perr(src, pos, "a helper name must come first")
	}
	if len(c.params) < h.min || (h.max >= 0 && len(c.params) > h.max) {
		return perr(src, pos, "%s: %s", name, h.arity())
	}
	return nil
}

func checkArgs(src string, pos int, c *call) error {
	all := append([]expr{}, c.params...)
	for _, h := range c.hash {
		all = append(all, h.val)
	}
	for _, e := range all {
		if e.kind != eSub {
			continue
		}
		if e.sub.head.kind == eSub {
			return perr(src, pos, "a subexpression cannot start with a subexpression")
		}
		if err := checkHelper(src, pos, e.sub, false); err != nil {
			return err
		}
		if err := checkArgs(src, pos, e.sub); err != nil {
			return err
		}
	}
	return nil
}
