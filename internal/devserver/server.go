// Package devserver is `sendio templates dev`: a local preview server for a
// folder of templates as `sendio templates pull` writes them
//
//	<dir>/<alias>/template.html, template.txt, meta.json
//	<fixtures>/<alias>.json   (data for the preview; default <dir>/fixtures)
//
// It renders with internal/preview (the backend's engine and rules), pushes
// a reload to the browser over Server-Sent Events when a file changes
// (fsnotify), and serves the UI from an embedded file system (no CDN).
package devserver

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"send.io/cli/internal/preview"
)

//go:embed ui
var uiFS embed.FS

// Meta is meta.json in a template folder.
type Meta struct {
	ID          string             `json:"id,omitempty"`
	Name        string             `json:"name"`
	Alias       string             `json:"alias,omitempty"`
	Subject     string             `json:"subject,omitempty"`
	From        string             `json:"from,omitempty"`
	ReplyTo     any                `json:"reply_to,omitempty"`
	PreviewText string             `json:"preview_text,omitempty"`
	Variables   []preview.Variable `json:"variables"`
	Engine      string             `json:"engine,omitempty"`
}

// Template is one template folder.
type Template struct {
	Key  string `json:"key"` // folder name
	Dir  string `json:"dir"`
	Meta Meta   `json:"meta"`
	HTML string `json:"html"`
	Text string `json:"text"`
}

// SendRequest is a "Send test" from the UI.
type SendRequest struct {
	// Mode "template": the API's template test (POST /templates/{id}/test,
	// the version saved on send.io, with the preview data as variables).
	// Mode "html": the locally rendered subject/html/text sent with
	// POST /emails.
	Mode     string          `json:"mode"`
	To       []string        `json:"to"`
	From     string          `json:"from,omitempty"`
	Data     map[string]any  `json:"data,omitempty"`
	Template *Template       `json:"-"`
	Rendered *preview.Result `json:"-"`
}

// Sender performs a send test and returns the API's response.
type Sender func(ctx context.Context, req SendRequest) (any, error)

// Options configure the server.
type Options struct {
	Dir      string
	Fixtures string // default <Dir>/fixtures
	Send     Sender // nil: sending is disabled
	// Log receives one line per notable event (file changes, sends).
	Log func(format string, args ...any)
}

// Server is a running preview server.
type Server struct {
	opts    Options
	mu      sync.Mutex
	clients map[chan string]bool
	watcher *fsnotify.Watcher
	srv     *http.Server
	ln      net.Listener
}

// New prepares a server; Start listens.
func New(o Options) (*Server, error) {
	abs, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, err
	}
	o.Dir = abs
	if st, err := os.Stat(o.Dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", o.Dir)
	}
	if o.Fixtures == "" {
		o.Fixtures = filepath.Join(o.Dir, "fixtures")
	} else if o.Fixtures, err = filepath.Abs(o.Fixtures); err != nil {
		return nil, err
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	return &Server{opts: o, clients: map[chan string]bool{}}, nil
}

// Templates lists the template folders: Dir itself when it holds meta.json,
// else its subfolders that do.
func (s *Server) Templates() ([]Template, error) {
	var dirs []string
	if _, err := os.Stat(filepath.Join(s.opts.Dir, "meta.json")); err == nil {
		dirs = []string{s.opts.Dir}
	} else {
		entries, err := os.ReadDir(s.opts.Dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				if _, err := os.Stat(filepath.Join(s.opts.Dir, e.Name(), "meta.json")); err == nil {
					dirs = append(dirs, filepath.Join(s.opts.Dir, e.Name()))
				}
			}
		}
	}
	sort.Strings(dirs)
	out := make([]Template, 0, len(dirs))
	for _, d := range dirs {
		t, err := loadTemplate(d)
		if err != nil {
			t = Template{Key: filepath.Base(d), Dir: d, Meta: Meta{Name: filepath.Base(d) + " (meta.json: " + err.Error() + ")"}}
		}
		out = append(out, t)
	}
	return out, nil
}

