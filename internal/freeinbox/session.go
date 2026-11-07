package freeinbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Inbox is one local inbox with its keys.
type Inbox struct {
	Address string
	Keys    *Keys
	// Relay is the relay recorded in key.json.
	Relay string
}

// OpenInbox loads a local inbox.
func (s *Store) OpenInbox(address string) (*Inbox, error) {
	kf, err := s.LoadKey(address)
	if err != nil {
		return nil, err
	}
	keys, err := kf.Keys()
	if err != nil {
		return nil, err
	}
	return &Inbox{Address: kf.Address, Keys: keys, Relay: kf.Relay}, nil
}

// Delivered is a message that has been decrypted and written to disk.
type Delivered struct {
	Address string
	Index   *MailIndex
	Raw     []byte
	// Path is the .eml file.
	Path string
	// New is false for a message that was already on disk (a replay of a
	// message whose ack did not reach the relay).
	New bool
}

// Receiver turns sealed messages into local mail: decrypt, write the .eml
// and .json, then (and only then) ack.
type Receiver struct {
	Store *Store
	// OnMessage is called for every stored message, in order (also for
	// replays, with New false).
	OnMessage func(Delivered)
	// OnWarn reports problems that do not stop receiving.
	OnWarn func(format string, args ...any)

	mu      sync.Mutex
	cursors map[string]uint64
}

func (r *Receiver) warn(format string, args ...any) {
	if r.OnWarn != nil {
		r.OnWarn(format, args...)
	}
}

// Cursor is the last acked seq of an inbox (state.json).
func (r *Receiver) Cursor(address string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.cursors[address]; ok {
		return c
	}
	st, err := r.Store.LoadState(address)
	if err != nil {
		r.warn("%s: state.json: %v", address, err)
	}
	if r.cursors == nil {
		r.cursors = map[string]uint64{}
	}
	r.cursors[address] = st.Cursor
	return st.Cursor
}

// acked records a cumulative ack in state.json.
func (r *Receiver) acked(address string, seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cursors == nil {
		r.cursors = map[string]uint64{}
	}
	if seq <= r.cursors[address] {
		return
	}
	r.cursors[address] = seq
	if err := r.Store.SaveState(address, State{Cursor: seq}); err != nil {
		r.warn("%s: could not save state.json: %v", address, err)
	}
}

// store decrypts and saves one message. ok reports whether it may be acked
// (stored, already stored, or undecryptable and skipped).
func (r *Receiver) store(in *Inbox, m Message) (d *Delivered, ok bool) {
	if r.Store.HasMail(in.Address, m.Seq) {
		idx, raw := r.loadIndex(in.Address, m.Seq)
		if idx != nil {
			return &Delivered{Address: in.Address, Index: idx, Raw: raw, Path: r.Store.EMLPath(in.Address, m.Seq)}, true
		}
	}
	env, err := m.EnvelopeBytes()
	if err == nil {
		var raw []byte
		raw, err = in.Keys.Open(env)
		if err == nil {
			idx := Index(raw, m.Seq, m.ID, m.ReceivedAt)
			if err := r.Store.SaveMail(in.Address, raw, idx); err != nil {
				r.warn("%s: could not save message %d: %v (not acked; it will be delivered again)", in.Address, m.Seq, err)
				return nil, false
			}
			return &Delivered{Address: in.Address, Index: idx, Raw: raw, Path: r.Store.EMLPath(in.Address, m.Seq), New: true}, true
		}
	}
	r.warn("%s: message %d skipped: %v", in.Address, m.Seq, err)
	return nil, true
}

func (r *Receiver) loadIndex(address string, seq uint64) (*MailIndex, []byte) {
	raw, err := r.Store.RawMail(address, seq)
	if err != nil {
		return nil, nil
	}
	return Index(raw, seq, "", ""), raw
}

// PollOnce long-polls one inbox (waiting up to wait seconds), stores what
// arrives and acks it.
func (r *Receiver) PollOnce(ctx context.Context, c *Client, in *Inbox, wait, limit int) ([]Delivered, error) {
	msgs, err := c.Poll(ctx, in.Address, in.Keys, r.Cursor(in.Address), wait, limit)
	if err != nil {
		return nil, err
	}
	var out []Delivered
	var ackTo uint64
	for _, m := range msgs {
		if m.Address == "" {
			m.Address = in.Address
		}
		d, ok := r.store(in, m)
		if !ok {
			break
		}
		ackTo = m.Seq
		if d != nil {
			out = append(out, *d)
			if r.OnMessage != nil {
				r.OnMessage(*d)
			}
		}
	}
	if ackTo > 0 {
		if err := c.Ack(ctx, in.Address, in.Keys, ackTo); err != nil {
			return out, fmt.Errorf("ack: %w", err)
		}
		r.acked(in.Address, ackTo)
	}
	return out, nil
}

