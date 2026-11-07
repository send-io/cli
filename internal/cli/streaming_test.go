package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"send.io/cli/internal/spec"
)

// appFor returns an agent-mode App with an API key, pointed at base.
func appFor(t *testing.T, base string) (*harness, *App, *lockedBuf) {
	h := newHarness(t, base)
	h.env["SENDIO_API_KEY"] = "mx_test_key"
	a := h.newApp("")
	out, errb := &lockedBuf{}, &lockedBuf{}
	a.Stdout, a.Stderr = out, errb
	_ = a.resolveModes()
	h.errBuf = errb
	return h, a, out
}

func ndjson(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not NDJSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func wsRead(ctx context.Context, c *websocket.Conn) (map[string]any, error) {
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m, nil
}

func wsWrite(ctx context.Context, c *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	_ = c.Write(ctx, websocket.MessageText, b)
}

func TestListenForwardsAcksAndReconnects(t *testing.T) {
	type got struct {
		method, path, body string
		header             http.Header
	}
	gotCh := make(chan got, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotCh <- got{r.Method, r.URL.Path, string(b), r.Header.Clone()}
		w.WriteHeader(202)
	}))
	defer target.Close()

	var mu sync.Mutex
	var sessions int
	var queries []string
	acks := make(chan map[string]any, 4)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/cli/listen" || r.Header.Get("Authorization") != "Bearer mx_test_key" {
			w.WriteHeader(401)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		mu.Lock()
		sessions++
		n := sessions
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		ctx := r.Context()
		wsWrite(ctx, c, map[string]any{"type": "ready", "session_id": fmt.Sprintf("lis_%d", n), "secret": fmt.Sprintf("whsec_%d", n), "format": "sendio",
			"events": []string{"email.delivered"}, "signature_scheme": "svix", "endpoint": "x"})
		wsWrite(ctx, c, map[string]any{"type": "ping"})
		ev := map[string]any{"type": "event", "id": fmt.Sprintf("evt_%d", n), "event_type": "email.delivered", "format": "sendio", "created_at": "2026-10-08T12:00:00Z",
			"headers": map[string]string{"svix-id": "msg_1", "svix-signature": "v1,abc", "content-type": "application/json", "user-agent": "Sendio-Webhooks/1.0", "content-length": "999"},
			"body":    `{"type":"email.delivered","data":{"email_id":"e1"}}`}
		if n == 2 {
			ev["method"] = "PUT"
		}
		wsWrite(ctx, c, ev)
		for {
			m, err := wsRead(ctx, c)
			if err != nil {
				return
			}
			if m["type"] == "ack" {
				acks <- m
				break
			}
			if m["type"] != "pong" {
				t.Errorf("unexpected client frame %v", m)
			}
		}
		if n == 1 {
			c.Close(websocket.StatusGoingAway, "server restarting; reconnect")
			return
		}
		<-ctx.Done()
	}))
	defer api.Close()

	h, a, stdout := appFor(t, api.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.listen(ctx, listenOpts{forward: strings.TrimPrefix(target.URL, "http://") + "/hooks", format: "sendio",
			events: []string{"email.delivered"}, headers: []string{"X-Extra: 1"}, timeout: 5 * time.Second})
	}()
	for i := 1; i <= 2; i++ {
		select {
		case g := <-gotCh:
			wantMethod := "POST"
			if i == 2 {
				wantMethod = "PUT"
			}
			if g.method != wantMethod || g.path != "/hooks" || g.body != `{"type":"email.delivered","data":{"email_id":"e1"}}` ||
				g.header.Get("Svix-Signature") != "v1,abc" || g.header.Get("Svix-Id") != "msg_1" || g.header.Get("Content-Type") != "application/json" ||
				g.header.Get("User-Agent") != "Sendio-Webhooks/1.0" || g.header.Get("X-Extra") != "1" {
				t.Fatalf("forwarded request %d: %+v", i, g)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d was not forwarded", i)
		}
		select {
		case ack := <-acks:
			if ack["id"] != fmt.Sprintf("evt_%d", i) || ack["status"] != float64(202) {
				t.Fatalf("ack %v", ack)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no ack %d", i)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("listen returned %v", err)
	}
	mu.Lock()
	if sessions != 2 || !strings.Contains(queries[0], "events=email.delivered") || !strings.Contains(queries[0], "format=sendio") || !strings.Contains(queries[0], "endpoint=http") {
		t.Fatalf("sessions %d queries %v", sessions, queries)
	}
	mu.Unlock()
	recs := ndjson(t, stdout.String())
	var kinds []string
	for _, r := range recs {
		kinds = append(kinds, fmt.Sprint(r["type"]))
	}
	if strings.Join(kinds, ",") != "ready,event,ready,event" {
		t.Fatalf("records %v\n%s", kinds, stdout.String())
	}
	if recs[0]["secret"] != "whsec_1" || recs[2]["secret"] != "whsec_2" {
		t.Fatalf("ready records %v", recs)
	}
	fw, _ := recs[1]["forward"].(map[string]any)
	if fw["status"] != float64(202) {
		t.Fatalf("event record %v", recs[1])
	}
	if body, _ := recs[1]["body"].(map[string]any); body["type"] != "email.delivered" {
		t.Fatalf("body not decoded: %v", recs[1]["body"])
	}
	if !strings.Contains(h.errBuf.String(), "reconnecting") {
		t.Fatalf("no reconnect notice: %s", h.errBuf.String())
	}
}

func TestListenPrintSecretAndErrors(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") == "nope" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(422)
			io.WriteString(w, `{"statusCode":422,"name":"validation_error","message":"format must be one of"}`)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		wsWrite(r.Context(), c, map[string]any{"type": "ready", "session_id": "lis_1", "secret": "whsec_abc", "signature_scheme": "svix"})
		_, _ = wsRead(r.Context(), c)
	}))
	defer api.Close()
	h := newHarness(t, api.URL)
	h.env["SENDIO_API_KEY"] = "mx_k"
	if code := h.run("listen", "--print-secret", "--jq", ".secret"); code != 0 || strings.TrimSpace(h.out()) != "whsec_abc" {
		t.Fatalf("code %d out %q err %s", code, h.out(), h.errOut())
	}
	if code := h.run("listen", "--format", "bogus"); code != 2 {
		t.Fatalf("bad format: %d", code)
	}
	if code := h.run("listen", "--forward-to", "ftp://x"); code != 2 {
		t.Fatalf("bad forward-to: %d", code)
	}
	h.env["SENDIO_API_KEY"] = ""
	if code := h.run("listen"); code != 3 {
		t.Fatalf("not signed in: %d %s", code, h.errOut())
	}
}

