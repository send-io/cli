package cli

// sendio trigger: synthetic webhook events (contract §3).
//
//	GET  /v0/cli/trigger/events
//	POST /v0/cli/trigger {"event","overrides":{"a.b":v},"deliver":"listeners|webhooks|both"}

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
)

func init() { Register(func(a *App, root *cobra.Command) { a.addTriggerCommand(root) }) }

func (a *App) addTriggerCommand(root *cobra.Command) {
	var list bool
	var overrides []string
	var deliver string
	cmd := &cobra.Command{
		Use:   "trigger <event>",
		Short: "Send a test webhook event to your listeners and/or webhooks",
		Long: `Produce a realistic test event (ids prefixed test_, data.test = true) and
deliver it to running "sendio listen" sessions (default), to the team's
webhook endpoints subscribed to it (--deliver webhooks), or both.

--override sets a field of the payload by its dotted path: path=value sets a
string, path:=json any JSON value (number, boolean, array, object).
"sendio trigger --list" lists the events.`,
		Example: `  sendio trigger email.delivered
  sendio trigger email.bounced --override data.bounce.type=Permanent --override 'data.to:=["ada@example.com"]'
  sendio trigger contact.created --deliver both
  sendio trigger --list`,
		Args:        cobra.MaximumNArgs(1),
		Annotations: map[string]string{annWrite: "true", annMethod: "POST", annPath: "/v0/cli/trigger"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := a.Client(true)
			if err != nil {
				return err
			}
			if list {
				return a.triggerList(ctx, c)
			}
			event := ""
			if len(args) == 1 {
				event = args[0]
			}
			if event == "" {
				if !a.Interactive() {
					return api.UsageError("name the event (sendio trigger --list shows them)")
				}
				ev, err := a.pickTriggerEvent(ctx, c)
				if err != nil {
					return err
				}
				event = ev
			}
			return a.trigger(ctx, c, event, overrides, deliver)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&list, "list", false, "list the events that can be triggered")
	f.StringArrayVar(&overrides, "override", nil, "set a payload field: path=string or path:=json (repeatable)")
	f.StringVar(&deliver, "deliver", "listeners", "where to deliver: listeners, webhooks or both")
	root.AddCommand(cmd)
}

type triggerEvent struct {
	Event       string `json:"event"`
	Description string `json:"description"`
}

func (a *App) triggerEvents(ctx context.Context, c *api.Client) ([]triggerEvent, error) {
	resp, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/v0/cli/trigger/events"})
	if err != nil {
		return nil, err
	}
	var body struct {
		Data []triggerEvent `json:"data"`
	}
	if err := resp.Decode(&body); err != nil {
		return nil, api.Errorf(api.ExitAPI, "invalid_response", "trigger events: %v", err)
	}
	return body.Data, nil
}

func (a *App) triggerList(ctx context.Context, c *api.Client) error {
	evs, err := a.triggerEvents(ctx, c)
	if err != nil {
		return err
	}
	return a.P().Result(map[string]any{"data": evs}, func(w io.Writer) {
		rows := make([][]string, 0, len(evs))
		for _, e := range evs {
			rows = append(rows, []string{e.Event, e.Description})
		}
		a.P().Table(w, []string{"event", "description"}, rows)
	})
}

func (a *App) pickTriggerEvent(ctx context.Context, c *api.Client) (string, error) {
	evs, err := a.triggerEvents(ctx, c)
	if err != nil {
		return "", err
	}
	opts := make([]huh.Option[string], 0, len(evs))
	for _, e := range evs {
		opts = append(opts, huh.NewOption(e.Event+"  "+a.P().S().Dim.Render(e.Description), e.Event))
	}
	var pick string
	if err := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Event to trigger").Options(opts...).Value(&pick))).
		WithTheme(a.huhTheme()).WithShowHelp(false).Run(); err != nil {
		return "", &api.Error{Exit: api.ExitUsage, Code: "canceled", Message: "Canceled."}
	}
	return pick, nil
}

// parseOverrides turns path=value / path:=json flags into the overrides map.
func parseOverrides(list []string) (map[string]any, error) {
	m := map[string]any{}
	for _, s := range list {
		if i := strings.Index(s, ":="); i > 0 && (strings.Index(s, "=") == i+1) {
			v, err := decodeJSON(s[i+2:])
			if err != nil {
				return nil, api.UsageError("--override %s: the value after := must be JSON: %v", s[:i], err)
			}
			m[s[:i]] = v
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, api.UsageError("--override must be path=value or path:=json, got %q", s)
		}
		m[k] = v
	}
	return m, nil
}

func (a *App) trigger(ctx context.Context, c *api.Client, event string, overrides []string, deliver string) error {
	switch deliver {
	case "listeners", "webhooks", "both":
	default:
		return api.UsageError("--deliver must be listeners, webhooks or both")
	}
	ov, err := parseOverrides(overrides)
	if err != nil {
		return err
	}
	body := map[string]any{"event": event, "deliver": deliver}
	if len(ov) > 0 {
		body["overrides"] = ov
	}
	resp, err := c.Do(ctx, &api.Request{Method: "POST", Path: "/v0/cli/trigger", JSON: body})
	if err != nil {
		return err
	}
	v, err := resp.Value()
	if err != nil {
		return err
	}
	m, _ := v.(map[string]any)
	return a.P().Result(v, func(w io.Writer) {
		p := a.P()
		d, _ := m["delivered"].(map[string]any)
		listeners, webhooks := fmt.Sprint(d["listeners"]), fmt.Sprint(d["webhooks"])
		parts := []string{}
		if deliver != "webhooks" {
			parts = append(parts, listeners+" listener(s)")
		}
		if deliver != "listeners" {
			parts = append(parts, webhooks+" webhook(s)")
		}
		p.Success("Triggered %s (%s): delivered to %s.", str(m, "event"), str(m, "id"), strings.Join(parts, " and "))
		if deliver != "webhooks" && listeners == "0" {
			fmt.Fprintln(w, p.S().Dim.Render("No listener took it: run `sendio listen` in another terminal first."))
		}
	})
}
