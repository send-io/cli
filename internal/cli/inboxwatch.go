package cli

// sendio inboxes watch: new messages of an agent inbox, live (contract §5,
// Appendix A). WebSocket wss://<API>/v0 with the bearer header, then
// {"type":"subscribe","inbox_ids":[…],"event_types":["message.received"]};
// the server answers {"type":"subscribed"} and sends one event frame per
// message. Events are not replayed after a reconnect, so a reconnect lists
// the inbox's newest messages (GET /inboxes/{id}/messages) and prints the
// inbound ones that arrived while disconnected.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/out"
)

func init() { Register(func(a *App, root *cobra.Command) { a.addInboxWatchCommand(root) }) }

type inboxWatchOpts struct {
	exec        string
	execTimeout time.Duration
	spam        bool
}

func (a *App) addInboxWatchCommand(root *cobra.Command) {
	var o inboxWatchOpts
	cmd := &cobra.Command{
		Use:   "watch <inbox-id-or-address>",
		Short: "Print (or handle) new messages of an agent inbox as they arrive",
		Long: `Watch an agent inbox and print each new message as it arrives: time, from,
subject and a snippet; one NDJSON line per message (the full message object:
inbox_id, thread_id, message_id, from, to, subject, preview, text, html,
attachments, …) in agent mode or with --json.

--exec runs a shell command for every message, one at a time, with the
message JSON on stdin and SENDIO_MESSAGE_ID, SENDIO_INBOX_ID and
SENDIO_THREAD_ID in its environment. A command that fails or runs past
--exec-timeout is reported and watching continues.

Spam is skipped unless --include-spam. After a dropped connection the
messages that arrived meanwhile are listed and printed (marked catch_up).
Ctrl-C stops (exit 0).`,
		Example: `  sendio inboxes watch support@agents.example.com
  sendio inboxes watch support@agents.example.com --json | jq -r .subject
  sendio inboxes watch 6f1c…-inbox-uuid --exec './handle-message.sh'
  sendio inboxes watch support@agents.example.com --exec 'python3 triage.py' --exec-timeout 2m`,
		Args:        exactArgs(1),
		Annotations: map[string]string{annMethod: "GET", annPath: "/v0 (WebSocket)"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.inboxWatch(cmd.Context(), args[0], o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.exec, "exec", "", "shell command to run per message (message JSON on stdin)")
	f.DurationVar(&o.execTimeout, "exec-timeout", time.Minute, "time limit of each --exec run")
	f.BoolVar(&o.spam, "include-spam", false, "also print messages classified as spam")
	// "sendio inbox" is the free inbox group; its watch falls back to this
	// one for addresses that are not free inboxes.
	a.group(root, []string{"inboxes"}).AddCommand(cmd)
}

type inboxWatcher struct {
	a     *App
	o     inboxWatchOpts
	inbox string
	seen  map[string]bool
	last  time.Time
}

func (a *App) inboxWatch(ctx context.Context, inbox string, o inboxWatchOpts) error {
	c, err := a.Client(true)
	if err != nil {
		return err
	}
	w := &inboxWatcher{a: a, o: o, inbox: inbox, seen: map[string]bool{}, last: a.now().UTC()}
	return a.keepConnected(ctx, "inbox watch", func(ctx context.Context, attempt int) (bool, error) {
		return w.session(ctx, c, attempt)
	})
}

func (w *inboxWatcher) session(ctx context.Context, c *api.Client, attempt int) (bool, error) {
	a := w.a
	conn, err := a.dialWS(ctx, c, "/v0", nil)
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()
	types := []string{"message.received"}
	if w.o.spam {
		types = append(types, "message.received.spam")
	}
	if err := writeFrame(ctx, conn, map[string]any{"type": "subscribe", "inbox_ids": []string{w.inbox}, "event_types": types}); err != nil {
		return false, closeReason(err)
	}
	subscribed := false
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				conn.Close(websocket.StatusNormalClosure, "")
				return subscribed, nil
			}
			return subscribed, closeReason(err)
		}
		var fr map[string]any
		if json.Unmarshal(data, &fr) != nil {
			continue
		}
		switch str(fr, "type") {
		case "subscribed":
			subscribed = true
			if attempt == 0 {
				a.P().Info("Watching %s for new messages… (^C to quit)", w.inbox)
			} else {
				a.P().Info("Reconnected.")
				w.catchUp(ctx, c)
			}
		case "error":
			return subscribed, api.Errorf(api.ExitAPI, str(fr, "name"), "%s", str(fr, "message"))
		case "event":
			et := str(fr, "event_type")
			if !strings.HasPrefix(et, "message.received") {
				continue
			}
			msg, _ := fr["message"].(map[string]any)
			if msg == nil {
				continue
			}
			w.emit(ctx, msg, et, false)
		}
	}
}

