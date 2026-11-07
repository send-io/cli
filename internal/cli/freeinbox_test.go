package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"send.io/cli/internal/freeinbox"
	"send.io/cli/internal/freeinbox/freetest"
)

func freeHarness(t *testing.T, relay string) *harness {
	t.Helper()
	h := newHarness(t, "")
	h.env["SENDIO_HOME"] = filepath.Join(h.home, "sendio-home")
	h.env["SENDIO_FREE_RELAY"] = relay
	return h
}

func otp(to, code string) []byte {
	return []byte(fmt.Sprintf("From: Acme <no-reply@acme.test>\r\nTo: %s\r\nSubject: Your Acme code\r\n\r\nYour verification code is %s.\r\n", to, code))
}

func TestFreeInboxCommands(t *testing.T) {
	r := freetest.New()
	defer r.Close()
	h := freeHarness(t, r.URL)

	// No sign-in needed; flags are required off a terminal.
	if code := h.run("inbox", "create"); code != 2 {
		t.Fatalf("create without flags: exit %d %s", code, h.errOut())
	}
	if code := h.run("inbox", "create", "--name", "Bot", "--org", "Acme", "--json"); code != 0 {
		t.Fatalf("create: %d %s", code, h.errOut())
	}
	m := h.outJSON()
	addr := "bot@acme." + freetest.Pool
	if m["address"] != addr || !strings.Contains(fmt.Sprint(m["recovery_phrase"]), "-") || m["key_path"] == nil {
		t.Fatalf("create output %v", m)
	}
	key := filepath.Join(h.env["SENDIO_HOME"], "free", addr, "key.json")
	if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key.json: %v %v", err, fi)
	}
	// Taken: exit 1 with the suggestions in the hint.
	if code := h.run("inbox", "create", "--name", "bot", "--org", "acme"); code != 1 {
		t.Fatalf("taken: exit %d", code)
	}
	e := h.errJSON()
	if e["code"] != "taken" || !strings.Contains(fmt.Sprint(e["hint"]), "bot2@acme."+freetest.Pool) {
		t.Fatalf("taken error %v", e)
	}
	if code := h.run("inbox", "check", "--name", "bot", "--org", "acme"); code != 0 || h.outJSON()["available"] != false {
		t.Fatalf("check: %d %s", code, h.out())
	}

	// wait --code prints the bare code.
	go func() {
		time.Sleep(300 * time.Millisecond)
		r.Deliver(addr, []byte("From: x@y.test\r\nSubject: hello\r\n\r\nno code\r\n"))
		r.Deliver(addr, otp(addr, "482913"))
	}()
	if code := h.run("inbox", "wait", "--code", "--timeout", "10s"); code != 0 {
		t.Fatalf("wait: %d %s", code, h.errOut())
	}
	if strings.TrimSpace(h.out()) != "482913" {
		t.Fatalf("wait printed %q", h.out())
	}
	// --json gives the message with the code.
	go func() { time.Sleep(200 * time.Millisecond); r.Deliver(addr, otp(addr, "111222")) }()
	if code := h.run("inbox", "wait", "--code", "--from", "acme", "--json"); code != 0 {
		t.Fatalf("wait --json: %d %s", code, h.errOut())
	}
	if w := h.outJSON(); w["code"] != "111222" || w["subject"] != "Your Acme code" {
		t.Fatalf("wait --json %v", w)
	}
	// Timeout: exit 1.
	if code := h.run("inbox", "wait", "--timeout", "300ms"); code != 1 || h.errJSON()["code"] != "timeout" {
		t.Fatalf("timeout: %d %s", code, h.errOut())
	}

	// poll and read.
	r.Deliver(addr, otp(addr, "333444"))
	if code := h.run("inbox", "poll", "--wait", "0"); code != 0 {
		t.Fatalf("poll: %d %s", code, h.errOut())
	}
	if msgs := h.outJSON()["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("poll messages %v", msgs)
	}
	if code := h.run("inbox", "read", "--limit", "2"); code != 0 {
		t.Fatalf("read: %d %s", code, h.errOut())
	}
	data := h.outJSON()["data"].([]any)
	if len(data) != 2 || data[0].(map[string]any)["code"] != "333444" {
		t.Fatalf("read data %v", data)
	}
	if code := h.run("inbox", "read", "--id", "1"); code != 0 || h.outJSON()["subject"] != "hello" {
		t.Fatalf("read --id: %d %s", code, h.out())
	}
	if r.Pending(addr) != 0 {
		t.Fatalf("%d messages not acked", r.Pending(addr))
	}

	// list, recover, remove.
	if code := h.run("inbox", "list"); code != 0 {
		t.Fatal(h.errOut())
	}
	list := h.outJSON()["data"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["messages"] != float64(4) {
		t.Fatalf("list %v", list)
	}
	phrase := fmt.Sprint(m["recovery_phrase"])
	if code := h.run("inbox", "remove", addr); code != 2 || h.errJSON()["code"] != "confirmation_required" {
		t.Fatalf("remove without --yes: %d", code)
	}
	if code := h.run("inbox", "remove", addr, "--yes"); code != 0 {
		t.Fatal(h.errOut())
	}
	if code := h.run("inbox", "read"); code != 5 {
		t.Fatalf("read with no inbox: %d", code)
	}
	if code := h.run("inbox", "recover", phrase, addr); code != 0 {
		t.Fatalf("recover: %s", h.errOut())
	}
	if code := h.run("inbox", "recover", strings.Repeat("aaaa-", 12)+"aaaa", addr); code == 0 {
		t.Fatal("recover with a wrong phrase succeeded")
	}
}

