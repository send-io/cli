package cli

// sendio listen: webhook forwarding (contract §2, Appendix B).
//
// GET /v0/cli/listen?events=…&format=…&endpoint=… upgraded to a WebSocket.
// The server sends a ready frame (session secret), then one event frame per
// HTTP request a webhook endpoint in that format would get (exact body,
// headers and signature, signed with the session secret). The CLI replays
// each one against --forward-to with the same method, headers and body,
// acks it ({"type":"ack","id","status","duration_ms"}) and prints a line.
// Ping frames get a pong. A new session (after a reconnect) has a new
// secret, which is printed again.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/config"
	"send.io/cli/internal/out"
)

func init() { Register(func(a *App, root *cobra.Command) { a.addListenCommand(root) }) }

// listenFormats are the webhook formats the API renders (Appendix B).
var listenFormats = []string{"sendio", "resend", "sendgrid", "postmark", "mailgun", "mailchimp", "mailchimp_list", "brevo", "mailjet"}

type listenOpts struct {
	forward    string
	events     []string
	format     string
	endpoint   string
	printSec   bool
	printJSON  bool
	skipVerify bool
	headers    []string
	timeout    time.Duration
}

// listenFrame is a server → client frame.
type listenFrame struct {
	Type            string            `json:"type"`
	SessionID       string            `json:"session_id,omitempty"`
	Secret          string            `json:"secret,omitempty"`
	Format          string            `json:"format,omitempty"`
	Events          []string          `json:"events,omitempty"`
	SignatureScheme string            `json:"signature_scheme,omitempty"`
	Endpoint        string            `json:"endpoint,omitempty"`
	PublicKey       string            `json:"public_key,omitempty"`
	ID              string            `json:"id,omitempty"`
	EventType       string            `json:"event_type,omitempty"`
	CreatedAt       string            `json:"created_at,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            string            `json:"body,omitempty"`
	Method          string            `json:"method,omitempty"`
	Test            bool              `json:"test,omitempty"`
}

func (a *App) addListenCommand(root *cobra.Command) {
	var o listenOpts
	cmd := &cobra.Command{
		Use:   "listen",
		Short: "Receive your webhook events locally and forward them to your app",
		Long: `Open a live session that receives the team's webhook events as they happen
and forwards each one to a local URL: the same body, headers, method and
signature a real webhook endpoint in that format would get, signed with a
secret made for this session (printed when it starts; a reconnect starts a
new session with a new secret). Nothing is created on your account.

--forward-to takes a URL or host:port/path (localhost:3000/webhooks,
:3000/webhooks, https://localhost:8443/hooks). Without it the events are
only printed (--print-json for the full bodies).

--format renders the events as another provider's webhooks, so code written
for them can be tested unchanged: ` + strings.Join(listenFormats, ", ") + `.
sendio (default, = resend) is signed Svix-style (svix-id, svix-timestamp,
svix-signature); mailgun, mailchimp (Mandrill: the signature covers the URL,
set with --endpoint, default the --forward-to URL) and sendgrid (ECDSA; the
public key is printed) are signed with their own schemes; postmark, mailjet,
brevo and mailchimp_list do not sign.

In agent mode each event is one NDJSON line on stdout (the first line is the
session: {"type":"ready","secret":…}). Ctrl-C stops (exit 0).`,
		Example: `  sendio listen --forward-to localhost:3000/webhooks
  sendio listen --events email.delivered,email.bounced --forward-to http://localhost:8080/hooks
  sendio listen --format mailgun --forward-to localhost:3000/mailgun
  sendio listen --print-json
  sendio trigger email.delivered      # in another terminal`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return a.listen(cmd.Context(), o) },
	}
	f := cmd.Flags()
	f.StringVarP(&o.forward, "forward-to", "f", "", "URL or host:port/path to forward events to")
	f.StringSliceVarP(&o.events, "events", "e", nil, "event types to receive (comma-separated; default all)")
	f.StringVar(&o.format, "format", "sendio", "webhook format: "+strings.Join(listenFormats, ", "))
	f.StringVar(&o.endpoint, "endpoint", "", "URL the events are signed for (Mandrill signs the URL; default the --forward-to URL)")
	f.BoolVar(&o.printSec, "print-secret", false, "print the session's signing secret and exit")
	f.BoolVar(&o.printJSON, "print-json", false, "print each event's full body")
	f.BoolVar(&o.skipVerify, "skip-verify", false, "do not verify the TLS certificate of an https --forward-to (self-signed local servers)")
	f.StringArrayVarP(&o.headers, "header", "H", nil, "extra header for forwarded requests, \"Name: value\" (repeatable)")
	f.DurationVar(&o.timeout, "timeout", 10*time.Second, "timeout of each forwarded request")
	root.AddCommand(cmd)
}

