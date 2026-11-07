package cli

import (
	"strings"
	"testing"

	"send.io/cli/internal/api"
)

// The branded look must never leak into machine output or plain terminals.
func TestLookNeverBreaksScripting(t *testing.T) {
	f := newFake(t)
	f.json("GET /domains", 200, `{"data":[{"id":"d1","name":"a.dev","status":"verified"}],"has_more":false}`)
	f.json("POST /emails", 402, `{"statusCode":402,"name":"insufficient_credits","message":"No credits left."}`)
	h := newHarness(t, f.URL)
	withKey(h)

	// --version off a terminal: exactly the old line, in agent and human mode.
	for _, args := range [][]string{{"--version"}, {"-v"}, {"--version", "--agent", "no"}} {
		if code := h.run(args...); code != 0 || h.out() != "sendio dev\n" {
			t.Fatalf("%v: exit %d, stdout %q stderr %q", args, code, h.out(), h.errOut())
		}
	}

	// Agent mode errors: the envelope is unchanged (no human hint added).
	h.run("emails", "send", "--from", "a@x.dev", "--to", "b@y.dev", "--subject", "s", "--text", "t")
	e := h.errJSON()
	if e["code"] != "insufficient_credits" || e["hint"] != nil {
		t.Fatalf("envelope %v", e)
	}
	if strings.Contains(h.errOut(), "\x1b") {
		t.Fatalf("escape codes in agent stderr: %q", h.errOut())
	}

	// Human mode off a terminal: plain classic error line plus the new hint.
	h.run("emails", "send", "--from", "a@x.dev", "--to", "b@y.dev", "--subject", "s", "--text", "t", "--agent", "no")
	if !strings.HasPrefix(h.errOut(), "Error: No credits left. (HTTP 402, insufficient_credits)\n") ||
		!strings.Contains(h.errOut(), "`sendio billing balance`") || strings.Contains(h.errOut(), "\x1b") {
		t.Fatalf("human plain error %q", h.errOut())
	}

	// A terminal with NO_COLOR, TERM=dumb or --no-color: no escape codes at all.
	for _, env := range []map[string]string{{"NO_COLOR": "1"}, {"TERM": "dumb"}, {"--no-color": "1"}} {
		args := []string{"domains", "list", "--agent", "no"}
		for k, v := range env {
			if strings.HasPrefix(k, "--") {
				args = append(args, k)
			} else {
				h.env[k] = v
				defer delete(h.env, k)
			}
		}
		a := h.newApp("")
		a.StdoutTTY, a.StderrTTY = true, true
		h.stdout.Reset()
		h.stderr.Reset()
		if code := a.Run(args); code != 0 {
			t.Fatalf("%v: exit %d %s", env, code, h.errOut())
		}
		if o := h.out() + h.errOut(); strings.Contains(o, "\x1b") || !strings.Contains(o, "verified") {
			t.Fatalf("%v: %q", env, o)
		}
		if a.P().Motion() {
			t.Fatalf("%v: motion allowed", env)
		}
		for k := range env {
			delete(h.env, k)
		}
	}

	// CI on a terminal: colour is fine, motion and the home screen are not.
	h.env["CI"] = "true"
	a := h.newApp("")
	a.StdoutTTY, a.StderrTTY, a.StdinTTY = true, true, true
	h.stdout.Reset()
	if code := a.Run([]string{"--agent", "no"}); code != 0 {
		t.Fatalf("CI root: %d %s", code, h.errOut())
	}
	if a.P().Motion() || !strings.Contains(h.out(), "Available Commands") {
		t.Fatalf("CI: motion %v, out %q", a.P().Motion(), h.out())
	}
	delete(h.env, "CI")

	// A colour terminal does get the brand: orange success, styled help.
	a = h.newApp("")
	a.StdoutTTY, a.StderrTTY = true, true
	h.stdout.Reset()
	a.Run([]string{"domains", "list", "--agent", "no"})
	if !strings.Contains(h.out(), "\x1b[") {
		t.Fatalf("no colour on a colour terminal: %q", h.out())
	}
	if !a.P().Motion() {
		t.Fatal("motion should be allowed on a colour terminal")
	}
	a = h.newApp("")
	a.StdoutTTY, a.StderrTTY = true, true
	h.stdout.Reset()
	a.Run([]string{"--version", "--agent", "no"})
	if !strings.Contains(h.out(), " send ") || !strings.Contains(h.out(), "dev") || !strings.Contains(h.out(), "\x1b[") {
		t.Fatalf("fancy version %q", h.out())
	}
}

func TestHumanErrorHints(t *testing.T) {
	a := &App{Getenv: func(k string) string {
		if k == "SENDIO_BASE_URL" {
			return "https://api.sendio.run"
		}
		return ""
	}}
	cases := []struct {
		in   api.Error
		want string
	}{
		{api.Error{Status: 402, Code: "insufficient_credits", Message: "x"}, "https://sendio.run/settings/billing"},
		{api.Error{Status: 403, Code: "validation_error", Message: "The a.dev domain is not verified."}, "`sendio domains list`"},
		{api.Error{Status: 403, Code: "invalid_permission", Message: "nope"}, "`sendio whoami`"},
		{api.Error{Status: 429, RetryAfter: "3", Message: "slow"}, "retry in 3s"},
		{api.Error{Status: 401, Message: "bad key", Hint: "Run `sendio login`, or pass --api-key / SENDIO_API_KEY."}, "`sendio login`"},
		{api.Error{Exit: api.ExitNetwork, Code: "network_error", Message: "dial"}, "`sendio doctor`"},
		{api.Error{Status: 500, Message: "boom"}, "Retry"},
	}
	for _, c := range cases {
		e := c.in
		got := a.humanError(&e)
		if !strings.Contains(got.Hint, c.want) {
			t.Errorf("%+v: hint %q, want %q", c.in, got.Hint, c.want)
		}
		if got.Code != c.in.Code || got.Status != c.in.Status || got.Exit != c.in.Exit {
			t.Errorf("%+v: changed identity: %+v", c.in, got)
		}
	}
	nf := a.humanError(&api.Error{Exit: api.ExitAuth, Code: "not_authenticated", Message: "You are not signed in.", Hint: "Run `sendio login` (or `sendio login --api-key mx_…`), or set SENDIO_API_KEY."})
	if nf.Message != "Not signed in." || !strings.HasPrefix(nf.Hint, "Run `sendio login`") {
		t.Fatalf("not signed in: %+v", nf)
	}
}

func TestRequestLabelAndCredits(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"GET", "/domains"}:                    "Loading domains",
		{"GET", "https://api.x/v0/cli/whoami"}: "Loading cli",
		{"POST", "/emails"}:                    "Sending",
		{"POST", "/broadcasts/b1/send"}:        "Sending",
		{"POST", "/domains/d1/verify"}:         "Verifying",
		{"POST", "/webhooks"}:                  "Creating",
		{"PATCH", "/domains/d1"}:               "Saving",
		{"DELETE", "/domains/d1"}:              "Deleting",
		{"GET", "/billing/balance?x=1"}:        "Loading billing",
	} {
		if got := requestLabel(&api.Request{Method: in[0], Path: in[1]}); got != want {
			t.Errorf("%v: %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{0: "0", 5: "5", 1240.5: "1,240.5", 1234567: "1,234,567", 12.3456: "12.35", -4000: "-4,000"} {
		if got := formatCredits(in); got != want {
			t.Errorf("formatCredits(%v) = %q, want %q", in, got, want)
		}
	}
}
