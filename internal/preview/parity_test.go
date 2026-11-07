package preview

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// parityCase is one template version + variables, rendered by both the
// backend (templates.Render, the code sends run) and Render.
type parityCase struct {
	Name     string         `json:"name"`
	V        map[string]any `json:"v"`
	Defs     []any          `json:"defs"`
	Provided map[string]any `json:"provided"`
}

var parityCases = []parityCase{
	{Name: "simple", V: map[string]any{"engine": "simple", "from": "Acme <hi@acme.test>", "subject": "Hi {{name}}", "preview_text": "For {{name}}",
		"html": "<body><p>Hello {{name}} &amp; {{{raw}}}, {{ company }} {{unknown}}</p></body>", "text": "Hello {{name}}", "reply_to": []any{"{{name}}@acme.test"}},
		Defs:     []any{map[string]any{"key": "name", "type": "string", "fallback": "friend"}, map[string]any{"key": "company", "type": "string", "fallback": "Acme"}, map[string]any{"key": "raw", "type": "string"}},
		Provided: map[string]any{"name": "<Ada>", "raw": "<b>bold</b>"}},
	{Name: "simple-fallback", V: map[string]any{"engine": "simple", "subject": "Order {{order}}", "html": "<p>{{order}} for {{name}}</p>"},
		Defs: []any{map[string]any{"key": "name", "type": "string", "fallback": "friend"}, map[string]any{"key": "order", "type": "number"}}, Provided: map[string]any{"order": 42.0}},
	{Name: "handlebars", V: map[string]any{"engine": "handlebars", "subject": "{{#if vip}}VIP: {{/if}}{{upper name}}", "preview_text": "{{items.length}} items",
		"html": "<html><head></head><body><ul>{{#each items}}<li>{{@index}} {{this.title}} {{price}}</li>{{else}}<li>none</li>{{/each}}</ul>{{> missing}}{{name}} {{{html}}} {{contact.first_name}} {{unsubscribe_url}}</body></html>",
		"text": "{{#each items}}- {{title}}\n{{/each}}"},
		Defs:     []any{map[string]any{"key": "name", "type": "string", "fallback": "there"}},
		Provided: map[string]any{"vip": true, "html": "<i>x</i>", "items": []any{map[string]any{"title": "Mug & cup", "price": 9.5}, map[string]any{"title": "Tee", "price": 20.0}}}},
	{Name: "handlebars-helpers", V: map[string]any{"engine": "handlebars", "subject": "{{default nickname \"pal\"}}", "html": "{{#unless done}}todo{{/unless}} {{#with user}}{{name}}{{/with}} {{lower \"ABC\"}}"},
		Provided: map[string]any{"user": map[string]any{"name": "Grace"}, "done": false}},
	{Name: "missing", V: map[string]any{"engine": "simple", "subject": "Hi {{name}}", "html": "<p>{{name}}</p>"},
		Defs: []any{map[string]any{"key": "name", "type": "string"}}},
	{Name: "missing-handlebars", V: map[string]any{"engine": "handlebars", "subject": "Hi {{name}}", "html": "<p>{{name}}</p>"},
		Defs: []any{map[string]any{"key": "name", "type": "string"}}},
	{Name: "wrong-type", V: map[string]any{"engine": "simple", "subject": "{{n}}", "html": "{{n}}"},
		Defs: []any{map[string]any{"key": "n", "type": "number"}}, Provided: map[string]any{"n": "seven"}},
}

const parityLib = `package templates

import (
	"fmt"

	"sendio/internal/foundation/httpx"
)

// CLIParityRender is injected by the CLI's parity test (go build -overlay);
// it is not part of the backend.
func CLIParityRender(v httpx.M, defs []httpx.M, provided httpx.M) (out httpx.M, errMsg string) {
	defer func() {
		if r := recover(); r != nil {
			errMsg = fmt.Sprint(r)
		}
	}()
	b := httpx.M{}
	Render(v, defs, provided, b, func(k string) { httpx.Invalid("Missing template variable: " + k) })
	return b, ""
}
`

const parityMain = `package main

import (
	"encoding/json"
	"os"

	"sendio/internal/foundation/templaterender"
)

func main() {
	var cases []struct {
		V        map[string]any   ` + "`json:\"v\"`" + `
		Defs     []map[string]any ` + "`json:\"defs\"`" + `
		Provided map[string]any   ` + "`json:\"provided\"`" + `
	}
	if err := json.NewDecoder(os.Stdin).Decode(&cases); err != nil {
		panic(err)
	}
	out := []any{}
	for _, c := range cases {
		b, e := templates.CLIParityRender(c.V, c.Defs, c.Provided)
		out = append(out, map[string]any{"b": b, "error": e})
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}
`

