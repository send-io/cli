package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"send.io/cli/internal/spec"
)

func TestEveryToolIsMapped(t *testing.T) {
	mapped := map[string]bool{}
	paths := map[string]string{}
	for _, rc := range resourceTable {
		if _, ok := spec.Lookup(rc.Tool); !ok {
			t.Errorf("resourceTable names %q, which is not in the MCP registry (internal/spec/tools.json)", rc.Tool)
		}
		if mapped[rc.Tool] {
			t.Errorf("tool %q is mapped twice", rc.Tool)
		}
		mapped[rc.Tool] = true
		if other, dup := paths[rc.Path]; dup {
			t.Errorf("command path %q is used by %s and %s", rc.Path, other, rc.Tool)
		}
		paths[rc.Path] = rc.Tool
		if tl, ok := spec.Lookup(rc.Tool); ok {
			for _, a := range rc.Args {
				if p, ok := tl.Param(a); !ok || p.Type != "string" {
					t.Errorf("%s: positional %q is not a string param of %s", rc.Path, a, rc.Tool)
				}
			}
		}
	}
	for _, tl := range spec.Tools() {
		if !mapped[tl.Name] {
			t.Errorf("MCP tool %q (%s %s) has no CLI command: add a row to resourceTable", tl.Name, tl.Method, tl.Path)
		}
	}
	// Every row resolves to a real command in the tree.
	h := newHarness(t, "")
	root := h.newApp("").Root()
	for _, rc := range resourceTable {
		c, _, err := root.Find(strings.Fields(rc.Path))
		if err != nil || c.CommandPath() != "sendio "+rc.Path {
			t.Errorf("command %q not found in the tree (got %v)", rc.Path, err)
		}
	}
}

func withKey(h *harness) { h.env["SENDIO_API_KEY"] = "mx_test_key_123456" }

