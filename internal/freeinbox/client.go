package freeinbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client talks to a relay over HTTP.
type Client struct {
	// Relay is the relay origin, e.g. https://relay.sendioinbox.com.
	Relay     string
	HTTP      *http.Client
	UserAgent string
	// Now is the clock for signed requests (nil: time.Now).
	Now func() time.Time
}

// NormalizeRelay turns any relay form (https origin, wss stream URL) into
// the https/http origin.
func NormalizeRelay(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultRelay
	}
	switch {
	case strings.HasPrefix(s, "wss://"):
		s = "https://" + strings.TrimPrefix(s, "wss://")
	case strings.HasPrefix(s, "ws://"):
		s = "http://" + strings.TrimPrefix(s, "ws://")
	case !strings.Contains(s, "://"):
		s = "https://" + s
	}
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, "/v1/stream")
	return strings.TrimRight(s, "/")
}

// StreamURL is the relay's WebSocket URL.
func (c *Client) StreamURL() string {
	r := NormalizeRelay(c.Relay)
	switch {
	case strings.HasPrefix(r, "https://"):
		r = "wss://" + strings.TrimPrefix(r, "https://")
	case strings.HasPrefix(r, "http://"):
		r = "ws://" + strings.TrimPrefix(r, "http://")
	}
	return r + "/v1/stream"
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// Error is a relay error response, or a transport failure (Status 0).
type Error struct {
	Status      int      `json:"status,omitempty"`
	Code        string   `json:"error"`
	Message     string   `json:"message"`
	Suggestions []string `json:"suggestions,omitempty"`
	RetryAfter  string   `json:"-"`
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return "relay unreachable: " + e.Message
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("relay error %d %s", e.Status, e.Code)
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	u := NormalizeRelay(c.Relay) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Code: "network_error", Message: err.Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return &Error{Code: "network_error", Message: err.Error()}
	}
	if resp.StatusCode >= 400 {
		e := &Error{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
		if json.Unmarshal(data, e) != nil || e.Code == "" {
			e.Code = strings.ToLower(strings.ReplaceAll(http.StatusText(resp.StatusCode), " ", "_"))
			e.Message = strings.TrimSpace(string(data))
		}
		if e.Message == "" {
			e.Message = e.Code
		}
		return e
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("relay %s %s: bad JSON response: %v", method, path, err)
		}
	}
	return nil
}

// Health is GET /v1/health.
func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, "GET", "/v1/health", nil, nil, &out)
	return out, err
}

// CheckResult is GET /v1/check.
type CheckResult struct {
	Address     string   `json:"address"`
	Available   bool     `json:"available"`
	Reason      string   `json:"reason,omitempty"`
	HoldToken   string   `json:"hold_token,omitempty"`
	Suggestions []string `json:"suggestions"`
}

// Check asks whether name@org.<pool> is available.
func (c *Client) Check(ctx context.Context, name, org string) (*CheckResult, error) {
	var out CheckResult
	err := c.do(ctx, "GET", "/v1/check", url.Values{"name": {name}, "org": {org}}, nil, &out)
	if out.Suggestions == nil {
		out.Suggestions = []string{}
	}
	return &out, err
}

// Challenge is POST /v1/pow.
type Challenge struct {
	Challenge  string `json:"challenge"`
	Difficulty int    `json:"difficulty"`
	ExpiresAt  string `json:"expires_at"`
}

// PoW fetches a proof-of-work challenge.
func (c *Client) PoW(ctx context.Context) (*Challenge, error) {
	var out Challenge
	err := c.do(ctx, "POST", "/v1/pow", nil, map[string]any{}, &out)
	return &out, err
}