// TestParityWithBackend renders the cases with the backend's own code
// (compiled with an overlay that adds a tiny exported wrapper, nothing
// written into the backend tree) and with Render, and compares.
func TestParityWithBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	backend, err := filepath.Abs("../../../backend/core")
	if err != nil || !fileExists(filepath.Join(backend, "go.mod")) {
		t.Skip("backend not checked out next to the CLI")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	tmp := t.TempDir()
	lib, mainF := filepath.Join(tmp, "lib.go"), filepath.Join(tmp, "main.go")
	_ = os.WriteFile(lib, []byte(parityLib), 0o644)
	_ = os.WriteFile(mainF, []byte(parityMain), 0o644)
	ov, _ := json.Marshal(map[string]any{"Replace": map[string]string{
		filepath.Join(backend, "internal", "foundation", "templaterender", "zz_cli_parity.go"): lib,
		filepath.Join(backend, "cmd", "zz-cli-parity", "main.go"):                              mainF,
	}})
	ovPath := filepath.Join(tmp, "overlay.json")
	_ = os.WriteFile(ovPath, ov, 0o644)
	in, _ := json.Marshal(parityCases)
	cmd := exec.Command("go", "run", "-overlay", ovPath, "./cmd/zz-cli-parity")
	cmd.Dir = backend
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "CGO_ENABLED=0")
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("running the backend renderer: %v\n%s", err, stderr.String())
	}
	var got []struct {
		B     map[string]any `json:"b"`
		Error string         `json:"error"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != len(parityCases) {
		t.Fatalf("backend output: %v %s", err, raw)
	}
	for i, c := range parityCases {
		be := got[i]
		res := Render(templateOf(c), c.Provided)
		var errs []string
		for _, w := range res.Warnings {
			if w.Level == "error" {
				errs = append(errs, w.Message)
			}
		}
		if be.Error != "" {
			if len(errs) == 0 {
				t.Errorf("%s: the backend refuses (%s) but the preview reports no error", c.Name, be.Error)
			}
			continue
		}
		if len(errs) > 0 {
			t.Errorf("%s: preview errors %v but the backend renders", c.Name, errs)
		}
		for _, k := range []string{"from", "subject", "html", "text"} {
			want, _ := be.B[k].(string)
			var have string
			switch k {
			case "from":
				have = res.From
			case "subject":
				have = res.Subject
			case "html":
				have = res.HTML
			case "text":
				have = res.Text
			}
			if want != have {
				t.Errorf("%s: %s differs\nbackend: %q\npreview: %q", c.Name, k, want, have)
			}
		}
		var wantRT []string
		if l, ok := be.B["reply_to"].([]any); ok {
			for _, x := range l {
				wantRT = append(wantRT, x.(string))
			}
		}
		if !reflect.DeepEqual(wantRT, res.ReplyTo) {
			t.Errorf("%s: reply_to %v vs %v", c.Name, wantRT, res.ReplyTo)
		}
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func templateOf(c parityCase) Template {
	s := func(k string) string { v, _ := c.V[k].(string); return v }
	t := Template{Engine: s("engine"), From: s("from"), Subject: s("subject"), HTML: s("html"), Text: s("text"), PreviewText: s("preview_text")}
	if l, ok := c.V["reply_to"].([]any); ok {
		for _, x := range l {
			t.ReplyTo = append(t.ReplyTo, x.(string))
		}
	}
	for _, d := range c.Defs {
		m := d.(map[string]any)
		v := Variable{Key: m["key"].(string), Type: m["type"].(string), FallbackValue: m["fallback"]}
		t.Variables = append(t.Variables, v)
	}
	return t
}

func TestWarnings(t *testing.T) {
	big := strings.Repeat("x", GmailClipBytes+10)
	res := Render(Template{Engine: "handlebars", Subject: "", HTML: `<a href="http://plain.test/x">a</a><a href="https://ok.test">b</a><a href="mailto:a@b.c">m</a><img src="https://img.test/a.png"><img src="https://img.test/b.png" alt="">` + "{{missing_thing}}" + big},
		map[string]any{})
	codes := map[string]bool{}
	for _, w := range res.Warnings {
		codes[w.Code] = true
	}
	for _, c := range []string{"empty_subject", "insecure_link", "image_alt", "gmail_clipping", "no_value", "no_text_part"} {
		if !codes[c] {
			t.Errorf("missing warning %s in %+v", c, res.Warnings)
		}
	}
	n := 0
	for _, w := range res.Warnings {
		if w.Code == "image_alt" && strings.HasPrefix(w.Message, "1 image(s)") {
			n++
		}
		if w.Code == "insecure_link" && !strings.Contains(w.Message, "http://plain.test/x") {
			t.Errorf("wrong link flagged: %s", w.Message)
		}
	}
	if n != 1 {
		t.Errorf("alt warning should count exactly 1 image: %+v", res.Warnings)
	}
	clean := Render(Template{Subject: "Hi", HTML: `<a href="https://x.test">x</a><img src="https://x.test/a.png" alt="A">`, Text: "Hi"}, nil)
	if len(clean.Warnings) != 0 {
		t.Errorf("clean template has warnings: %+v", clean.Warnings)
	}
}