// TestFlagToRequest checks the generated commands against golden requests.
func TestFlagToRequest(t *testing.T) {
	f := newFake(t)
	f.on("/", func(w http.ResponseWriter, r *http.Request) {})
	cases := []struct {
		name   string
		args   []string
		method string
		path   string
		query  string
		body   string
		stdin  string
	}{
		{"domains create", []string{"domains", "create", "--name", "example.com", "--open-tracking", "--region", "eu-west-1"},
			"POST", "/domains", "", `{"name":"example.com","open_tracking":true,"region":"eu-west-1"}`, ""},
		{"false bool is sent when set", []string{"domains", "update", "d1", "--click-tracking=false"},
			"PATCH", "/domains/d1", "", `{"click_tracking":false}`, ""},
		{"domains create receiving", []string{"domains", "create", "--name", "inbound.example.com", "--receiving"},
			"POST", "/domains", "", `{"name":"inbound.example.com","receiving":true}`, ""},
		{"domains add receiving", []string{"domains", "add", "inbound.example.com", "--receiving"},
			"POST", "/domains", "", `{"name":"inbound.example.com","receiving":true}`, ""},
		{"domains receiving off", []string{"domains", "update", "d1", "--receiving=false"},
			"PATCH", "/domains/d1", "", `{"receiving":false}`, ""},
		{"contacts create objects", []string{"contacts", "create", "--email", "ada@example.com", "--first-name", "Ada",
			"--properties", "plan=pro", "--properties", "seats:=3", "--segments", `{"id":"s1"}`, "--segments", `[{"id":"s2","status":"pending"}]`},
			"POST", "/contacts", "", `{"email":"ada@example.com","first_name":"Ada","properties":{"plan":"pro","seats":3},"segments":[{"id":"s1"},{"id":"s2","status":"pending"}]}`, ""},
		{"contacts list query", []string{"contacts", "list", "--limit", "5", "--status", "subscribed", "--search", "ada"},
			"GET", "/contacts", "limit=5&search=ada&status=subscribed", "", ""},
		{"path params", []string{"contacts", "segments", "add", "ada@example.com", "seg_1"},
			"POST", "/contacts/ada@example.com/segments/seg_1", "", `{}`, ""},
		{"webhooks lists", []string{"webhooks", "create", "--endpoint", "https://x.dev/hook", "--events", "email.sent,email.delivered", "--events", "email.bounced", "--headers", "X-A=1"},
			"POST", "/webhooks", "", `{"endpoint":"https://x.dev/hook","events":["email.sent","email.delivered","email.bounced"],"headers":{"X-A":"1"}}`, ""},
		{"broadcast test-to hook", []string{"broadcasts", "send", "b1", "--test-to", "a@x.dev,b@x.dev"},
			"POST", "/broadcasts/b1/test", "", `{"to":["a@x.dev","b@x.dev"]}`, ""},
		{"broadcast send schedule", []string{"broadcasts", "send", "b1", "--scheduled-at", "in 2h"},
			"POST", "/broadcasts/b1/send", "", `{"scheduled_at":"2026-10-08T14:00:00Z"}`, ""},
		{"data from stdin plus override", []string{"templates", "update", "welcome", "--data", "-", "--subject", "New"},
			"PATCH", "/templates/welcome", "", `{"name":"W","html":"<p>x</p>","subject":"New"}`, `{"name":"W","html":"<p>x</p>","subject":"Old"}`},
		{"inbox threads renamed flag", []string{"inboxes", "threads", "list", "in_1", "--query", "invoice", "--filter", "unread"},
			"GET", "/inboxes/in_1/threads", "filter=unread&q=invoice", "", ""},
		{"update-thread top-level path", []string{"inboxes", "threads", "update", "th_1", "--read", "--add-labels", "a,b"},
			"PATCH", "/inbox/threads/th_1", "", `{"add_labels":["a","b"],"read":true}`, ""},
		{"delete with yes", []string{"domains", "delete", "d1", "--yes"}, "DELETE", "/domains/d1", "", "", ""},
		{"mailboxes list", []string{"mailboxes", "list"}, "GET", "/mailboxes", "", "", ""},
		{"mailboxes create", []string{"mailboxes", "create", "--address", "hello@acme.test"},
			"POST", "/mailboxes", "", `{"address":"hello@acme.test"}`, ""},
		{"mailboxes warmup off", []string{"mailboxes", "update", "mb_1", "--warmup-enabled=false"},
			"PATCH", "/mailboxes/mb_1", "", `{"warmup_enabled":false}`, ""},
		{"mailboxes reset-password", []string{"mailboxes", "reset-password", "mb_1", "--yes"}, "POST", "/mailboxes/mb_1/password", "", `{}`, ""},
		{"mailboxes delete", []string{"mailboxes", "delete", "mb_1", "--yes"}, "DELETE", "/mailboxes/mb_1", "", "", ""},
		{"agent off keeps the mailbox", []string{"inboxes", "delete", "in_1", "--keep-mailbox", "--yes"},
			"DELETE", "/inboxes/in_1", "keep_mailbox=true", "", ""},
		{"automation templates by category", []string{"automations", "templates", "list", "--category", "billing"},
			"GET", "/automation-templates", "category=billing", "", ""},
		{"automation template preview", []string{"automations", "templates", "get", "abandoned-cart"},
			"GET", "/automation-templates/abandoned-cart", "", "", ""},
		{"automation template use: slug positional, objects as key=value", []string{"automations", "templates", "use", "email-verification-reminder",
			"--version", "2", "--sender", "from=hello@acme.test", "--params", "wait_1=2 days", "--params", "resend_url=https://app.acme.test/verify",
			"--events", `verified:={"mode":"existing","name":"account.confirmed"}`, "--idempotency-key", "setup-1"},
			"POST", "/automations/from-template", "", `{"slug":"email-verification-reminder","version":2,"sender":{"from":"hello@acme.test"},
			"params":{"wait_1":"2 days","resend_url":"https://app.acme.test/verify"},"events":{"verified":{"mode":"existing","name":"account.confirmed"}}}`, ""},
		{"suppressions add alias create", []string{"suppressions", "create", "--email", "x@y.dev", "--origin", "manual"},
			"POST", "/suppressions", "", `{"email":"x@y.dev","origin":"manual"}`, ""},
		{"transactional create", []string{"transactional", "create", "--name", "Sign-in code", "--template-id", "signin-code", "--from", "Acme <security@acme.test>",
			"--subject", "Your code is {{CODE}}", "--reply-to", "help@acme.test", "--track-opens=false", "--idempotency-key", "tx-1"},
			"POST", "/transactional", "", `{"name":"Sign-in code","template_id":"signin-code","from":"Acme <security@acme.test>","subject":"Your code is {{CODE}}",
			"reply_to":["help@acme.test"],"track_opens":false}`, ""},
		{"transactional list filters", []string{"transactional", "list", "--search", "code", "--template-id", "signin-code"},
			"GET", "/transactional", "search=code&template_id=signin-code", "", ""},
		{"transactional update with expected_updated_at", []string{"transactional", "update", "tx_2hX9Yq1bVv0kq3W4m8sZJt1aB7c", "--subject", "New", "--expected-updated-at", "2026-11-16T09:00:00Z"},
			"PATCH", "/transactional/tx_2hX9Yq1bVv0kq3W4m8sZJt1aB7c", "", `{"subject":"New","expected_updated_at":"2026-11-16T09:00:00Z"}`, ""},
		{"transactional test", []string{"transactional", "test", "tx_2hX9Yq1bVv0kq3W4m8sZJt1aB7c", "--to", "me@acme.test", "--variables", "CODE=123456"},
			"POST", "/transactional/tx_2hX9Yq1bVv0kq3W4m8sZJt1aB7c/test", "", `{"to":["me@acme.test"],"variables":{"CODE":"123456"}}`, ""},
		{"transactional stats", []string{"transactional", "stats", "tx_2hX9Yq1bVv0kq3W4m8sZJt1aB7c", "--granularity", "hourly"},
			"GET", "/transactional/tx_2hX9Yq1bVv0kq3W4m8sZJt1aB7c/stats", "granularity=hourly", "", ""},
		{"transactional delete", []string{"transactional", "delete", "tx_2hX9Yq1bVv0kq3W4m8sZJt1aB7c", "--yes"},
			"DELETE", "/transactional/tx_2hX9Yq1bVv0kq3W4m8sZJt1aB7c", "", "", ""},
		{"emails list by transactional email", []string{"emails", "list", "--transactional-id", "tx_2hX9Yq1bVv0kq3W4m8sZJt1aB7c"},
			"GET", "/emails", "transactional_id=tx_2hX9Yq1bVv0kq3W4m8sZJt1aB7c", "", ""},
		{"template publish impact", []string{"templates", "publish-impact", "welcome", "--version", "3"},
			"GET", "/templates/welcome/publish-impact", "version=3", "", ""},
		{"template test with from", []string{"templates", "send-test", "welcome", "--to", "me@acme.test", "--from", "Acme <hi@acme.test>"},
			"POST", "/templates/welcome/test", "", `{"to":["me@acme.test"],"from":"Acme <hi@acme.test>"}`, ""},
		{"broadcast version only", []string{"broadcasts", "update", "b1", "--template-version-id", "6a1f4c2e-8b3d-4e5f-9a7b-0c1d2e3f4a5b"},
			"PATCH", "/broadcasts/b1", "", `{"template_version_id":"6a1f4c2e-8b3d-4e5f-9a7b-0c1d2e3f4a5b"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.on(tc.method+" "+tc.path, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"object":"x","id":"ok"}`)
			})
			h := newHarness(t, f.URL)
			withKey(h)
			if code := h.runIn(tc.stdin, tc.args...); code != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, h.out(), h.errOut())
			}
			r := f.last(t)
			if r.Method != tc.method || r.Path != tc.path {
				t.Fatalf("got %s %s, want %s %s", r.Method, r.Path, tc.method, tc.path)
			}
			if r.Auth != "Bearer mx_test_key_123456" {
				t.Fatalf("auth header %q", r.Auth)
			}
			if tc.query != "" {
				q, _ := url.ParseQuery(r.RawQuery)
				if q.Encode() != tc.query {
					t.Fatalf("query %q, want %q", q.Encode(), tc.query)
				}
			}
			if tc.body != "" {
				mustEqualJSON(t, r.Body, tc.body)
			}
			if strings.Contains(strings.Join(tc.args, " "), "--idempotency-key setup-1") && r.Header.Get("Idempotency-Key") != "setup-1" {
				t.Fatalf("Idempotency-Key header %q", r.Header.Get("Idempotency-Key"))
			}
			if !strings.Contains(r.Header.Get("User-Agent"), "sendio-cli/") {
				t.Fatalf("user agent %q", r.Header.Get("User-Agent"))
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	h := newHarness(t, "http://127.0.0.1:1")
	withKey(h)
	for _, args := range [][]string{
		{"domains", "get"}, // missing arg
		{"automations", "templates", "use", "--sender", "from=a@b.dev"}, // missing positional body arg
		{"domains", "list", "--nope"},                                   // unknown flag
		{"nope"},                                                        // unknown command
		{"domains", "create"},                                           // missing required --name
		{"topics", "create", "--name", "x", "--default-subscription", "maybe"}, // enum
		{"--agent", "sometimes", "whoami"},                                     // bad mode
		{"domains", "list", "--jq", ".[["},                                     // bad jq
	} {
		if code := h.run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2 (stderr %s)", args, code, h.errOut())
			continue
		}
		e := h.errJSON()
		if e["message"] == "" || e["code"] == "" {
			t.Errorf("%v: bad envelope %v", args, e)
		}
	}
}

