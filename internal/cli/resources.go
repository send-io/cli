package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/out"
	"send.io/cli/internal/spec"
)

// Command annotations read by `sendio commands --json`.
const (
	annTool        = "sendio/tool"
	annMethod      = "sendio/method"
	annPath        = "sendio/path"
	annWrite       = "sendio/write"
	annDestructive = "sendio/destructive"
	annPaginated   = "sendio/paginated"
)

// group finds or creates the group command at path under root.
func (a *App) group(root *cobra.Command, path []string) *cobra.Command {
	cur := root
	for i, name := range path {
		var next *cobra.Command
		for _, c := range cur.Commands() {
			if c.Name() == name {
				next = c
				break
			}
		}
		if next == nil {
			key := strings.Join(path[:i+1], " ")
			next = &cobra.Command{Use: name, Short: groupShort[key]}
			cur.AddCommand(next)
		}
		cur = next
	}
	return cur
}

func (a *App) addResourceCommands(root *cobra.Command) {
	for _, rc := range resourceTable {
		if rc.Hand {
			continue
		}
		t, ok := spec.Lookup(rc.Tool)
		if !ok {
			continue // TestEveryToolIsMapped reports it
		}
		parts := strings.Fields(rc.Path)
		parent := a.group(root, parts[:len(parts)-1])
		parent.AddCommand(a.toolCommand(rc, t, parts[len(parts)-1]))
	}
}

// toolBinding ties a command's flags to one API operation.
type toolBinding struct {
	rc    resCmd
	t     spec.Tool
	cmd   *cobra.Command
	flags map[string]string // param → flag name
	strs  map[string]*string
	bools map[string]*bool
	ints  map[string]*int
	lists map[string]*[]string
	objs  map[string]*[]string
	data  string
	all   bool
	// mutate adjusts the request after it is built (tool-specific extras).
	mutate func(req *api.Request, args []string) error
}

func argName(p string) string { return "<" + strings.ReplaceAll(p, "_", "-") + ">" }

func (a *App) toolCommand(rc resCmd, t spec.Tool, verb string) *cobra.Command {
	pp := append(t.PathParams(), rc.Args...)
	use := verb
	for _, p := range pp {
		use += " " + argName(p)
	}
	cmd := &cobra.Command{
		Use:     use,
		Aliases: rc.Aliases,
		Short:   strings.TrimSuffix(t.Description, "."),
		Long:    fmt.Sprintf("%s\n\nAPI: %s %s  (MCP tool: %s)", t.Description, t.Method, t.Path, t.Name),
		Args:    exactArgs(len(pp)),
		Annotations: map[string]string{
			annTool: t.Name, annMethod: t.Method, annPath: t.Path,
		},
	}
	b := a.bindTool(cmd, rc, t)
	if hook, ok := toolHooks[t.Name]; ok {
		hook(a, b)
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return a.runTool(cmd.Context(), b, args)
	}
	return cmd
}

// exactArgs is cobra.ExactArgs with a usage error (exit 2).
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return &api.Error{Exit: api.ExitUsage, Code: "usage_error",
				Message: fmt.Sprintf("%s takes %d argument(s), got %d", cmd.CommandPath(), n, len(args)),
				Hint:    "Usage: " + cmd.UseLine()}
		}
		return nil
	}
}

func isDestructive(rc resCmd, t spec.Tool) bool { return t.Method == "DELETE" || rc.Confirm }

