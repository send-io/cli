package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	sendio "send.io/sendio-go"

	"send.io/cli/internal/api"
	"send.io/cli/internal/out"
)

// SDK returns a send.io Go SDK client that runs through c (auth, refresh,
// dry-run, typed errors).
func (a *App) SDK(c *api.Client) *sendio.Client {
	s := sendio.NewCustomClient(&http.Client{Transport: c.Transport()}, "cli")
	_ = s.SetBaseURL(c.BaseURL)
	s.UserAgent = userAgent()
	return s
}

type sendFlags struct {
	from, subject, html, text, template, data, scheduledAt, idem, topic, wait string
	to, cc, bcc, replyTo, vars, attach, headers, tags                         []string
	timeout, interval                                                         time.Duration
}

func (a *App) addEmailCommands(root *cobra.Command) {
	emails := a.group(root, []string{"emails"})
	var f sendFlags
	send := &cobra.Command{
		Use:   "send",
		Short: "Send an email (inline content or a template), optionally waiting for delivery",
		Long: `Send an email.

Content: --html/--text take a string, @file or - (stdin). Or --template
<id|alias> with --var key=value (key:=json for typed values) and/or
--data @vars.json (with --template, --data is the variables object; without
it, --data is the raw request body and flags override its fields).
--template alias@3 renders version 3 and sends the result inline.

--wait delivered|bounced|sent|opened|clicked|any polls the email until its
last event matches (exit 0), a terminal failure happens (exit 1) or
--timeout passes (exit 7), then prints the event.

API: POST /emails  (MCP tool: send-email)`,
		Example: `  sendio emails send --from "Acme <hi@acme.dev>" --to ada@example.com --subject Hi --text "Hello"
  sendio emails send --from hi@acme.dev --to a@x.dev,b@x.dev --html @mail.html --subject Report --attach ./report.pdf
  sendio emails send --template welcome --var name=Ada --to ada@example.com --wait delivered
  echo "<p>hi</p>" | sendio emails send --from hi@acme.dev --to ada@example.com --subject Hi --html -
  sendio emails send ... --scheduled-at "in 1h" --idempotency-key auto`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annTool: "send-email", annMethod: "POST", annPath: "/emails", annWrite: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.emailsSend(cmd, &f)
		},
	}
	fl := send.Flags()
	fl.StringVar(&f.from, "from", "", `sender, "Name <user@verified-domain>" or an address`)
	fl.StringArrayVar(&f.to, "to", nil, "recipient (repeatable or comma-separated)")
	fl.StringArrayVar(&f.cc, "cc", nil, "Cc recipient (repeatable or comma-separated)")
	fl.StringArrayVar(&f.bcc, "bcc", nil, "Bcc recipient (repeatable or comma-separated)")
	fl.StringArrayVar(&f.replyTo, "reply-to", nil, "Reply-To address (repeatable or comma-separated)")
	fl.StringVar(&f.subject, "subject", "", "subject line")
	fl.StringVar(&f.html, "html", "", "HTML body: a string, @file or - for stdin")
	fl.StringVar(&f.text, "text", "", "plain-text body: a string, @file or - for stdin")
	fl.StringVar(&f.template, "template", "", "published template id or alias, optionally @version")
	fl.StringArrayVar(&f.vars, "var", nil, "template variable key=value (key:=json for numbers/booleans; repeatable)")
	fl.StringVar(&f.data, "data", "", "with --template: variables JSON (@file, - or literal); otherwise the raw request body")
	fl.StringArrayVar(&f.attach, "attach", nil, "attach a file (path or https URL; repeatable)")
	fl.StringArrayVar(&f.headers, "header", nil, "custom header Name=value (repeatable)")
	fl.StringArrayVar(&f.tags, "tag", nil, "tag name=value (repeatable)")
	fl.StringVar(&f.scheduledAt, "scheduled-at", "", `send later: RFC 3339 or relative time ("in 1h", "in 30 minutes", "in 2 days")`)
	fl.StringVar(&f.idem, "idempotency-key", "", `Idempotency-Key header ("auto" generates one)`)
	fl.StringVar(&f.topic, "topic-id", "", "topic the email belongs to (opted-out contacts are skipped)")
	fl.StringVar(&f.wait, "wait", "", "wait for: delivered, bounced, sent, opened, clicked or any")
	fl.DurationVar(&f.timeout, "timeout", 60*time.Second, "how long --wait waits")
	fl.DurationVar(&f.interval, "poll-interval", time.Second, "how often --wait polls")
	_ = fl.MarkHidden("poll-interval")
	emails.AddCommand(send)

	var file, batchIdem string
	batch := &cobra.Command{
		Use:   "batch [file]",
		Short: "Send up to 100 emails in one request from a JSON array or NDJSON file",
		Long: `Send up to 100 emails in one request. The input is a JSON array of email
bodies or NDJSON (one body per line), from --file, a positional path, or -
for stdin. Each body is shaped like the POST /emails body.

API: POST /emails/batch  (MCP tool: send-batch-emails)`,
		Example:     "  sendio emails batch --file emails.ndjson\n  jq -c '.[]' list.json | sendio emails batch -",
		Args:        cobra.MaximumNArgs(1),
		Annotations: map[string]string{annTool: "send-batch-emails", annMethod: "POST", annPath: "/emails/batch", annWrite: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			src := file
			if len(args) == 1 {
				src = args[0]
			}
			if src == "" {
				return api.UsageError("pass --file x.json|x.ndjson|- (or a path)")
			}
			if src != "-" && !strings.HasPrefix(src, "@") {
				src = "@" + src
			}
			raw, err := a.readValue(src)
			if err != nil {
				return err
			}
			items, err := readNDJSONOrArray(raw)
			if err != nil {
				return api.UsageError("batch input: %v", err)
			}
			if len(items) == 0 || len(items) > 100 {
				return api.UsageError("a batch holds 1 to 100 emails, got %d", len(items))
			}
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					if v, ok := m["scheduled_at"].(string); ok {
						m["scheduled_at"] = parseWhen(v, a.now())
					}
				}
			}
			req := &api.Request{Method: "POST", Path: "/emails/batch", JSON: items}
			if k := idemKey(batchIdem); k != "" {
				req.Header = http.Header{"Idempotency-Key": {k}}
			}
			c, err := a.Client(true)
			if err != nil {
				return err
			}
			resp, err := c.Do(cmd.Context(), req)
			if err != nil {
				return err
			}
			v, _ := resp.Value()
			return a.P().Result(v, func(w io.Writer) {
				a.P().Success("Sent %d emails.", len(items))
				a.renderHuman(w, nil, v)
			})
		},
	}
	batch.Flags().StringVar(&file, "file", "", "JSON array or NDJSON file, or - for stdin")
	batch.Flags().StringVar(&batchIdem, "idempotency-key", "", `Idempotency-Key header ("auto" generates one)`)
	emails.AddCommand(batch)
}

