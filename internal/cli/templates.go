package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/out"
)

// templateMeta is meta.json in a pulled template folder.
type templateMeta struct {
	ID          string        `json:"id,omitempty"`
	Name        string        `json:"name"`
	Alias       string        `json:"alias,omitempty"`
	Subject     string        `json:"subject,omitempty"`
	From        string        `json:"from,omitempty"`
	ReplyTo     any           `json:"reply_to,omitempty"`
	PreviewText string        `json:"preview_text,omitempty"`
	Variables   []templateVar `json:"variables"`
	Engine      string        `json:"engine,omitempty"`
}

type templateVar struct {
	Key           string `json:"key"`
	Type          string `json:"type"`
	FallbackValue any    `json:"fallback_value,omitempty"`
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		return "template"
	}
	return s
}

func metaFrom(t map[string]any) templateMeta {
	m := templateMeta{ID: str(t, "id"), Name: str(t, "name"), Alias: str(t, "alias"), Subject: str(t, "subject"), From: str(t, "from"),
		PreviewText: str(t, "preview_text"), Engine: str(t, "template_engine"), Variables: []templateVar{}}
	if rt := t["reply_to"]; rt != nil {
		if l, ok := rt.([]any); !ok || len(l) > 0 {
			m.ReplyTo = rt
		}
	}
	if vs, ok := t["variables"].([]any); ok {
		for _, v := range vs {
			vm, _ := v.(map[string]any)
			m.Variables = append(m.Variables, templateVar{Key: str(vm, "key"), Type: str(vm, "type"), FallbackValue: vm["fallback_value"]})
		}
	}
	return m
}

func (a *App) addTemplateCommands(root *cobra.Command) {
	templates := a.group(root, []string{"templates"})
	pull := &cobra.Command{
		Use:   "pull <dir>",
		Short: "Download every template into <dir>/<alias>/ (template.html, template.txt, meta.json)",
		Long: `Download templates as folders you can keep in git:

  <dir>/<alias or name>/template.html
  <dir>/<alias or name>/template.txt
  <dir>/<alias or name>/meta.json   (name, alias, subject, from, reply_to,
                                     preview_text, variables, engine)

Edit them and run "sendio templates push <dir>".`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return a.templatesPull(cmd.Context(), args[0]) },
	}
	templates.AddCommand(pull)

	var publish bool
	push := &cobra.Command{
		Use:   "push <dir>",
		Short: "Create or update templates from folders (matched by alias), with a diff summary",
		Long: `Push template folders (as written by "templates pull") to send.io. <dir> is
one template folder or a folder of them. Each is matched to an existing
template by alias (else by the id in meta.json) and updated when it
differs, or created. --publish publishes each changed template.
--dry-run shows the plan and the exact requests without sending them.`,
		Args:        exactArgs(1),
		Annotations: map[string]string{annWrite: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.templatesPush(cmd.Context(), args[0], publish)
		},
	}
	push.Flags().BoolVar(&publish, "publish", false, "publish each created or updated template")
	templates.AddCommand(push)
}

func (a *App) templatesPull(ctx context.Context, dir string) error {
	c, err := a.Client(true)
	if err != nil {
		return err
	}
	root := a.path(dir)
	var ids []string
	err = c.Paginate(ctx, &api.Request{Method: "GET", Path: "/templates"}, 100, 0, func(raw json.RawMessage) error {
		var t struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &t)
		ids = append(ids, t.ID)
		return nil
	})
	if err != nil {
		return err
	}
	used := map[string]bool{}
	results := []any{}
	for _, id := range ids {
		resp, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/templates/" + url.PathEscape(id)})
		if err != nil {
			return err
		}
		var t map[string]any
		if err := resp.Decode(&t); err != nil {
			return api.Errorf(api.ExitAPI, "invalid_response", "template %s: %v", id, err)
		}
		meta := metaFrom(t)
		name := meta.Alias
		if name == "" {
			name = slug(meta.Name)
		}
		if used[name] {
			name += "-" + strings.SplitN(id, "-", 2)[0]
		}
		used[name] = true
		folder := filepath.Join(root, name)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			return api.Errorf(api.ExitUsage, "write_failed", "%v", err)
		}
		files := []string{"meta.json", "template.html"}
		mb, _ := out.MarshalIndent(meta)
		if err := os.WriteFile(filepath.Join(folder, "meta.json"), append(mb, '\n'), 0o644); err != nil {
			return api.Errorf(api.ExitUsage, "write_failed", "%v", err)
		}
		if err := os.WriteFile(filepath.Join(folder, "template.html"), []byte(str(t, "html")), 0o644); err != nil {
			return api.Errorf(api.ExitUsage, "write_failed", "%v", err)
		}
		txt := filepath.Join(folder, "template.txt")
		if s := str(t, "text"); s != "" {
			files = append(files, "template.txt")
			if err := os.WriteFile(txt, []byte(s), 0o644); err != nil {
				return api.Errorf(api.ExitUsage, "write_failed", "%v", err)
			}
		} else {
			_ = os.Remove(txt)
		}
		results = append(results, map[string]any{"id": id, "name": meta.Name, "alias": meta.Alias, "dir": folder, "files": files})
	}
	return a.P().Result(map[string]any{"dir": root, "templates": results}, func(w io.Writer) {
		a.P().Success("Pulled %d template(s) into %s.", len(results), root)
		a.renderList(w, results, []string{"alias", "name", "dir"}, false)
	})
}