// bindTool registers one flag per non-path parameter.
func (a *App) bindTool(cmd *cobra.Command, rc resCmd, t spec.Tool) *toolBinding {
	b := &toolBinding{rc: rc, t: t, cmd: cmd, flags: map[string]string{}, strs: map[string]*string{}, bools: map[string]*bool{},
		ints: map[string]*int{}, lists: map[string]*[]string{}, objs: map[string]*[]string{}}
	path := map[string]bool{}
	for _, p := range append(t.PathParams(), rc.Args...) {
		path[p] = true
	}
	f := cmd.Flags()
	for _, p := range t.Params {
		if path[p.Name] {
			continue
		}
		name := kebab(p.Name)
		if rn, ok := rc.Flags[p.Name]; ok {
			name = rn
		}
		b.flags[p.Name] = name
		usage := p.Description
		if usage == "" {
			usage = strings.ReplaceAll(p.Name, "_", " ")
		}
		if len(p.Enum) > 0 && !strings.Contains(usage, "|") {
			usage += " (" + strings.Join(p.Enum, "|") + ")"
		}
		if p.Required {
			usage += " [required]"
		}
		switch {
		case p.Type == "boolean":
			b.bools[p.Name] = f.Bool(name, false, usage)
		case p.Type == "integer":
			b.ints[p.Name] = f.Int(name, 0, usage)
		case p.Type == "array" && p.Items == "object":
			b.objs[p.Name] = f.StringArray(name, nil, usage+" (JSON object/array or @file; repeatable)")
		case p.Type == "array":
			b.lists[p.Name] = f.StringSlice(name, nil, usage+" (repeatable or comma-separated)")
		case p.Type == "object":
			b.objs[p.Name] = f.StringArray(name, nil, usage+" (key=value, key:=json, JSON object or @file; repeatable)")
		default:
			if t.Upload == p.Name {
				usage += " (@file or - for stdin)"
			}
			b.strs[p.Name] = f.String(name, "", usage)
		}
	}
	ann := cmd.Annotations
	if api.IsWrite(t.Method) {
		f.StringVar(&b.data, "data", "", "raw JSON request body (JSON, @file.json or - for stdin); flags override its fields")
		ann[annWrite] = "true"
	}
	if isDestructive(rc, t) {
		ann[annDestructive] = "true"
	}
	if t.Paginated() {
		f.BoolVar(&b.all, "all", false, "fetch every page (streams NDJSON in JSON mode)")
		ann[annPaginated] = "true"
	}
	return b
}

func (b *toolBinding) changed(param string) bool {
	name, ok := b.flags[param]
	return ok && b.cmd.Flags().Changed(name)
}

// request builds the API request from positional args and flags.
func (b *toolBinding) request(a *App, args []string) (*api.Request, error) {
	t := b.t
	pp := t.PathParams()
	path := t.Path
	for i, p := range pp {
		if i >= len(args) || args[i] == "" {
			return nil, api.UsageError("missing %s", argName(p))
		}
		path = strings.Replace(path, "{"+p+"}", url.PathEscape(args[i]), 1)
	}
	vals := map[string]any{}
	if b.data != "" {
		v, err := a.readJSON("data", b.data)
		if err != nil {
			return nil, err
		}
		m, ok := v.(map[string]any)
		if !ok {
			// Some endpoints take an array body (emails batch).
			if t.Method != "GET" {
				return &api.Request{Method: t.Method, Path: path, JSON: v}, nil
			}
			return nil, api.UsageError("--data must be a JSON object")
		}
		vals = m
	}
	for _, p := range t.Params {
		if !b.changed(p.Name) {
			continue
		}
		flag := b.flags[p.Name]
		switch {
		case b.strs[p.Name] != nil:
			v := *b.strs[p.Name]
			if t.Upload == p.Name || strings.HasPrefix(v, "@") && (p.Name == "html" || p.Name == "text" || p.Name == "csv") || v == "-" {
				s, err := a.readValue(v)
				if err != nil {
					return nil, err
				}
				v = s
			}
			if len(p.Enum) > 0 && !contains(p.Enum, v) {
				return nil, api.UsageError("--%s must be one of %s", flag, strings.Join(p.Enum, ", "))
			}
			if p.Name == "scheduled_at" || p.Name == "scheduledAt" {
				v = parseWhen(v, a.now())
			}
			vals[p.Name] = v
		case b.bools[p.Name] != nil:
			vals[p.Name] = *b.bools[p.Name]
		case b.ints[p.Name] != nil:
			vals[p.Name] = *b.ints[p.Name]
		case b.lists[p.Name] != nil:
			l := splitList(*b.lists[p.Name])
			anyl := make([]any, len(l))
			for i, s := range l {
				anyl[i] = s
			}
			vals[p.Name] = anyl
		case b.objs[p.Name] != nil:
			if p.Type == "object" {
				m, err := a.objectFlag(flag, *b.objs[p.Name])
				if err != nil {
					return nil, err
				}
				vals[p.Name] = m
			} else {
				l, err := a.objectsFlag(flag, *b.objs[p.Name])
				if err != nil {
					return nil, err
				}
				vals[p.Name] = l
			}
		}
	}
	for i, p := range b.rc.Args {
		if i+len(pp) >= len(args) || args[i+len(pp)] == "" {
			return nil, api.UsageError("missing %s", argName(p))
		}
		vals[p] = args[i+len(pp)]
	}
	for _, p := range t.Params {
		if p.Required && !contains(pp, p.Name) {
			if _, ok := vals[p.Name]; !ok {
				return nil, api.UsageError("missing required flag --%s", b.flags[p.Name])
			}
		}
	}
	// The MCP registry's camelCase names map to the API's snake_case.
	for from, to := range map[string]string{"replyTo": "reply_to", "scheduledAt": "scheduled_at"} {
		if v, ok := vals[from]; ok {
			vals[to] = v
			delete(vals, from)
		}
	}
	req := &api.Request{Method: t.Method, Path: path}
	if k, ok := vals["idempotencyKey"]; ok {
		req.Header = map[string][]string{"Idempotency-Key": {fmt.Sprint(k)}}
		delete(vals, "idempotencyKey")
	}
	// GET and DELETE send their flags as query parameters
	// (inboxes delete --keep-mailbox: DELETE /inboxes/{id}?keep_mailbox=true).
	if t.Method == "GET" || t.Method == "DELETE" {
		q := url.Values{}
		for _, k := range sortedKeys(vals) {
			switch v := vals[k].(type) {
			case []any:
				for _, x := range v {
					q.Add(k, fmt.Sprint(x))
				}
			case map[string]any:
				bs, _ := out.Marshal(v)
				q.Set(k, string(bs))
			default:
				q.Set(k, fmt.Sprint(v))
			}
		}
		req.Query = q
	} else if file, ok := vals[t.Upload].(string); t.Upload != "" && ok && file != "" {
		delete(vals, t.Upload)
		raw, ct, err := multipartBody(vals, "file", "contacts.csv", []byte(file))
		if err != nil {
			return nil, err
		}
		req.Raw, req.ContentType = raw, ct
	} else {
		req.JSON = vals
	}
	if b.mutate != nil {
		if err := b.mutate(req, args); err != nil {
			return nil, err
		}
	}
	return req, nil
}

