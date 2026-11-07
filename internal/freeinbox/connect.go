package freeinbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// DefaultAppURL is the send.io web app that opens connect links
// (SENDIO_APP_URL overrides it).
const DefaultAppURL = "https://app.send.io"

// ConnectTTL is how long a connect link can be used (once).
const ConnectTTL = 30 * time.Minute

// ConnectLink is a one-use link that lets whoever opens it (signed in to
// send.io) attach the inbox to their workspace (ADMIN-API.md "Connect links").
// After that the inbox key can no longer stream or poll it.
type ConnectLink struct {
	Address   string    `json:"address"`
	Link      string    `json:"link"`
	ExpiresAt time.Time `json:"expires_at"`
	TS        int64     `json:"-"`
	Nonce     string    `json:"-"`
	Sig       string    `json:"-"`
}

// NormalizeAppURL trims an app origin; empty means DefaultAppURL.
func NormalizeAppURL(u string) string {
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	if u == "" {
		return DefaultAppURL
	}
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	return u
}

// NewConnectLink signs a fresh connect link for in (random 16-byte nonce,
// ts = now).
func NewConnectLink(appURL string, in *Inbox, now time.Time) (*ConnectLink, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return BuildConnectLink(appURL, in.Keys, in.Address, now.Unix(), nonce), nil
}

// BuildConnectLink is NewConnectLink with a fixed ts and nonce (tests).
func BuildConnectLink(appURL string, k *Keys, address string, ts int64, nonce []byte) *ConnectLink {
	address = strings.ToLower(strings.TrimSpace(address))
	n := base64.RawURLEncoding.EncodeToString(nonce)
	sig := base64.RawURLEncoding.EncodeToString(k.SignConnect(ts, address, n))
	// "@" is legal in a fragment; keep the address readable.
	a := strings.ReplaceAll(url.QueryEscape(address), "%40", "@")
	link := NormalizeAppURL(appURL) + "/connect#a=" + a + "&t=" + strconv.FormatInt(ts, 10) + "&n=" + n + "&s=" + sig
	return &ConnectLink{Address: address, Link: link, ExpiresAt: time.Unix(ts, 0).Add(ConnectTTL).UTC(), TS: ts, Nonce: n, Sig: sig}
}

// InboxStatus asks the relay, over the stream, whether in can still be read
// with its key: "active", "claimed" (moved into a send.io workspace),
// "suspended", "deleted" or "auth_failed". The HTTP long-poll API answers
// 401 auth_failed for all but the first, the stream tells them apart. No
// mail is received or acked. Note that, like any stream connection, it takes
// over an active inbox from another live connection: call it only after the
// relay refused the key.
func InboxStatus(ctx context.Context, c *Client, in *Inbox) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	h := http.Header{}
	if c.UserAgent != "" {
		h.Set("User-Agent", c.UserAgent)
	}
	var hc *http.Client
	if c.HTTP != nil {
		hc = &http.Client{Transport: c.HTTP.Transport}
	}
	conn, _, err := websocket.Dial(ctx, c.StreamURL(), &websocket.DialOptions{HTTPHeader: h, HTTPClient: hc})
	if err != nil {
		return "", fmt.Errorf("connect %s: %w", c.StreamURL(), err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 20)
	read := func(d time.Duration) (*frame, error) {
		rctx, rc := context.WithTimeout(ctx, d)
		defer rc()
		_, data, err := conn.Read(rctx)
		if err != nil {
			return nil, err
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("bad frame from the relay: %v", err)
		}
		return &f, nil
	}
	f, err := read(10 * time.Second)
	if err != nil {
		return "", err
	}
	if f.Type != "challenge" || f.Nonce == "" {
		return "", fmt.Errorf("expected a challenge from the relay, got %q", f.Type)
	}
	// The probe closes as soon as an active inbox is ready, before any mail.
	auth := map[string]any{"type": "auth",
		"inboxes": []map[string]string{{"address": in.Address, "sig": base64.StdEncoding.EncodeToString(in.Keys.SignAuth(f.Nonce, in.Address))}}}
	b, _ := json.Marshal(auth)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return "", err
	}
	ready := false
	for {
		wait := 10 * time.Second
		if ready {
			wait = 2 * time.Second // ended inboxes are reported right after ready
		}
		f, err := read(wait)
		if err != nil {
			if ready {
				conn.Close(websocket.StatusNormalClosure, "")
				return "active", nil
			}
			return "", err
		}
		switch f.Type {
		case "ready":
			ready = true
			if len(f.Inboxes) > 0 {
				conn.Close(websocket.StatusNormalClosure, "")
				return "active", nil
			}
		case "error":
			if f.Code == "auth_failed" || endedCodes[f.Code] {
				return f.Code, nil
			}
			return "", fmt.Errorf("relay error: %s %s", f.Code, f.ErrText)
		}
	}
}