// forwardURL normalises --forward-to: host:port/path and :port/path get
// http:// (and localhost).
func forwardURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if strings.HasPrefix(s, ":") {
		s = "localhost" + s
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", api.UsageError("--forward-to must be a URL or host:port/path, got %q", s)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

func parseHeaderFlags(hs []string) (http.Header, error) {
	h := http.Header{}
	for _, s := range hs {
		k, v, ok := strings.Cut(s, ":")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, api.UsageError("--header must be \"Name: value\", got %q", s)
		}
		h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	return h, nil
}

func (a *App) listen(ctx context.Context, o listenOpts) error {
	target, err := forwardURL(o.forward)
	if err != nil {
		return err
	}
	if !contains(listenFormats, o.format) {
		return api.UsageError("--format must be one of %s", strings.Join(listenFormats, ", "))
	}
	extra, err := parseHeaderFlags(o.headers)
	if err != nil {
		return err
	}
	if o.timeout <= 0 {
		o.timeout = 10 * time.Second
	}
	c, err := a.Client(true)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("format", o.format)
	if id := config.DeviceID(config.DefaultDir(a.env)); id != "" {
		q.Set("device", id) // keeps the signing secret stable across reconnects
	}
	if ev := splitList(o.events); len(ev) > 0 {
		q.Set("events", strings.Join(ev, ","))
	}
	switch {
	case o.endpoint != "":
		q.Set("endpoint", o.endpoint)
	case target != "":
		q.Set("endpoint", target)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if o.skipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // --skip-verify, local servers
	}
	fwd := &http.Client{Transport: tr, Timeout: o.timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	l := &listener{a: a, o: o, target: target, extra: extra, http: fwd}
	return a.keepConnected(ctx, "listen", func(ctx context.Context, attempt int) (bool, error) {
		return l.session(ctx, c, q, attempt)
	})
}

type listener struct {
	a      *App
	o      listenOpts
	target string
	extra  http.Header
	http   *http.Client
}

// session runs one WebSocket session until it ends.
func (l *listener) session(ctx context.Context, c *api.Client, q url.Values, attempt int) (bool, error) {
	a := l.a
	conn, err := a.dialWS(ctx, c, "/v0/cli/listen", q)
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan listenFrame, 64)
	readErr := make(chan error, 1)
	go func() {
		defer close(events)
		for {
			_, data, err := conn.Read(sctx)
			if err != nil {
				readErr <- err
				return
			}
			var fr listenFrame
			if json.Unmarshal(data, &fr) != nil {
				continue
			}
			switch fr.Type {
			case "ping":
				_ = writeFrame(sctx, conn, map[string]any{"type": "pong"})
			case "ready", "event":
				select {
				case events <- fr:
				case <-sctx.Done():
					return
				}
			}
		}
	}()
	ready := false
	for fr := range events {
		switch fr.Type {
		case "ready":
			ready = true
			if l.o.printSec {
				p := a.P()
				_ = p.Result(map[string]any{"secret": fr.Secret, "session_id": fr.SessionID, "signature_scheme": fr.SignatureScheme, "public_key": nilIfEmpty(fr.PublicKey)},
					func(w io.Writer) { fmt.Fprintln(w, fr.Secret) })
				conn.Close(websocket.StatusNormalClosure, "")
				return true, errDone
			}
			l.printReady(fr, attempt)
		case "event":
			l.handle(sctx, conn, fr)
		}
	}
	if ctx.Err() != nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return ready, nil
	}
	return ready, closeReason(<-readErr)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func writeFrame(ctx context.Context, conn *websocket.Conn, v any) error {
	b, _ := json.Marshal(v)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func (l *listener) printReady(fr listenFrame, attempt int) {
	a := l.a
	p := a.P()
	if p.Machine {
		rec := map[string]any{"type": "ready", "session_id": fr.SessionID, "secret": fr.Secret, "format": fr.Format,
			"events": fr.Events, "signature_scheme": fr.SignatureScheme, "endpoint": fr.Endpoint, "forward_to": nilIfEmpty(l.target)}
		if fr.PublicKey != "" {
			rec["public_key"] = fr.PublicKey
		}
		_ = p.Line(rec)
		return
	}
	s := p.S()
	w := a.Stdout
	if attempt > 0 {
		fmt.Fprintln(w, s.Warn.Render("Reconnected: this is a new session with a new signing secret."))
	}
	fmt.Fprintf(w, "%s Your webhook signing secret is %s %s\n", s.OK.Render("Ready!"), s.Bold.Render(fr.Secret), s.Dim.Render("(^C to quit)"))
	switch fr.SignatureScheme {
	case "svix":
		fmt.Fprintln(w, s.Dim.Render("Signed like send.io webhooks (svix-id, svix-timestamp, svix-signature)."))
	case "mailgun":
		fmt.Fprintln(w, s.Dim.Render("Signed like Mailgun webhooks; use the secret as your Mailgun webhook signing key."))
	case "mandrill":
		fmt.Fprintln(w, s.Dim.Render("Signed like Mandrill webhooks (X-Mandrill-Signature) for "+fr.Endpoint+"; use the secret as the webhook key."))
	case "sendgrid_ecdsa":
		fmt.Fprintln(w, s.Dim.Render("Signed like SendGrid's Event Webhook (ECDSA). Verification key:"))
		fmt.Fprintln(w, s.Bold.Render(fr.PublicKey))
	case "none":
		fmt.Fprintln(w, s.Dim.Render("The "+fr.Format+" format is not signed (the provider does not sign its webhooks)."))
	}
	evs := strings.Join(fr.Events, ", ")
	if evs == "*" || evs == "" {
		evs = "all events"
	}
	dest := "printing events here"
	if l.target != "" {
		dest = "forwarding to " + l.target
	}
	fmt.Fprintf(w, "%s\n", s.Dim.Render(fmt.Sprintf("Format %s, %s, %s.", fr.Format, evs, dest)))
}

type forwardResult struct {
	Status   int    `json:"status,omitempty"`
	Duration int64  `json:"duration_ms"`
	Error    string `json:"error,omitempty"`
	URL      string `json:"url"`
}

// forward replays one event against the target.
func (l *listener) forward(ctx context.Context, fr listenFrame) forwardResult {
	res := forwardResult{URL: l.target}
	method := fr.Method
	if method == "" {
		method = http.MethodPost
	}
	var body io.Reader
	if fr.Body != "" || method != http.MethodGet {
		body = strings.NewReader(fr.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.target, body)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	for k, v := range fr.Headers {
		switch strings.ToLower(k) {
		case "content-length", "host", "connection", "transfer-encoding":
			continue
		}
		req.Header.Set(k, v)
	}
	for k, vs := range l.extra {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	start := time.Now()
	resp, err := l.http.Do(req)
	res.Duration = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		if ue, ok := err.(*url.Error); ok {
			res.Error = ue.Err.Error()
		}
		return res
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	res.Status = resp.StatusCode
	return res
}

func (l *listener) handle(ctx context.Context, conn *websocket.Conn, fr listenFrame) {
	a := l.a
	p := a.P()
	var fres *forwardResult
	if l.target != "" {
		r := l.forward(ctx, fr)
		fres = &r
		ack := map[string]any{"type": "ack", "id": fr.ID, "status": r.Status, "duration_ms": r.Duration}
		if r.Error != "" {
			ack["error"] = r.Error
		}
		_ = writeFrame(ctx, conn, ack)
	}
	if p.Machine {
		rec := map[string]any{"type": "event", "id": fr.ID, "event_type": fr.EventType, "format": fr.Format, "created_at": fr.CreatedAt,
			"test": fr.Test, "headers": fr.Headers, "body": bodyValue(fr.Body, fr.Headers["content-type"])}
		if fr.Method != "" {
			rec["method"] = fr.Method
		}
		if fres != nil {
			rec["forward"] = fres
		}
		_ = p.Line(rec)
		return
	}
	s := p.S()
	line := fmt.Sprintf("%s  %s  %s", a.clock(fr.CreatedAt), s.Bold.Render(fmt.Sprintf("%-22s", fr.EventType)), s.Dim.Render(fr.ID))
	if fr.Test {
		line += s.Dim.Render(" (test)")
	}
	if fres != nil {
		switch {
		case fres.Error != "":
			line += "  → " + s.Err.Render("error: "+fres.Error)
		case fres.Status >= 200 && fres.Status < 300:
			line += "  → " + s.OK.Render(fmt.Sprintf("%d %s", fres.Status, http.StatusText(fres.Status)))
		default:
			line += "  → " + s.Err.Render(fmt.Sprintf("%d %s", fres.Status, http.StatusText(fres.Status)))
		}
		line += s.Dim.Render(fmt.Sprintf(" [%dms]", fres.Duration))
	}
	fmt.Fprintln(a.Stdout, line)
	if l.o.printJSON {
		if v, ok := bodyValue(fr.Body, fr.Headers["content-type"]).(map[string]any); ok {
			b, _ := out.MarshalIndent(v)
			fmt.Fprintln(a.Stdout, string(b))
		} else if v, ok := bodyValue(fr.Body, fr.Headers["content-type"]).([]any); ok {
			b, _ := out.MarshalIndent(v)
			fmt.Fprintln(a.Stdout, string(b))
		} else {
			fmt.Fprintln(a.Stdout, fr.Body)
		}
	}
}

// bodyValue returns a JSON body as JSON, a form body as an object, else the
// string.
func bodyValue(body, ct string) any {
	if strings.Contains(ct, "json") || strings.HasPrefix(strings.TrimSpace(body), "{") || strings.HasPrefix(strings.TrimSpace(body), "[") {
		if v, err := decodeJSON(body); err == nil {
			return out.Normalize(v)
		}
	}
	if strings.Contains(ct, "x-www-form-urlencoded") {
		if vals, err := url.ParseQuery(body); err == nil {
			m := map[string]any{}
			for k, v := range vals {
				if len(v) == 1 {
					m[k] = v[0]
				} else {
					m[k] = v
				}
			}
			return m
		}
	}
	return body
}