func idemKey(v string) string {
	if strings.EqualFold(v, "auto") {
		return randomKey()
	}
	return v
}

// splitTemplateRef splits "alias@version" (version: digits or an id).
func splitTemplateRef(s string) (string, string) {
	i := strings.LastIndex(s, "@")
	if i <= 0 || i == len(s)-1 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

func (a *App) emailsSend(cmd *cobra.Command, f *sendFlags) error {
	ctx := cmd.Context()
	switch f.wait {
	case "", "delivered", "bounced", "sent", "opened", "clicked", "any":
	default:
		return api.UsageError("--wait must be delivered, bounced, sent, opened, clicked or any")
	}
	body := map[string]any{}
	vars := map[string]any{}
	if f.data != "" {
		v, err := a.readJSON("data", f.data)
		if err != nil {
			return err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return api.UsageError("--data must be a JSON object")
		}
		if f.template != "" {
			vars = m
		} else {
			body = m
		}
	}
	for _, kv := range f.vars {
		k, v, err := parseKV("var", kv)
		if err != nil {
			return err
		}
		vars[k] = v
	}
	if len(f.vars) > 0 && f.template == "" {
		return api.UsageError("--var needs --template")
	}
	setList := func(key string, vals []string) {
		if l := splitList(vals); len(l) > 0 {
			anyl := make([]any, len(l))
			for i, s := range l {
				anyl[i] = s
			}
			body[key] = anyl
		}
	}
	setList("to", f.to)
	setList("cc", f.cc)
	setList("bcc", f.bcc)
	setList("reply_to", f.replyTo)
	if f.from != "" {
		body["from"] = f.from
	}
	if f.subject != "" {
		body["subject"] = f.subject
	}
	if f.topic != "" {
		body["topic_id"] = f.topic
	}
	stdinUsed := false
	for _, part := range []struct{ key, val string }{{"html", f.html}, {"text", f.text}} {
		if part.val == "" {
			continue
		}
		if part.val == "-" {
			if stdinUsed {
				return api.UsageError("only one of --html/--text can read stdin")
			}
			stdinUsed = true
		}
		s, err := a.readValue(part.val)
		if err != nil {
			return err
		}
		body[part.key] = s
	}
	if f.scheduledAt != "" {
		body["scheduled_at"] = parseWhen(f.scheduledAt, a.now())
	}
	if len(f.headers) > 0 {
		h := map[string]any{}
		for _, kv := range f.headers {
			k, v, err := parseKV("header", strings.Replace(kv, ": ", "=", 1))
			if err != nil {
				return err
			}
			h[k] = fmt.Sprint(v)
		}
		body["headers"] = h
	}
	if len(f.tags) > 0 {
		var tags []sendio.Tag
		for _, kv := range f.tags {
			k, v, err := parseKV("tag", kv)
			if err != nil {
				return err
			}
			tags = append(tags, sendio.Tag{Name: k, Value: fmt.Sprint(v)})
		}
		body["tags"] = tags
	}
	if len(f.attach) > 0 {
		var atts []map[string]any
		for _, p := range f.attach {
			att, err := a.attachment(p)
			if err != nil {
				return err
			}
			atts = append(atts, att)
		}
		body["attachments"] = atts
	}
	if _, ok := body["to"]; !ok {
		return api.UsageError("missing --to")
	}

	c, err := a.Client(true)
	if err != nil {
		return err
	}
	if f.template != "" {
		id, version := splitTemplateRef(f.template)
		if version == "" {
			body["template"] = sendio.EmailTemplate{Id: id, Variables: vars}
		} else if err := a.renderVersionInto(ctx, c, id, version, vars, body); err != nil {
			return err
		}
	}
	req := &api.Request{Method: "POST", Path: "/emails", JSON: body}
	if k := idemKey(f.idem); k != "" {
		req.Header = http.Header{"Idempotency-Key": {k}}
	}
	resp, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	var sent map[string]any
	if err := resp.Decode(&sent); err != nil || str(sent, "id") == "" {
		return api.Errorf(api.ExitAPI, "invalid_response", "the send response had no id")
	}
	if f.wait == "" {
		return a.P().Result(sent, func(w io.Writer) {
			msg := "Sent"
			if f.scheduledAt != "" {
				msg = "Scheduled"
			}
			a.P().Success("%s in %s  %s", msg, out.Millis(resp.Duration), a.P().S().Dim.Render(str(sent, "id")))
		})
	}
	return a.waitEmail(ctx, c, str(sent, "id"), f.wait, f.timeout, f.interval)
}

// renderVersionInto renders one saved template version and puts the result
// (and its sender defaults) into body, since sends always use the published
// version.
func (a *App) renderVersionInto(ctx context.Context, c *api.Client, id, version string, vars map[string]any, body map[string]any) error {
	resp, err := c.Do(ctx, &api.Request{Method: "POST", Path: "/templates/" + url.PathEscape(id) + "/render", ReadOnly: true,
		JSON: map[string]any{"variables": vars, "version_id": version}})
	if err != nil {
		return err
	}
	var r map[string]any
	_ = resp.Decode(&r)
	for _, k := range []string{"subject", "html", "text"} {
		if _, set := body[k]; !set && str(r, k) != "" {
			body[k] = r[k]
		}
	}
	if _, ok := body["from"]; !ok {
		if vr, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/templates/" + url.PathEscape(id) + "/versions/" + url.PathEscape(version)}); err == nil {
			var v map[string]any
			_ = vr.Decode(&v)
			if s := str(v, "from"); s != "" {
				body["from"] = s
			}
			if rt, ok := v["reply_to"]; ok && rt != nil {
				if _, set := body["reply_to"]; !set {
					body["reply_to"] = rt
				}
			}
		}
	}
	return nil
}

