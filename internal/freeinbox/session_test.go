package freeinbox_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"send.io/cli/internal/freeinbox"
	"send.io/cli/internal/freeinbox/freetest"
)

func otpMail(to, code string) []byte {
	return []byte(fmt.Sprintf("From: Acme <no-reply@acme.test>\r\nTo: %s\r\nSubject: Verify your email\r\n\r\nYour verification code is %s.\r\n", to, code))
}

func setup(t *testing.T) (*freetest.Relay, *freeinbox.Client, *freeinbox.Store, *freeinbox.Inbox) {
	t.Helper()
	r := freetest.New()
	t.Cleanup(r.Close)
	c := &freeinbox.Client{Relay: r.URL}
	st := &freeinbox.Store{Root: t.TempDir()}
	res, err := freeinbox.Create(context.Background(), c, st, freeinbox.CreateOptions{Name: "Agent", Org: "Org"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Address != "agent@org."+freetest.Pool || res.RecoveryPhrase == "" {
		t.Fatalf("created %+v", res)
	}
	in, err := st.OpenInbox(res.Address)
	if err != nil {
		t.Fatal(err)
	}
	return r, c, st, in
}

func TestCreateTaken(t *testing.T) {
	r, c, st, in := setup(t)
	_, err := freeinbox.Create(context.Background(), c, st, freeinbox.CreateOptions{Name: "agent", Org: "org"})
	var te *freeinbox.TakenError
	if !errors.As(err, &te) || te.Reason != "taken" || len(te.Suggestions) != 3 || te.Suggestions[0] != "agent2@org."+freetest.Pool {
		t.Fatalf("err = %v", err)
	}
	// Picking a suggestion creates it.
	res, err := freeinbox.Create(context.Background(), c, st, freeinbox.CreateOptions{Address: te.Suggestions[0]})
	if err != nil || res.Address != te.Suggestions[0] {
		t.Fatalf("%v %+v", err, res)
	}
	_ = r
	_ = in
}

func TestStreamReplayReconnectNoDuplicates(t *testing.T) {
	r, c, st, in := setup(t)
	// Two messages wait before the client connects.
	r.Deliver(in.Address, otpMail(in.Address, "111111"))
	r.Deliver(in.Address, otpMail(in.Address, "222222"))

	var mu sync.Mutex
	var got []uint64
	var newCount int
	recv := &freeinbox.Receiver{Store: st, OnMessage: func(d freeinbox.Delivered) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, d.Index.Seq)
		if d.New {
			newCount++
		}
	}}
	ready := make(chan struct{}, 10)
	s := &freeinbox.Stream{Client: c, Receiver: recv, Inboxes: []*freeinbox.Inbox{in},
		Sleep: func(ctx context.Context, d time.Duration) error { return nil },
		OnEvent: func(e freeinbox.Event) {
			if e.Kind == "ready" {
				ready <- struct{}{}
			}
		}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-ready
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return newCount == 2 })
	waitFor(t, func() bool { return r.Pending(in.Address) == 0 })

	r.Deliver(in.Address, otpMail(in.Address, "333333"))
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return newCount == 3 })
	waitFor(t, func() bool { return r.Pending(in.Address) == 0 })

	r.DropConnections()
	<-ready // reconnected
	r.Deliver(in.Address, otpMail(in.Address, "444444"))
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return newCount == 4 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(got) != "[1 2 3 4]" {
		t.Fatalf("delivered %v", got)
	}
	stt, _ := st.LoadState(in.Address)
	if stt.Cursor != 4 {
		t.Fatalf("cursor %d", stt.Cursor)
	}
	mail, _ := st.Mail(in.Address)
	if len(mail) != 4 || mail[0].Subject != "Verify your email" {
		t.Fatalf("mail %v", mail)
	}
}

func TestPollAndAck(t *testing.T) {
	r, c, st, in := setup(t)
	r.Deliver(in.Address, otpMail(in.Address, "123456"))
	recv := &freeinbox.Receiver{Store: st}
	ds, err := recv.PollOnce(context.Background(), c, in, 0, 100)
	if err != nil || len(ds) != 1 || !ds[0].New {
		t.Fatalf("%v %v", err, ds)
	}
	if r.Pending(in.Address) != 0 {
		t.Fatal("not acked")
	}
	// Long poll returns when mail arrives.
	go func() { time.Sleep(200 * time.Millisecond); r.Deliver(in.Address, otpMail(in.Address, "654321")) }()
	ds, err = recv.PollOnce(context.Background(), c, in, 5, 100)
	if err != nil || len(ds) != 1 || ds[0].Index.Seq != 2 {
		t.Fatalf("%v %v", err, ds)
	}
}

