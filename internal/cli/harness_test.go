package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorded is one request the fake API received.
type recorded struct {
	Method, Path, RawQuery, Auth, ContentType string
	Header                                    http.Header
	Body                                      []byte
}

func (r recorded) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, r.Body)
	}
	return m
}

// fakeAPI is an httptest server with per-route handlers that records requests.
type fakeAPI struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []recorded
	routes map[string]http.HandlerFunc
}

func newFake(t *testing.T) *fakeAPI {
	f := &fakeAPI{routes: map[string]http.HandlerFunc{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Auth: r.Header.Get("Authorization"),
			ContentType: r.Header.Get("Content-Type"), Header: r.Header.Clone(), Body: body})
		h, ok := f.routes[r.Method+" "+r.URL.Path]
		if !ok {
			h, ok = f.routes[r.URL.Path]
		}
		f.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			io.WriteString(w, `{"statusCode":404,"name":"not_found","message":"The requested endpoint does not exist."}`)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) on(route string, h http.HandlerFunc) {
	f.mu.Lock()
	f.routes[route] = h
	f.mu.Unlock()
}

func (f *fakeAPI) json(route string, status int, body string) {
	f.on(route, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
}

func (f *fakeAPI) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func (f *fakeAPI) last(t *testing.T) recorded {
	t.Helper()
	r := f.requests()
	if len(r) == 0 {
		t.Fatal("no request reached the fake API")
	}
	return r[len(r)-1]
}

// fakeClock drives App.Now/App.Sleep without real waiting.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

type harness struct {
	t      *testing.T
	app    *App
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	env    map[string]string
	dir    string
	home   string
	opened []string
	ran    [][]string
	clock  *fakeClock
	errBuf *lockedBuf // stderr of appFor apps
}

// newHarness returns a non-TTY app (agent mode by default) pointed at base.
func newHarness(t *testing.T, base string) *harness {
	t.Helper()
	h := &harness{t: t, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, dir: t.TempDir(), home: t.TempDir(),
		clock: &fakeClock{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}}
	h.env = map[string]string{
		"SENDIO_CONFIG_DIR":         h.dir,
		"SENDIO_CREDENTIAL_STORE":   "file",
		"SENDIO_NO_UPDATE_NOTIFIER": "1",
		"DISPLAY":                   ":0",
	}
	if base != "" {
		h.env["SENDIO_BASE_URL"] = base
	}
	return h
}

func (h *harness) newApp(stdin string) *App {
	a := &App{
		Stdin: strings.NewReader(stdin), Stdout: h.stdout, Stderr: h.stderr,
		Getenv: func(k string) string { return h.env[k] },
		Environ: func() []string {
			var l []string
			for k, v := range h.env {
				l = append(l, k+"="+v)
			}
			return l
		},
		OpenURL:  func(u string) error { h.opened = append(h.opened, u); return nil },
		Hostname: func() (string, error) { return "test-host", nil },
		LookPath: func(s string) (string, error) { return "", &lookErr{} },
		RunCmd: func(name string, args ...string) error {
			h.ran = append(h.ran, append([]string{name}, args...))
			return nil
		},
		HomeDir: h.home, WorkDir: h.home,
		Now: h.clock.Now, Sleep: h.clock.Sleep,
	}
	h.app = a
	return a
}

type lookErr struct{}

func (*lookErr) Error() string { return "not found" }

// run executes args and returns the exit code; output is in h.stdout/h.stderr (reset per run).
func (h *harness) run(args ...string) int {
	return h.runIn("", args...)
}

func (h *harness) runIn(stdin string, args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	return h.newApp(stdin).Run(args)
}

func (h *harness) out() string    { return h.stdout.String() }
func (h *harness) errOut() string { return h.stderr.String() }

func (h *harness) outJSON() map[string]any {
	h.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &m); err != nil {
		h.t.Fatalf("stdout is not one JSON object: %v\n%s\nstderr: %s", err, h.out(), h.errOut())
	}
	return m
}

func (h *harness) errJSON() map[string]any {
	h.t.Helper()
	var m map[string]any
	line := strings.TrimSpace(h.stderr.String())
	if i := strings.LastIndex(line, "\n"); i >= 0 {
		line = line[i+1:]
	}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		h.t.Fatalf("stderr last line is not JSON: %v\n%s", err, h.errOut())
	}
	e, _ := m["error"].(map[string]any)
	if e == nil {
		h.t.Fatalf("stderr has no error envelope: %s", line)
	}
	return e
}

func mustEqualJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got invalid JSON: %v: %s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want invalid JSON: %v", err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("body mismatch\n got: %s\nwant: %s", gb, wb)
	}
}
