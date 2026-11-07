package tmpl

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// RenderError is a failure while rendering (a helper given a value it cannot
// use); Line and Col point at the tag.
type RenderError struct {
	Line, Col int
	Msg       string
}

func (e *RenderError) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Col, e.Msg)
}

// Options control one render.
type Options struct {
	// Escape HTML-escapes {{x}} output (the HTML part). Off, {{x}} prints
	// the value as-is, like {{{x}}} (subject, plain text).
	Escape bool
	// Keep, when set, leaves a top-level tag exactly as written when its
	// first path segment is missing from the data and Keep(segment) is true.
	// send.io uses it for {{contact.x}} and unsubscribe tags, which per-contact
	// personalization fills after the template renders.
	Keep func(head string) bool
}

// MaxOutput bounds the rendered size of one template.
const MaxOutput = 10 << 20

// Render parses and renders src in one step.
func Render(src string, data any, o Options) (string, error) {
	t, err := Parse(src)
	if err != nil {
		return "", err
	}
	return t.Render(data, o)
}

type frame struct {
	ctx    any
	data   map[string]any
	params map[string]any
	parent *frame
}

type renderer struct {
	o    Options
	sb   strings.Builder
	root *frame
}

type renderPanic struct{ err error }

// Render renders the template with data (maps, slices and scalars as
// encoding/json decodes them; other maps and slices work through reflection).
func (t *Template) Render(data any, o Options) (out string, err error) {
	r := &renderer{o: o}
	r.root = &frame{ctx: data, data: map[string]any{}}
	r.root.data["root"] = data
	defer func() {
		if v := recover(); v != nil {
			if p, ok := v.(renderPanic); ok {
				err = p.err
				return
			}
			panic(v)
		}
	}()
	r.nodes(t.root, r.root)
	return r.sb.String(), nil
}

func (r *renderer) write(s string) {
	if r.sb.Len()+len(s) > MaxOutput {
		panic(renderPanic{fmt.Errorf("the rendered template is larger than %d MB", MaxOutput>>20)})
	}
	r.sb.WriteString(s)
}

func (r *renderer) fail(line, col int, format string, args ...any) {
	panic(renderPanic{&RenderError{Line: line, Col: col, Msg: fmt.Sprintf(format, args...)}})
}

func (r *renderer) nodes(ns []node, f *frame) {
	for _, n := range ns {
		switch n := n.(type) {
		case *textNode:
			r.write(n.s)
		case *mustacheNode:
			r.mustache(n, f)
		case *blockNode:
			r.block(n, f)
		}
	}
}

func (r *renderer) out(v any, escape bool) {
	s := toString(v)
	if escape && r.o.Escape {
		s = EscapeHTML(s)
	}
	r.write(s)
}

// kept reports whether a top-level path should stay as written (Options.Keep).
func (r *renderer) kept(p pathExpr, f *frame) bool {
	if r.o.Keep == nil || p.data || p.this || p.depth > 0 || len(p.parts) == 0 || !r.o.Keep(p.parts[0]) {
		return false
	}
	if _, ok := r.blockParam(p.parts[0], f); ok {
		return false
	}
	if _, ok := field(f.ctx, p.parts[0]); ok {
		return false
	}
	_, ok := field(r.root.ctx, p.parts[0])
	return !ok
}

func (r *renderer) mustache(n *mustacheNode, f *frame) {
	if n.merge {
		if r.kept(n.path, f) {
			r.write(n.orig)
			return
		}
		v, _ := r.lookup(n.path, f)
		s := toString(v)
		if s == "" {
			s = n.fb
		}
		r.out(s, n.escape)
		return
	}
	c := n.c
	if len(c.params) == 0 && len(c.hash) == 0 && c.head.kind == ePath && r.kept(c.head.path, f) {
		r.write(n.orig)
		return
	}
	r.out(r.evalCall(c, f, n.line, n.col), n.escape)
}

// evalCall evaluates a tag's expression: a helper call or a plain value.
func (r *renderer) evalCall(c *call, f *frame, line, col int) any {
	if len(c.params) == 0 && len(c.hash) == 0 {
		return r.eval(c.head, f, line, col)
	}
	h := helpers[c.simpleName()]
	args := make([]any, len(c.params))
	for i, p := range c.params {
		args[i] = r.eval(p, f, line, col)
	}
	var hash map[string]any
	if len(c.hash) > 0 {
		hash = map[string]any{}
		for _, kv := range c.hash {
			hash[kv.key] = r.eval(kv.val, f, line, col)
		}
	}
	v, err := h.fn(args, hash)
	if err != nil {
		r.fail(line, col, "%s: %v", c.simpleName(), err)
	}
	return v
}

