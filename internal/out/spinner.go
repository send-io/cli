package out

import (
	"fmt"
	"strings"
	"time"

	"send.io/cli/internal/theme"
)

// SpinnerFrame is how often the spinner redraws (about 11 frames a second:
// alive, not busy).
const SpinnerFrame = 90 * time.Millisecond

// SetMotion allows or forbids animation. The CLI turns it on only for a
// person at a colour terminal (stdout and stderr TTYs, not CI, not
// TERM=dumb, no --json/--quiet); spinners are always drawn on stderr.
func (p *Printer) SetMotion(on bool) {
	p.motion = on && !p.Machine && !p.Quiet && p.TTY && p.ErrTTY && p.Color
}

// Motion reports whether animations are allowed.
func (p *Printer) Motion() bool { return p.motion }

// Spinner is the brand loader on stderr: orange liquid rising in a small
// square, and an orange selection sweeping across the label.
type Spinner struct {
	p     *Printer
	label string
	start time.Time
	stop  chan struct{}
	done  chan struct{}
	noop  bool
}

// Spin starts a spinner that appears after delay (so fast calls never
// flicker). It is a no-op without motion or while another spinner runs.
// Always call Stop.
func (p *Printer) Spin(label string, delay time.Duration) *Spinner {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.motion || p.spin != nil {
		return &Spinner{noop: true}
	}
	s := &Spinner{p: p, label: label, start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	p.spin = s
	go s.loop(delay)
	return s
}

func (s *Spinner) loop(delay time.Duration) {
	defer close(s.done)
	t := time.NewTimer(delay)
	select {
	case <-s.stop:
		t.Stop()
		return
	case <-t.C:
	}
	tick := time.NewTicker(SpinnerFrame)
	defer tick.Stop()
	fmt.Fprint(s.p.Err, "\x1b[?25l") // hide the cursor while drawing
	for i := 0; ; i++ {
		fmt.Fprint(s.p.Err, "\r"+s.frame(i, time.Since(s.start))+"\x1b[K")
		select {
		case <-s.stop:
			fmt.Fprint(s.p.Err, "\r\x1b[2K\x1b[?25h")
			return
		case <-tick.C:
		}
	}
}

// frame renders frame i: "▕▃▄▏ Sending…  1.2s".
func (s *Spinner) frame(i int, elapsed time.Duration) string {
	return SpinnerFrameText(s.p.estyles, s.label, i, elapsed)
}

// SpinnerFrameText renders one spinner frame with the given styles; split
// out so the look can be tested and screenshotted without a terminal.
func SpinnerFrameText(st Styles, label string, i int, elapsed time.Duration) string {
	n := len(theme.Liquid)
	// Two cells of liquid, a step apart, rise and drain like a wave.
	level := func(k int) rune {
		c := k % (2 * n)
		if c >= n {
			c = 2*n - 1 - c
		}
		return theme.Liquid[c]
	}
	glass := st.Dim.Render("▕") + st.OK.Render(string([]rune{level(i), level(i + 2)})) + st.Dim.Render("▏")

	// The selection: a three-letter orange block gliding over the label.
	r := []rune(label)
	var b strings.Builder
	if len(r) > 0 {
		span := 3
		pos := i % (len(r) + span)
		lo, hi := pos-span, pos
		if lo < 0 {
			lo = 0
		}
		if hi > len(r) {
			hi = len(r)
		}
		b.WriteString(st.Text.Render(string(r[:lo])))
		if hi > lo {
			b.WriteString(st.Select.Render(string(r[lo:hi])))
		}
		b.WriteString(st.Text.Render(string(r[hi:])))
	}
	b.WriteString(st.Dim.Render("…"))
	if elapsed >= time.Second {
		b.WriteString(st.Dim.Render(fmt.Sprintf("  %.1fs", elapsed.Seconds())))
	}
	return glass + " " + b.String()
}

// Stop clears the spinner line and waits for the drawing goroutine.
func (s *Spinner) Stop() {
	if s == nil || s.noop {
		return
	}
	select {
	case <-s.stop:
		return
	default:
		close(s.stop)
	}
	<-s.done
	s.p.mu.Lock()
	if s.p.spin == s {
		s.p.spin = nil
	}
	s.p.mu.Unlock()
}

// Elapsed is the time since the spinner started.
func (s *Spinner) Elapsed() time.Duration {
	if s == nil || s.noop {
		return 0
	}
	return time.Since(s.start)
}

// Millis formats a duration for people: "412 ms", "1.4 s".
func Millis(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1f s", d.Seconds())
}