func TestForwardURL(t *testing.T) {
	for in, want := range map[string]string{
		"localhost:3000/webhooks":   "http://localhost:3000/webhooks",
		":3000/x":                   "http://localhost:3000/x",
		"https://localhost:8443/h":  "https://localhost:8443/h",
		"127.0.0.1:9000":            "http://127.0.0.1:9000/",
		"http://example.test/a?b=c": "http://example.test/a?b=c",
	} {
		if got, err := forwardURL(in); err != nil || got != want {
			t.Errorf("forwardURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestTrigger(t *testing.T) {
	f := newFake(t)
	f.json("GET /v0/cli/trigger/events", 200, `{"object":"list","data":[{"event":"email.bounced","description":"Bounced."},{"event":"email.delivered","description":"Delivered."}]}`)
	f.json("POST /v0/cli/trigger", 200, `{"object":"test_event","id":"evt_1","event":"email.bounced","delivered":{"listeners":1,"webhooks":2},"payload":{"type":"email.bounced"}}`)
	h := newHarness(t, f.URL)
	h.env["SENDIO_API_KEY"] = "mx_k"
	if code := h.run("trigger", "email.bounced", "--override", "data.bounce.type=Permanent", "--override", `data.to:=["x@y.z"]`, "--override", "data.n:=3", "--deliver", "both"); code != 0 {
		t.Fatalf("code %d %s", code, h.errOut())
	}
	mustEqualJSON(t, f.last(t).Body, `{"event":"email.bounced","deliver":"both","overrides":{"data.bounce.type":"Permanent","data.to":["x@y.z"],"data.n":3}}`)
	if h.outJSON()["id"] != "evt_1" {
		t.Fatalf("out %s", h.out())
	}
	if code := h.run("trigger", "--list", "--jq", ".data[].event"); code != 0 || h.out() != "email.bounced\nemail.delivered\n" {
		t.Fatalf("list %d %q", code, h.out())
	}
	if code := h.run("trigger"); code != 2 {
		t.Fatalf("no event: %d", code)
	}
	if code := h.run("trigger", "email.sent", "--deliver", "everywhere"); code != 2 {
		t.Fatalf("bad deliver: %d", code)
	}
	if code := h.run("trigger", "email.sent", "--override", "novalue"); code != 2 {
		t.Fatalf("bad override: %d", code)
	}
	// Human mode.
	if code := h.run("trigger", "email.bounced", "--agent", "no"); code != 0 || !strings.Contains(h.out(), "delivered to 1 listener(s)") {
		t.Fatalf("human %d %q", code, h.out())
	}
}

func TestLogsTailResumesWithLastEventID(t *testing.T) {
	var mu sync.Mutex
	var lastIDs []string
	var queries []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := len(lastIDs)
		lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		if r.URL.Path != "/v0/cli/logs/stream" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, "retry: 3000\n: connected\n\n")
		if n == 0 {
			io.WriteString(w, "id: log_1\nevent: log\ndata: {\"id\":\"log_1\",\"method\":\"POST\",\"endpoint\":\"/emails\",\"response_status\":200}\n\n")
			io.WriteString(w, ": keep-alive\n\n")
			io.WriteString(w, "id: log_2\nevent: log\ndata: {\"id\":\"log_2\",\n")
			io.WriteString(w, "data: \"method\":\"GET\",\"endpoint\":\"/domains\",\"response_status\":404}\n\n")
			fl.Flush()
			return // drop the stream
		}
		io.WriteString(w, "id: log_3\nevent: log\ndata: {\"id\":\"log_3\",\"method\":\"POST\",\"endpoint\":\"/emails\",\"response_status\":422}\n\n")
		fl.Flush()
		<-r.Context().Done()
	}))
	defer api.Close()
	_, a, stdout := appFor(t, api.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.tail(ctx, tailSpec{name: "logs tail", path: "/v0/cli/logs/stream", event: "log", query: map[string][]string{"status": {"4xx"}}, human: a.logLine})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stdout.String(), "log_3") {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	recs := ndjson(t, stdout.String())
	if len(recs) != 3 || recs[1]["endpoint"] != "/domains" || recs[2]["id"] != "log_3" {
		t.Fatalf("records %s", stdout.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lastIDs) < 2 || lastIDs[0] != "" || lastIDs[1] != "log_2" || queries[0] != "status=4xx" {
		t.Fatalf("Last-Event-ID %v queries %v", lastIDs, queries)
	}
}

func TestEventsTailCommand(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/cli/events/stream" || r.URL.Query().Get("type") != "email.bounced,email.delivered" || r.URL.Query().Get("email_id") != "e1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(422)
			io.WriteString(w, `{"statusCode":422,"name":"validation_error","message":"bad"}`)
			return
		}
		w.WriteHeader(401)
	}))
	defer api.Close()
	h := newHarness(t, api.URL)
	h.env["SENDIO_API_KEY"] = "mx_k"
	// The first connection's error is the exit code (no endless retries).
	if code := h.run("events", "tail", "--type", "email.bounced,email.delivered", "--email", "e1"); code != 3 {
		t.Fatalf("code %d %s", code, h.errOut())
	}
	if code := h.run("events", "tail", "--type", "x"); code != 1 {
		t.Fatalf("422: %d", code)
	}
}