func (r *renderer) eval(e expr, f *frame, line, col int) any {
	switch e.kind {
	case eString:
		return e.str
	case eNumber:
		return e.num
	case eBool:
		return e.b
	case eNull:
		return nil
	case eSub:
		return r.evalCall(e.sub, f, line, col)
	}
	v, _ := r.lookup(e.path, f)
	return v
}

func (r *renderer) blockParam(name string, f *frame) (any, bool) {
	for x := f; x != nil; x = x.parent {
		if v, ok := x.params[name]; ok {
			return v, true
		}
	}
	return nil, false
}

func (r *renderer) lookup(p pathExpr, f *frame) (any, bool) {
	for i := 0; i < p.depth && f.parent != nil; i++ {
		f = f.parent
	}
	parts := p.parts
	var cur any
	switch {
	case p.data:
		if parts[0] == "root" {
			cur = r.root.ctx
		} else {
			v, ok := f.data[parts[0]]
			if !ok {
				return nil, false
			}
			cur = v
		}
		parts = parts[1:]
	case !p.this && p.depth == 0 && len(parts) > 0:
		if v, ok := r.blockParam(parts[0], f); ok {
			cur = v
			parts = parts[1:]
		} else {
			cur = f.ctx
		}
	default:
		cur = f.ctx
	}
	for _, k := range parts {
		v, ok := field(cur, k)
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// field reads key from a map or index from a slice; "length" works on
// slices and strings, as in JavaScript.
func field(v any, key string) (any, bool) {
	switch x := v.(type) {
	case nil:
		return nil, false
	case map[string]any:
		val, ok := x[key]
		return val, ok
	case []any:
		if i, e := strconv.Atoi(key); e == nil {
			if i >= 0 && i < len(x) {
				return x[i], true
			}
			return nil, false
		}
		if key == "length" {
			return float64(len(x)), true
		}
		return nil, false
	case string:
		if key == "length" {
			return float64(len([]rune(x))), true
		}
		return nil, false
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, false
		}
		val := rv.MapIndex(reflect.ValueOf(key).Convert(rv.Type().Key()))
		if !val.IsValid() {
			return nil, false
		}
		return val.Interface(), true
	case reflect.Slice, reflect.Array:
		if i, e := strconv.Atoi(key); e == nil {
			if i >= 0 && i < rv.Len() {
				return rv.Index(i).Interface(), true
			}
			return nil, false
		}
		if key == "length" {
			return float64(rv.Len()), true
		}
	}
	return nil, false
}

// iterate lists the elements of an array (key = index) or the entries of an
// object (keys sorted, since JSON objects arrive unordered); ok=false when v
// is neither.
func iterate(v any) (keys []any, vals []any, ok bool) {
	switch x := v.(type) {
	case []any:
		for i, e := range x {
			keys = append(keys, float64(i))
			vals = append(vals, e)
		}
		return keys, vals, true
	case map[string]any:
		ks := make([]string, 0, len(x))
		for k := range x {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			keys = append(keys, k)
			vals = append(vals, x[k])
		}
		return keys, vals, true
	case nil, string:
		return nil, nil, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			keys = append(keys, float64(i))
			vals = append(vals, rv.Index(i).Interface())
		}
		return keys, vals, true
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, nil, false
		}
		ks := make([]string, 0, rv.Len())
		for _, k := range rv.MapKeys() {
			ks = append(ks, k.String())
		}
		sort.Strings(ks)
		for _, k := range ks {
			keys = append(keys, k)
			vals = append(vals, rv.MapIndex(reflect.ValueOf(k).Convert(rv.Type().Key())).Interface())
		}
		return keys, vals, true
	}
	return nil, nil, false
}

func isArray(v any) bool {
	if _, ok := v.([]any); ok {
		return true
	}
	if v == nil {
		return false
	}
	k := reflect.ValueOf(v).Kind()
	return k == reflect.Slice || k == reflect.Array
}

// isEmpty is Handlebars' isEmpty: undefined, null, false, "" and [].
func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	}
	if isArray(v) {
		return reflect.ValueOf(v).Len() == 0
	}
	return false
}

// Truthy is the #if test: not empty, and not 0 unless includeZero.
func Truthy(v any, includeZero bool) bool {
	if isEmpty(v) {
		return false
	}
	if isNumberType(v) && !includeZero {
		if n, _ := toNumber(v); n == 0 {
			return false
		}
	}
	return true
}