// LeadingZeroBits counts the leading zero bits of b.
func LeadingZeroBits(b []byte) int {
	n := 0
	for _, x := range b {
		if x == 0 {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(x)
	}
	return n
}

// SolvePoW finds a decimal nonce such that sha256(challenge + ":" + nonce)
// has difficulty leading zero bits, using every CPU.
func SolvePoW(ctx context.Context, challenge string, difficulty int) (string, error) {
	if difficulty <= 0 {
		return "0", nil
	}
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	var found atomic.Bool
	var result atomic.Uint64
	var wg sync.WaitGroup
	prefix := []byte(challenge + ":")
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(start uint64) {
			defer wg.Done()
			buf := make([]byte, 0, len(prefix)+20)
			for n := start; ; n += uint64(workers) {
				if n&0xfff == start&0xfff && (found.Load() || ctx.Err() != nil) {
					return
				}
				buf = strconv.AppendUint(append(buf[:0], prefix...), n, 10)
				sum := sha256.Sum256(buf)
				if LeadingZeroBits(sum[:]) >= difficulty {
					if found.CompareAndSwap(false, true) {
						result.Store(n)
					}
					return
				}
			}
		}(uint64(w))
	}
	wg.Wait()
	if !found.Load() {
		return "", ctx.Err()
	}
	return strconv.FormatUint(result.Load(), 10), nil
}

// VerifyPoW checks a solution (tests).
func VerifyPoW(challenge, nonce string, difficulty int) bool {
	sum := sha256.Sum256([]byte(challenge + ":" + nonce))
	return LeadingZeroBits(sum[:]) >= difficulty
}

// Created is the 201 body of POST /v1/inboxes.
type Created struct {
	Address         string `json:"address"`
	CreatedAt       string `json:"created_at"`
	HoldHours       int    `json:"hold_hours"`
	DailyLimit      int    `json:"daily_limit"`
	MaxMessageBytes int    `json:"max_message_bytes"`
	Relay           string `json:"relay"`
}

// CreateRequest is the body of POST /v1/inboxes.
type CreateRequest struct {
	Address   string            `json:"address"`
	SignPub   string            `json:"sign_pub"`
	BoxPub    string            `json:"box_pub"`
	PoW       map[string]string `json:"pow"`
	HoldToken string            `json:"hold_token,omitempty"`
	OrgSig    string            `json:"org_sig,omitempty"`
}

// Create registers an inbox with already solved proof of work.
func (c *Client) Create(ctx context.Context, req CreateRequest) (*Created, error) {
	var out Created
	err := c.do(ctx, "POST", "/v1/inboxes", nil, req, &out)
	return &out, err
}

// Message is one sealed message (a WebSocket "message" frame or a poll item).
type Message struct {
	Address    string `json:"address"`
	Seq        uint64 `json:"seq"`
	ID         string `json:"id"`
	ReceivedAt string `json:"received_at"`
	Size       int    `json:"size"`
	Envelope   string `json:"envelope"`
}

// EnvelopeBytes decodes the base64 envelope.
func (m *Message) EnvelopeBytes() ([]byte, error) {
	return base64.StdEncoding.DecodeString(m.Envelope)
}

// Poll is POST /v1/messages: messages after seq, waiting up to wait seconds.
func (c *Client) Poll(ctx context.Context, address string, keys *Keys, after uint64, wait, limit int) ([]Message, error) {
	ts := c.now().Unix()
	body := map[string]any{"address": address, "ts": ts, "sig": base64.StdEncoding.EncodeToString(keys.SignPoll(ts, address)),
		"after": after, "wait": wait, "limit": limit}
	var out struct {
		Messages []Message `json:"messages"`
	}
	hc := c.http()
	if wait > 0 && hc.Timeout != 0 && hc.Timeout < time.Duration(wait+30)*time.Second {
		cp := *hc
		cp.Timeout = time.Duration(wait+30) * time.Second
		hc = &cp
	}
	cc := *c
	cc.HTTP = hc
	err := cc.do(ctx, "POST", "/v1/messages", nil, body, &out)
	return out.Messages, err
}

// Ack is POST /v1/ack (cumulative).
func (c *Client) Ack(ctx context.Context, address string, keys *Keys, seq uint64) error {
	ts := c.now().Unix()
	body := map[string]any{"address": address, "ts": ts, "sig": base64.StdEncoding.EncodeToString(keys.SignAck(ts, address, seq)), "seq": seq}
	return c.do(ctx, "POST", "/v1/ack", nil, body, nil)
}
