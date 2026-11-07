package freeinbox

import (
	"context"
	"errors"
	"strings"
	"time"
)

// WaitOptions filter the message Wait returns.
type WaitOptions struct {
	// From and Subject are case-insensitive substrings of those headers.
	From, Subject string
	// Code requires a 6-8 digit code in the message (and returns it).
	Code bool
	// Since also accepts mail already stored locally that arrived within
	// this window, and ignores anything older. Zero: only mail delivered by
	// the relay during the wait (mail not yet on this machine).
	Since time.Duration
	// Settle is how long to keep reading after the first match so a burst
	// of replayed messages resolves to the newest match (default 750 ms).
	Settle time.Duration
	// Now is the clock (nil: time.Now).
	Now func() time.Time
}

// ErrWaitTimeout is returned when no message matched in time.
var ErrWaitTimeout = errors.New("no matching email arrived before the timeout")

// Match is a message Wait found.
type Match struct {
	Delivered
	Code string
}

func (o *WaitOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// matches applies the filters to an index; it returns the code (when
// Code) and whether the message matches.
func (o *WaitOptions) matches(idx *MailIndex, raw []byte, since time.Time) (string, bool) {
	if idx == nil {
		return "", false
	}
	if !since.IsZero() {
		if t, err := time.Parse(time.RFC3339Nano, idx.ReceivedAt); err == nil && t.Before(since) {
			return "", false
		}
	}
	if o.From != "" && !strings.Contains(strings.ToLower(idx.From), strings.ToLower(o.From)) {
		return "", false
	}
	if o.Subject != "" && !strings.Contains(strings.ToLower(idx.Subject), strings.ToLower(o.Subject)) {
		return "", false
	}
	if !o.Code {
		return "", true
	}
	p := Parse(raw)
	code := ExtractCode(idx.Subject, p.Text)
	return code, code != ""
}

// Wait returns the newest matching message delivered to in before ctx ends
// (a context deadline is the timeout: ErrWaitTimeout). Every message that
// arrives meanwhile is stored and acked as usual. onEvent (optional) sees
// the connection events; onWarn (optional) non-fatal problems.
func Wait(ctx context.Context, c *Client, st *Store, in *Inbox, opt WaitOptions, onEvent func(Event), onWarn func(string, ...any)) (*Match, error) {
	settle := opt.Settle
	if settle == 0 {
		settle = 750 * time.Millisecond
	}
	var since time.Time
	if opt.Since > 0 {
		since = opt.now().Add(-opt.Since)
		// Mail already here.
		list, _ := st.Mail(in.Address)
		for _, idx := range list { // newest first
			raw, err := st.RawMail(in.Address, idx.Seq)
			if err != nil {
				continue
			}
			if code, ok := opt.matches(idx, raw, since); ok {
				return &Match{Delivered: Delivered{Address: in.Address, Index: idx, Raw: raw, Path: st.EMLPath(in.Address, idx.Seq)}, Code: code}, nil
			}
		}
	}
	found := make(chan Match, 64)
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	recv := &Receiver{Store: st, OnWarn: onWarn, OnMessage: func(d Delivered) {
		if !d.New {
			return
		}
		if code, ok := opt.matches(d.Index, d.Raw, since); ok {
			select {
			case found <- Match{Delivered: d, Code: code}:
			default:
			}
		}
	}}
	s := &Stream{Client: c, Receiver: recv, Inboxes: []*Inbox{in}, OnEvent: onEvent}
	done := make(chan error, 1)
	go func() { done <- s.Run(rctx) }()

	var best *Match
	var settleC <-chan time.Time
	for {
		select {
		case m := <-found:
			if best == nil || m.Index.Seq > best.Index.Seq {
				best = &m
			}
			if settleC == nil {
				settleC = time.After(settle)
			}
		case <-settleC:
			cancel()
			<-done
			return best, nil
		case err := <-done:
			if best != nil {
				return best, nil
			}
			if ctx.Err() != nil {
				return nil, ErrWaitTimeout
			}
			if err == nil {
				err = ErrWaitTimeout
			}
			return nil, err
		case <-ctx.Done():
			cancel()
			<-done
			// Drain matches that raced the deadline.
		drain:
			for {
				select {
				case m := <-found:
					if best == nil || m.Index.Seq > best.Index.Seq {
						best = &m
					}
				default:
					break drain
				}
			}
			if best != nil {
				return best, nil
			}
			return nil, ErrWaitTimeout
		}
	}
}
