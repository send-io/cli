package cli

// Shared plumbing of the long-running commands (listen, logs tail, events
// tail, inbox watch): WebSocket dialing with the profile's auth, a
// Server-Sent Events reader, and the reconnect loop.
//
// Behaviour common to all of them:
//   - Ctrl-C ends them cleanly with exit 0.
//   - The first connection must succeed: an error there exits with the usual
//     code (3 auth, 4 network, 6 rate limited...). After that, a dropped
//     connection (network, server restart = close 1001, 5xx) is retried with
//     exponential backoff (0.5 s doubling to 30 s, with jitter); the token is
//     refreshed first when it is about to expire (oauthSource.Token) and
//     after a 401.
//   - In agent mode stdout carries only NDJSON records; reconnect notices go
//     to stderr as {"warning": …} lines.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"send.io/cli/internal/api"
)

// backoff is exponential with jitter, capped at 30 s.
type backoff struct{ n int }

func (b *backoff) next() time.Duration {
	d := 500 * time.Millisecond << min(b.n, 6)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	b.n++
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

func (b *backoff) reset() { b.n = 0 }

// errDone ends keepConnected successfully (listen --print-secret).
var errDone = errors.New("done")

// permanent marks an error that reconnecting will not fix.
func permanent(err error) bool {
	var e *api.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Exit {
	case api.ExitAuth, api.ExitUsage, api.ExitNotFound:
		return true
	}
	return e.Status >= 400 && e.Status < 500 && e.Status != 408 && e.Status != 429
}

// keepConnected runs once until the context ends: the first call's error is
// returned as is; later failures are retried with backoff. once reports
// whether it got connected (which resets the backoff).
func (a *App) keepConnected(ctx context.Context, what string, once func(ctx context.Context, attempt int) (bool, error)) error {
	var b backoff
	everConnected := false
	for attempt := 0; ; attempt++ {
		connected, err := once(ctx, attempt)
		if ctx.Err() != nil || errors.Is(err, errDone) {
			return nil
		}
		if connected {
			everConnected = true
			b.reset()
		}
		if err != nil && (!everConnected || permanent(err)) {
			return err
		}
		reason := "closed"
		if err != nil {
			reason = err.Error()
		}
		d := b.next()
		a.P().Warn("%s: connection lost (%s); reconnecting in %s", what, reason, d.Round(100*time.Millisecond))
		if a.sleep(ctx, d) != nil {
			return nil
		}
	}
}

// wsURL turns an API URL into a ws(s) URL.
func wsURL(c *api.Client, path string, q url.Values) string {
	u := c.URL(path, q)
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

// dialWS opens an authenticated WebSocket (Authorization: Bearer header,
// never the key in the URL). A 401 is retried once after a token refresh.
func (a *App) dialWS(ctx context.Context, c *api.Client, path string, q url.Values) (*websocket.Conn, error) {
	var transport http.RoundTripper
	if c.HTTP != nil {
		transport = c.HTTP.Transport
	}
	for attempt := 0; ; attempt++ {
		h := http.Header{}
		h.Set("User-Agent", c.UserAgent)
		if c.Auth != nil {
			tok, err := c.Auth.Token(ctx)
			if err != nil {
				return nil, err
			}
			if tok != "" {
				h.Set("Authorization", "Bearer "+tok)
			}
		}
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		conn, resp, err := websocket.Dial(dctx, wsURL(c, path, q), &websocket.DialOptions{
			HTTPClient: &http.Client{Transport: transport}, HTTPHeader: h,
		})
		cancel()
		if err == nil {
			conn.SetReadLimit(32 << 20)
			return conn, nil
		}
		if resp == nil || resp.StatusCode == http.StatusSwitchingProtocols {
			return nil, api.NetworkError(err)
		}
		var body []byte
		if resp.Body != nil {
			body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.Auth != nil {
			ok, ferr := c.Auth.Refresh(ctx)
			if ferr != nil {
				return nil, ferr
			}
			if ok {
				continue
			}
		}
		return nil, api.FromResponse(resp.StatusCode, resp.Header, body)
	}
}

// closeReason describes why a WebSocket ended.
func closeReason(err error) error {
	if err == nil {
		return nil
	}
	if st := websocket.CloseStatus(err); st != -1 {
		var ce websocket.CloseError
		if errors.As(err, &ce) && ce.Reason != "" {
			return fmt.Errorf("closed by the server: %d %s", st, ce.Reason)
		}
		return fmt.Errorf("closed by the server: %d", st)
	}
	return api.NetworkError(err)
}

// sseEvent is one Server-Sent Event.
type sseEvent struct {
	ID, Event, Data string
	Retry           time.Duration
}

// readSSE parses an event stream, calling fn per event; it returns when the
// stream ends (io.EOF is returned as nil).
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	var ev sseEvent
	var data []string
	hasData := false
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if hasData {
				ev.Data = strings.Join(data, "\n")
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev, data, hasData = sseEvent{Retry: ev.Retry}, nil, false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			ev.ID = value
		case "event":
			ev.Event = value
		case "data":
			data = append(data, value)
			hasData = true
		case "retry":
			var ms int
			if _, err := fmt.Sscan(value, &ms); err == nil && ms > 0 {
				ev.Retry = time.Duration(ms) * time.Millisecond
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return api.NetworkError(err)
	}
	return nil
}

// clock formats an RFC 3339 time as the local "2006-01-02 15:04:05" for
// human lines (now when absent).
func (a *App) clock(ts string) string {
	t := a.now()
	// RFC 3339, or Postgres text ("2026-10-08 13:41:46.029999+00") as the
	// logs stream sends it.
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07", "2006-01-02 15:04:05.999999999-07:00"} {
		if p, err := time.Parse(layout, ts); err == nil {
			t = p
			break
		}
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