func TestDestructiveNeedsYesInAgentMode(t *testing.T) {
	f := newFake(t)
	f.json("DELETE /domains/d1", 200, `{"object":"domain","id":"d1","deleted":true}`)
	h := newHarness(t, f.URL)
	withKey(h)
	if code := h.run("domains", "delete", "d1"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if e := h.errJSON(); e["code"] != "confirmation_required" {
		t.Fatalf("envelope %v", e)
	}
	if len(f.requests()) != 0 {
		t.Fatal("a request was sent without --yes")
	}
	if code := h.run("domains", "delete", "d1", "-y"); code != 0 {
		t.Fatalf("exit %d with -y: %s", code, h.errOut())
	}
}

func TestExitCodes(t *testing.T) {
	f := newFake(t)
	f.json("GET /domains/missing", 404, `{"statusCode":404,"name":"not_found","message":"Domain not found."}`)
	f.json("GET /domains/denied", 401, `{"statusCode":401,"name":"invalid_api_key","message":"API key is invalid."}`)
	f.json("GET /domains/forbidden", 403, `{"statusCode":403,"name":"restricted_api_key","message":"This key can only send."}`)
	f.json("GET /domains/slow", 429, `{"statusCode":429,"name":"rate_limit_exceeded","message":"Too many requests."}`)
	f.json("GET /domains/boom", 500, `{"statusCode":500,"name":"application_error","message":"Oops."}`)
	f.json("GET /domains/bad", 422, `{"statusCode":422,"name":"validation_error","message":"Bad."}`)
	h := newHarness(t, f.URL)
	withKey(h)
	for id, want := range map[string]int{"missing": 5, "denied": 3, "forbidden": 3, "slow": 6, "boom": 1, "bad": 1} {
		code := h.run("domains", "get", id)
		if code != want {
			t.Errorf("%s: exit %d, want %d", id, code, want)
			continue
		}
		e := h.errJSON()
		if e["status"] == nil || e["code"] == "" || e["message"] == "" {
			t.Errorf("%s: envelope %v", id, e)
		}
	}
	// Network failure.
	dead := newHarness(t, "http://127.0.0.1:1")
	withKey(dead)
	if code := dead.run("domains", "list"); code != 4 {
		t.Fatalf("network: exit %d, want 4", code)
	}
	if e := dead.errJSON(); e["code"] != "network_error" || e["status"] != nil {
		t.Fatalf("network envelope %v", e)
	}
	// No credentials at all.
	none := newHarness(t, f.URL)
	if code := none.run("domains", "list"); code != 3 {
		t.Fatalf("no auth: exit %d, want 3", code)
	}
	if e := none.errJSON(); e["code"] != "not_authenticated" {
		t.Fatalf("no auth envelope %v", e)
	}
}

func TestOutputModes(t *testing.T) {
	f := newFake(t)
	f.json("GET /domains", 200, `{"object":"list","has_more":false,"data":[{"id":"d1","name":"a.dev","status":"verified","region":"us-east-1","created_at":"2026-10-01"},{"id":"d2","name":"b.dev","status":"pending","region":"eu-west-1","created_at":"2026-10-02"}]}`)
	h := newHarness(t, f.URL)
	withKey(h)

	// Agent mode (stdout not a TTY): JSON.
	if code := h.run("domains", "list"); code != 0 {
		t.Fatal(h.errOut())
	}
	if m := h.outJSON(); len(m["data"].([]any)) != 2 {
		t.Fatalf("agent output %v", m)
	}

	// Human mode: a table, no JSON.
	if code := h.run("domains", "list", "--agent", "no"); code != 0 {
		t.Fatal(h.errOut())
	}
	o := h.out()
	if !strings.Contains(o, "NAME") || !strings.Contains(o, "a.dev") || strings.Contains(o, "{") {
		t.Fatalf("human output:\n%s", o)
	}
	if strings.Contains(o, "\x1b[") {
		t.Fatalf("colours on a non-TTY:\n%q", o)
	}

	// --json in human mode.
	h.run("domains", "list", "--agent", "no", "--json")
	h.outJSON()

	// --jq: strings raw, one per line.
	h.run("domains", "list", "--agent", "no", "--jq", ".data[].name")
	if h.out() != "a.dev\nb.dev\n" {
		t.Fatalf("jq output %q", h.out())
	}
	h.run("domains", "list", "--jq", ".data[0] | {id, status}")
	mustEqualJSON(t, h.stdout.Bytes(), `{"id":"d1","status":"verified"}`)

	// Agent env var forces agent mode even with --agent auto on a TTY.
	h.env["CLAUDECODE"] = "1"
	a := h.newApp("")
	a.StdoutTTY = true
	h.stdout.Reset()
	if a.Run([]string{"domains", "list"}) != 0 {
		t.Fatal(h.errOut())
	}
	h.outJSON()

	// Human errors are a readable line, not JSON.
	f.json("GET /domains/x", 404, `{"statusCode":404,"name":"not_found","message":"Domain not found."}`)
	h.run("domains", "get", "x", "--agent", "no")
	if !strings.HasPrefix(h.errOut(), "Error: Domain not found.") {
		t.Fatalf("human error %q", h.errOut())
	}
}

func TestDryRun(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f.URL)
	withKey(h)
	code := h.run("emails", "send", "--from", "a@x.dev", "--to", "b@y.dev", "--subject", "Hi", "--text", "Hello", "--idempotency-key", "k1", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests sent in dry-run", n)
	}
	m := h.outJSON()
	if m["dry_run"] != true || m["method"] != "POST" || m["url"] != f.URL+"/emails" {
		t.Fatalf("dry-run output %v", m)
	}
	hd := m["headers"].(map[string]any)
	if hd["Idempotency-Key"] != "k1" || strings.Contains(fmt.Sprint(hd["Authorization"]), "123456") {
		t.Fatalf("headers %v", hd)
	}
	b, _ := json.Marshal(m["body"])
	mustEqualJSON(t, b, `{"from":"a@x.dev","to":["b@y.dev"],"subject":"Hi","text":"Hello"}`)

	// Destructive + dry-run: no confirmation needed, nothing sent.
	if code := h.run("domains", "delete", "d1", "--dry-run"); code != 0 {
		t.Fatalf("delete dry-run exit %d: %s", code, h.errOut())
	}
	if len(f.requests()) != 0 {
		t.Fatal("delete was sent in dry-run")
	}
	// Human dry-run shows the request line.
	h.run("webhooks", "create", "--endpoint", "https://x.dev", "--events", "email.sent", "--dry-run", "--agent", "no")
	if !strings.Contains(h.out(), "POST "+f.URL+"/webhooks") || !strings.Contains(h.out(), `"endpoint": "https://x.dev"`) {
		t.Fatalf("human dry-run:\n%s", h.out())
	}
}

