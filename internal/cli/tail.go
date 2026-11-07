package cli

// sendio logs tail / sendio events tail: Server-Sent Events (contract §4).
//
//	GET /v0/cli/logs/stream?status=&method=&path=     event: log, id: <log id>
//	GET /v0/cli/events/stream?type=&email_id=         event: email_event, id: evt_…
//
// On a dropped stream the CLI reconnects with Last-Event-ID set to the last
// id it printed (logs resume from there; events are best effort).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/out"
)

func init() { Register(func(a *App, root *cobra.Command) { a.addTailCommands(root) }) }

type tailSpec struct {
	name  string // logs, events
	path  string
	event string // SSE event name
	query url.Values
	human func(m map[string]any) string
}

func (a *App) addTailCommands(root *cobra.Command) {
	var status, method, path string
	logs := &cobra.Command{
		Use:   "tail",
		Short: "Stream API request logs live",
		Long: `Print the team's API requests as they are logged (method, path, status,
user agent). Filters: --status 2xx|4xx|5xx or an exact code, --method POST,
--path /emails (prefix). In agent mode each log is one NDJSON line (the same
shape as "sendio logs list" items). Reconnects resume after the last log
printed. Ctrl-C stops (exit 0).`,
		Example:     "  sendio logs tail\n  sendio logs tail --status 4xx --method POST --path /emails\n  sendio logs tail --jq 'select(.response_status >= 500) | .id'",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annMethod: "GET", annPath: "/v0/cli/logs/stream"},
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if status != "" {
				q.Set("status", strings.ToLower(status))
			}
			if method != "" {
				q.Set("method", strings.ToUpper(method))
			}
			if path != "" {
				q.Set("path", path)
			}
			return a.tail(cmd.Context(), tailSpec{name: "logs tail", path: "/v0/cli/logs/stream", event: "log", query: q, human: a.logLine})
		},
	}
	logs.Flags().StringVar(&status, "status", "", "status filter: 2xx, 4xx, 5xx or a code like 404")
	logs.Flags().StringVar(&method, "method", "", "HTTP method filter (POST, GET, …)")
	logs.Flags().StringVar(&path, "path", "", "path prefix filter (/emails)")
	a.group(root, []string{"logs"}).AddCommand(logs)

	var types []string
	var emailID string
	events := &cobra.Command{
		Use:   "tail",
		Short: "Stream email events (sent, delivered, bounced, opened, …) live",
		Long: `Print the team's email events as they happen: the events webhooks get
(email.sent, email.delivered, email.bounced, email.complained, email.opened,
email.clicked, …), in send.io's native format. Filter with --type (comma-
separated) and --email <id>. In agent mode each event is one NDJSON line
{type, email_id, to, subject, created_at, data}. Ctrl-C stops (exit 0).`,
		Example:     "  sendio events tail\n  sendio events tail --type email.bounced,email.complained\n  sendio events tail --email 4ef9a417-…",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annMethod: "GET", annPath: "/v0/cli/events/stream"},
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if t := splitList(types); len(t) > 0 {
				q.Set("type", strings.Join(t, ","))
			}
			if emailID != "" {
				q.Set("email_id", emailID)
			}
			return a.tail(cmd.Context(), tailSpec{name: "events tail", path: "/v0/cli/events/stream", event: "email_event", query: q, human: a.emailEventLine})
		},
	}
	events.Flags().StringSliceVar(&types, "type", nil, "event types (comma-separated), e.g. email.bounced,email.delivered")
	events.Flags().StringVar(&emailID, "email", "", "only events of this email id")
	a.group(root, []string{"events"}).AddCommand(events)
}

func (a *App) tail(ctx context.Context, sp tailSpec) error {
	c, err := a.Client(true)
	if err != nil {
		return err
	}
	lastID := ""
	err = a.keepConnected(ctx, sp.name, func(ctx context.Context, attempt int) (bool, error) {
		h := http.Header{"Accept": {"text/event-stream"}, "Cache-Control": {"no-cache"}}
		if lastID != "" {
			h.Set("Last-Event-ID", lastID)
		}
		resp, err := c.Stream(ctx, &api.Request{Method: "GET", Path: sp.path, Query: sp.query, Header: h})
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		if attempt == 0 {
			a.P().Info("Streaming %s… (^C to quit)", strings.TrimSuffix(sp.name, " tail"))
		} else {
			a.P().Info("Reconnected.")
		}
		err = readSSE(resp.Body, func(ev sseEvent) error {
			if ev.ID != "" {
				lastID = ev.ID
			}
			if ev.Event != "" && ev.Event != sp.event {
				return nil
			}
			return a.printStreamItem(ev.Data, sp.human)
		})
		if ctx.Err() != nil {
			return true, nil
		}
		if err == nil {
			err = api.NetworkError(fmt.Errorf("the stream ended"))
		}
		return true, err
	})
	return err
}

func (a *App) printStreamItem(data string, human func(map[string]any) string) error {
	p := a.P()
	var v any
	d := json.NewDecoder(strings.NewReader(data))
	d.UseNumber()
	if d.Decode(&v) != nil {
		v = data
	}
	if p.Machine {
		return p.Line(v)
	}
	m, ok := v.(map[string]any)
	if !ok {
		fmt.Fprintln(a.Stdout, data)
		return nil
	}
	fmt.Fprintln(a.Stdout, human(m))
	return nil
}

func (a *App) logLine(m map[string]any) string {
	s := a.P().S()
	code := out.Cell(m["response_status"], 0)
	st := s.OK
	switch {
	case strings.HasPrefix(code, "4"):
		st = s.Warn
	case strings.HasPrefix(code, "5"):
		st = s.Err
	}
	return fmt.Sprintf("%s  %-6s %-40s %s  %s", a.clock(str(m, "created_at")), str(m, "method"), str(m, "endpoint"), st.Render(code),
		s.Dim.Render(out.Cell(m["user_agent"], 40)))
}

func (a *App) emailEventLine(m map[string]any) string {
	s := a.P().S()
	typ := str(m, "type")
	st := s.Bold
	switch typ {
	case "email.bounced", "email.complained", "email.failed":
		st = s.Err
	case "email.delivered":
		st = s.OK
	case "email.delivery_delayed":
		st = s.Warn
	}
	return fmt.Sprintf("%s  %s  %s  %s  %s", a.clock(str(m, "created_at")), st.Render(fmt.Sprintf("%-22s", typ)),
		s.Dim.Render(str(m, "email_id")), out.Cell(m["to"], 40), out.Cell(m["subject"], 50))
}
