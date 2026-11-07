package out

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"send.io/cli/internal/api"
)

func TestErrorPlainAndFancy(t *testing.T) {
	e := &api.Error{Status: 402, Code: "insufficient_credits", Message: "No credits.", Hint: "Run `sendio billing balance`."}

	var plain bytes.Buffer
	p, _ := New(&bytes.Buffer{}, &plain, false, false, false, false, false, false, "")
	p.Error(e)
	if plain.String() != "Error: No credits. (HTTP 402, insufficient_credits)\nRun `sendio billing balance`.\n" {
		t.Fatalf("plain error %q", plain.String())
	}

	var fancy bytes.Buffer
	p, _ = New(&bytes.Buffer{}, &fancy, false, false, false, true, true, true, "")
	p.SetTheme(true, true)
	p.Error(e)
	o := fancy.String()
	if !strings.Contains(o, "✗ No credits.") || !strings.Contains(o, "→") || strings.Contains(o, "`") ||
		!strings.Contains(o, "38;2;224;40;79") /* crimson */ || !strings.Contains(o, "38;2;255;73;28") /* orange command */ {
		t.Fatalf("fancy error %q", o)
	}

	var machine bytes.Buffer
	p, _ = New(&bytes.Buffer{}, &machine, true, false, false, true, true, true, "")
	p.Error(e)
	if !strings.HasPrefix(machine.String(), `{"error":{`) {
		t.Fatalf("machine error %q", machine.String())
	}
}

func TestSpinnerGating(t *testing.T) {
	var errb bytes.Buffer
	// No motion: Spin is a no-op and writes nothing.
	p, _ := New(&bytes.Buffer{}, &errb, false, false, false, true, true, true, "")
	p.SetMotion(false)
	p.Spin("Loading", 0).Stop()
	// Motion requested but stdout is not a terminal: still off.
	p2, _ := New(&bytes.Buffer{}, &errb, false, false, false, false, true, true, "")
	p2.SetMotion(true)
	if p2.Motion() {
		t.Fatal("motion with a non-TTY stdout")
	}
	// Machine mode: off.
	p3, _ := New(&bytes.Buffer{}, &errb, true, false, false, true, true, true, "")
	p3.SetMotion(true)
	if p3.Motion() {
		t.Fatal("motion in machine mode")
	}
	if errb.Len() != 0 {
		t.Fatalf("spinner wrote without motion: %q", errb.String())
	}

	// With motion it draws on stderr only and clears its line.
	var out, e2 bytes.Buffer
	p4, _ := New(&out, &e2, false, false, false, true, true, true, "")
	p4.SetMotion(true)
	s := p4.Spin("Sending", 0)
	time.Sleep(3 * SpinnerFrame)
	s.Stop()
	if out.Len() != 0 || !strings.Contains(e2.String(), "Send") || !strings.HasSuffix(e2.String(), "\r\x1b[2K\x1b[?25h") {
		t.Fatalf("stdout %q stderr %q", out.String(), e2.String())
	}
	// Only one spinner at a time.
	a := p4.Spin("A", time.Hour)
	b := p4.Spin("B", time.Hour)
	if !b.noop || a.noop {
		t.Fatal("second concurrent spinner should be a no-op")
	}
	a.Stop()
	b.Stop()
}

func TestSpinnerFrameAndMillis(t *testing.T) {
	p, _ := New(&bytes.Buffer{}, &bytes.Buffer{}, false, false, false, false, false, false, "")
	f := SpinnerFrameText(p.ES(), "Sending", 4, 1500*time.Millisecond)
	if !strings.Contains(f, "Sending…") || !strings.Contains(f, "1.5s") || !strings.ContainsAny(f, "▁▂▃▄▅▆▇█") {
		t.Fatalf("frame %q", f)
	}
	if Millis(412*time.Millisecond) != "412 ms" || Millis(1400*time.Millisecond) != "1.4 s" {
		t.Fatal("Millis")
	}
}
