package cli

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// The send.io logo in the terminal is the square mark: a Signal Orange block
// with "S." cut out of it (the cut-outs are blank cells, so they show the
// terminal's own background on dark and light themes alike), with the name,
// the title and a hint to its right. A soft diagonal shine sweeps across the
// square once when it first appears.
//
//	██████████████
//	███    ███████   send.io
//	███ ██████████   <title>
//	███    ███████   <subtitle>
//	██████ ███████
//	███    ███  ██
//	██████████████
const (
	squareW = 14
	squareH = 7
	// logoMinWidth is the narrowest terminal that gets the square with text
	// beside it; narrower ones get the square alone.
	logoMinWidth = 44
	signalOrange = "#FF4A1C"
)

// squareCut marks the cells cut out of the square: an S (rows 1-5, cols 3-6)
// and the dot (row 5, cols 9-10).
var squareCut = func() [squareH][squareW]bool {
	var c [squareH][squareW]bool
	s := [5]string{"####", "#   ", "####", "   #", "####"}
	for r, row := range s {
		for i, ch := range row {
			if ch == '#' {
				c[r+1][3+i] = true
			}
		}
	}
	c[5][9], c[5][10] = true, true
	return c
}()

// shineColor blends Signal Orange toward a warm white by k (0..1).
func shineColor(k float64) string {
	from, to := [3]float64{0xFF, 0x4A, 0x1C}, [3]float64{0xFF, 0xF4, 0xEE}
	var out [3]int
	for i := range out {
		out[i] = int(from[i] + (to[i]-from[i])*k + 0.5)
	}
	return fmt.Sprintf("#%02X%02X%02X", out[0], out[1], out[2])
}

// squareRows renders the square with the shine band centred at column pos
// (pos far outside the square = no shine).
func (a *App) squareRows(toStderr bool, pos float64) [squareH]string {
	p := a.P()
	var rows [squareH]string
	for r := 0; r < squareH; r++ {
		var b strings.Builder
		for c := 0; c < squareW; c++ {
			if squareCut[r][c] {
				b.WriteByte(' ')
				continue
			}
			k := 0.0
			if d := float64(c) - float64(r)*0.9 - pos; d > -3 && d < 3 {
				k = 0.8 * (0.5 + 0.5*math.Cos(math.Pi*d/3))
			}
			col := signalOrange
			if k > 0.01 {
				col = shineColor(k)
			}
			b.WriteString(p.Paint(toStderr, col, "█"))
		}
		rows[r] = b.String()
	}
	return rows
}

// printLogo draws the square mark for people at a terminal only: never in
// agent or JSON mode, not with --quiet. title and subtitle sit to its right.
// With animate (and motion allowed) a shine sweeps across it once, about
// 0.6 s. Without colour it is a plain line.
func (a *App) printLogo(w io.Writer, title, subtitle string, animate bool) {
	p := a.P()
	if p.Machine || p.Quiet || !p.TTY {
		return
	}
	toStderr := w == a.Stderr
	s := p.S()
	if toStderr {
		s = p.ES()
	}
	if !p.Color {
		fmt.Fprintln(w)
		fmt.Fprintln(w, " send.io  "+title)
		if subtitle != "" {
			fmt.Fprintln(w, "          "+subtitle)
		}
		fmt.Fprintln(w)
		return
	}
	var right [squareH]string
	if a.termWidth() >= logoMinWidth {
		right[2] = "   " + s.Bold.Render("send.io")
		right[3] = "   " + s.Bold.Render(title)
		if subtitle != "" {
			right[4] = "   " + p.Commands(s, subtitle)
		}
	}
	draw := func(pos float64) {
		rows := a.squareRows(toStderr, pos)
		for i, r := range rows {
			fmt.Fprintln(w, strings.TrimRight(" "+r+right[i], " "))
		}
	}
	fmt.Fprintln(w)
	if animate && p.Motion() {
		const frames = 18
		for f := 0; f <= frames; f++ {
			// ease-in-out across the square plus a margin on both sides
			t := float64(f) / frames
			e := t * t * (3 - 2*t)
			draw(-6 + e*(squareW+12))
			time.Sleep(32 * time.Millisecond)
			fmt.Fprintf(w, "\x1b[%dA", squareH)
		}
	}
	draw(-999)
	fmt.Fprintln(w)
}

// wordmarkChip is the one-line wordmark: " send " on orange, then ".io".
func (a *App) wordmarkChip() string {
	s := a.P().S()
	return s.Select.Render(" send ") + s.Bold.Render(".io")
}

// ticket renders a credit amount on the send ticket: an orange ticket with
// a notch bitten out of each end and a tear line before the unit. The
// notches are drawn in reverse video so they take the terminal's own
// background colour, whatever it is.
func (a *App) ticket(amount string) string {
	p := a.P()
	s := p.S()
	if !p.Fancy() {
		return amount + " credits"
	}
	notch := s.Select.UnsetBackground().UnsetBold().Foreground(p.Palette.Block).Reverse(true)
	body := s.Select
	return notch.Render("◗") + body.Render(" "+amount+" ") + body.UnsetBold().Render("┆ credits ") + notch.Render("◖")
}

// termWidth is the terminal width in columns (COLUMNS wins; 80 when unknown).
func (a *App) termWidth() int {
	if n, err := strconv.Atoi(strings.TrimSpace(a.env("COLUMNS"))); err == nil && n > 0 {
		return n
	}
	if f, ok := a.Stdout.(*os.File); ok {
		if n, _, err := term.GetSize(int(f.Fd())); err == nil && n > 0 {
			return n
		}
	}
	return 80
}
