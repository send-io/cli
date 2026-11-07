package cli

// The free inbox tools of the local MCP server (sendio mcp serve). They run
// on this machine with the keys in ~/.sendio/free and need no send.io
// sign-in.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"send.io/cli/internal/api"
	"send.io/cli/internal/freeinbox"
	"send.io/cli/internal/out"
)

// freeToolFamily is the --tools family of the free inbox tools.
const freeToolFamily = "free-inbox"

type freeTool struct {
	name, description string
	schema            string
	readOnly          bool
	run               func(a *App, ctx context.Context, args map[string]any) *mcp.CallToolResult
}

const freeAddressProp = `"address":{"type":"string","description":"The free inbox address (name@org.sendioinbox.com). Optional when there is only one free inbox on this machine."}`

var freeTools = []freeTool{
	{
		name: "free_inbox_create",
		description: "Create a free anonymous email inbox name@org.sendioinbox.com that can receive mail (no send.io account needed). " +
			"Returns the address, or available=false with up to 3 available suggestions when the name is taken (call again with address set to one of them). " +
			"The inbox key is stored on this machine. The result includes a recovery phrase: show it to the user once and tell them to save it; it is the only way to use the inbox on another machine.",
		schema: `{"type":"object","properties":{` +
			`"name":{"type":"string","description":"The part before @, 2-32 characters a-z, 0-9, '.' and '-' (e.g. signup-bot)."},` +
			`"org":{"type":"string","description":"The org label, 2-32 characters a-z, 0-9 and '-' (e.g. acme gives @acme.sendioinbox.com)."},` +
			`"address":{"type":"string","description":"Create this exact address instead of name and org (e.g. one of the suggestions)."}}}`,
		run: (*App).toolFreeCreate,
	},
	{
		name:        "free_inbox_list",
		description: "List the free inboxes stored on this machine with how many messages each has saved.",
		schema:      `{"type":"object","properties":{}}`,
		readOnly:    true,
		run:         (*App).toolFreeList,
	},
	{
		name: "free_inbox_wait_for_email",
		description: "Wait for an email to arrive in a free inbox and return it (from, subject, text, attachments), or only the verification code with extract_code. " +
			"Use it right after triggering a sign-up or login email. Only mail that arrives during the wait counts. Returns an error result on timeout.",
		schema: `{"type":"object","properties":{` + freeAddressProp + `,` +
			`"timeout_seconds":{"type":"integer","minimum":1,"maximum":300,"description":"How long to wait, up to 300 seconds (default 120)."},` +
			`"from":{"type":"string","description":"Only mail whose From contains this text (case-insensitive)."},` +
			`"subject":{"type":"string","description":"Only mail whose Subject contains this text (case-insensitive)."},` +
			`"extract_code":{"type":"boolean","description":"Return the 6-8 digit code in the mail; mail without a code is skipped."}}}`,
		readOnly: true,
		run:      (*App).toolFreeWait,
	},
	{
		name:        "free_inbox_read",
		description: "Read the newest messages of a free inbox (fetches mail waiting on the relay first). Returns from, subject, received time, text and attachments; risky attachments are flagged and must not be opened.",
		schema: `{"type":"object","properties":{` + freeAddressProp + `,` +
			`"limit":{"type":"integer","minimum":1,"maximum":50,"description":"How many messages, newest first (default 10)."}}}`,
		readOnly: true,
		run:      (*App).toolFreeRead,
	},
	{
		name: "free_inbox_connect",
		description: "Make a one-time connect link for a free inbox so it can also send email: the user opens it in a browser, signs in to send.io, and the inbox moves into their send.io workspace " +
			"(read in the dashboard, sends with the workspace's credits, up to 50 a day). The link is signed with the inbox key on this machine, works once and expires in 30 minutes. " +
			"Give it only to the user: anyone holding it can take the inbox. After the move, the free_inbox_* tools can no longer read that inbox (they return code \"claimed\").",
		schema: `{"type":"object","properties":{` + freeAddressProp + `}}`,
		run:    (*App).toolFreeConnect,
	},
}

// splitFreeTools separates free inbox families and tool names from the
// --tools list.
func splitFreeTools(only []string) (free, rest []string) {
	for _, o := range only {
		l := strings.ToLower(o)
		if l == freeToolFamily || l == "free" || l == "free-inboxes" || strings.HasPrefix(l, "free_inbox_") {
			free = append(free, l)
		} else {
			rest = append(rest, o)
		}
	}
	return free, rest
}

