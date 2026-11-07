// Package api is the CLI's HTTP layer: auth headers, automatic token refresh
// on 401, --dry-run interception, typed errors with exit codes and cursor
// pagination. Typed SDK calls (send.io/sendio-go) run through the same
// layer via Client.Transport.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrDryRun is returned after a write request was printed instead of sent.
var ErrDryRun = errors.New("dry run: request not sent")

// TokenSource supplies the bearer token and refreshes it after a 401.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	// Refresh gets a new token after the API answered 401. It returns false
	// when the credential cannot be refreshed (an API key).
	Refresh(ctx context.Context) (bool, error)
}

// StaticToken is an API key.
type StaticToken string

// Token implements TokenSource.
func (s StaticToken) Token(context.Context) (string, error) { return string(s), nil }

// Refresh implements TokenSource.
func (StaticToken) Refresh(context.Context) (bool, error) { return false, nil }

// Client calls the send.io API.
type Client struct {
	BaseURL   string
	HTTP      *http.Client
	UserAgent string
	Auth      TokenSource
	// DryRun prints write requests (anything but GET/HEAD) via OnDryRun and
	// returns ErrDryRun instead of sending them.
	DryRun   bool
	OnDryRun func(r *http.Request, body []byte)
	// Progress is called as each request goes out and returns the function
	// to call when its response is in (the CLI draws a spinner on stderr).
	Progress func(req *Request) (done func())
}

// Request is one API call.
type Request struct {
	Method string
	// Path is relative to BaseURL ("/emails") or an absolute URL.
	Path  string
	Query url.Values
	// JSON is marshalled as the body; Raw is sent as is with ContentType.
	JSON        any
	Raw         []byte
	ContentType string
	Header      http.Header
	NoAuth      bool
	// ReadOnly lets a POST that changes nothing (a render, a preview)
	// through --dry-run.
	ReadOnly bool
}

// Response is a successful API response.
type Response struct {
	Status   int
	Header   http.Header
	Body     []byte
	Duration time.Duration
}

// Decode unmarshals the response body.
func (r *Response) Decode(v any) error {
	if len(bytes.TrimSpace(r.Body)) == 0 {
		return nil
	}
	return json.Unmarshal(r.Body, v)
}

// Value returns the body as generic JSON (nil for an empty body).
func (r *Response) Value() (any, error) {
	if len(bytes.TrimSpace(r.Body)) == 0 {
		return nil, nil
	}
	var v any
	d := json.NewDecoder(bytes.NewReader(r.Body))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return strings.TrimRight(string(r.Body), "\n"), nil
	}
	return v, nil
}