// catchUp prints inbound messages that arrived while disconnected.
func (w *inboxWatcher) catchUp(ctx context.Context, c *api.Client) {
	resp, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/inboxes/" + url.PathEscape(w.inbox) + "/messages", Query: url.Values{"limit": {"50"}}})
	if err != nil {
		w.a.P().Warn("inbox watch: could not list messages missed while disconnected: %v", err)
		return
	}
	var page struct {
		Data []map[string]any `json:"data"`
	}
	if resp.Decode(&page) != nil {
		return
	}
	for i := len(page.Data) - 1; i >= 0; i-- {
		m := page.Data[i]
		if str(m, "direction") != "inbound" || (m["is_spam"] == true && !w.o.spam) {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, str(m, "created_at"))
		if err != nil || !t.After(w.last) {
			continue
		}
		msg := map[string]any{"inbox_id": w.inbox, "message_id": m["id"], "thread_id": m["thread_id"], "from": m["from"], "to": m["to"],
			"subject": m["subject"], "preview": m["snippet"], "extracted_text": m["extracted_text"], "created_at": m["created_at"], "timestamp": m["created_at"]}
		w.emit(ctx, msg, "message.received", true)
	}
}

func (w *inboxWatcher) emit(ctx context.Context, msg map[string]any, eventType string, catchUp bool) {
	a := w.a
	id := str(msg, "message_id")
	if id != "" {
		if w.seen[id] {
			return
		}
		w.seen[id] = true
	}
	ts := str(msg, "timestamp")
	if ts == "" {
		ts = str(msg, "created_at")
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil && t.After(w.last) {
		w.last = t
	}
	if catchUp {
		msg["catch_up"] = true
	}
	if eventType != "message.received" {
		msg["event_type"] = eventType
	}
	p := a.P()
	if p.Machine {
		_ = p.Line(msg)
	} else {
		s := p.S()
		line := fmt.Sprintf("%s  %s  %s", a.clock(ts), s.Bold.Render(out.Cell(msg["from"], 40)), out.Cell(msg["subject"], 60))
		snippet := str(msg, "preview")
		if snippet == "" {
			snippet = str(msg, "extracted_text")
		}
		if snippet != "" {
			line += s.Dim.Render("  " + out.Cell(snippet, 80))
		}
		if catchUp {
			line += s.Dim.Render(" (while disconnected)")
		}
		if eventType == "message.received.spam" {
			line += s.Warn.Render(" [spam]")
		}
		fmt.Fprintln(a.Stdout, line)
	}
	if w.o.exec != "" {
		w.runExec(ctx, msg)
	}
}

// runExec runs --exec for one message.
func (w *inboxWatcher) runExec(ctx context.Context, msg map[string]any) {
	a := w.a
	body, _ := out.Marshal(msg)
	ectx, cancel := context.WithTimeout(ctx, w.o.execTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ectx, "cmd", "/C", w.o.exec)
	} else {
		cmd = exec.CommandContext(ectx, "sh", "-c", w.o.exec)
	}
	cmd.Dir = a.WorkDir
	cmd.Stdin = bytes.NewReader(append(body, '\n'))
	inbox := str(msg, "inbox_id")
	if inbox == "" {
		inbox = w.inbox
	}
	cmd.Env = append(os.Environ(), "SENDIO_MESSAGE_ID="+str(msg, "message_id"), "SENDIO_INBOX_ID="+inbox, "SENDIO_THREAD_ID="+str(msg, "thread_id"))
	var outw io.Writer = a.Stdout
	if a.P().Machine {
		outw = a.Stderr // stdout carries only the NDJSON records
	}
	cmd.Stdout, cmd.Stderr = outw, a.Stderr
	cmd.WaitDelay = 2 * time.Second
	start := time.Now()
	err := cmd.Run()
	switch {
	case ectx.Err() == context.DeadlineExceeded:
		a.P().Warn("--exec timed out after %s for message %s", w.o.execTimeout, str(msg, "message_id"))
	case ctx.Err() != nil:
	case err != nil:
		a.P().Warn("--exec failed for message %s (%v, %s)", str(msg, "message_id"), err, time.Since(start).Round(time.Millisecond))
	}
}