// Stream errors.
var (
	// ErrAuthFailed is the relay refusing the inbox signatures.
	ErrAuthFailed = errors.New("the relay refused this inbox's key (auth_failed): the inbox does not exist on this relay or the key is wrong")
	// ErrReplaced means every inbox moved to a newer connection.
	ErrReplaced = errors.New("another client connected to this inbox; this one stopped receiving (one live connection per inbox)")
	// ErrClaimed matches an EndedError with code "claimed": the inbox was
	// attached to a send.io workspace (e.g. via a connect link) and its key
	// can no longer read it.
	ErrClaimed = errors.New("this inbox was moved into a send.io workspace")
)

// EndedError means the relay ended an inbox for good: "claimed" (moved
// into a send.io workspace), "suspended" or "deleted". Do not reconnect.
type EndedError struct {
	Address string
	Code    string
}

func (e *EndedError) Error() string {
	if e.Code == "claimed" {
		return e.Address + " was moved into a send.io workspace; read it in the dashboard"
	}
	return e.Address + " was " + e.Code + " by send.io"
}

// Is makes errors.Is(err, ErrClaimed) true for a claimed inbox.
func (e *EndedError) Is(target error) bool { return target == ErrClaimed && e.Code == "claimed" }

// endedCodes are relay error codes that end one inbox (not the connection).
var endedCodes = map[string]bool{"claimed": true, "suspended": true, "deleted": true}

// Event is a connection state change reported by Stream.
type Event struct {
	// Kind is connecting, ready, reconnecting, replaced, or ended (the relay
	// ended Address for good; Code says why: claimed, suspended, deleted).
	Kind    string
	Address string
	Code    string
	Attempt int
	Delay   time.Duration
	Err     error
}

// Stream receives over the relay WebSocket and reconnects (backoff 1 s
// doubling to 60 s, with jitter), resuming from each inbox's cursor.
type Stream struct {
	Client   *Client
	Receiver *Receiver
	Inboxes  []*Inbox
	OnEvent  func(Event)
	// Sleep waits between reconnects (nil: a timer).
	Sleep func(context.Context, time.Duration) error
	// IdleTimeout closes a connection that has been silent this long (90 s).
	IdleTimeout time.Duration

	mu       sync.Mutex
	replaced map[string]bool
	// ended holds inboxes the relay ended for good (code by address).
	ended map[string]string
}

func (s *Stream) event(e Event) {
	if s.OnEvent != nil {
		s.OnEvent(e)
	}
}

func (s *Stream) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Backoff returns the reconnect delay after n failures: 1 s doubling to
// 60 s, plus up to 25% jitter.
func Backoff(n int) time.Duration {
	d := time.Second << min(n, 6)
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d + time.Duration(rand.Int63n(int64(d/4)+1))
}

// Run streams until ctx ends (nil) or a permanent error.
func (s *Stream) Run(ctx context.Context) error {
	fails := 0
	for attempt := 0; ; attempt++ {
		s.event(Event{Kind: "connecting", Attempt: attempt})
		ready, err := s.once(ctx, attempt)
		if ctx.Err() != nil {
			return nil
		}
		var ended *EndedError
		if errors.Is(err, ErrAuthFailed) || errors.Is(err, ErrReplaced) || errors.As(err, &ended) {
			return err
		}
		if ready {
			fails = 0
		}
		d := Backoff(fails)
		fails++
		s.event(Event{Kind: "reconnecting", Attempt: attempt, Delay: d, Err: err})
		if s.sleep(ctx, d) != nil {
			return nil
		}
	}
}

func (s *Stream) active() []*Inbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Inbox
	for _, in := range s.Inboxes {
		if !s.replaced[in.Address] && s.ended[in.Address] == "" {
			out = append(out, in)
		}
	}
	return out
}

type frame struct {
	Type    string   `json:"type"`
	Nonce   string   `json:"nonce"`
	Code    string   `json:"code"`
	ErrText string   `json:"message"`
	Inboxes []string `json:"inboxes"`
	Message
}