// addFreeTools registers the free inbox tools selected by only (empty: all).
func (a *App) addFreeTools(srv *mcp.Server, readOnly bool, only []string) int {
	want := map[string]bool{}
	for _, o := range only {
		want[o] = true
	}
	all := len(only) == 0 || want[freeToolFamily] || want["free"] || want["free-inboxes"]
	n := 0
	for _, t := range freeTools {
		t := t
		if (readOnly && !t.readOnly) || (!all && !want[t.name]) {
			continue
		}
		no := false
		ann := &mcp.ToolAnnotations{ReadOnlyHint: t.readOnly, DestructiveHint: &no, OpenWorldHint: &no}
		srv.AddTool(&mcp.Tool{Name: t.name, Description: t.description, InputSchema: json.RawMessage(t.schema), Annotations: ann},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				args := map[string]any{}
				if len(req.Params.Arguments) > 0 {
					if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
						return toolText("invalid arguments: "+err.Error(), true), nil
					}
				}
				return t.run(a, ctx, args), nil
			})
		n++
	}
	return n
}

func toolJSON(v any, isErr bool) *mcp.CallToolResult {
	b, _ := out.MarshalIndent(v)
	return toolText(string(b), isErr)
}

func toolErr(err error) *mcp.CallToolResult {
	body := map[string]any{"error": err.Error()}
	var ae *api.Error
	if errors.As(freeErr(err), &ae) {
		body["error"], body["code"] = ae.Message, ae.Code
		if ae.Hint != "" {
			body["hint"] = ae.Hint
		}
	}
	return toolJSON(body, true)
}

func argStr(args map[string]any, k string) string {
	s, _ := args[k].(string)
	return strings.TrimSpace(s)
}

