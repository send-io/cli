package cli

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmailsSendRequest(t *testing.T) {
	f := newFake(t)
	f.json("POST /emails", 200, `{"id":"e1"}`)
	h := newHarness(t, f.URL)
	withKey(h)
	att := filepath.Join(h.home, "report.pdf")
	os.WriteFile(att, []byte("%PDF-1.4"), 0o644)
	os.WriteFile(filepath.Join(h.home, "mail.html"), []byte("<p>Hi</p>"), 0o644)
	code := h.runIn("plain text\n", "emails", "send", "--from", "Acme <hi@acme.dev>", "--to", "a@x.dev,b@x.dev", "--to", "c@x.dev",
		"--cc", "d@x.dev", "--reply-to", "r@acme.dev", "--subject", "Report", "--html", "@mail.html", "--text", "-",
		"--attach", "report.pdf", "--header", "X-Campaign=q4", "--tag", "kind=report", "--scheduled-at", "in 1h",
		"--idempotency-key", "abc", "--topic-id", "top1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	r := f.last(t)
	if r.Header.Get("Idempotency-Key") != "abc" {
		t.Fatalf("idempotency key %q", r.Header.Get("Idempotency-Key"))
	}
	mustEqualJSON(t, r.Body, `{"from":"Acme <hi@acme.dev>","to":["a@x.dev","b@x.dev","c@x.dev"],"cc":["d@x.dev"],"reply_to":["r@acme.dev"],
		"subject":"Report","html":"<p>Hi</p>","text":"plain text\n","topic_id":"top1","scheduled_at":"2026-10-08T13:00:00Z",
		"headers":{"X-Campaign":"q4"},"tags":[{"name":"kind","value":"report"}],
		"attachments":[{"filename":"report.pdf","content":"`+base64.StdEncoding.EncodeToString([]byte("%PDF-1.4"))+`","content_type":"application/pdf"}]}`)
	if m := h.outJSON(); m["id"] != "e1" {
		t.Fatalf("output %v", m)
	}

	// Template with variables (from --data and --var), auto idempotency key.
	os.WriteFile(filepath.Join(h.home, "vars.json"), []byte(`{"plan":"pro","seats":3}`), 0o644)
	if code := h.run("emails", "send", "--template", "welcome", "--to", "a@x.dev", "--data", "@vars.json", "--var", "name=Ada", "--var", "vip:=true", "--idempotency-key", "auto"); code != 0 {
		t.Fatal(h.errOut())
	}
	r = f.last(t)
	mustEqualJSON(t, r.Body, `{"to":["a@x.dev"],"template":{"id":"welcome","variables":{"plan":"pro","seats":3,"name":"Ada","vip":true}}}`)
	if len(r.Header.Get("Idempotency-Key")) != 36 {
		t.Fatalf("auto key %q", r.Header.Get("Idempotency-Key"))
	}

	// Template version: rendered and sent inline.
	f.json("POST /templates/welcome/render", 200, `{"subject":"Hi Ada","html":"<p>v3</p>","text":"v3"}`)
	f.json("GET /templates/welcome/versions/3", 200, `{"from":"hi@acme.dev","reply_to":["r@acme.dev"]}`)
	if code := h.run("emails", "send", "--template", "welcome@3", "--to", "a@x.dev", "--var", "name=Ada"); code != 0 {
		t.Fatal(h.errOut())
	}
	mustEqualJSON(t, f.last(t).Body, `{"to":["a@x.dev"],"from":"hi@acme.dev","reply_to":["r@acme.dev"],"subject":"Hi Ada","html":"<p>v3</p>","text":"v3"}`)

	if code := h.run("emails", "send", "--from", "a@x.dev", "--subject", "x"); code != 2 {
		t.Fatalf("missing --to exit %d", code)
	}
}

func TestEmailsSendWaitDelivered(t *testing.T) {
	f := newFake(t)
	f.json("POST /emails", 200, `{"id":"e1"}`)
	polls := 0
	f.on("GET /emails/e1", func(w http.ResponseWriter, r *http.Request) {
		polls++
		ev := map[int]string{1: "queued", 2: "sent"}[polls]
		if ev == "" {
			ev = "delivered"
		}
		fmt.Fprintf(w, `{"id":"e1","object":"email","last_event":%q,"to":["a@x.dev"]}`, ev)
	})
	f.json("GET /emails/e1/events", 200, `{"object":"list","email_id":"e1","data":[{"type":"sent","created_at":"t1"},{"type":"delivered","recipient":"a@x.dev","created_at":"t2","detail":"250 OK"}]}`)
	h := newHarness(t, f.URL)
	withKey(h)
	code := h.run("emails", "send", "--from", "hi@acme.dev", "--to", "a@x.dev", "--subject", "x", "--text", "y", "--wait", "delivered")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	m := h.outJSON()
	if m["last_event"] != "delivered" || m["ok"] != true {
		t.Fatalf("result %v", m)
	}
	ev, _ := m["event"].(map[string]any)
	if ev["detail"] != "250 OK" {
		t.Fatalf("event %v", ev)
	}
	if polls != 3 {
		t.Fatalf("polled %d times", polls)
	}
}

func TestEmailsSendWaitTimeoutAndBounce(t *testing.T) {
	f := newFake(t)
	f.json("POST /emails", 200, `{"id":"e1"}`)
	state := "sent"
	f.on("GET /emails/e1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"e1","last_event":%q}`, state)
	})
	f.json("GET /emails/e1/events", 200, `{"data":[{"type":"bounced","detail":"550 no such user"}]}`)
	h := newHarness(t, f.URL)
	withKey(h)
	code := h.run("emails", "send", "--from", "hi@acme.dev", "--to", "a@x.dev", "--subject", "x", "--text", "y", "--wait", "delivered", "--timeout", "10s")
	if code != 7 {
		t.Fatalf("timeout exit %d: %s", code, h.errOut())
	}
	if e := h.errJSON(); e["code"] != "wait_timeout" || !strings.Contains(fmt.Sprint(e["message"]), "still sent") {
		t.Fatalf("envelope %v", e)
	}
	state = "bounced"
	code = h.run("emails", "send", "--from", "hi@acme.dev", "--to", "a@x.dev", "--subject", "x", "--text", "y", "--wait", "delivered")
	if code != 1 {
		t.Fatalf("bounce exit %d", code)
	}
	if e := h.errJSON(); e["code"] != "email_bounced" {
		t.Fatalf("bounce envelope %v", e)
	}
	if m := h.outJSON(); m["ok"] != false || m["last_event"] != "bounced" {
		t.Fatalf("bounce result %v", m)
	}
	// --wait bounced succeeds on a bounce.
	if code := h.run("emails", "send", "--from", "hi@acme.dev", "--to", "a@x.dev", "--subject", "x", "--text", "y", "--wait", "bounced"); code != 0 {
		t.Fatalf("wait bounced exit %d", code)
	}
}

