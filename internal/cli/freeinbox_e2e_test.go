package cli

// End-to-end test of the free inbox commands and MCP tools against the real
// relay and free-mx (the devstack binary: embedded NATS + relay + free-mx).
// It runs only when SENDIO_FREE_DEVSTACK names the binary:
//
//	SENDIO_FREE_DEVSTACK=/path/to/devstack go test ./internal/cli -run E2E -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type devstack struct {
	bin, store       string
	relayAddr, smtpA string
	cmd              *exec.Cmd
	t                *testing.T
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func (d *devstack) start() {
	d.t.Helper()
	d.cmd = exec.Command(d.bin, "-relay", d.relayAddr, "-smtp", d.smtpA, "-nats-port", "-1", "-store", d.store)
	d.cmd.Stdout, d.cmd.Stderr = os.Stderr, os.Stderr
	if err := d.cmd.Start(); err != nil {
		d.t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get("http://" + d.relayAddr + "/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("devstack did not start: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (d *devstack) stop() {
	if d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = d.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = d.cmd.Process.Kill()
			<-done
		}
		d.cmd = nil
	}
}

// send delivers a message over plain SMTP (no STARTTLS: the dev cert is
// self-signed). It may run in a goroutine, so it reports with Errorf.
func (d *devstack) send(to, subject, body string) {
	if err := d.trySend(to, subject, body); err != nil {
		d.t.Errorf("smtp to %s: %v", to, err)
	}
}

func (d *devstack) trySend(to, subject, body string) error {
	c, err := smtp.Dial(d.smtpA)
	if err != nil {
		return err
	}
	defer c.Close()
	msg := fmt.Sprintf("From: Acme Login <no-reply@acme.example>\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%d@acme.example>\r\n\r\n%s\r\n",
		to, subject, time.Now().Format(time.RFC1123Z), time.Now().UnixNano(), body)
	if err := c.Mail("no-reply@acme.example"); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("RCPT: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	return c.Quit()
}

func TestFreeInboxE2E(t *testing.T) {
	bin := os.Getenv("SENDIO_FREE_DEVSTACK")
	if bin == "" {
		t.Skip("set SENDIO_FREE_DEVSTACK to the devstack binary to run the end-to-end test")
	}
	d := &devstack{bin: bin, store: t.TempDir(), relayAddr: freePort(t), smtpA: freePort(t), t: t}
	d.start()
	defer d.stop()
	relay := "http://" + d.relayAddr

	h := newHarness(t, "")
	h.env["SENDIO_HOME"] = filepath.Join(h.home, "sendio-home")

	// create (signed out, --relay flag).
	if code := h.run("inbox", "create", "--name", "e2e", "--org", "cliorg", "--relay", relay, "--json"); code != 0 {
		t.Fatalf("create: %d %s", code, h.errOut())
	}
	cr := h.outJSON()
	addr := fmt.Sprint(cr["address"])
	if addr != "e2e@cliorg.sendioinbox.test" || cr["recovery_phrase"] == nil {
		t.Fatalf("create %v", cr)
	}
	t.Logf("created %s", addr)
	// Taken, with suggestions from the real relay.
	if code := h.run("inbox", "create", "--name", "e2e", "--org", "cliorg", "--relay", relay); code != 1 {
		t.Fatalf("taken: exit %d %s", code, h.errOut())
	}
	t.Logf("taken: %v", h.errJSON())

	// wait --code: the code from a mail sent over SMTP. The relay comes
	// from key.json from here on.
	go func() {
		time.Sleep(time.Second)
		d.send(addr, "Your Acme verification code", "Your verification code is 582034. It expires in 10 minutes.")
	}()
	if code := h.run("inbox", "wait", "--code", "--timeout", "60s"); code != 0 {
		t.Fatalf("wait: %d %s", code, h.errOut())
	}
	if got := strings.TrimSpace(h.out()); got != "582034" {
		t.Fatalf("wait --code printed %q", got)
	}
	t.Logf("wait --code: %s", strings.TrimSpace(h.out()))

	// watch: live, across a relay restart, then a replay after the watcher
	// was down; no duplicates.
	a := h.newApp("")
	outb, errb := &lockedBuf{}, &lockedBuf{}
	a.Stdout, a.Stderr = outb, errb
	a.Sleep = nil // real backoff
	_ = a.resolveModes()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.freeWatch(ctx, nil, "") }()
	time.Sleep(500 * time.Millisecond)
	d.send(addr, "Live 1", "first live message")
	d.send(addr, "Live 2", "second live message")
	waitUntil(t, func() bool { return strings.Count(outb.String(), "\n") == 2 })
	d.stop()
	d.start()
	time.Sleep(4 * time.Second) // reconnect backoff
	d.send(addr, "After restart", "message after the relay restart")
	waitUntil(t, func() bool { return strings.Count(outb.String(), "\n") == 3 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	d.send(addr, "While nobody watched", "queued while the watcher was down")
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- a.freeWatch(ctx, nil, "") }()
	waitUntil(t, func() bool { return strings.Count(outb.String(), "\n") == 4 })
	time.Sleep(time.Second)
	cancel()
	<-done
	lines := ndjson(t, outb.String())
	seen := map[float64]bool{}
	var subjects []string
	for _, l := range lines {
		s := l["seq"].(float64)
		if seen[s] {
			t.Fatalf("duplicate seq %v in %s", s, outb.String())
		}
		seen[s] = true
		subjects = append(subjects, fmt.Sprint(l["subject"]))
	}
	if strings.Join(subjects, "|") != "Live 1|Live 2|After restart|While nobody watched" {
		t.Fatalf("watch got %v", subjects)
	}
	t.Logf("watch: %v; stderr: %s", subjects, strings.TrimSpace(errb.String()))

	// poll.
	d.send(addr, "Polled", "fetched by long poll")
	if code := h.run("inbox", "poll", "--wait", "10"); code != 0 {
		t.Fatalf("poll: %d %s", code, h.errOut())
	}
	pm := h.outJSON()["messages"].([]any)
	if len(pm) != 1 || pm[0].(map[string]any)["subject"] != "Polled" {
		t.Fatalf("poll %v", pm)
	}
	if code := h.run("inbox", "poll", "--wait", "1"); code != 0 || len(h.outJSON()["messages"].([]any)) != 0 {
		t.Fatalf("second poll not empty: %s", h.out())
	}
	if code := h.run("inbox", "read", "--limit", "10"); code != 0 || len(h.outJSON()["data"].([]any)) != 6 {
		t.Fatalf("read: %s %s", h.out(), h.errOut())
	}

	// recover on a fresh home.
	h2 := newHarness(t, "")
	h2.env["SENDIO_HOME"] = filepath.Join(h2.home, "other-home")
	if code := h2.run("inbox", "recover", fmt.Sprint(cr["recovery_phrase"]), addr, "--relay", relay); code != 0 {
		t.Fatalf("recover: %s", h2.errOut())
	}

	// MCP tools in-process, signed out.
	h.env["SENDIO_FREE_RELAY"] = relay
	ma := h.newApp("")
	_ = ma.resolveModes()
	srv, _, err := ma.mcpServer(nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs := connectMCP(t, srv)
	call := func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &m)
		return m, res.IsError
	}
	m, isErr := call("free_inbox_create", map[string]any{"name": "mcp-agent", "org": "cliorg"})
	if isErr || m["address"] != "mcp-agent@cliorg.sendioinbox.test" {
		t.Fatalf("mcp create %v", m)
	}
	maddr := fmt.Sprint(m["address"])
	go func() { time.Sleep(time.Second); d.send(maddr, "Sign in to Acme", "Use 904417 to sign in.") }()
	m, isErr = call("free_inbox_wait_for_email", map[string]any{"address": maddr, "timeout_seconds": 60, "extract_code": true})
	if isErr || m["code"] != "904417" {
		t.Fatalf("mcp wait %v", m)
	}
	t.Logf("mcp: created %s, code %v", maddr, m["code"])
}