func TestPaginationAllNDJSON(t *testing.T) {
	f := newFake(t)
	f.on("GET /contacts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("after") {
		case "":
			fmt.Fprint(w, `{"object":"list","has_more":true,"data":[{"id":"c1"},{"id":"c2"}]}`)
		case "c2":
			fmt.Fprint(w, `{"object":"list","has_more":true,"data":[{"id":"c3"},{"id":"c4"}]}`)
		case "c4":
			fmt.Fprint(w, `{"object":"list","has_more":false,"data":[{"id":"c5"}]}`)
		default:
			w.WriteHeader(400)
		}
	})
	h := newHarness(t, f.URL)
	withKey(h)
	if code := h.run("contacts", "list", "--all", "--limit", "2", "--status", "subscribed"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	lines := strings.Split(strings.TrimSpace(h.out()), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines:\n%s", len(lines), h.out())
	}
	for i, l := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil || m["id"] != fmt.Sprintf("c%d", i+1) {
			t.Fatalf("line %d: %s", i, l)
		}
	}
	for _, r := range f.requests() {
		q, _ := url.ParseQuery(r.RawQuery)
		if q.Get("limit") != "2" || q.Get("status") != "subscribed" {
			t.Fatalf("page query %s", r.RawQuery)
		}
	}
	// --jq applies per item.
	h.run("contacts", "list", "--all", "--jq", ".id")
	if h.out() != "c1\nc2\nc3\nc4\nc5\n" {
		t.Fatalf("jq per item: %q", h.out())
	}
	// Human --all: one table.
	h.run("contacts", "list", "--all", "--agent", "no")
	if strings.Count(h.out(), "ID") != 1 || !strings.Contains(h.out(), "c5") {
		t.Fatalf("human --all:\n%s", h.out())
	}
}