func TestWaitOutcome(t *testing.T) {
	cases := []struct {
		want, state string
		out         int
	}{
		{"delivered", "sent", 0}, {"delivered", "delivered", 1}, {"delivered", "opened", 1}, {"delivered", "bounced", -1},
		{"delivered", "delivery_delayed", 0}, {"bounced", "delivered", -1}, {"bounced", "bounced", 1}, {"any", "queued", 0},
		{"any", "bounced", 1}, {"sent", "queued", 0}, {"sent", "sent", 1}, {"sent", "delivered", 1}, {"sent", "failed", -1},
		{"opened", "clicked", 1}, {"opened", "delivered", 0},
	}
	for _, c := range cases {
		if got := waitOutcome(c.want, c.state); got != c.out {
			t.Errorf("waitOutcome(%s, %s) = %d, want %d", c.want, c.state, got, c.out)
		}
	}
}

func TestEmailsBatch(t *testing.T) {
	f := newFake(t)
	f.json("POST /emails/batch", 200, `{"data":[{"id":"e1"},{"id":"e2"}]}`)
	h := newHarness(t, f.URL)
	withKey(h)
	nd := `{"from":"a@x.dev","to":["b@x.dev"],"subject":"1","text":"x"}` + "\n" + `{"from":"a@x.dev","to":["c@x.dev"],"subject":"2","text":"y"}` + "\n"
	if code := h.runIn(nd, "emails", "batch", "-"); code != 0 {
		t.Fatal(h.errOut())
	}
	mustEqualJSON(t, f.last(t).Body, `[{"from":"a@x.dev","to":["b@x.dev"],"subject":"1","text":"x"},{"from":"a@x.dev","to":["c@x.dev"],"subject":"2","text":"y"}]`)
	os.WriteFile(filepath.Join(h.home, "b.json"), []byte(`[{"from":"a@x.dev","to":"b@x.dev","subject":"1","text":"x"}]`), 0o644)
	if code := h.run("emails", "batch", "--file", "b.json"); code != 0 {
		t.Fatal(h.errOut())
	}
}