func TestFreeInboxWatch(t *testing.T) {
	r := freetest.New()
	defer r.Close()
	h := freeHarness(t, r.URL)
	if code := h.run("inbox", "create", "--name", "watcher", "--org", "acme"); code != 0 {
		t.Fatal(h.errOut())
	}
	addr := "watcher@acme." + freetest.Pool
	r.Deliver(addr, otp(addr, "000001")) // waiting before watch starts
	a := h.newApp("")
	outb, errb := &lockedBuf{}, &lockedBuf{}
	a.Stdout, a.Stderr = outb, errb
	_ = a.resolveModes()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.freeWatch(ctx, nil, "") }()
	waitUntil(t, func() bool { return strings.Count(outb.String(), "\n") == 1 })
	r.Deliver(addr, append([]byte("From: a@b.test\r\nSubject: tool\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nhi\r\n"),
		[]byte("--B\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"setup.exe\"\r\n\r\nMZxx\r\n--B--\r\n")...))
	waitUntil(t, func() bool { return strings.Count(outb.String(), "\n") == 2 })
	r.DropConnections()
	time.Sleep(100 * time.Millisecond)
	r.Deliver(addr, otp(addr, "000003"))
	waitUntil(t, func() bool { return strings.Count(outb.String(), "\n") == 3 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lines := ndjson(t, outb.String())
	for i, l := range lines {
		if l["seq"] != float64(i+1) {
			t.Fatalf("line %d: %v", i, l)
		}
	}
	if lines[1]["risky"] != true || !strings.Contains(errb.String(), "setup.exe") {
		t.Fatalf("risky not flagged: %v / %s", lines[1], errb.String())
	}
}

func TestFreeInboxMCP(t *testing.T) {
	r := freetest.New()
	defer r.Close()
	h := freeHarness(t, r.URL)
	a := h.newApp("")
	_ = a.resolveModes()
	// Signed out: only the free tools.
	srv, n, err := a.mcpServer(nil, false, nil)
	if err != nil || n != len(freeTools) {
		t.Fatalf("%d tools, %v", n, err)
	}
	cs := connectMCP(t, srv)
	ctx := context.Background()
	call := func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return m, res.IsError
	}
	m, isErr := call("free_inbox_create", map[string]any{"name": "mcp", "org": "acme"})
	addr := "mcp@acme." + freetest.Pool
	if isErr || m["address"] != addr || m["recovery_phrase"] == nil || !strings.Contains(fmt.Sprint(m["note"]), "once") {
		t.Fatalf("create %v", m)
	}
	m, isErr = call("free_inbox_create", map[string]any{"name": "mcp", "org": "acme"})
	if isErr || m["available"] != false || len(m["suggestions"].([]any)) == 0 {
		t.Fatalf("taken %v", m)
	}
	go func() { time.Sleep(300 * time.Millisecond); r.Deliver(addr, otp(addr, "765432")) }()
	m, isErr = call("free_inbox_wait_for_email", map[string]any{"timeout_seconds": 10, "extract_code": true, "from": "acme"})
	if isErr || m["code"] != "765432" {
		t.Fatalf("wait %v", m)
	}
	m, isErr = call("free_inbox_wait_for_email", map[string]any{"address": addr, "timeout_seconds": 1})
	if !isErr || m["error"] != "timeout" {
		t.Fatalf("timeout %v", m)
	}
	r.Deliver(addr, otp(addr, "123123"))
	m, isErr = call("free_inbox_read", map[string]any{"limit": 5})
	if isErr || len(m["messages"].([]any)) != 2 {
		t.Fatalf("read %v", m)
	}
	m, _ = call("free_inbox_list", nil)
	if len(m["inboxes"].([]any)) != 1 {
		t.Fatalf("list %v", m)
	}
	// --tools free-inbox --read-only.
	if _, n, _ := a.mcpServer(nil, true, []string{"free-inbox"}); n != 3 {
		t.Fatalf("read-only free tools: %d", n)
	}
	if _, n, _ := a.mcpServer(nil, false, []string{"free_inbox_create"}); n != 1 {
		t.Fatalf("one tool: %d", n)
	}
}