func argInt(args map[string]any, k string, def int) int {
	switch v := args[k].(type) {
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return def
}

// toolInbox opens the inbox named by args["address"], or the only one.
func (a *App) toolInbox(args map[string]any) (*freeinbox.Inbox, *freeinbox.Client, error) {
	st := a.freeStore()
	addr := strings.ToLower(argStr(args, "address"))
	if addr == "" {
		list, err := st.List()
		if err != nil {
			return nil, nil, err
		}
		switch len(list) {
		case 0:
			return nil, nil, errors.New("there is no free inbox on this machine; create one with free_inbox_create")
		case 1:
			addr = list[0].Address
		default:
			var names []string
			for _, k := range list {
				names = append(names, k.Address)
			}
			return nil, nil, fmt.Errorf("several free inboxes on this machine; pass address (one of %s)", strings.Join(names, ", "))
		}
	}
	in, err := st.OpenInbox(addr)
	if err != nil {
		return nil, nil, err
	}
	return in, a.freeClient(a.freeRelay("", in)), nil
}

func (a *App) toolFreeCreate(ctx context.Context, args map[string]any) *mcp.CallToolResult {
	opt := freeinbox.CreateOptions{Name: argStr(args, "name"), Org: argStr(args, "org"), Address: argStr(args, "address")}
	if opt.Address == "" && (opt.Name == "" || opt.Org == "") {
		return toolJSON(map[string]any{"error": "pass name and org (or address)"}, true)
	}
	c := a.freeClient(a.freeRelay("", nil))
	res, err := freeinbox.Create(ctx, c, a.freeStore(), opt)
	var te *freeinbox.TakenError
	if errors.As(err, &te) {
		return toolJSON(map[string]any{"available": false, "address": te.Address, "reason": te.Reason, "suggestions": te.Suggestions,
			"next": "Call free_inbox_create again with address set to one of the suggestions, or with another name."}, false)
	}
	if err != nil {
		return toolErr(err)
	}
	return toolJSON(map[string]any{
		"available": true, "address": res.Address, "created_at": res.CreatedAt, "hold_hours": res.HoldHours,
		"daily_limit": res.DailyLimit, "max_message_bytes": res.MaxMessageBytes, "key_path": res.KeyPath,
		"recovery_phrase": res.RecoveryPhrase,
		"note":            "Show the recovery phrase to the user once and ask them to save it: it is the only way to read this inbox on another machine, and send.io cannot recover it. Do not repeat it later.",
	}, false)
}

func (a *App) toolFreeList(ctx context.Context, args map[string]any) *mcp.CallToolResult {
	st := a.freeStore()
	list, err := st.List()
	if err != nil {
		return toolErr(err)
	}
	rows := []map[string]any{}
	for _, k := range list {
		mail, _ := st.Mail(k.Address)
		rows = append(rows, map[string]any{"address": k.Address, "created_at": k.CreatedAt, "messages": len(mail), "relay": k.Relay})
	}
	return toolJSON(map[string]any{"inboxes": rows}, false)
}

// toolMessage is a message for the model: the text is capped.
func toolMessage(d freeinbox.Delivered) map[string]any {
	m := freeRecord(d, true)
	if t, _ := m["text"].(string); len(t) > 20000 {
		m["text"] = t[:20000] + "\n[truncated]"
	}
	delete(m, "path")
	return m
}

func (a *App) toolFreeWait(ctx context.Context, args map[string]any) *mcp.CallToolResult {
	in, c, err := a.toolInbox(args)
	if err != nil {
		return toolErr(err)
	}
	timeout := argInt(args, "timeout_seconds", 120)
	if timeout < 1 {
		timeout = 1
	}
	if timeout > 300 {
		timeout = 300
	}
	code, _ := args["extract_code"].(bool)
	wctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	m, err := freeinbox.Wait(wctx, c, a.freeStore(), in, freeinbox.WaitOptions{From: argStr(args, "from"), Subject: argStr(args, "subject"), Code: code}, nil, nil)
	if errors.Is(err, freeinbox.ErrWaitTimeout) {
		return toolJSON(map[string]any{"error": "timeout", "message": fmt.Sprintf("No matching email arrived at %s within %d seconds.", in.Address, timeout)}, true)
	}
	if err != nil {
		return toolErr(err)
	}
	if code {
		return toolJSON(map[string]any{"code": m.Code, "address": in.Address, "from": m.Index.From, "subject": m.Index.Subject, "received_at": m.Index.ReceivedAt}, false)
	}
	return toolJSON(toolMessage(m.Delivered), false)
}

func (a *App) toolFreeRead(ctx context.Context, args map[string]any) *mcp.CallToolResult {
	in, c, err := a.toolInbox(args)
	if err != nil {
		return toolErr(err)
	}
	limit := argInt(args, "limit", 10)
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	st := a.freeStore()
	recv := &freeinbox.Receiver{Store: st}
	var syncErr error
	for {
		ds, err := recv.PollOnce(ctx, c, in, 0, 100)
		if err != nil {
			syncErr = err
			break
		}
		if len(ds) < 100 {
			break
		}
	}
	list, err := st.Mail(in.Address)
	if err != nil {
		return toolErr(err)
	}
	if len(list) > limit {
		list = list[:limit]
	}
	msgs := []map[string]any{}
	for _, idx := range list {
		raw, _ := st.RawMail(in.Address, idx.Seq)
		msgs = append(msgs, toolMessage(freeinbox.Delivered{Address: in.Address, Index: idx, Raw: raw}))
	}
	res := map[string]any{"address": in.Address, "messages": msgs}
	if syncErr != nil {
		var ee *freeinbox.EndedError
		if errors.As(freeExplain(ctx, c, in, syncErr), &ee) {
			res["warning"] = ee.Error() + " (showing mail saved on this machine)"
			res["code"] = ee.Code
		} else {
			res["warning"] = "could not fetch new mail from the relay: " + syncErr.Error() + " (showing saved mail)"
		}
	}
	return toolJSON(res, false)
}

func (a *App) toolFreeConnect(ctx context.Context, args map[string]any) *mcp.CallToolResult {
	in, _, err := a.toolInbox(args)
	if err != nil {
		return toolErr(err)
	}
	cl, err := freeinbox.NewConnectLink(a.freeAppURL(), in, a.now())
	if err != nil {
		return toolErr(err)
	}
	return toolJSON(map[string]any{
		"address": cl.Address, "link": cl.Link, "expires_at": cl.ExpiresAt.Format(time.RFC3339),
		"instructions": "Give this link to your user: they open it, sign in, and the mailbox moves into their send.io workspace where it can also send.",
	}, false)
}
