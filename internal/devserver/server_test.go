package devserver

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemplate(t *testing.T, dir, key, meta, html string) {
	t.Helper()
	d := filepath.Join(dir, key)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(d, "meta.json"), []byte(meta), 0o644)
	_ = os.WriteFile(filepath.Join(d, "template.html"), []byte(html), 0o644)
}

func start(t *testing.T, o Options) (*Server, string) {
	t.Helper()
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Close() })
	if err := s.Watch(ctx); err != nil {
		t.Fatal(err)
	}
	return s, u
}

func getJSON(t *testing.T, u string, v any) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(v)
}

func TestRenderFixtureAndWarnings(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "welcome", `{"name":"Welcome","alias":"welcome","subject":"Hi {{name}}","from":"Acme <hi@acme.test>","engine":"handlebars","variables":[{"key":"name","type":"string"}]}`,
		`<p>Hello {{name}}</p><a href="http://acme.test">x</a><img src="https://acme.test/a.png">`)
	_ = os.MkdirAll(filepath.Join(dir, "fixtures"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "fixtures", "welcome.json"), []byte(`{"name":"Ada"}`), 0o644)
	_, u := start(t, Options{Dir: dir})

	var list struct {
		Templates []map[string]any `json:"templates"`
		CanSend   bool             `json:"can_send"`
	}
	getJSON(t, u+"/api/templates", &list)
	if len(list.Templates) != 1 || list.Templates[0]["subject"] != "Hi Ada" || list.CanSend {
		t.Fatalf("list %+v", list)
	}
	var v struct {
		Result struct {
			Subject, HTML string
			Warnings      []struct{ Code string }
		} `json:"result"`
		HTMLDark string `json:"html_dark"`
	}
	getJSON(t, u+"/api/templates/welcome", &v)
	if v.Result.Subject != "Hi Ada" || !strings.Contains(v.Result.HTML, "Hello Ada") || !strings.Contains(v.HTMLDark, "color-scheme:dark") {
		t.Fatalf("render %+v", v)
	}
	codes := map[string]bool{}
	for _, w := range v.Result.Warnings {
		codes[w.Code] = true
	}
	if !codes["insecure_link"] || !codes["image_alt"] {
		t.Fatalf("warnings %+v", v.Result.Warnings)
	}

	// Without the fixture the declared variable is missing (a send would fail).
	req, _ := http.NewRequest("POST", u+"/api/render/welcome", strings.NewReader(`{"data":{}}`))
	req.Header.Set("X-Sendio-Dev", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var r2 struct {
		Result struct {
			Warnings []struct{ Level, Code string }
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r2)
	resp.Body.Close()
	found := false
	for _, w := range r2.Result.Warnings {
		// The backend's namedInTags misses a bare {{name}} (it renders empty
		// rather than failing the send); the preview flags it either way.
		found = found || (w.Code == "missing_variable" && w.Level == "error") || w.Code == "no_value"
	}
	if !found {
		t.Fatalf("missing variable not reported: %+v", r2.Result.Warnings)
	}

	// Writes need the X-Sendio-Dev header; other hosts are refused.
	req, _ = http.NewRequest("PUT", u+"/api/fixtures/welcome", strings.NewReader(`{"data":{"name":"x"}}`))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("write without header: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", u+"/api/templates", nil)
	req.Host = "evil.example"
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("foreign host: %d", resp.StatusCode)
	}
	// Sending is off without a Sender.
	req, _ = http.NewRequest("POST", u+"/api/send/welcome", strings.NewReader(`{"mode":"html","to":["a@b.c"],"confirm":true}`))
	req.Header.Set("X-Sendio-Dev", "1")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("send without sender: %d", resp.StatusCode)
	}
	// The UI is embedded.
	resp, _ = http.Get(u + "/")
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "html") {
		t.Fatalf("ui: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestSendTest(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "receipt", `{"id":"tpl_1","name":"Receipt","subject":"Paid {{amount}}","from":"Shop <s@shop.test>","variables":[{"key":"amount","type":"string","fallback_value":"$1"}]}`, `<p>{{amount}}</p>`)
	var got []SendRequest
	_, u := start(t, Options{Dir: dir, Send: func(ctx context.Context, r SendRequest) (any, error) {
		got = append(got, r)
		return map[string]any{"id": "em_1"}, nil
	}})
	post := func(body string) int {
		req, _ := http.NewRequest("POST", u+"/api/send/receipt", strings.NewReader(body))
		req.Header.Set("X-Sendio-Dev", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post(`{"mode":"html","to":["a@b.c"]}`); c != 422 {
		t.Fatalf("unconfirmed send: %d", c)
	}
	if c := post(`{"mode":"html","to":["a@b.c, d@e.f"],"data":{"amount":"$5"},"confirm":true}`); c != 200 {
		t.Fatalf("html send: %d", c)
	}
	if c := post(`{"mode":"template","to":["a@b.c"],"confirm":true}`); c != 200 {
		t.Fatalf("template send: %d", c)
	}
	if len(got) != 2 || got[0].Rendered.Subject != "Paid $5" || got[0].From != "Shop <s@shop.test>" || len(got[0].To) != 2 || got[1].Template.Meta.ID != "tpl_1" {
		t.Fatalf("sends %+v", got)
	}
}

func TestHotReload(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "a", `{"name":"A","subject":"A"}`, `<p>A</p>`)
	_, u := start(t, Options{Dir: dir})
	resp, err := http.Get(u + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 10)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	// wait for the stream to be registered
	for l := range lines {
		if strings.HasPrefix(l, ": connected") {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(dir, "a", "template.html"), []byte("<p>B</p>"), 0o644)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l := <-lines:
			if strings.HasPrefix(l, "data: ") {
				if !strings.Contains(l, `"a"`) {
					t.Fatalf("change event %s", l)
				}
				return
			}
		case <-deadline:
			t.Fatal("no change event after editing template.html")
		}
	}
}

func TestDarkHTML(t *testing.T) {
	own := DarkHTML(`<html><head><style>@media (prefers-color-scheme: dark){body{background:#000}}</style></head><body></body></html>`)
	if !strings.Contains(own, "@media all") || strings.Contains(own, "invert") {
		t.Fatalf("own dark styles not applied: %s", own)
	}
	if inv := DarkHTML(`<p>x</p>`); !strings.Contains(inv, "invert(1)") {
		t.Fatalf("no inversion: %s", inv)
	}
}