func loadTemplate(d string) (Template, error) {
	t := Template{Key: filepath.Base(d), Dir: d}
	mb, err := os.ReadFile(filepath.Join(d, "meta.json"))
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(mb, &t.Meta); err != nil {
		return t, err
	}
	h, err := os.ReadFile(filepath.Join(d, "template.html"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return t, err
	}
	t.HTML = string(h)
	txt, _ := os.ReadFile(filepath.Join(d, "template.txt"))
	t.Text = string(txt)
	if t.Meta.Name == "" {
		t.Meta.Name = t.Key
	}
	return t, nil
}

func (s *Server) template(key string) (Template, bool) {
	ts, _ := s.Templates()
	for _, t := range ts {
		if t.Key == key {
			return t, true
		}
	}
	return Template{}, false
}

// FixturePath is where a template's preview data lives.
func (s *Server) FixturePath(t Template) string {
	p := filepath.Join(s.opts.Fixtures, t.Key+".json")
	if t.Meta.Alias != "" && t.Meta.Alias != t.Key {
		if _, err := os.Stat(p); err != nil {
			if alt := filepath.Join(s.opts.Fixtures, t.Meta.Alias+".json"); fileExists(alt) {
				return alt
			}
		}
	}
	return p
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// Fixture reads a template's data (nil when there is no fixture file).
func (s *Server) Fixture(t Template) (map[string]any, error) {
	b, err := os.ReadFile(s.FixturePath(t))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %v", s.FixturePath(t), err)
	}
	return m, nil
}

// ReplyToList returns meta.json reply_to as a list.
func (m Meta) ReplyToList() []string {
	switch x := m.ReplyTo.(type) {
	case string:
		if x != "" {
			return []string{x}
		}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

// Render renders a template with data (the fixture when data is nil).
func (s *Server) Render(t Template, data map[string]any) preview.Result {
	return preview.Render(preview.Template{
		Engine: t.Meta.Engine, From: t.Meta.From, Subject: t.Meta.Subject, HTML: t.HTML, Text: t.Text,
		PreviewText: t.Meta.PreviewText, ReplyTo: t.Meta.ReplyToList(), Variables: t.Meta.Variables,
	}, data)
}

var darkMediaRe = regexp.MustCompile(`(?i)@media\s*\(\s*prefers-color-scheme\s*:\s*dark\s*\)`)

// DarkHTML approximates how the HTML looks in a dark-mode client: when the
// email ships its own dark styles (prefers-color-scheme), they are applied;
// otherwise the colours are inverted the way Gmail's and Outlook's apps do
// (images kept as they are).
func DarkHTML(h string) string {
	inject := `<style data-sendio-dark>:root{color-scheme:dark}</style>`
	if darkMediaRe.MatchString(h) {
		h = darkMediaRe.ReplaceAllString(h, "@media all")
	} else {
		inject = `<style data-sendio-dark>:root{color-scheme:dark}html{background:#fff;filter:invert(1) hue-rotate(180deg)}img,picture,video,[style*="background-image"]{filter:invert(1) hue-rotate(180deg)}</style>`
	}
	if i := strings.Index(strings.ToLower(h), "</head>"); i >= 0 {
		return h[:i] + inject + h[i:]
	}
	return inject + h
}

// Handler is the HTTP API and UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	ui, _ := fs.Sub(uiFS, "ui")
	mux.Handle("GET /", http.FileServer(http.FS(ui)))
	mux.HandleFunc("GET /api/templates", s.listTemplates)
	mux.HandleFunc("GET /api/templates/{key}", s.getTemplate)
	mux.HandleFunc("POST /api/render/{key}", s.renderTemplate)
	mux.HandleFunc("PUT /api/fixtures/{key}", s.saveFixture)
	mux.HandleFunc("POST /api/send/{key}", s.send)
	mux.HandleFunc("GET /api/events", s.events)
	return s.guard(mux)
}

// guard refuses requests for another host (DNS rebinding) and writes
// without the X-Sendio-Dev header (a cross-site form cannot set it).
func (s *Server) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			host = hh
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" && host != "[::1]" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Sendio-Dev") != "1" {
			http.Error(w, "missing X-Sendio-Dev header", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	e := json.NewEncoder(w)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]any{"error": fmt.Sprintf(format, args...)})
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Templates()
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	list := []any{}
	for _, t := range ts {
		data, _ := s.Fixture(t)
		res := s.Render(t, data)
		errs, warns := 0, 0
		for _, x := range res.Warnings {
			switch x.Level {
			case "error":
				errs++
			case "warning":
				warns++
			}
		}
		list = append(list, map[string]any{"key": t.Key, "name": t.Meta.Name, "alias": t.Meta.Alias, "id": t.Meta.ID,
			"subject": res.Subject, "engine": res.Engine, "errors": errs, "warnings": warns})
	}
	writeJSON(w, 200, map[string]any{"dir": s.opts.Dir, "fixtures": s.opts.Fixtures, "can_send": s.opts.Send != nil, "templates": list})
}