func (s *Stream) once(ctx context.Context, attempt int) (bool, error) {
	inboxes := s.active()
	if len(inboxes) == 0 {
		return false, s.noneLeft()
	}
	byAddr := map[string]*Inbox{}
	for _, in := range inboxes {
		byAddr[strings.ToLower(in.Address)] = in
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	h := http.Header{}
	if s.Client.UserAgent != "" {
		h.Set("User-Agent", s.Client.UserAgent)
	}
	var hc *http.Client
	if s.Client.HTTP != nil {
		hc = &http.Client{Transport: s.Client.HTTP.Transport}
	}
	conn, _, err := websocket.Dial(dctx, s.Client.StreamURL(), &websocket.DialOptions{HTTPHeader: h, HTTPClient: hc})
	cancel()
	if err != nil {
		return false, fmt.Errorf("connect %s: %w", s.Client.StreamURL(), err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 20)
	idle := s.IdleTimeout
	if idle == 0 {
		idle = 90 * time.Second
	}
	read := func() (*frame, error) {
		rctx, cancel := context.WithTimeout(ctx, idle)
		defer cancel()
		_, data, err := conn.Read(rctx)
		if err != nil {
			if ctx.Err() == nil && rctx.Err() != nil {
				return nil, fmt.Errorf("no frame from the relay for %s", idle)
			}
			return nil, err
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("bad frame from the relay: %v", err)
		}
		return &f, nil
	}
	write := func(v any) error {
		b, _ := json.Marshal(v)
		wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}

	f, err := read()
	if err != nil {
		return false, err
	}
	if f.Type != "challenge" || f.Nonce == "" {
		return false, fmt.Errorf("expected a challenge from the relay, got %q", f.Type)
	}
	type authInbox struct {
		Address string `json:"address"`
		Sig     string `json:"sig"`
	}
	auth := struct {
		Type    string            `json:"type"`
		Inboxes []authInbox       `json:"inboxes"`
		Cursors map[string]uint64 `json:"cursors"`
	}{Type: "auth", Cursors: map[string]uint64{}}
	for _, in := range inboxes {
		auth.Inboxes = append(auth.Inboxes, authInbox{Address: in.Address, Sig: base64.StdEncoding.EncodeToString(in.Keys.SignAuth(f.Nonce, in.Address))})
		auth.Cursors[in.Address] = s.Receiver.Cursor(in.Address)
	}
	if err := write(auth); err != nil {
		return false, err
	}
	ready := false
	for {
		f, err := read()
		if err != nil {
			if ctx.Err() != nil {
				conn.Close(websocket.StatusNormalClosure, "")
			}
			return ready, err
		}
		switch f.Type {
		case "ready":
			ready = true
			s.event(Event{Kind: "ready", Attempt: attempt})
		case "error":
			if f.Code == "auth_failed" {
				return ready, ErrAuthFailed
			}
			if in := byAddr[strings.ToLower(f.Address)]; in != nil && endedCodes[f.Code] {
				s.mu.Lock()
				if s.ended == nil {
					s.ended = map[string]string{}
				}
				s.ended[in.Address] = f.Code
				s.mu.Unlock()
				s.event(Event{Kind: "ended", Address: in.Address, Code: f.Code})
				if len(s.active()) == 0 {
					conn.Close(websocket.StatusNormalClosure, "")
					return ready, s.noneLeft()
				}
				continue
			}
			return ready, fmt.Errorf("relay error: %s %s", f.Code, f.ErrText)
		case "ping":
			if err := write(map[string]string{"type": "pong"}); err != nil {
				return ready, err
			}
		case "replaced":
			addr := strings.ToLower(f.Address)
			s.mu.Lock()
			if s.replaced == nil {
				s.replaced = map[string]bool{}
			}
			if in := byAddr[addr]; in != nil {
				s.replaced[in.Address] = true
			}
			s.mu.Unlock()
			s.event(Event{Kind: "replaced", Address: f.Address})
			if len(s.active()) == 0 {
				conn.Close(websocket.StatusNormalClosure, "")
				return ready, s.noneLeft()
			}
		case "message":
			in := byAddr[strings.ToLower(f.Address)]
			if in == nil {
				continue
			}
			d, ok := s.Receiver.store(in, f.Message)
			if !ok {
				// Not on disk: do not ack; reconnect to have it again.
				return ready, fmt.Errorf("could not store message %d", f.Seq)
			}
			if d != nil && s.Receiver.OnMessage != nil {
				s.Receiver.OnMessage(*d)
			}
			if err := write(map[string]any{"type": "ack", "address": in.Address, "seq": f.Seq}); err != nil {
				return ready, err
			}
			s.Receiver.acked(in.Address, f.Seq)
		}
	}
}

// noneLeft is the error once no inbox is left: an EndedError when the relay
// ended one (claimed first), else ErrReplaced.
func (s *Stream) noneLeft() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var e *EndedError
	for _, in := range s.Inboxes {
		code := s.ended[in.Address]
		if code == "" {
			continue
		}
		if e == nil || (code == "claimed" && e.Code != "claimed") {
			e = &EndedError{Address: in.Address, Code: code}
		}
	}
	if e != nil {
		return e
	}
	return ErrReplaced
}