func TestReadSSE(t *testing.T) {
	var evs []sseEvent
	err := readSSE(strings.NewReader("retry: 1500\n: c\n\nid: 1\nevent: a\ndata: x\ndata: y\n\ndata:z\n\n"), func(e sseEvent) error { evs = append(evs, e); return nil })
	if err != nil || len(evs) != 2 || evs[0].Data != "x\ny" || evs[0].ID != "1" || evs[0].Event != "a" || evs[1].Data != "z" || evs[0].Retry != 1500*time.Millisecond {
		t.Fatalf("%v %+v", err, evs)
	}
}

func TestInboxWatchExecAndCatchUp(t *testing.T) {
	var mu sync.Mutex
	sessions := 0
	var subs []map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inboxes/agent@x.test/messages" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"list","data":[
				{"id":"m3","direction":"inbound","thread_id":"t3","from":"c@d.e","subject":"While away","snippet":"hi","created_at":"2099-01-01T00:00:00Z"},
				{"id":"o1","direction":"outbound","thread_id":"t1","subject":"Re","created_at":"2099-01-01T00:00:00Z"},
				{"id":"m1","direction":"inbound","thread_id":"t1","subject":"Old","created_at":"2000-01-01T00:00:00Z"}]}`)
			return
		}
		if r.URL.Path != "/v0" || r.Header.Get("Authorization") != "Bearer mx_test_key" {
			w.WriteHeader(401)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		sub, err := wsRead(ctx, c)
		if err != nil {
			return
		}
		mu.Lock()
		sessions++
		n := sessions
		subs = append(subs, sub)
		mu.Unlock()
		wsWrite(ctx, c, map[string]any{"type": "subscribed", "inbox_ids": sub["inbox_ids"]})
		if n == 1 {
			wsWrite(ctx, c, map[string]any{"type": "event", "event_type": "message.delivered", "delivery": map[string]any{}})
			wsWrite(ctx, c, map[string]any{"type": "event", "event_type": "message.received", "event_id": "evt_1", "message": map[string]any{
				"inbox_id": "agent@x.test", "thread_id": "t1", "message_id": "m1", "from": "Ada <ada@x.test>", "subject": "Hello", "preview": "First line",
				"text": "First line", "timestamp": "2026-10-08T12:00:00Z"}})
			time.Sleep(300 * time.Millisecond)
			c.Close(websocket.StatusGoingAway, "restart")
			return
		}
		<-ctx.Done()
	}))
	defer api.Close()
	h, a, stdout := appFor(t, api.URL)
	outFile := filepath.Join(t.TempDir(), "exec.log")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.inboxWatch(ctx, "agent@x.test", inboxWatchOpts{
			exec:        `printf '%s|%s|%s|' "$SENDIO_MESSAGE_ID" "$SENDIO_INBOX_ID" "$SENDIO_THREAD_ID" >> ` + outFile + `; cat >> ` + outFile + `; exit 3`,
			execTimeout: 5 * time.Second})
	}()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(outFile)
		if strings.Count(string(b), "|") >= 6 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(outFile)
	log := string(b)
	if !strings.HasPrefix(log, "m1|agent@x.test|t1|{") || !strings.Contains(log, `"subject":"Hello"`) || !strings.Contains(log, "m3|agent@x.test|t3|") {
		t.Fatalf("exec log %q", log)
	}
	recs := ndjson(t, stdout.String())
	if len(recs) != 2 || recs[0]["message_id"] != "m1" || recs[1]["message_id"] != "m3" || recs[1]["catch_up"] != true {
		t.Fatalf("records %s", stdout.String())
	}
	if !strings.Contains(h.errBuf.String(), "--exec failed") {
		t.Fatalf("exec failure not reported: %s", h.errBuf.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if subs[0]["type"] != "subscribe" || fmt.Sprint(subs[0]["inbox_ids"]) != "[agent@x.test]" || fmt.Sprint(subs[0]["event_types"]) != "[message.received]" {
		t.Fatalf("subscribe %v", subs[0])
	}
}

func TestMCPServe(t *testing.T) {
	f := newFake(t)
	f.json("GET /domains", 200, `{"object":"list","data":[{"id":"d1","name":"example.test"}]}`)
	f.json("POST /emails", 200, `{"id":"em_1"}`)
	f.json("GET /emails/em_404", 404, `{"statusCode":404,"name":"not_found","message":"Email not found"}`)
	_, a, _ := appFor(t, f.URL)
	c, err := a.Client(true)
	if err != nil {
		t.Fatal(err)
	}
	run := func(readOnly bool, only []string) *mcp.ClientSession {
		srv, _, err := a.mcpServer(c, readOnly, only)
		if err != nil {
			t.Fatal(err)
		}
		st, ct := mcp.NewInMemoryTransports()
		ctx := context.Background()
		if _, err := srv.Connect(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cs.Close() })
		return cs
	}
	ctx := context.Background()
	cs := run(false, nil)
	if cs.InitializeResult().ServerInfo.Name != "sendio" {
		t.Fatalf("server info %+v", cs.InitializeResult().ServerInfo)
	}
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != len(spec.Tools())+len(freeTools) {
		t.Fatalf("%d tools, tools.json has %d (+%d free inbox tools)", len(list.Tools), len(spec.Tools()), len(freeTools))
	}
	var send *mcp.Tool
	for _, tl := range list.Tools {
		if tl.Name == "send-email" {
			send = tl
		}
	}
	sch, _ := json.Marshal(send.InputSchema)
	if !strings.Contains(string(sch), `"to"`) || !strings.Contains(string(sch), `"required":["to"]`) {
		t.Fatalf("send-email schema %s", sch)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list-domains", Arguments: map[string]any{"limit": 5}})
	if err != nil || res.IsError {
		t.Fatalf("list-domains %v %+v", err, res)
	}
	if txt := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, "example.test") {
		t.Fatalf("result %s", txt)
	}
	if r := f.last(t); r.RawQuery != "limit=5" || r.Auth != "Bearer mx_test_key" || r.Header.Get("X-Sendio-Origin") != "mcp" {
		t.Fatalf("request %+v", r)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "send-email", Arguments: map[string]any{"from": "a@b.c", "to": []string{"x@y.z"}, "subject": "S", "text": "T",
		"replyTo": []string{"r@b.c"}, "idempotencyKey": "k1"}})
	if err != nil || res.IsError {
		t.Fatalf("send-email %v %+v", err, res)
	}
	r := f.last(t)
	mustEqualJSON(t, r.Body, `{"from":"a@b.c","to":["x@y.z"],"subject":"S","text":"T","reply_to":["r@b.c"]}`)
	if r.Header.Get("Idempotency-Key") != "k1" {
		t.Fatalf("idempotency key %v", r.Header)
	}
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "get-email", Arguments: map[string]any{"id": "em_404"}})
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "not_found") {
		t.Fatalf("404 %+v", res)
	}

	ro := run(true, []string{"domains"})
	l2, _ := ro.ListTools(ctx, nil)
	for _, tl := range l2.Tools {
		if !strings.HasPrefix(tl.Name, "list-") && !strings.HasPrefix(tl.Name, "get-") {
			t.Fatalf("read-only exposes %s", tl.Name)
		}
		if !tl.Annotations.ReadOnlyHint {
			t.Fatalf("%s not marked read-only", tl.Name)
		}
	}
	if len(l2.Tools) == 0 || len(l2.Tools) > 5 {
		t.Fatalf("domains read-only tools: %d", len(l2.Tools))
	}
	if _, _, err := a.mcpServer(c, false, []string{"nonsense"}); err == nil {
		t.Fatal("unknown family accepted")
	}
}

// The MCP tools send DELETE arguments as query parameters: delete-inbox with
// keep_mailbox turns the agent off and keeps its mailbox.
func TestMCPToolRequestDeleteQuery(t *testing.T) {
	tl, _ := spec.Lookup("delete-inbox")
	req, err := toolRequest(tl, map[string]any{"id": "in_1", "keep_mailbox": true})
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "DELETE" || req.Path != "/inboxes/in_1" || req.Query.Encode() != "keep_mailbox=true" || len(req.Raw) != 0 {
		t.Fatalf("delete-inbox request %+v", req)
	}
	tl, _ = spec.Lookup("update-mailbox")
	req, err = toolRequest(tl, map[string]any{"id": "mb_1", "warmup_enabled": true})
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "PATCH" || req.Path != "/mailboxes/mb_1" || len(req.Query) != 0 {
		t.Fatalf("update-mailbox request %+v", req)
	}
	if string(req.Raw) != `{"warmup_enabled":true}` {
		t.Fatalf("update-mailbox body %s", req.Raw)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestMCPServeStdio(t *testing.T) {
	f := newFake(t)
	f.json("GET /domains", 200, `{"data":[]}`)
	h := newHarness(t, f.URL)
	h.env["SENDIO_API_KEY"] = "mx_k"
	pr, pw := io.Pipe()
	stdout := &lockedBuf{}
	a := h.newApp("")
	a.Stdin, a.Stdout = pr, stdout
	done := make(chan int, 1)
	go func() { done <- a.Run([]string{"mcp", "serve", "--read-only"}) }()
	io.WriteString(pw, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`+"\n")
	io.WriteString(pw, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	io.WriteString(pw, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stdout.String(), `"id":2`) {
		time.Sleep(10 * time.Millisecond)
	}
	pw.Close()
	if code := <-done; code != 0 {
		t.Fatalf("code %d %s", code, h.errOut())
	}
	sc := bufio.NewScanner(strings.NewReader(stdout.String()))
	sc.Buffer(nil, 4<<20)
	n := 0
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			t.Fatalf("stdout is not JSON-RPC: %q", sc.Text())
		}
		if m["id"] == float64(2) {
			res, _ := m["result"].(map[string]any)
			tools, _ := res["tools"].([]any)
			n = len(tools)
		}
	}
	reads := 0
	for _, tl := range spec.Tools() {
		if tl.Method == "GET" {
			reads++
		}
	}
	for _, ft := range freeTools {
		if ft.readOnly {
			reads++
		}
	}
	if n != reads {
		t.Fatalf("tools/list over stdio: %d, want %d GET + read-only free inbox tools\n%s", n, reads, stdout.String())
	}
}