// templateDirs returns dir itself when it holds meta.json, else its
// subfolders that do.
func templateDirs(dir string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err == nil {
		return []string{dir}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var outd []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "meta.json")); err == nil {
				outd = append(outd, filepath.Join(dir, e.Name()))
			}
		}
	}
	sort.Strings(outd)
	return outd, nil
}

func templateBody(m templateMeta, html, text string) map[string]any {
	b := map[string]any{"name": m.Name, "html": html}
	put := func(k, v string) {
		if v != "" {
			b[k] = v
		}
	}
	put("alias", m.Alias)
	put("subject", m.Subject)
	put("from", m.From)
	put("preview_text", m.PreviewText)
	put("text", text)
	put("template_engine", m.Engine)
	if m.ReplyTo != nil {
		b["reply_to"] = m.ReplyTo
	}
	vars := []any{}
	for _, v := range m.Variables {
		vm := map[string]any{"key": v.Key, "type": v.Type}
		if v.FallbackValue != nil {
			vm["fallback_value"] = v.FallbackValue
		}
		vars = append(vars, vm)
	}
	b["variables"] = vars
	return b
}

func lineDelta(old, new string) string {
	count := func(s string) map[string]int {
		m := map[string]int{}
		for _, l := range strings.Split(s, "\n") {
			m[l]++
		}
		return m
	}
	o, n := count(old), count(new)
	add, del := 0, 0
	for l, c := range n {
		if d := c - o[l]; d > 0 {
			add += d
		}
	}
	for l, c := range o {
		if d := c - n[l]; d > 0 {
			del += d
		}
	}
	return fmt.Sprintf("+%d/-%d lines", add, del)
}

// templateChanges lists the fields that differ between the folder and the API.
func templateChanges(cur templateMeta, curHTML, curText string, want templateMeta, html, text string) []string {
	var ch []string
	cmp := func(name, a, b string) {
		if a != b {
			ch = append(ch, name)
		}
	}
	cmp("name", cur.Name, want.Name)
	cmp("alias", cur.Alias, want.Alias)
	cmp("subject", cur.Subject, want.Subject)
	cmp("from", cur.From, want.From)
	cmp("preview_text", cur.PreviewText, want.PreviewText)
	if want.Engine != "" {
		cmp("engine", cur.Engine, want.Engine)
	}
	if !reflect.DeepEqual(out.Normalize(cur.ReplyTo), out.Normalize(want.ReplyTo)) && !(isEmptyList(cur.ReplyTo) && isEmptyList(want.ReplyTo)) {
		ch = append(ch, "reply_to")
	}
	if curHTML != html {
		ch = append(ch, "html ("+lineDelta(curHTML, html)+")")
	}
	if curText != text {
		ch = append(ch, "text ("+lineDelta(curText, text)+")")
	}
	cv, _ := json.Marshal(out.Normalize(cur.Variables))
	wv, _ := json.Marshal(out.Normalize(want.Variables))
	if string(cv) != string(wv) {
		ch = append(ch, "variables")
	}
	return ch
}

func isEmptyList(v any) bool {
	if v == nil {
		return true
	}
	l, ok := v.([]any)
	return ok && len(l) == 0
}

func (a *App) templatesPush(ctx context.Context, dir string, publish bool) error {
	c, err := a.Client(true)
	if err != nil {
		return err
	}
	dirs, err := templateDirs(a.path(dir))
	if err != nil {
		return api.UsageError("%v", err)
	}
	if len(dirs) == 0 {
		return api.UsageError("no template folders (with meta.json) in %s", dir)
	}
	var planned []any
	if a.g.dryRun {
		c.OnDryRun = func(r *http.Request, body []byte) {
			var bv any = string(body)
			if v, err := decodeJSON(string(body)); err == nil {
				bv = out.Normalize(v)
			}
			planned = append(planned, map[string]any{"method": r.Method, "url": r.URL.String(), "body": bv})
		}
	}
	results := []any{}
	for _, d := range dirs {
		res, err := a.pushOne(ctx, c, d, publish)
		if err != nil {
			return err
		}
		results = append(results, res)
	}
	summary := map[string]any{"dry_run": a.g.dryRun, "templates": results}
	if a.g.dryRun {
		if planned == nil {
			planned = []any{}
		}
		summary["requests"] = planned
	}
	return a.P().Result(summary, func(w io.Writer) {
		p := a.P()
		rows := [][]string{}
		for _, r := range results {
			m := r.(map[string]any)
			chs, _ := m["changes"].([]string)
			rows = append(rows, []string{str(m, "template"), a.actionCell(str(m, "action")), dash(strings.Join(chs, ", "))})
		}
		p.Table(w, []string{"template", "action", "changes"}, rows)
		if a.g.dryRun {
			fmt.Fprintln(w, p.S().Warn.Render("\nDry run: nothing was sent. Requests that would be made:"))
			for _, r := range planned {
				m := r.(map[string]any)
				fmt.Fprintf(w, "  %s %s\n", str(m, "method"), str(m, "url"))
			}
		}
	})
}

