package cli

// sendio templates dev: local preview server (internal/devserver).

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/devserver"
)

func init() { Register(func(a *App, root *cobra.Command) { a.addTemplatesDevCommand(root) }) }

func (a *App) addTemplatesDevCommand(root *cobra.Command) {
	var port int
	var data string
	var open bool
	cmd := &cobra.Command{
		Use:   "dev [dir]",
		Short: "Preview a templates folder locally with hot reload",
		Long: `Serve a local preview of a templates folder (as "sendio templates pull"
writes it: <dir>/<alias>/template.html, template.txt, meta.json; dir
defaults to the current folder) at http://localhost:3456.

Templates render with send.io's own engine (the same code sends use:
simple {{var}} or Handlebars, declared variables and fallbacks, the hidden
preview text), with data from <dir>/fixtures/<alias>.json (--data for
another folder), which you can edit and save in the page. The page reloads
when a file changes and shows: a desktop/mobile width toggle, a dark-mode
preview, the plain-text part, the subject, and checks (missing variables,
links that are not https, images without alt text, HTML over Gmail's 102 KB
clipping limit). "Send test" sends the preview (POST /emails) or the
template saved on send.io (its test send) after you confirm.

Ctrl-C stops the server (exit 0).`,
		Example: "  sendio templates pull ./emails && sendio templates dev ./emails --open\n  sendio templates dev ./emails --port 4000 --data ./fixtures",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return a.templatesDev(cmd.Context(), a.path(dir), data, port, open)
		},
	}
	cmd.Flags().IntVar(&port, "port", 3456, "port to listen on (127.0.0.1)")
	cmd.Flags().StringVar(&data, "data", "", "fixtures folder with <alias>.json data files (default <dir>/fixtures)")
	cmd.Flags().BoolVar(&open, "open", false, "open the preview in the browser")
	a.group(root, []string{"templates"}).AddCommand(cmd)
}

func (a *App) templatesDev(ctx context.Context, dir, data string, port int, open bool) error {
	if data != "" {
		data = a.path(data)
	}
	opts := devserver.Options{Dir: dir, Fixtures: data, Log: func(format string, args ...any) { a.P().Info(format, args...) }}
	// Sending needs a credential; without one the preview still works.
	if c, err := a.Client(true); err == nil {
		opts.Send = func(ctx context.Context, r devserver.SendRequest) (any, error) { return a.devSend(ctx, c, r) }
	}
	srv, err := devserver.New(opts)
	if err != nil {
		return api.UsageError("%v", err)
	}
	ts, err := srv.Templates()
	if err != nil {
		return api.UsageError("%v", err)
	}
	u, err := srv.Start(fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return api.Errorf(api.ExitUsage, "listen_failed", "cannot listen on port %d: %v", port, err)
	}
	defer srv.Close()
	if err := srv.Watch(ctx); err != nil {
		a.P().Warn("hot reload is off: %v", err)
	}
	p := a.P()
	_ = p.Result(map[string]any{"url": u, "dir": dir, "templates": len(ts), "can_send": opts.Send != nil}, func(w io.Writer) {
		s := p.S()
		fmt.Fprintf(w, "%s %s %s\n", s.OK.Render("Previewing"), s.Bold.Render(fmt.Sprintf("%d template(s)", len(ts))), s.Dim.Render("from "+dir))
		fmt.Fprintf(w, "  %s  %s\n", s.Bold.Render(u), s.Dim.Render("(^C to stop)"))
		if len(ts) == 0 {
			fmt.Fprintln(w, s.Warn.Render("  No template folders (with meta.json) here yet: run `sendio templates pull "+dir+"`."))
		}
		if opts.Send == nil {
			fmt.Fprintln(w, s.Dim.Render("  Not signed in: \"Send test\" is off (sendio login, or SENDIO_API_KEY)."))
		}
	})
	if open && a.OpenURL != nil {
		_ = a.OpenURL(u)
	}
	<-ctx.Done()
	return nil
}

// devSend performs a "Send test" from the preview.
func (a *App) devSend(ctx context.Context, c *api.Client, r devserver.SendRequest) (any, error) {
	var req *api.Request
	switch r.Mode {
	case "template":
		ref := r.Template.Meta.ID
		if ref == "" {
			ref = r.Template.Meta.Alias
		}
		body := map[string]any{"to": r.To}
		if len(r.Data) > 0 {
			body["variables"] = r.Data
		}
		req = &api.Request{Method: "POST", Path: "/templates/" + url.PathEscape(ref) + "/test", JSON: body}
	default:
		res := r.Rendered
		subject := res.Subject
		if !strings.HasPrefix(subject, "[Test] ") {
			subject = "[Test] " + subject
		}
		body := map[string]any{"from": r.From, "to": r.To, "subject": subject, "html": res.HTML,
			"tags": []any{map[string]any{"name": "source", "value": "templates-dev"}}}
		if strings.TrimSpace(res.Text) != "" {
			body["text"] = res.Text
		}
		if len(res.ReplyTo) > 0 {
			body["reply_to"] = res.ReplyTo
		}
		req = &api.Request{Method: "POST", Path: "/emails", JSON: body}
	}
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.Value()
}