func multipartBody(fields map[string]any, fileField, fileName string, file []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, k := range sortedKeys(fields) {
		v := fields[k]
		s, isStr := v.(string)
		if !isStr {
			bs, _ := out.Marshal(v)
			s = string(bs)
		}
		if err := mw.WriteField(k, s); err != nil {
			return nil, "", err
		}
	}
	fw, err := mw.CreateFormFile(fileField, fileName)
	if err != nil {
		return nil, "", err
	}
	if _, err := fw.Write(file); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// runTool executes a generated resource command.
func (a *App) runTool(ctx context.Context, b *toolBinding, args []string) error {
	req, err := b.request(a, args)
	if err != nil {
		return err
	}
	if isDestructive(b.rc, b.t) {
		if err := a.Confirm(fmt.Sprintf("%s %s?", b.cmd.CommandPath(), strings.Join(args, " "))); err != nil {
			return err
		}
	}
	c, err := a.Client(true)
	if err != nil {
		return err
	}
	if b.all {
		return a.listAll(ctx, c, req, b.rc.Columns)
	}
	resp, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	v, err := resp.Value()
	if err != nil {
		return err
	}
	return a.P().Result(v, func(w io.Writer) { a.renderHuman(w, b, v) })
}

// listAll walks every page: NDJSON in machine mode, one table for humans.
func (a *App) listAll(ctx context.Context, c *api.Client, req *api.Request, cols []string) error {
	p := a.P()
	size := 100
	if l := req.Query.Get("limit"); l != "" {
		fmt.Sscan(l, &size)
	}
	var items []any
	err := c.Paginate(ctx, req, size, 0, func(raw json.RawMessage) error {
		v := out.Normalize(raw)
		if p.Machine {
			return p.Line(v)
		}
		items = append(items, v)
		return nil
	})
	if err != nil {
		return err
	}
	if !p.Machine {
		a.renderList(a.Stdout, items, cols, false)
	}
	return nil
}

// preferredColumns orders the auto-picked table columns.
var preferredColumns = []string{"id", "name", "email", "address", "endpoint", "alias", "from", "to", "subject", "status", "last_event",
	"type", "event", "method", "path", "endpoint", "response_status", "permission", "region", "key", "created_at"}

func pickColumns(items []any) []string {
	first, _ := firstMap(items)
	if first == nil {
		return []string{"value"}
	}
	var cols []string
	for _, k := range preferredColumns {
		if _, ok := first[k]; ok && !contains(cols, k) {
			cols = append(cols, k)
		}
		if len(cols) == 6 {
			break
		}
	}
	if len(cols) < 2 {
		keys := sortedKeys(first)
		for _, k := range keys {
			if _, nested := first[k].(map[string]any); !nested && !contains(cols, k) {
				cols = append(cols, k)
			}
			if len(cols) == 5 {
				break
			}
		}
	}
	return cols
}

func firstMap(items []any) (map[string]any, bool) {
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			return m, true
		}
	}
	return nil, false
}