func TestMCPInstallLocal(t *testing.T) {
	h := newHarness(t, "https://api.example.test")
	exe := filepath.Join(h.home, "bin", "sendio")
	a := h.newApp("")
	_ = a
	h.env["SENDIO_AGENT"] = "yes"
	run := func(args ...string) map[string]any {
		h.stdout.Reset()
		h.stderr.Reset()
		app := h.newApp("")
		app.Executable = func() (string, error) { return exe, nil }
		if code := app.Run(args); code != 0 {
			t.Fatalf("%v: %d %s", args, code, h.errOut())
		}
		return h.outJSON()
	}
	run("mcp", "install", "--client", "cursor", "--local", "--read-only")
	b, err := os.ReadFile(filepath.Join(h.home, ".cursor", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	mustEqualJSON(t, b, `{"mcpServers":{"sendio":{"command":"`+exe+`","args":["mcp","serve","--read-only"]}}}`)
	plan := run("-p", "work", "mcp", "install", "--client", "claude-code", "--local")
	cmd := fmt.Sprint(plan["command"])
	if !strings.Contains(cmd, "--transport stdio sendio") || !strings.Contains(cmd, "-- "+exe+" --profile work mcp serve") {
		t.Fatalf("claude-code plan %v", plan)
	}
	run("mcp", "install", "--client", "codex", "--local")
	toml, _ := os.ReadFile(filepath.Join(h.home, ".codex", "config.toml"))
	if !strings.Contains(string(toml), `command = "`+exe+`"`) || !strings.Contains(string(toml), `args = ["mcp", "serve"]`) {
		t.Fatalf("codex %s", toml)
	}
	h.stdout.Reset()
	if code := h.newApp("").Run([]string{"mcp", "install", "--client", "claude-ai", "--local"}); code != 2 {
		t.Fatalf("claude-ai local: %d", code)
	}
}