func (r *renderer) withFrame(f *frame, ctx any, bp []string, bpVals []any, data map[string]any) *frame {
	nf := &frame{ctx: ctx, parent: f, data: f.data}
	if data != nil {
		nf.data = make(map[string]any, len(f.data)+len(data))
		for k, v := range f.data {
			nf.data[k] = v
		}
		for k, v := range data {
			nf.data[k] = v
		}
	}
	if len(bp) > 0 {
		nf.params = map[string]any{}
		for i, name := range bp {
			if i < len(bpVals) {
				nf.params[name] = bpVals[i]
			}
		}
	}
	return nf
}

func (r *renderer) each(b *blockNode, f *frame, v any) {
	keys, vals, ok := iterate(v)
	if !ok || len(vals) == 0 {
		r.nodes(b.inverse, f)
		return
	}
	for i := range vals {
		data := map[string]any{"index": float64(i), "key": keys[i], "first": i == 0, "last": i == len(vals)-1}
		r.nodes(b.body, r.withFrame(f, vals[i], b.blockParams, []any{vals[i], keys[i]}, data))
	}
}

func (r *renderer) block(b *blockNode, f *frame) {
	c := b.c
	name := c.simpleName()
	if b.inverted {
		v := r.eval(c.head, f, b.line, b.col)
		if isEmpty(v) {
			r.nodes(b.body, f)
		} else {
			r.nodes(b.inverse, f)
		}
		return
	}
	if _, ok := blockHelpers[name]; ok {
		v := r.eval(c.params[0], f, b.line, b.col)
		includeZero := false
		for _, kv := range c.hash {
			if kv.key == "includeZero" {
				includeZero = Truthy(r.eval(kv.val, f, b.line, b.col), true)
			}
		}
		switch name {
		case "if":
			if Truthy(v, includeZero) {
				r.nodes(b.body, f)
			} else {
				r.nodes(b.inverse, f)
			}
		case "unless":
			if !Truthy(v, includeZero) {
				r.nodes(b.body, f)
			} else {
				r.nodes(b.inverse, f)
			}
		case "with":
			if isEmpty(v) {
				r.nodes(b.inverse, f)
			} else {
				r.nodes(b.body, r.withFrame(f, v, b.blockParams, []any{v}, nil))
			}
		case "each":
			r.each(b, f, v)
		}
		return
	}
	if len(c.params) > 0 || len(c.hash) > 0 {
		// A value helper as a block: {{#eq a b}}yes{{else}}no{{/eq}}.
		if Truthy(r.evalCall(c, f, b.line, b.col), false) {
			r.nodes(b.body, f)
		} else {
			r.nodes(b.inverse, f)
		}
		return
	}
	// A Mustache-style section on a value.
	v := r.eval(c.head, f, b.line, b.col)
	switch {
	case v == true:
		r.nodes(b.body, f)
	case isEmpty(v):
		r.nodes(b.inverse, f)
	case isArray(v):
		r.each(b, f, v)
	default:
		r.nodes(b.body, r.withFrame(f, v, b.blockParams, []any{v}, nil))
	}
}

// ---------------------------------------------------------------------------
// Values
// ---------------------------------------------------------------------------

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;", "`", "&#x60;", "=", "&#x3D;")

// EscapeHTML escapes like Handlebars' escapeExpression.
func EscapeHTML(s string) string {
	if !strings.ContainsAny(s, "&<>\"'`=") {
		return s
	}
	return htmlEscaper.Replace(s)
}

func formatNumber(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 0) {
		if f > 0 {
			return "Infinity"
		}
		return "-Infinity"
	}
	a := math.Abs(f)
	if a != 0 && (a >= 1e21 || a < 1e-6) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// toString prints a value the way Handlebars (JavaScript) does.
func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return formatNumber(x)
	case float32:
		return formatNumber(float64(x))
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return x.String()
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = toString(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	case fmt.Stringer:
		return x.String()
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10)
	case reflect.Slice, reflect.Array:
		parts := make([]string, rv.Len())
		for i := range parts {
			parts[i] = toString(rv.Index(i).Interface())
		}
		return strings.Join(parts, ",")
	case reflect.Map, reflect.Struct:
		return "[object Object]"
	}
	return fmt.Sprint(v)
}

func isNumberType(v any) bool {
	switch v.(type) {
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return true
	}
	return false
}

// toNumber converts numbers and numeric strings.
func toNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, e := x.Float64()
		return f, e == nil
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, false
		}
		f, e := strconv.ParseFloat(s, 64)
		return f, e == nil
	case bool, nil:
		return 0, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}