func (a *App) renderList(w io.Writer, items []any, cols []string, hasMore bool) {
	p := a.P()
	if len(items) == 0 {
		fmt.Fprintln(w, p.S().Dim.Render("No results."))
		return
	}
	if len(cols) == 0 {
		cols = pickColumns(items)
	}
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			rows = append(rows, []string{out.Cell(it, 80)})
			continue
		}
		r := make([]string, len(cols))
		for i, c := range cols {
			r[i] = out.Cell(m[c], 48)
			switch c {
			case "id":
				r[i] = out.Cell(m[c], 0)
			case "status":
				if s, ok := m[c].(string); ok && s != "" {
					r[i] = a.statusCell(s)
				}
			}
		}
		rows = append(rows, r)
	}
	p.Table(w, cols, rows)
	if hasMore {
		last, _ := items[len(items)-1].(map[string]any)
		fmt.Fprintln(w, p.S().Dim.Render(fmt.Sprintf("More results: --after %s, or --all for every page.", str(last, "id"))))
	}
}

// renderHuman prints an API response for people.
func (a *App) renderHuman(w io.Writer, b *toolBinding, v any) {
	p := a.P()
	var cols []string
	if b != nil {
		cols = b.rc.Columns
	}
	switch x := v.(type) {
	case nil:
		p.Success("Done.")
	case map[string]any:
		if data, ok := x["data"].([]any); ok {
			more, _ := x["has_more"].(bool)
			a.renderList(w, data, cols, more)
			return
		}
		if d, _ := x["deleted"].(bool); d {
			p.Success("Deleted %s.", str(x, "id"))
			return
		}
		a.renderObject(w, x)
	case []any:
		a.renderList(w, x, cols, false)
	default:
		fmt.Fprintln(w, out.Cell(x, 0))
	}
}

// renderObject prints one object as aligned key/value lines; nested lists
// of objects (records, variables) become small tables.
func (a *App) renderObject(w io.Writer, m map[string]any) {
	p := a.P()
	keys := sortedKeys(m)
	sort.SliceStable(keys, func(i, j int) bool { return keys[i] == "id" && keys[j] != "id" })
	flat := map[string]any{}
	var nested []string
	for _, k := range keys {
		if l, ok := m[k].([]any); ok && len(l) > 0 {
			if _, isObj := l[0].(map[string]any); isObj {
				nested = append(nested, k)
				continue
			}
		}
		v := m[k]
		if s, ok := v.(string); ok && len([]rune(s)) > 300 {
			v = string([]rune(s)[:300]) + "… (truncated; use --json for the full value)"
		}
		flat[k] = v
	}
	p.KV(w, flat, keys)
	for _, k := range nested {
		fmt.Fprintln(w)
		fmt.Fprintln(w, p.S().Bold.Render(k))
		a.renderList(w, m[k].([]any), nil, false)
	}
}

// toolHooks add tool-specific flags and request tweaks to generated commands.
var toolHooks = map[string]func(a *App, b *toolBinding){
	// broadcasts send --test-to: send a test to addresses instead.
	"send-broadcast": func(a *App, b *toolBinding) {
		testTo := b.cmd.Flags().StringSlice("test-to", nil, "send a test of the broadcast to these addresses instead of sending it (repeatable or comma-separated)")
		b.cmd.Annotations[annPath] += " | POST /broadcasts/{id}/test (--test-to)"
		b.mutate = func(req *api.Request, args []string) error {
			if len(*testTo) == 0 {
				return nil
			}
			req.Path = "/broadcasts/" + url.PathEscape(args[0]) + "/test"
			to := []any{}
			for _, s := range splitList(*testTo) {
				to = append(to, s)
			}
			req.JSON = map[string]any{"to": to}
			return nil
		}
	},
}