func (a *App) actionCell(s string) string {
	st := a.P().S()
	switch s {
	case "created", "would create":
		return st.OK.Render(s)
	case "updated", "would update", "published", "updated+published", "created+published":
		return st.Warn.Render(s)
	default:
		return st.Dim.Render(s)
	}
}

func (a *App) pushOne(ctx context.Context, c *api.Client, d string, publish bool) (map[string]any, error) {
	mb, err := os.ReadFile(filepath.Join(d, "meta.json"))
	if err != nil {
		return nil, api.UsageError("%v", err)
	}
	var want templateMeta
	if err := json.Unmarshal(mb, &want); err != nil {
		return nil, api.UsageError("%s: %v", filepath.Join(d, "meta.json"), err)
	}
	if want.Variables == nil {
		want.Variables = []templateVar{}
	}
	if want.Name == "" {
		want.Name = filepath.Base(d)
	}
	htmlB, err := os.ReadFile(filepath.Join(d, "template.html"))
	if err != nil {
		return nil, api.UsageError("%s needs template.html: %v", d, err)
	}
	textB, _ := os.ReadFile(filepath.Join(d, "template.txt"))
	html, text := string(htmlB), string(textB)
	label := want.Alias
	if label == "" {
		label = filepath.Base(d)
	}
	res := map[string]any{"template": label, "dir": d, "changes": []string{}}
	ref := want.Alias
	if ref == "" {
		ref = want.ID
	}
	var cur map[string]any
	if ref != "" {
		resp, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/templates/" + url.PathEscape(ref)})
		switch {
		case err == nil:
			_ = resp.Decode(&cur)
		case api.IsStatus(err, 404):
		default:
			return nil, err
		}
	}
	body := templateBody(want, html, text)
	dry := func(err error) bool { return errors.Is(err, api.ErrDryRun) }
	if cur == nil {
		res["action"] = "created"
		resp, err := c.Do(ctx, &api.Request{Method: "POST", Path: "/templates", JSON: body})
		if dry(err) {
			res["action"] = "would create"
			if publish {
				res["action"] = "would create+publish"
			}
			return res, nil
		}
		if err != nil {
			return nil, err
		}
		var created map[string]any
		_ = resp.Decode(&created)
		res["id"] = str(created, "id")
		if want.ID == "" && str(created, "id") != "" {
			want.ID = str(created, "id")
			nb, _ := out.MarshalIndent(want)
			_ = os.WriteFile(filepath.Join(d, "meta.json"), append(nb, '\n'), 0o644)
		}
	} else {
		id := str(cur, "id")
		res["id"] = id
		changes := templateChanges(metaFrom(cur), str(cur, "html"), str(cur, "text"), want, html, text)
		res["changes"] = changes
		if len(changes) == 0 {
			res["action"] = "unchanged"
			if publish && cur["has_unpublished_versions"] == true {
				if _, err := c.Do(ctx, &api.Request{Method: "POST", Path: "/templates/" + url.PathEscape(id) + "/publish"}); dry(err) {
					res["action"] = "would publish"
				} else if err != nil {
					return nil, err
				} else {
					res["action"] = "published"
				}
			}
			return res, nil
		}
		res["action"] = "updated"
		if _, err := c.Do(ctx, &api.Request{Method: "PATCH", Path: "/templates/" + url.PathEscape(id), JSON: body}); dry(err) {
			res["action"] = "would update"
			if publish {
				_, _ = c.Do(ctx, &api.Request{Method: "POST", Path: "/templates/" + url.PathEscape(id) + "/publish"})
				res["action"] = "would update+publish"
			}
			return res, nil
		} else if err != nil {
			return nil, err
		}
	}
	if publish {
		if _, err := c.Do(ctx, &api.Request{Method: "POST", Path: "/templates/" + url.PathEscape(str(res, "id")) + "/publish"}); err != nil {
			return nil, err
		}
		res["action"] = str(res, "action") + "+published"
	}
	return res, nil
}
