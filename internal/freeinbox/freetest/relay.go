// Package freetest is an in-memory relay implementing contracts/free-inbox/PROTOCOL.md's
// HTTP and WebSocket APIs, for tests of the CLI and the MCP tools.
package freetest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"send.io/cli/internal/freeinbox"
)

// Pool is the fake pool domain.
const Pool = "sendioinbox.test"

type msg struct {
	seq        uint64
	id, at     string
	size       int
	envelope   string
	acked      bool
	inboxToken string
}

type inbox struct {
	address string
	signPub ed25519.PublicKey
	boxPub  [32]byte
	conn    *liveConn
	// status is "" (active) or "claimed".
	status string
}

type liveConn struct {
	mu   sync.Mutex
	c    *websocket.Conn
	ctx  context.Context
	kick func()
}

func (l *liveConn) send(v any) error {
	b, _ := json.Marshal(v)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.c.Write(l.ctx, websocket.MessageText, b)
}

// Relay is the fake relay.
type Relay struct {
	*httptest.Server
	mu      sync.Mutex
	inboxes map[string]*inbox
	msgs    map[string][]*msg
	seq     uint64
	chal    map[string]bool
	// Difficulty of the proof of work (default 8).
	Difficulty int
	// Taken addresses report taken in /v1/check.
	Taken map[string]bool
	// Acks counts ack frames/requests.
	Acks int
	// AuthCount counts successful WebSocket auths.
	AuthCount int
}

// New starts a fake relay.
func New() *Relay {
	r := &Relay{inboxes: map[string]*inbox{}, msgs: map[string][]*msg{}, chal: map[string]bool{}, Difficulty: 8, Taken: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": "fake"})
	})
	mux.HandleFunc("/v1/check", r.check)
	mux.HandleFunc("/v1/pow", r.pow)
	mux.HandleFunc("/v1/inboxes", r.create)
	mux.HandleFunc("/v1/messages", r.messages)
	mux.HandleFunc("/v1/ack", r.ack)
	mux.HandleFunc("/v1/stream", r.stream)
	r.Server = httptest.NewServer(mux)
	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (r *Relay) check(w http.ResponseWriter, req *http.Request) {
	name, org := freeinbox.Normalize(req.URL.Query().Get("name")), freeinbox.Normalize(req.URL.Query().Get("org"))
	addr := name + "@" + org + "." + Pool
	r.mu.Lock()
	defer r.mu.Unlock()
	if freeinbox.CheckName(name) != nil || freeinbox.CheckOrg(org) != nil {
		writeJSON(w, 200, map[string]any{"address": addr, "available": false, "reason": "invalid", "suggestions": []string{}})
		return
	}
	if r.inboxes[addr] != nil || r.Taken[addr] {
		var sug []string
		for i := 2; i <= 9 && len(sug) < 3; i++ {
			a := name + strconv.Itoa(i) + "@" + org + "." + Pool
			if r.inboxes[a] == nil && !r.Taken[a] {
				sug = append(sug, a)
			}
		}
		writeJSON(w, 200, map[string]any{"address": addr, "available": false, "reason": "taken", "suggestions": sug})
		return
	}
	writeJSON(w, 200, map[string]any{"address": addr, "available": true, "hold_token": "hold-" + name, "suggestions": []string{}})
}

func (r *Relay) pow(w http.ResponseWriter, _ *http.Request) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	c := hex.EncodeToString(b)
	r.mu.Lock()
	r.chal[c] = true
	r.mu.Unlock()
	writeJSON(w, 200, map[string]any{"challenge": c, "difficulty": r.Difficulty, "expires_at": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)})
}

