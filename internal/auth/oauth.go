// Package auth implements the CLI sign-in flows of the sendio CLI contract
// (section 1): the loopback PKCE browser flow (RFC 8252 + RFC 7636), the
// device authorization flow (RFC 8628), token refresh (with rotation) and
// revocation.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"send.io/cli/internal/api"
)

// ClientID is the CLI's first-party public OAuth client.
const ClientID = "sendio-cli"

// Scope is the scope the CLI asks for.
const Scope = "cli"

// Token is a token endpoint response.
type Token struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// ExpiresAt converts expires_in to an absolute time (zero when unknown).
func (t *Token) ExpiresAt(now time.Time) time.Time {
	if t.ExpiresIn <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// Flow holds what both sign-in flows need.
type Flow struct {
	// APIBase is the API origin; it is also the RFC 8707 resource.
	APIBase    string
	HTTP       *http.Client
	DeviceName string
	// OpenBrowser opens a URL; an error makes the flow print it instead.
	OpenBrowser func(string) error
	// Notify prints progress for humans (stderr).
	Notify func(string)
	// Sleep waits between device-flow polls (tests shorten it).
	Sleep func(context.Context, time.Duration) error
	// ListenAddr is the loopback address (default 127.0.0.1:0).
	ListenAddr string
}

func (f *Flow) http() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (f *Flow) notify(format string, a ...any) {
	if f.Notify != nil {
		f.Notify(fmt.Sprintf(format, a...))
	}
}

func (f *Flow) base() string { return strings.TrimRight(f.APIBase, "/") }

// SanitizeDeviceName keeps a printable device name of at most 64 characters.
func SanitizeDeviceName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if r := []rune(out); len(r) > 64 {
		out = string(r[:64])
	}
	return out
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// PKCE returns a verifier and its S256 challenge.
func PKCE() (verifier, challenge string) {
	verifier = randomString(32)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

type callbackResult struct {
	code string
	err  error
}

// Browser runs the loopback PKCE flow: it listens on 127.0.0.1 on a random
// port, opens /v0/authorize in the browser and exchanges the code it gets
// back on /callback.
func (f *Flow) Browser(ctx context.Context) (*Token, error) {
	addr := f.ListenAddr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, api.Errorf(api.ExitAuth, "loopback_unavailable", "cannot listen on %s for the sign-in callback: %v (try `sendio login --device`)", addr, err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	verifier, challenge := PKCE()
	state := randomString(24)

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("scope", Scope)
	q.Set("resource", f.base())
	if d := SanitizeDeviceName(f.DeviceName); d != "" {
		q.Set("device_name", d)
	}
	authURL := f.base() + "/v0/authorize?" + q.Encode()

	results := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		var res callbackResult
		switch {
		case v.Get("state") != state:
			res.err = api.Errorf(api.ExitAuth, "invalid_state", "the sign-in callback had an unexpected state; try again")
		case v.Get("error") != "":
			msg := v.Get("error_description")
			if msg == "" {
				msg = "sign-in was not completed (" + v.Get("error") + ")"
			}
			res.err = &api.Error{Exit: api.ExitAuth, Code: v.Get("error"), Message: msg}
		case v.Get("code") == "":
			res.err = api.Errorf(api.ExitAuth, "invalid_callback", "the sign-in callback had no code")
		default:
			res.code = v.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		title, body := "Signed in", "The send.io CLI is signed in. You can close this tab and return to your terminal."
		if res.err != nil {
			title, body = "Sign-in failed", html.EscapeString(res.err.Error())
		}
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>send.io CLI</title><body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem"><h1>%s</h1><p>%s</p></body>`, title, body)
		select {
		case results <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	opened := false
	if f.OpenBrowser != nil {
		opened = f.OpenBrowser(authURL) == nil
	}
	if opened {
		f.notify("Opening your browser to sign in to send.io…\nIf it did not open, visit:\n  %s", authURL)
	} else {
		f.notify("Open this URL in your browser to sign in to send.io:\n  %s", authURL)
	}

	var res callbackResult
	select {
	case <-ctx.Done():
		return nil, api.Errorf(api.ExitTimeout, "login_timeout", "timed out waiting for the browser sign-in")
	case res = <-results:
	}
	if res.err != nil {
		return nil, res.err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", res.code)
	form.Set("redirect_uri", redirect)
	form.Set("client_id", ClientID)
	form.Set("code_verifier", verifier)
	form.Set("resource", f.base())
	return postToken(ctx, f.http(), f.base(), form)
}

// DeviceCode is the device authorization response.
type DeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// Device runs the RFC 8628 flow. show is called once with the code to
// display (and may open the verification URL).
func (f *Flow) Device(ctx context.Context, show func(DeviceCode)) (*Token, error) {
	form := url.Values{}
	form.Set("client_id", ClientID)
	form.Set("scope", Scope)
	form.Set("resource", f.base())
	if d := SanitizeDeviceName(f.DeviceName); d != "" {
		form.Set("device_name", d)
	}
	var dc DeviceCode
	if err := postForm(ctx, f.http(), f.base()+"/v0/oauth/device_authorization", form, &dc); err != nil {
		return nil, err
	}
	if dc.DeviceCode == "" {
		return nil, api.Errorf(api.ExitAPI, "invalid_response", "the device authorization response had no device_code")
	}
	if show != nil {
		show(dc)
	}
	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if dc.ExpiresIn > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(dc.ExpiresIn)*time.Second)
		defer cancel()
	}
	sleep := f.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	poll := url.Values{}
	poll.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	poll.Set("device_code", dc.DeviceCode)
	poll.Set("client_id", ClientID)
	poll.Set("resource", f.base())
	for {
		if err := sleep(ctx, interval); err != nil {
			return nil, api.Errorf(api.ExitAuth, "expired_token", "the device code expired before it was approved; run `sendio login --device` again")
		}
		tok, err := postToken(ctx, f.http(), f.base(), poll)
		if err == nil {
			return tok, nil
		}
		var e *api.Error
		if !errors.As(err, &e) {
			return nil, err
		}
		switch e.Code {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied":
			return nil, &api.Error{Exit: api.ExitAuth, Code: "access_denied", Message: "the sign-in request was denied"}
		case "expired_token":
			return nil, &api.Error{Exit: api.ExitAuth, Code: "expired_token", Message: "the device code expired before it was approved; run `sendio login --device` again"}
		default:
			if e.Exit == api.ExitNetwork {
				continue
			}
			return nil, e
		}
	}
}

// Refresh exchanges a refresh token for a new (rotated) pair.
func Refresh(ctx context.Context, hc *http.Client, apiBase, refreshToken string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", ClientID)
	form.Set("resource", strings.TrimRight(apiBase, "/"))
	return postToken(ctx, hc, strings.TrimRight(apiBase, "/"), form)
}

// Revoke revokes a token (RFC 7009); errors are returned but harmless.
func Revoke(ctx context.Context, hc *http.Client, apiBase, token string) error {
	form := url.Values{}
	form.Set("token", token)
	form.Set("client_id", ClientID)
	return postForm(ctx, hc, strings.TrimRight(apiBase, "/")+"/v0/revoke", form, nil)
}

func postToken(ctx context.Context, hc *http.Client, base string, form url.Values) (*Token, error) {
	var t Token
	if err := postForm(ctx, hc, base+"/v0/token", form, &t); err != nil {
		return nil, err
	}
	if t.AccessToken == "" {
		return nil, api.Errorf(api.ExitAPI, "invalid_response", "the token response had no access_token")
	}
	return &t, nil
}

func postForm(ctx context.Context, hc *http.Client, u string, form url.Values, out any) error {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return api.NetworkError(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return api.NetworkError(err)
	}
	if resp.StatusCode >= 400 {
		e := api.FromResponse(resp.StatusCode, resp.Header, data)
		// OAuth errors are 400s; authorization problems are auth failures.
		if resp.StatusCode == 400 || resp.StatusCode == 401 {
			e.Exit = api.ExitAuth
			e.Hint = ""
		}
		if resp.StatusCode == 404 {
			e.Hint = "This API does not offer CLI sign-in yet; use `sendio login --api-key mx_…`."
		}
		return e
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return api.Errorf(api.ExitAPI, "invalid_response", "unexpected response from %s", u)
		}
	}
	return nil
}