// URL resolves a path against the base URL.
func (c *Client) URL(path string, q url.Values) string {
	u := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		u = strings.TrimRight(c.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	}
	if len(q) > 0 {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + q.Encode()
	}
	return u
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

// IsWrite reports whether a method changes state (and so honours --dry-run).
func IsWrite(method string) bool {
	m := strings.ToUpper(method)
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

func (c *Client) build(ctx context.Context, req *Request, body []byte) (*http.Request, error) {
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	hr, err := http.NewRequestWithContext(ctx, method, c.URL(req.Path, req.Query), rd)
	if err != nil {
		return nil, UsageError("invalid request: %v", err)
	}
	if body != nil {
		ct := req.ContentType
		if ct == "" {
			ct = "application/json"
		}
		hr.Header.Set("Content-Type", ct)
	}
	hr.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		hr.Header.Set("User-Agent", c.UserAgent)
	}
	for k, vs := range req.Header {
		hr.Header.Del(k)
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	if !req.NoAuth && c.Auth != nil && hr.Header.Get("Authorization") == "" {
		tok, err := c.Auth.Token(ctx)
		if err != nil {
			return nil, err
		}
		if tok != "" {
			hr.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	return hr, nil
}

func (c *Client) body(req *Request) ([]byte, error) {
	if req.Raw != nil {
		return req.Raw, nil
	}
	if req.JSON != nil {
		b, err := json.Marshal(req.JSON)
		if err != nil {
			return nil, UsageError("encoding the request body: %v", err)
		}
		return b, nil
	}
	return nil, nil
}

// Do sends a request and reads the whole response. A 401 is retried once
// after refreshing the token; error statuses come back as *Error.
func (c *Client) Do(ctx context.Context, req *Request) (*Response, error) {
	body, err := c.body(req)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		hr, err := c.build(ctx, req, body)
		if err != nil {
			return nil, err
		}
		if c.DryRun && IsWrite(hr.Method) && !req.ReadOnly {
			if c.OnDryRun != nil {
				c.OnDryRun(hr, body)
			}
			return nil, ErrDryRun
		}
		start := time.Now()
		done := func() {}
		if c.Progress != nil {
			done = c.Progress(req)
		}
		resp, err := c.httpClient().Do(hr)
		if err != nil {
			done()
			return nil, NetworkError(err)
		}
		data, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		done()
		if rerr != nil {
			return nil, NetworkError(rerr)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.Auth != nil && !req.NoAuth && req.Header.Get("Authorization") == "" {
			ok, ferr := c.Auth.Refresh(ctx)
			if ferr != nil {
				return nil, ferr
			}
			if ok {
				continue
			}
		}
		if resp.StatusCode >= 400 {
			return nil, FromResponse(resp.StatusCode, resp.Header, data)
		}
		return &Response{Status: resp.StatusCode, Header: resp.Header, Body: data, Duration: time.Since(start)}, nil
	}
}

// Stream opens a long-lived request (SSE, chunked) and returns the live
// response; the caller closes its body. Extension point for `logs tail`,
// `events tail` and similar streaming commands.
func (c *Client) Stream(ctx context.Context, req *Request) (*http.Response, error) {
	body, err := c.body(req)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		hr, err := c.build(ctx, req, body)
		if err != nil {
			return nil, err
		}
		resp, err := (&http.Client{Transport: c.httpClient().Transport}).Do(hr)
		if err != nil {
			return nil, NetworkError(err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.Auth != nil && !req.NoAuth {
			resp.Body.Close()
			if ok, ferr := c.Auth.Refresh(ctx); ferr != nil {
				return nil, ferr
			} else if ok {
				continue
			}
			return nil, FromResponse(401, resp.Header, nil)
		}
		if resp.StatusCode >= 400 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return nil, FromResponse(resp.StatusCode, resp.Header, data)
		}
		return resp, nil
	}
}

// NetworkError wraps a transport failure (exit 4).
func NetworkError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	msg := err.Error()
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		msg = "request timed out: " + msg
	}
	if errors.Is(err, context.Canceled) {
		msg = "canceled"
	}
	return &Error{Exit: ExitNetwork, Code: "network_error", Message: msg, Hint: "Check your connection and --base-url."}
}

// Transport adapts the client to an http.RoundTripper for the typed SDK:
// requests get this client's auth, refresh, dry-run and error handling, and
// API errors surface as *Error (wrapped in *url.Error by net/http).
func (c *Client) Transport() http.RoundTripper { return sdkTransport{c} }

type sdkTransport struct{ c *Client }

func (t sdkTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	h := http.Header{}
	for _, k := range []string{"Idempotency-Key", "X-Batch-Validation"} {
		if v := r.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	req := &Request{Method: r.Method, Path: r.URL.String(), Raw: body, ContentType: r.Header.Get("Content-Type"), Header: h}
	if len(body) == 0 {
		req.Raw = nil
	}
	resp, err := t.c.Do(r.Context(), req)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: resp.Status, Status: fmt.Sprintf("%d %s", resp.Status, http.StatusText(resp.Status)),
		Header: resp.Header, Body: io.NopCloser(bytes.NewReader(resp.Body)), Request: r,
		ContentLength: int64(len(resp.Body)), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
	}, nil
}

// Page is one page of a list endpoint.
type Page struct {
	Data    []json.RawMessage `json:"data"`
	HasMore bool              `json:"has_more"`
}

// Paginate walks a cursor-paginated list (limit/after, has_more), calling fn
// for every item. max caps the number of items (0 = all).
func (c *Client) Paginate(ctx context.Context, req *Request, pageSize, max int, fn func(json.RawMessage) error) error {
	q := url.Values{}
	for k, v := range req.Query {
		q[k] = append([]string(nil), v...)
	}
	if pageSize > 0 {
		q.Set("limit", fmt.Sprint(pageSize))
	}
	seen := 0
	for {
		r := *req
		r.Query = q
		resp, err := c.Do(ctx, &r)
		if err != nil {
			return err
		}
		var p Page
		if err := json.Unmarshal(resp.Body, &p); err != nil {
			return &Error{Exit: ExitAPI, Code: "invalid_response", Message: "the list response is not a {data, has_more} page"}
		}
		for _, it := range p.Data {
			if err := fn(it); err != nil {
				return err
			}
			seen++
			if max > 0 && seen >= max {
				return nil
			}
		}
		if !p.HasMore || len(p.Data) == 0 {
			return nil
		}
		var last struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(p.Data[len(p.Data)-1], &last)
		if last.ID == "" {
			return nil
		}
		q.Set("after", last.ID)
		q.Del("before")
	}
}