func connectMCP(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
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

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFreeInboxConnect(t *testing.T) {
	r := freetest.New()
	defer r.Close()
	h := freeHarness(t, r.URL)
	h.clock.now = time.Now().Truncate(time.Second)
	if code := h.run("inbox", "connect"); code != 5 || h.errJSON()["code"] != "no_inbox" {
		t.Fatalf("connect without inbox: %d %s", code, h.errOut())
	}
	if code := h.run("inbox", "create", "--name", "conn", "--org", "acme"); code != 0 {
		t.Fatal(h.errOut())
	}
	addr := "conn@acme." + freetest.Pool

	// Agent/--json: {address, link, expires_at}; the only inbox is picked.
	if code := h.run("inbox", "connect", "--json"); code != 0 {
		t.Fatalf("connect: %d %s", code, h.errOut())
	}
	m := h.outJSON()
	link := fmt.Sprint(m["link"])
	if len(m) != 3 || m["address"] != addr || m["expires_at"] != h.clock.now.Add(30*time.Minute).UTC().Format(time.RFC3339) ||
		!strings.HasPrefix(link, freeinbox.DefaultAppURL+"/connect#a="+addr+"&t=") {
		t.Fatalf("connect output %v", m)
	}
	// SENDIO_APP_URL moves the origin; --open opens it (best effort).
	h.env["SENDIO_APP_URL"] = "https://dev.app.sendio.test/"
	if code := h.run("inbox", "connect", addr, "--open", "--json"); code != 0 {
		t.Fatalf("connect --open: %d %s", code, h.errOut())
	}
	if l := fmt.Sprint(h.outJSON()["link"]); !strings.HasPrefix(l, "https://dev.app.sendio.test/connect#a=") || len(h.opened) != 1 || h.opened[0] != l {
		t.Fatalf("link %s opened %v", l, h.opened)
	}
	delete(h.env, "SENDIO_APP_URL")
	// Human output: the message, then the link on its own line.
	h.stdout.Reset()
	a := h.newApp("")
	_ = a.resolveModes()
	a.P().Machine = false
	if err := a.freeConnect([]string{addr}, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(h.out(), "\n")
	if lines[0] != "Open this link in a browser to attach "+addr+" to your send.io workspace and enable sending. It works once and expires in 30 minutes." ||
		!strings.HasPrefix(lines[2], "https://app.send.io/connect#a=") {
		t.Fatalf("human output:\n%s", h.out())
	}

	// The user opens the link: the backend claims with its fields.
	u, _ := url.Parse(link)
	q, _ := url.ParseQuery(u.Fragment)
	ts, _ := strconv.ParseInt(q.Get("t"), 10, 64)
	if err := r.ClaimWithLink(q.Get("a"), ts, q.Get("n"), q.Get("s")); err != nil {
		t.Fatal(err)
	}
	// wait, poll and watch stop cleanly with a clear message.
	claimedMsg := addr + " was moved into a send.io workspace; read it in the dashboard."
	for _, args := range [][]string{{"inbox", "wait", "--timeout", "5s"}, {"inbox", "poll", "--wait", "0"}, {"inbox", "watch"}} {
		if code := h.run(args...); code != 1 {
			t.Fatalf("%v: exit %d %s", args, code, h.errOut())
		}
		if e := h.errJSON(); e["code"] != "claimed" || e["message"] != claimedMsg || !strings.Contains(fmt.Sprint(e["hint"]), "workspace") {
			t.Fatalf("%v: %v", args, e)
		}
	}
	// read still shows what is saved here, with a warning.
	if code := h.run("inbox", "read"); code != 0 || !strings.Contains(h.errOut(), "moved into a send.io workspace") {
		t.Fatalf("read: %d %s", code, h.errOut())
	}
}

func TestFreeInboxConnectMCP(t *testing.T) {
	r := freetest.New()
	defer r.Close()
	h := freeHarness(t, r.URL)
	h.clock.now = time.Now()
	a := h.newApp("")
	_ = a.resolveModes()
	srv, _, err := a.mcpServer(nil, false, []string{"free_inbox_create", "free_inbox_connect", "free_inbox_wait_for_email", "free_inbox_read"})
	if err != nil {
		t.Fatal(err)
	}
	cs := connectMCP(t, srv)
	ctx := context.Background()
	call := func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return m, res.IsError
	}
	if m, isErr := call("free_inbox_connect", nil); !isErr || !strings.Contains(fmt.Sprint(m["error"]), "no free inbox") {
		t.Fatalf("connect without inbox %v", m)
	}
	if _, isErr := call("free_inbox_create", map[string]any{"name": "tool", "org": "acme"}); isErr {
		t.Fatal("create")
	}
	addr := "tool@acme." + freetest.Pool
	m, isErr := call("free_inbox_connect", map[string]any{"address": addr})
	if isErr || m["address"] != addr || !strings.HasPrefix(fmt.Sprint(m["link"]), "https://app.send.io/connect#a="+addr) || m["expires_at"] == nil ||
		m["instructions"] != "Give this link to your user: they open it, sign in, and the mailbox moves into their send.io workspace where it can also send." {
		t.Fatalf("connect %v", m)
	}
	u, _ := url.Parse(fmt.Sprint(m["link"]))
	q, _ := url.ParseQuery(u.Fragment)
	ts, _ := strconv.ParseInt(q.Get("t"), 10, 64)
	if err := r.ClaimWithLink(addr, ts, q.Get("n"), q.Get("s")); err != nil {
		t.Fatal(err)
	}
	if m, isErr := call("free_inbox_wait_for_email", map[string]any{"timeout_seconds": 5}); !isErr || m["code"] != "claimed" {
		t.Fatalf("wait after claim %v", m)
	}
	if m, isErr := call("free_inbox_read", nil); isErr || m["code"] != "claimed" || !strings.Contains(fmt.Sprint(m["warning"]), "workspace") {
		t.Fatalf("read after claim %v", m)
	}
	// Not offered read-only.
	if _, n, _ := a.mcpServer(nil, true, []string{"free_inbox_connect"}); n != 0 {
		t.Fatalf("read-only: %d", n)
	}
}