func (s *Server) view(t Template, data map[string]any, fixtureErr error) map[string]any {
	res := s.Render(t, data)
	if fixtureErr != nil {
		res.Warnings = append([]preview.Warning{{Level: "error", Code: "fixture", Message: fixtureErr.Error()}}, res.Warnings...)
	}
	if data == nil {
		data = map[string]any{}
	}
	return map[string]any{"template": t, "data": data, "fixture": s.FixturePath(t), "fixture_exists": fileExists(s.FixturePath(t)),
		"result": res, "html_dark": DarkHTML(res.HTML)}
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request) {
	t, ok := s.template(r.PathValue("key"))
	if !ok {
		writeErr(w, 404, "no template %q", r.PathValue("key"))
		return
	}
	data, err := s.Fixture(t)
	writeJSON(w, 200, s.view(t, data, err))
}

func readData(r *http.Request) (map[string]any, error) {
	var body struct {
		Data map[string]any `json:"data"`
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, fmt.Errorf("the data must be a JSON object: %v", err)
	}
	return body.Data, nil
}

func (s *Server) renderTemplate(w http.ResponseWriter, r *http.Request) {
	t, ok := s.template(r.PathValue("key"))
	if !ok {
		writeErr(w, 404, "no template %q", r.PathValue("key"))
		return
	}
	data, err := readData(r)
	if err != nil {
		writeErr(w, 422, "%v", err)
		return
	}
	writeJSON(w, 200, s.view(t, data, nil))
}