func TestAPICommand(t *testing.T) {
	f := newFake(t)
	f.json("GET /domains", 200, `{"object":"list","data":[{"id":"d1","name":"a.dev"}],"has_more":false}`)
	f.json("POST /compat/postmark/email", 200, `{"MessageID":"m1"}`)
	h := newHarness(t, f.URL)
	withKey(h)
	if code := h.run("api", "GET", "/domains", "-f", "limit=1", "--jq", ".data[].name"); code != 0 {
		t.Fatal(h.errOut())
	}
	if h.out() != "a.dev\n" || f.last(t).RawQuery != "limit=1" {
		t.Fatalf("api GET: %q %q", h.out(), f.last(t).RawQuery)
	}
	if code := h.run("api", "POST", "/compat/postmark/email", "-f", "From=a@x.dev", "-F", "TrackOpens=true", "-F", "Count=2", "-H", "X-Postmark-Server-Token: t"); code != 0 {
		t.Fatal(h.errOut())
	}
	r := f.last(t)
	mustEqualJSON(t, r.Body, `{"From":"a@x.dev","TrackOpens":true,"Count":2}`)
	if r.Header.Get("X-Postmark-Server-Token") != "t" {
		t.Fatal("custom header missing")
	}
	// Implicit method and --input from stdin.
	if code := h.runIn(`{"From":"b@x.dev"}`, "api", "/compat/postmark/email", "--input", "-"); code != 0 {
		t.Fatal(h.errOut())
	}
	mustEqualJSON(t, f.last(t).Body, `{"From":"b@x.dev"}`)
	if code := h.run("api", "GET", "/nope"); code != 5 {
		t.Fatalf("api 404 exit %d", code)
	}
}

func TestCommandsJSON(t *testing.T) {
	h := newHarness(t, "")
	if code := h.run("commands", "--json"); code != 0 {
		t.Fatal(h.errOut())
	}
	m := h.outJSON()
	if m["exit_codes"] == nil || m["global_flags"] == nil {
		t.Fatalf("missing sections: %v", m)
	}
	b := h.out()
	for _, want := range []string{`"path": "sendio emails send"`, `"name": "wait"`, `"mcp_tool": "list-domains"`, `"destructive": true`, `"path": "sendio auth switch"`} {
		if !strings.Contains(b, want) {
			t.Errorf("commands --json lacks %s", want)
		}
	}
}