func (a *App) attachment(p string) (map[string]any, error) {
	if strings.HasPrefix(p, "https://") || strings.HasPrefix(p, "http://") {
		u, err := url.Parse(p)
		if err != nil {
			return nil, api.UsageError("--attach %s: %v", p, err)
		}
		name := filepath.Base(u.Path)
		if name == "." || name == "/" {
			name = "attachment"
		}
		return map[string]any{"path": p, "filename": name}, nil
	}
	data, err := os.ReadFile(a.path(p))
	if err != nil {
		return nil, api.UsageError("--attach: %v", err)
	}
	name := filepath.Base(p)
	att := map[string]any{"filename": name, "content": base64.StdEncoding.EncodeToString(data)}
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		att["content_type"] = strings.SplitN(ct, ";", 2)[0]
	}
	return att, nil
}

var (
	deliveredStates = []string{"delivered", "opened", "clicked", "complained"}
	failureStates   = []string{"bounced", "failed", "canceled", "suppressed"}
	pendingStates   = []string{"", "queued", "scheduled", "sent", "delivery_delayed", "processing"}
)

// waitOutcome: 1 matched, -1 terminal mismatch, 0 keep waiting.
func waitOutcome(want, state string) int {
	switch want {
	case "any":
		if contains(pendingStates, state) {
			return 0
		}
		return 1
	case "delivered":
		if contains(deliveredStates, state) {
			return 1
		}
	case "sent":
		switch {
		case contains(failureStates, state):
			return -1
		case contains([]string{"", "queued", "scheduled", "processing"}, state):
			return 0
		}
		return 1
	case "opened":
		if state == "opened" || state == "clicked" {
			return 1
		}
	default:
		if state == want {
			return 1
		}
	}
	if contains(failureStates, state) || want == "bounced" && contains(deliveredStates, state) {
		return -1
	}
	return 0
}