func (s *Server) saveFixture(w http.ResponseWriter, r *http.Request) {
	t, ok := s.template(r.PathValue("key"))
	if !ok {
		writeErr(w, 404, "no template %q", r.PathValue("key"))
		return
	}
	data, err := readData(r)
	if err != nil {
		writeErr(w, 422, "%v", err)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	b, _ := json.MarshalIndent(data, "", "  ")
	p := s.FixturePath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	s.opts.Log("saved %s", p)
	writeJSON(w, 200, s.view(t, data, nil))
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	if s.opts.Send == nil {
		writeErr(w, 409, "Sending is not available: sign in with `sendio login` (or set SENDIO_API_KEY) and restart templates dev.")
		return
	}
	t, ok := s.template(r.PathValue("key"))
	if !ok {
		writeErr(w, 404, "no template %q", r.PathValue("key"))
		return
	}
	var req struct {
		SendRequest
		Confirm bool `json:"confirm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 5<<20)).Decode(&req); err != nil {
		writeErr(w, 422, "invalid request: %v", err)
		return
	}
	if !req.Confirm {
		writeErr(w, 422, "confirm the send (confirm: true)")
		return
	}
	var to []string
	for _, a := range req.To {
		for _, p := range strings.Split(a, ",") {
			if p = strings.TrimSpace(p); p != "" {
				to = append(to, p)
			}
		}
	}
	if len(to) == 0 || len(to) > 5 {
		writeErr(w, 422, "give 1 to 5 recipient addresses")
		return
	}
	req.To = to
	sr := req.SendRequest
	sr.Template = &t
	switch sr.Mode {
	case "template":
		if t.Meta.ID == "" && t.Meta.Alias == "" {
			writeErr(w, 422, "this template has no id or alias in meta.json: push it first (sendio templates push), or send the rendered HTML")
			return
		}
	case "html":
		res := s.Render(t, sr.Data)
		sr.Rendered = &res
		if sr.From == "" {
			sr.From = res.From
		}
		if sr.From == "" {
			writeErr(w, 422, "a From address is needed (meta.json has none)")
			return
		}
	default:
		writeErr(w, 422, `mode must be "template" or "html"`)
		return
	}
	out, err := s.opts.Send(r.Context(), sr)
	if err != nil {
		writeErr(w, 502, "%v", err)
		return
	}
	s.opts.Log("sent a test of %s (%s) to %s", t.Key, sr.Mode, strings.Join(to, ", "))
	writeJSON(w, 200, map[string]any{"ok": true, "response": out})
}

// events streams "change" events (the keys of the templates that changed;
// "*" for more than one or a new folder) as Server-Sent Events.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fmt.Fprint(w, "retry: 1000\n: connected\n\n")
	fl.Flush()
	ch := make(chan string, 16)
	s.mu.Lock()
	s.clients[ch] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, ch)
		s.mu.Unlock()
	}()
	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keep.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		case msg := <-ch:
			fmt.Fprintf(w, "event: change\ndata: %s\n\n", msg)
			fl.Flush()
		}
	}
}

func (s *Server) broadcast(keys []string) {
	b, _ := json.Marshal(map[string]any{"keys": keys})
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- string(b):
		default:
		}
	}
}

// Watch starts watching the folders; changes are pushed to the browsers.
func (s *Server) Watch(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	s.watcher = w
	add := func(p string) {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			_ = w.Add(p)
		}
	}
	add(s.opts.Dir)
	add(s.opts.Fixtures)
	if entries, err := os.ReadDir(s.opts.Dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				add(filepath.Join(s.opts.Dir, e.Name()))
			}
		}
	}
	go func() {
		defer w.Close()
		pending := map[string]bool{}
		var timer *time.Timer
		var fire <-chan time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Has(fsnotify.Create) {
					if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
						_ = w.Add(ev.Name)
						if filepath.Clean(ev.Name) == filepath.Clean(s.opts.Fixtures) {
							pending["*"] = true
						}
					}
				}
				if ev.Has(fsnotify.Chmod) && !ev.Has(fsnotify.Write) {
					continue
				}
				if k := s.keyFor(ev.Name); k != "" {
					pending[k] = true
				}
				if timer == nil {
					timer = time.NewTimer(120 * time.Millisecond)
				} else {
					timer.Reset(120 * time.Millisecond)
				}
				fire = timer.C
			case <-fire:
				fire = nil
				keys := make([]string, 0, len(pending))
				for k := range pending {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				pending = map[string]bool{}
				if len(keys) == 0 {
					continue
				}
				s.opts.Log("changed: %s", strings.Join(keys, ", "))
				s.broadcast(keys)
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	return nil
}

// keyFor maps a changed path to the template it belongs to.
func (s *Server) keyFor(p string) string {
	base := filepath.Base(p)
	if strings.HasPrefix(base, ".") || strings.HasSuffix(base, "~") || strings.HasSuffix(base, ".swp") {
		return ""
	}
	if rel, err := filepath.Rel(s.opts.Fixtures, p); err == nil && !strings.HasPrefix(rel, "..") {
		if strings.HasSuffix(base, ".json") {
			return strings.TrimSuffix(base, ".json")
		}
		return ""
	}
	rel, err := filepath.Rel(s.opts.Dir, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	if fileExists(filepath.Join(s.opts.Dir, "meta.json")) {
		return filepath.Base(s.opts.Dir)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 1 {
		return "*" // a folder added or removed
	}
	return parts[0]
}

// Start listens on addr ("127.0.0.1:3456"); port 0 picks a free one.
func (s *Server) Start(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	s.ln = ln
	s.srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return "http://localhost:" + port, nil
}

// Close stops the server.
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close() // also ends the open event streams
}