func TestWaitCode(t *testing.T) {
	r, c, st, in := setup(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		r.Deliver(in.Address, []byte("From: other@x.test\r\nSubject: hi\r\n\r\nno code\r\n"))
		r.Deliver(in.Address, otpMail(in.Address, "987654"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m, err := freeinbox.Wait(ctx, c, st, in, freeinbox.WaitOptions{From: "acme", Code: true, Settle: 50 * time.Millisecond}, nil, nil)
	if err != nil || m.Code != "987654" {
		t.Fatalf("%v %+v", err, m)
	}
	// Timeout.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if _, err := freeinbox.Wait(ctx2, c, st, in, freeinbox.WaitOptions{Code: true}, nil, nil); !errors.Is(err, freeinbox.ErrWaitTimeout) {
		t.Fatalf("err = %v", err)
	}
	// --since finds stored mail.
	m, err = freeinbox.Wait(context.Background(), c, st, in, freeinbox.WaitOptions{Code: true, Since: time.Hour}, nil, nil)
	if err != nil || m.Code != "987654" {
		t.Fatalf("since: %v %+v", err, m)
	}
}

func TestRecover(t *testing.T) {
	r, c, _, in := setup(t)
	phrase := freeinbox.RecoveryPhrase(in.Keys.Seed)
	st2 := &freeinbox.Store{Root: t.TempDir()}
	kf, err := freeinbox.Recover(context.Background(), c, st2, phrase, in.Address)
	if err != nil || kf.Address != in.Address {
		t.Fatal(err)
	}
	other, _ := freeinbox.NewKeys()
	if _, err := freeinbox.Recover(context.Background(), c, st2, freeinbox.RecoveryPhrase(other.Seed), in.Address); err == nil {
		t.Fatal("recovered with a wrong phrase")
	}
	_ = r
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// claim attaches in to a workspace through a fresh connect link, as the
// backend does after the user opens it.
func claim(t *testing.T, r *freetest.Relay, in *freeinbox.Inbox) {
	t.Helper()
	cl, err := freeinbox.NewConnectLink("", in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ClaimWithLink(cl.Address, cl.TS, cl.Nonce, cl.Sig); err != nil {
		t.Fatal(err)
	}
}

func TestStreamClaimedStops(t *testing.T) {
	r, c, st, in := setup(t)
	var mu sync.Mutex
	var events []freeinbox.Event
	ready := make(chan struct{}, 10)
	reconnects := 0
	s := &freeinbox.Stream{Client: c, Receiver: &freeinbox.Receiver{Store: st}, Inboxes: []*freeinbox.Inbox{in},
		Sleep: func(ctx context.Context, d time.Duration) error { return nil },
		OnEvent: func(e freeinbox.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, e)
			switch e.Kind {
			case "ready":
				ready <- struct{}{}
			case "reconnecting":
				reconnects++
			}
		}}
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	<-ready
	claim(t, r, in)
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream kept running after the inbox was claimed")
	}
	var ee *freeinbox.EndedError
	if !errors.Is(err, freeinbox.ErrClaimed) || !errors.As(err, &ee) || ee.Address != in.Address || ee.Code != "claimed" {
		t.Fatalf("err = %v", err)
	}
	if want := in.Address + " was moved into a send.io workspace; read it in the dashboard"; err.Error() != want {
		t.Fatalf("message %q", err.Error())
	}
	mu.Lock()
	defer mu.Unlock()
	if reconnects != 0 {
		t.Fatalf("%d reconnects", reconnects)
	}
	last := events[len(events)-1]
	if last.Kind != "ended" || last.Code != "claimed" || last.Address != in.Address {
		t.Fatalf("events %+v", events)
	}

	// A new stream for the claimed inbox ends at once, without retrying.
	s2 := &freeinbox.Stream{Client: c, Receiver: &freeinbox.Receiver{Store: st}, Inboxes: []*freeinbox.Inbox{in},
		Sleep: func(ctx context.Context, d time.Duration) error { t.Error("retried"); return nil }}
	if err := s2.Run(context.Background()); !errors.Is(err, freeinbox.ErrClaimed) {
		t.Fatalf("second stream: %v", err)
	}
	// Wait (wait, wait_for_email) reports it too.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := freeinbox.Wait(ctx, c, st, in, freeinbox.WaitOptions{}, nil, nil); !errors.Is(err, freeinbox.ErrClaimed) {
		t.Fatalf("wait: %v", err)
	}
	// Long poll only says auth_failed; the stream probe says why.
	if _, err := (&freeinbox.Receiver{Store: st}).PollOnce(context.Background(), c, in, 0, 10); err == nil {
		t.Fatal("poll of a claimed inbox succeeded")
	}
	if code, err := freeinbox.InboxStatus(context.Background(), c, in); err != nil || code != "claimed" {
		t.Fatalf("status %q %v", code, err)
	}
	// The link works once.
	cl, _ := freeinbox.NewConnectLink("", in, time.Now())
	if err := r.ClaimWithLink(cl.Address, cl.TS, cl.Nonce, cl.Sig); err == nil {
		t.Fatal("claimed twice")
	}
}

func TestStreamClaimOneOfTwo(t *testing.T) {
	r, c, st, in := setup(t)
	res, err := freeinbox.Create(context.Background(), c, st, freeinbox.CreateOptions{Name: "second", Org: "org"})
	if err != nil {
		t.Fatal(err)
	}
	in2, _ := st.OpenInbox(res.Address)
	if code, err := freeinbox.InboxStatus(context.Background(), c, in2); err != nil || code != "active" {
		t.Fatalf("status %q %v", code, err)
	}
	got := make(chan uint64, 10)
	ready := make(chan struct{}, 10)
	ended := make(chan freeinbox.Event, 10)
	s := &freeinbox.Stream{Client: c, Receiver: &freeinbox.Receiver{Store: st, OnMessage: func(d freeinbox.Delivered) { got <- d.Index.Seq }},
		Inboxes: []*freeinbox.Inbox{in, in2}, Sleep: func(ctx context.Context, d time.Duration) error { return nil },
		OnEvent: func(e freeinbox.Event) {
			switch e.Kind {
			case "ready":
				ready <- struct{}{}
			case "ended":
				ended <- e
			}
		}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-ready
	claim(t, r, in)
	if e := <-ended; e.Address != in.Address || e.Code != "claimed" {
		t.Fatalf("event %+v", e)
	}
	// The other inbox keeps receiving.
	r.Deliver(in2.Address, otpMail(in2.Address, "121212"))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("second inbox stopped receiving")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