func (a *App) waitEmail(ctx context.Context, c *api.Client, id, want string, timeout, interval time.Duration) error {
	p := a.P()
	sdk := a.SDK(c)
	start := a.now()
	deadline := start.Add(timeout)
	if interval <= 0 {
		interval = time.Second
	}
	spin := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	live := !p.Machine && !p.Quiet && a.StderrTTY
	state, frame := "", 0
	clear := func() {
		if live {
			fmt.Fprint(a.Stderr, "\r\033[2K")
		}
	}
	for {
		e, err := sdk.Emails.GetWithContext(ctx, id)
		if err != nil {
			if !api.IsStatus(err, 404) || a.now().Sub(start) > 10*time.Second {
				clear()
				return api.AsError(err)
			}
		} else {
			state = e.LastEvent
		}
		if live {
			fmt.Fprintf(a.Stderr, "\r\033[2K%s Waiting for %s… %s (%ds)", spin[frame%len(spin)], want, dash(state), int(a.now().Sub(start).Seconds()))
			frame++
		}
		switch waitOutcome(want, state) {
		case 1:
			clear()
			res := a.waitResult(ctx, c, id, want, state, start, true)
			return p.Result(res, func(w io.Writer) {
				p.Success("Email %s %s after %s.", id, state, a.now().Sub(start).Round(100*time.Millisecond))
				if ev, ok := res["event"].(map[string]any); ok {
					a.renderObject(w, ev)
				}
			})
		case -1:
			clear()
			res := a.waitResult(ctx, c, id, want, state, start, false)
			_ = p.Result(res, func(w io.Writer) {
				if ev, ok := res["event"].(map[string]any); ok {
					a.renderObject(w, ev)
				}
			})
			return &api.Error{Exit: api.ExitAPI, Code: "email_" + state, Message: fmt.Sprintf("email %s was %s (waited for %s)", id, state, want)}
		}
		if !a.now().Before(deadline) {
			clear()
			return &api.Error{Exit: api.ExitTimeout, Code: "wait_timeout",
				Message: fmt.Sprintf("email %s is still %s after %s (waited for %s)", id, dash(state), timeout, want),
				Hint:    "Check later with `sendio emails get " + id + "` or `sendio emails events " + id + "`."}
		}
		if err := a.sleep(ctx, interval); err != nil {
			clear()
			return api.NetworkError(err)
		}
		if interval < 3*time.Second {
			interval += interval / 2
		}
	}
}

// waitResult reports the final state with the timeline event behind it.
func (a *App) waitResult(ctx context.Context, c *api.Client, id, want, state string, start time.Time, ok bool) map[string]any {
	res := map[string]any{"id": id, "last_event": state, "wait": want, "ok": ok, "waited_ms": a.now().Sub(start).Milliseconds()}
	if resp, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/emails/" + url.PathEscape(id) + "/events"}); err == nil {
		var tl struct {
			Data []any `json:"data"`
		}
		_ = resp.Decode(&tl)
		var match any
		for _, ev := range tl.Data {
			m, _ := ev.(map[string]any)
			t := str(m, "type")
			if t == "" {
				t = str(m, "event")
			}
			if t == state || strings.TrimPrefix(t, "email.") == state {
				match = ev
			}
		}
		if match == nil && len(tl.Data) > 0 {
			match = tl.Data[len(tl.Data)-1]
		}
		if match != nil {
			res["event"] = out.Normalize(match)
		}
	}
	return res
}