func (r *Relay) create(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Address string            `json:"address"`
		SignPub string            `json:"sign_pub"`
		BoxPub  string            `json:"box_pub"`
		PoW     map[string]string `json:"pow"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.chal[body.PoW["challenge"]] || !freeinbox.VerifyPoW(body.PoW["challenge"], body.PoW["nonce"], r.Difficulty) {
		writeJSON(w, 403, map[string]any{"error": "pow_invalid", "message": "bad proof of work"})
		return
	}
	delete(r.chal, body.PoW["challenge"])
	addr := strings.ToLower(body.Address)
	if r.inboxes[addr] != nil || r.Taken[addr] {
		writeJSON(w, 409, map[string]any{"error": "taken", "message": "taken", "suggestions": []string{}})
		return
	}
	sp, _ := base64.StdEncoding.DecodeString(body.SignPub)
	bp, _ := base64.StdEncoding.DecodeString(body.BoxPub)
	if len(sp) != 32 || len(bp) != 32 {
		writeJSON(w, 400, map[string]any{"error": "invalid", "message": "bad keys"})
		return
	}
	in := &inbox{address: addr, signPub: sp}
	copy(in.boxPub[:], bp)
	r.inboxes[addr] = in
	writeJSON(w, 201, map[string]any{"address": addr, "created_at": time.Now().UTC().Format(time.RFC3339), "hold_hours": 72,
		"daily_limit": 100, "max_message_bytes": 10485760, "relay": "ws" + strings.TrimPrefix(r.URL, "http") + "/v1/stream"})
}

// Deliver seals raw for address and queues it (live connections get it now).
func (r *Relay) Deliver(address string, raw []byte) (uint64, error) {
	r.mu.Lock()
	in := r.inboxes[strings.ToLower(address)]
	if in == nil {
		r.mu.Unlock()
		return 0, fmt.Errorf("no such inbox %s", address)
	}
	env, err := freeinbox.Seal(raw, &in.boxPub)
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	r.seq++
	m := &msg{seq: r.seq, id: "m" + strconv.FormatUint(r.seq, 10), at: time.Now().UTC().Format(time.RFC3339Nano), size: len(raw), envelope: base64.StdEncoding.EncodeToString(env)}
	r.msgs[in.address] = append(r.msgs[in.address], m)
	conn := in.conn
	r.mu.Unlock()
	if conn != nil {
		_ = conn.send(frameOf(in.address, m))
	}
	return m.seq, nil
}

// Pending is the number of unacked messages for address.
func (r *Relay) Pending(address string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.msgs[strings.ToLower(address)] {
		if !m.acked {
			n++
		}
	}
	return n
}

// ClaimWithLink claims an inbox the way the relay does for a connect link
// (ADMIN-API.md "Connect links"): the signature (base64url, no padding) must
// verify "sendio-free-connect-v1:<ts>:<address>:<nonce>" with the inbox key
// and ts must be at most 30 minutes old. A live connection is told
// {"type":"error","code":"claimed"}; the key can no longer stream or poll.
func (r *Relay) ClaimWithLink(address string, ts int64, nonce, sig string) error {
	r.mu.Lock()
	in := r.inboxes[strings.ToLower(address)]
	if in == nil {
		r.mu.Unlock()
		return fmt.Errorf("no inbox %s", address)
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(in.signPub, []byte(freeinbox.ConnectString(ts, in.address, nonce)), raw) {
		r.mu.Unlock()
		return fmt.Errorf("auth_failed: bad connect signature")
	}
	if d := time.Since(time.Unix(ts, 0)); d > 30*time.Minute || d < -5*time.Minute {
		r.mu.Unlock()
		return fmt.Errorf("auth_failed: expired")
	}
	if in.status == "claimed" {
		r.mu.Unlock()
		return fmt.Errorf("link_used")
	}
	in.status = "claimed"
	lc := in.conn
	in.conn = nil
	r.mu.Unlock()
	if lc != nil {
		_ = lc.send(map[string]any{"type": "error", "code": "claimed", "address": in.address, "message": "this inbox was claimed by send.io"})
	}
	return nil
}

// DropConnections closes every live WebSocket (to test reconnects).
func (r *Relay) DropConnections() {
	r.mu.Lock()
	var kicks []func()
	for _, in := range r.inboxes {
		if in.conn != nil {
			kicks = append(kicks, in.conn.kick)
			in.conn = nil
		}
	}
	r.mu.Unlock()
	for _, k := range kicks {
		k()
	}
}

func frameOf(address string, m *msg) map[string]any {
	return map[string]any{"type": "message", "address": address, "seq": m.seq, "id": m.id, "received_at": m.at, "size": m.size, "envelope": m.envelope}
}

func (r *Relay) ackTo(address string, seq uint64) {
	r.Acks++
	for _, m := range r.msgs[address] {
		if m.seq <= seq {
			m.acked = true
		}
	}
}

func (r *Relay) verify(address, sig, msg string) *inbox {
	in := r.inboxes[strings.ToLower(address)]
	s, err := base64.StdEncoding.DecodeString(sig)
	if in == nil || err != nil || !ed25519.Verify(in.signPub, []byte(msg), s) {
		return nil
	}
	return in
}

// verifyActive is verify for the HTTP API: a claimed inbox is refused (401
// auth_failed, as on the real relay).
func (r *Relay) verifyActive(address, sig, msg string) *inbox {
	in := r.verify(address, sig, msg)
	if in == nil || in.status != "" {
		return nil
	}
	return in
}

func (r *Relay) messages(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Address string `json:"address"`
		TS      int64  `json:"ts"`
		Sig     string `json:"sig"`
		After   uint64 `json:"after"`
		Wait    int    `json:"wait"`
		Limit   int    `json:"limit"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	deadline := time.Now().Add(time.Duration(body.Wait) * time.Second)
	for {
		r.mu.Lock()
		in := r.verifyActive(body.Address, body.Sig, "sendio-free-poll-v1:"+strconv.FormatInt(body.TS, 10)+":"+body.Address)
		if in == nil {
			r.mu.Unlock()
			writeJSON(w, 401, map[string]any{"error": "auth_failed", "message": "bad signature"})
			return
		}
		out := []map[string]any{}
		for _, m := range r.msgs[in.address] {
			if !m.acked && m.seq > body.After && (body.Limit == 0 || len(out) < body.Limit) {
				f := frameOf(in.address, m)
				delete(f, "type")
				out = append(out, f)
			}
		}
		r.mu.Unlock()
		if len(out) > 0 || time.Now().After(deadline) {
			writeJSON(w, 200, map[string]any{"messages": out})
			return
		}
		select {
		case <-req.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (r *Relay) ack(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Address string `json:"address"`
		TS      int64  `json:"ts"`
		Sig     string `json:"sig"`
		Seq     uint64 `json:"seq"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	r.mu.Lock()
	defer r.mu.Unlock()
	in := r.verifyActive(body.Address, body.Sig, "sendio-free-ack-v1:"+strconv.FormatInt(body.TS, 10)+":"+body.Address+":"+strconv.FormatUint(body.Seq, 10))
	if in == nil {
		writeJSON(w, 401, map[string]any{"error": "auth_failed", "message": "bad signature"})
		return
	}
	r.ackTo(in.address, body.Seq)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (r *Relay) stream(w http.ResponseWriter, req *http.Request) {
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	defer c.CloseNow()
	lc := &liveConn{c: c, ctx: ctx, kick: func() { c.Close(websocket.StatusGoingAway, "bye") }}
	nb := make([]byte, 32)
	_, _ = rand.Read(nb)
	nonce := base64.StdEncoding.EncodeToString(nb)
	if lc.send(map[string]any{"type": "challenge", "nonce": nonce}) != nil {
		return
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		return
	}
	var auth struct {
		Inboxes []struct{ Address, Sig string }
		Cursors map[string]uint64
	}
	_ = json.Unmarshal(data, &auth)
	r.mu.Lock()
	var mine []*inbox
	var ended []map[string]any
	for _, a := range auth.Inboxes {
		in := r.verify(a.Address, a.Sig, "sendio-free-auth-v1:"+nonce+":"+a.Address)
		if in == nil {
			r.mu.Unlock()
			_ = lc.send(map[string]any{"type": "error", "code": "auth_failed"})
			c.Close(websocket.StatusPolicyViolation, "auth_failed")
			return
		}
		if in.status != "" {
			ended = append(ended, map[string]any{"type": "error", "code": in.status, "address": a.Address, "message": "this inbox was " + in.status})
			continue
		}
		mine = append(mine, in)
	}
	r.AuthCount++
	var names []string
	for _, in := range mine {
		if in.conn != nil {
			old := in.conn
			go func(addr string) { _ = old.send(map[string]any{"type": "replaced", "address": addr}) }(in.address)
		}
		in.conn = lc
		names = append(names, in.address)
	}
	lc.mu.Lock() // hold back live deliveries until the replay is sent
	r.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"type": "ready", "inboxes": names})
	_ = c.Write(ctx, websocket.MessageText, b)
	for _, e := range ended {
		b, _ := json.Marshal(e)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	if len(mine) == 0 {
		lc.mu.Unlock()
		c.Close(websocket.StatusNormalClosure, "no active inboxes")
		return
	}
	r.mu.Lock()
	var replay []map[string]any
	for _, in := range mine {
		for _, m := range r.msgs[in.address] {
			if !m.acked && m.seq > auth.Cursors[in.address] {
				replay = append(replay, frameOf(in.address, m))
			}
		}
	}
	r.mu.Unlock()
	for _, f := range replay {
		b, _ := json.Marshal(f)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	lc.mu.Unlock()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			r.mu.Lock()
			for _, in := range mine {
				if in.conn == lc {
					in.conn = nil
				}
			}
			r.mu.Unlock()
			return
		}
		var f struct {
			Type    string `json:"type"`
			Address string `json:"address"`
			Seq     uint64 `json:"seq"`
		}
		_ = json.Unmarshal(data, &f)
		if f.Type == "ack" {
			r.mu.Lock()
			r.ackTo(strings.ToLower(f.Address), f.Seq)
			r.mu.Unlock()
		}
	}
}