func TestTemplatesPullPush(t *testing.T) {
	f := newFake(t)
	f.json("GET /templates", 200, `{"object":"list","has_more":false,"data":[{"id":"t1","name":"Welcome","alias":"welcome"}]}`)
	f.json("GET /templates/t1", 200, `{"object":"template","id":"t1","name":"Welcome","alias":"welcome","subject":"Hi {{name}}","from":"a@x.dev","html":"<p>Hi {{name}}</p>","text":"Hi","template_engine":"simple","reply_to":null,"variables":[{"key":"name","type":"string","fallback_value":"there"}]}`)
	f.json("GET /templates/welcome", 200, `{"object":"template","id":"t1","name":"Welcome","alias":"welcome","subject":"Hi {{name}}","from":"a@x.dev","html":"<p>Hi {{name}}</p>","text":"Hi","template_engine":"simple","reply_to":null,"variables":[{"key":"name","type":"string","fallback_value":"there"}]}`)
	f.json("PATCH /templates/t1", 200, `{"object":"template","id":"t1"}`)
	f.json("POST /templates/t1/publish", 200, `{"object":"template","id":"t1"}`)
	f.json("POST /templates", 200, `{"object":"template","id":"t2"}`)
	f.json("POST /templates/t2/publish", 200, `{"object":"template","id":"t2"}`)
	h := newHarness(t, f.URL)
	withKey(h)
	dir := filepath.Join(h.home, "tpl")
	if code := h.run("templates", "pull", dir); code != 0 {
		t.Fatal(h.errOut())
	}
	meta, err := os.ReadFile(filepath.Join(dir, "welcome", "meta.json"))
	if err != nil || !strings.Contains(string(meta), `"alias": "welcome"`) || !strings.Contains(string(meta), `"engine": "simple"`) {
		t.Fatalf("meta.json: %v %s", err, meta)
	}
	// Unchanged push sends nothing.
	if code := h.run("templates", "push", dir); code != 0 {
		t.Fatal(h.errOut())
	}
	if !strings.Contains(h.out(), `"action": "unchanged"`) {
		t.Fatalf("push unchanged: %s", h.out())
	}
	// Edit html, add a new template, push --dry-run: plan only.
	os.WriteFile(filepath.Join(dir, "welcome", "template.html"), []byte("<p>Hello {{name}}</p>\n<p>Bye</p>"), 0o644)
	os.MkdirAll(filepath.Join(dir, "new"), 0o755)
	os.WriteFile(filepath.Join(dir, "new", "meta.json"), []byte(`{"name":"New one","alias":"new-one"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "new", "template.html"), []byte("<p>new</p>"), 0o644)
	n := len(f.requests())
	if code := h.run("templates", "push", dir, "--dry-run", "--publish"); code != 0 {
		t.Fatal(h.errOut())
	}
	for _, r := range f.requests()[n:] {
		if r.Method != "GET" {
			t.Fatalf("dry-run sent %s %s", r.Method, r.Path)
		}
	}
	out := h.outJSON()
	if len(out["requests"].([]any)) < 2 {
		t.Fatalf("planned requests: %v", out["requests"])
	}
	if !strings.Contains(h.out(), "would update") || !strings.Contains(h.out(), "would create") || !strings.Contains(h.out(), "html (+2/-1 lines)") {
		t.Fatalf("dry-run summary: %s", h.out())
	}
	// Real push with publish.
	if code := h.run("templates", "push", dir, "--publish"); code != 0 {
		t.Fatal(h.errOut())
	}
	var patched, published bool
	for _, r := range f.requests() {
		patched = patched || r.Method == "PATCH" && r.Path == "/templates/t1"
		published = published || r.Path == "/templates/t1/publish"
	}
	if !patched || !published {
		t.Fatal("push did not update and publish")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "new", "meta.json")); !strings.Contains(string(b), `"id": "t2"`) {
		t.Fatalf("created id not written back: %s", b)
	}
}

func TestDomainsAddAuto(t *testing.T) {
	f := newFake(t)
	f.json("POST /domains", 200, `{"id":"d1","name":"example.com","status":"not_started","records":[{"record":"SPF","type":"TXT","name":"send","value":"v=spf1 include:x ~all","status":"not_started","ttl":"Auto"}]}`)
	f.json("GET /domains/d1/domain-connect", 200, `{"object":"domain_connect","supported":true,"provider":"Cloudflare","url":"https://dash.cloudflare.example/apply?x=1"}`)
	h := newHarness(t, f.URL)
	withKey(h)
	if code := h.run("domains", "add", "example.com", "--auto", "--agent", "no"); code != 0 {
		t.Fatal(h.errOut())
	}
	if len(h.opened) != 1 || !strings.Contains(h.opened[0], "cloudflare") {
		t.Fatalf("opened %v", h.opened)
	}
	if !strings.Contains(h.out(), "v=spf1") || !strings.Contains(h.out(), "Cloudflare") {
		t.Fatalf("output:\n%s", h.out())
	}
	h.run("domains", "add", "example.com", "--auto")
	m := h.outJSON()
	if m["domain_connect"].(map[string]any)["supported"] != true {
		t.Fatalf("json %v", m)
	}
}

func TestDomainsVerifyWatch(t *testing.T) {
	f := newFake(t)
	calls := 0
	f.json("POST /domains/d1/verify", 200, `{"object":"domain","id":"d1"}`)
	f.on("GET /domains/d1", func(w http.ResponseWriter, r *http.Request) {
		calls++
		st := "pending"
		if calls >= 3 {
			st = "verified"
		}
		fmt.Fprintf(w, `{"id":"d1","name":"example.com","status":%q,"records":[{"record":"DKIM","type":"CNAME","name":"k._domainkey","value":"x","status":%q,"ttl":"Auto"}]}`, st, st)
	})
	h := newHarness(t, f.URL)
	withKey(h)
	if code := h.run("domains", "verify", "d1", "--watch", "--interval", "5s"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	if m := h.outJSON(); m["status"] != "verified" {
		t.Fatalf("result %v", m)
	}
	// Timeout.
	calls = -100
	if code := h.run("domains", "verify", "d1", "--watch", "--interval", "30s", "--timeout", "1m"); code != 7 {
		t.Fatalf("timeout exit %d", code)
	}
}

func TestContactsImportWait(t *testing.T) {
	f := newFake(t)
	f.on("POST /contacts/imports", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(400)
			return
		}
		file, _, _ := r.FormFile("file")
		b := make([]byte, 100)
		n, _ := file.Read(b)
		fmt.Fprintf(w, `{"id":"imp1","status":"queued","echo":%q,"segments":%q}`, string(b[:n]), r.FormValue("segments"))
	})
	polls := 0
	f.on("GET /contacts/imports/imp1", func(w http.ResponseWriter, r *http.Request) {
		polls++
		st := "in_progress"
		if polls > 1 {
			st = "completed"
		}
		fmt.Fprintf(w, `{"id":"imp1","status":%q,"imported":2}`, st)
	})
	h := newHarness(t, f.URL)
	withKey(h)
	os.WriteFile(filepath.Join(h.home, "c.csv"), []byte("email\na@x.dev\nb@x.dev\n"), 0o644)
	if code := h.run("contacts", "import", "c.csv", "--segment", "s1", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	if m := h.outJSON(); m["status"] != "completed" {
		t.Fatalf("result %v", m)
	}
	first := f.requests()[0]
	if !strings.HasPrefix(first.ContentType, "multipart/form-data") || !strings.Contains(string(first.Body), "a@x.dev") || !strings.Contains(string(first.Body), `[{"id":"s1"}]`) ||
		!strings.Contains(string(first.Body), `{"email":"email"}`) {
		t.Fatalf("upload: %s %s", first.ContentType, first.Body)
	}
}

// Without --column the import maps columns by header name (the server
// requires column_map.email), properties by key, and --column wins.
func TestContactsImportHeaderColumns(t *testing.T) {
	f := newFake(t)
	var got []string
	f.on("POST /contacts/imports", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		got = append(got, r.FormValue("column_map"))
		fmt.Fprint(w, `{"id":"imp1","status":"queued"}`)
	})
	f.on("GET /contact-properties", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"p1","key":"plan"}],"has_more":false}`)
	})
	h := newHarness(t, f.URL)
	withKey(h)
	write := func(name, body string) { os.WriteFile(filepath.Join(h.home, name), []byte(body), 0o644) }

	write("a.csv", "\ufeffE-mail;First Name;Last_Name;plan;Company Size\nann@x.dev;Ann;Lee;pro;10\n")
	if code := h.run("contacts", "import", "a.csv"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	mustEqualJSON(t, []byte(got[0]), `{"email":"E-mail","first_name":"First Name","last_name":"Last_Name","properties":{"plan":{"column":"plan"}}}`)

	// --column overrides one field; --create-missing-properties maps every valid key.
	write("b.csv", "email,work_email,tier\na@x.dev,b@x.dev,gold\n")
	if code := h.run("contacts", "import", "b.csv", "--column", "email=work_email", "--create-missing-properties"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	mustEqualJSON(t, []byte(got[1]), `{"email":"work_email","properties":{"tier":{"column":"tier"},"work_email":{"column":"work_email"}}}`)

	// No email column: a usage error before anything is uploaded.
	write("c.csv", "name\nAnn\n")
	if code := h.run("contacts", "import", "c.csv"); code != 2 || len(got) != 2 || !strings.Contains(h.errOut(), "--column email=") {
		t.Fatalf("no email column: exit %d, %d uploads: %s", code, len(got), h.errOut())
	}
	if code := h.run("contacts", "import", "c.csv", "--no-header"); code != 2 || !strings.Contains(h.errOut(), "email:=0") {
		t.Fatalf("no header: exit %d: %s", code, h.errOut())
	}
}

func TestMCPInstall(t *testing.T) {
	h := newHarness(t, "https://api.example.dev")
	// Cursor: merge into an existing file.
	p := filepath.Join(h.home, ".cursor", "mcp.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(`{"mcpServers":{"other":{"url":"https://o"}}}`), 0o644)
	if code := h.run("mcp", "install", "--client", "cursor"); code != 0 {
		t.Fatal(h.errOut())
	}
	b, _ := os.ReadFile(p)
	mustEqualJSON(t, b, `{"mcpServers":{"other":{"url":"https://o"},"sendio":{"url":"https://api.example.dev/mcp"}}}`)
	// Codex TOML with key, replacing an old section.
	tp := filepath.Join(h.home, ".codex", "config.toml")
	os.MkdirAll(filepath.Dir(tp), 0o755)
	os.WriteFile(tp, []byte("model = \"x\"\n\n[mcp_servers.sendio]\nurl = \"old\"\n\n[other]\na = 1\n"), 0o644)
	if code := h.run("mcp", "install", "--client", "codex", "--with-api-key"); code != 0 {
		t.Fatal(h.errOut())
	}
	tb, _ := os.ReadFile(tp)
	if !strings.Contains(string(tb), `url = "https://api.example.dev/mcp"`) || strings.Contains(string(tb), "old") || !strings.Contains(string(tb), "[other]") || !strings.Contains(string(tb), "bearer_token_env_var") {
		t.Fatalf("config.toml:\n%s", tb)
	}
	// Claude Code: runs claude mcp add when the binary exists.
	h.newApp("")
	a := h.newApp("")
	a.LookPath = func(string) (string, error) { return "/usr/bin/claude", nil }
	h.stdout.Reset()
	if code := a.Run([]string{"mcp", "install", "--client", "claude-code"}); code != 0 {
		t.Fatal(h.errOut())
	}
	if len(h.ran) != 1 || strings.Join(h.ran[0], " ") != "claude mcp add --transport http sendio https://api.example.dev/mcp --scope user" {
		t.Fatalf("ran %v", h.ran)
	}
	// claude.ai: manual steps.
	if code := h.run("mcp", "install", "--client", "claude-ai"); code != 0 || !strings.Contains(h.out(), "Add custom connector") {
		t.Fatalf("claude-ai: %d %s", code, h.out())
	}
	// Unknown client.
	if code := h.run("mcp", "install", "--client", "emacs"); code != 2 {
		t.Fatalf("unknown client exit %d", code)
	}
}

func TestAgentSetup(t *testing.T) {
	h := newHarness(t, "")
	os.MkdirAll(filepath.Join(h.home, ".claude"), 0o755)
	if code := h.run("agent", "setup"); code != 0 {
		t.Fatal(h.errOut())
	}
	b, err := os.ReadFile(filepath.Join(h.home, ".claude", "skills", "sendio-cli", "SKILL.md"))
	if err != nil || !strings.HasPrefix(string(b), "---\nname: sendio-cli") {
		t.Fatalf("skill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".codex")); err == nil {
		t.Fatal("codex was not detected and must not be written")
	}
	if code := h.run("agent", "setup", "--target", "cursor"); code != 0 {
		t.Fatal(h.errOut())
	}
	if b, _ := os.ReadFile(filepath.Join(h.home, ".cursor", "rules", "sendio-cli.mdc")); !strings.Contains(string(b), "alwaysApply: false") {
		t.Fatalf("cursor rule: %s", b)
	}
	h.run("agent", "setup", "--print", "--agent", "no")
	if !strings.Contains(h.out(), "Agent-mode contract") {
		t.Fatal("print")
	}
}

func TestOpenAndVersion(t *testing.T) {
	h := newHarness(t, "https://api.sendio.run")
	if code := h.run("open", "logs", "--agent", "no"); code != 0 {
		t.Fatal(h.errOut())
	}
	if len(h.opened) != 1 || h.opened[0] != "https://sendio.run/logs" {
		t.Fatalf("opened %v", h.opened)
	}
	h.run("version")
	if m := h.outJSON(); m["version"] != "dev" {
		t.Fatalf("version %v", m)
	}
	if AppURL("https://api.send.io") != "https://send.io" || AppURL("http://localhost:8080") != "http://localhost:8080" {
		t.Fatal("AppURL")
	}
}

func TestCompletion(t *testing.T) {
	h := newHarness(t, "")
	for _, sh := range []string{"bash", "zsh", "fish", "powershell"} {
		if code := h.run("completion", sh); code != 0 || !strings.Contains(h.out(), "sendio") {
			t.Fatalf("%s: %d", sh, code)
		}
	}
	if code := h.run("completion", "fish", "--install"); code != 0 {
		t.Fatal(h.errOut())
	}
	if _, err := os.Stat(filepath.Join(h.home, ".config", "fish", "completions", "sendio.fish")); err != nil {
		t.Fatal(err)
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	for in, want := range map[string]string{
		"in 1h": "2026-01-01T11:00:00Z", "in 30 minutes": "2026-01-01T10:30:00Z", "in 2 days": "2026-01-03T10:00:00Z",
		"in 1h30m": "2026-01-01T11:30:00Z", "2026-02-01T00:00:00Z": "2026-02-01T00:00:00Z", "tomorrow at 9am": "tomorrow at 9am",
	} {
		if got := parseWhen(in, now); got != want {
			t.Errorf("parseWhen(%q) = %q, want %q", in, got, want)
		}
	}
}
