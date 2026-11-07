// Package out renders command results: readable tables and colours for
// humans, JSON (optionally filtered with --jq) for agents and scripts, and
// the structured error envelope on stderr.
package out

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/itchyny/gojq"
	"github.com/muesli/termenv"

	"send.io/cli/internal/api"
	"send.io/cli/internal/theme"
)

// Printer writes results and diagnostics.
type Printer struct {
	Out, Err io.Writer
	// Machine selects JSON output (agent mode, --json or --jq).
	Machine bool
	// Agent is agent mode (implies Machine).
	Agent bool
	Quiet bool
	// TTY is whether stdout is a terminal (pretty JSON, colours).
	TTY    bool
	ErrTTY bool
	Color  bool
	// Palette is the brand palette for this terminal's background.
	Palette theme.Palette
	// motion allows animations (spinners) on stderr; see SetMotion.
	motion  bool
	spin    *Spinner
	mu      sync.Mutex
	jq      *gojq.Code
	jqExpr  string
	r       *lipgloss.Renderer
	er      *lipgloss.Renderer
	styles  Styles
	estyles Styles
}

// Styles are the brand styles used in human output (see package theme).
type Styles struct {
	// Bold is emphasis in the body colour; Dim is secondary (muted grey) text.
	Bold, Dim lipgloss.Style
	// OK marks success (Signal Orange), Err failures (crimson), Warn
	// warnings (amber) and Link URLs.
	OK, Err, Warn, Link lipgloss.Style
	// Header is table headings.
	Header lipgloss.Style
	// Accent is bold orange; Select is the brand selection (black on an
	// orange block); Text is the plain body colour.
	Accent, Select, Text lipgloss.Style
}

func newStyles(r *lipgloss.Renderer, pal theme.Palette) Styles {
	return Styles{
		Bold:   r.NewStyle().Bold(true).Foreground(pal.Text),
		Dim:    r.NewStyle().Foreground(pal.Subtle),
		OK:     r.NewStyle().Foreground(pal.Accent),
		Err:    r.NewStyle().Foreground(pal.Error).Bold(true),
		Warn:   r.NewStyle().Foreground(pal.Warn),
		Link:   r.NewStyle().Foreground(pal.Accent).Underline(true),
		Header: r.NewStyle().Bold(true).Foreground(pal.Subtle),
		Accent: r.NewStyle().Foreground(pal.Accent).Bold(true),
		Select: r.NewStyle().Foreground(pal.OnBlock).Background(pal.Block).Bold(true),
		Text:   r.NewStyle().Foreground(pal.Text),
	}
}

// New builds a printer. jqExpr is compiled here; a bad expression is a usage error.
func New(stdout, stderr io.Writer, machine, agent, quiet, tty, errTTY, color bool, jqExpr string) (*Printer, error) {
	p := &Printer{Out: stdout, Err: stderr, Machine: machine || agent || jqExpr != "", Agent: agent, Quiet: quiet, TTY: tty, ErrTTY: errTTY, Color: color, jqExpr: jqExpr}
	p.r = lipgloss.NewRenderer(stdout)
	p.er = lipgloss.NewRenderer(stderr)
	p.SetTheme(true, false)
	if jqExpr != "" {
		q, err := gojq.Parse(jqExpr)
		if err != nil {
			return nil, api.UsageError("invalid --jq expression: %v", err)
		}
		code, err := gojq.Compile(q)
		if err != nil {
			return nil, api.UsageError("invalid --jq expression: %v", err)
		}
		p.jq = code
	}
	return p, nil
}

// SetTheme resolves the brand palette for a dark or light terminal and picks
// the colour depth (24-bit when the terminal says so, else 256 colours).
// Without colour, or off a terminal, every style renders as plain text.
func (p *Printer) SetTheme(dark, trueColor bool) {
	p.Palette = theme.For(dark)
	prof := termenv.ANSI256
	if trueColor {
		prof = termenv.TrueColor
	}
	for _, x := range []struct {
		r   *lipgloss.Renderer
		tty bool
	}{{p.r, p.TTY}, {p.er, p.ErrTTY}} {
		x.r.SetHasDarkBackground(dark)
		if p.Color && x.tty {
			x.r.SetColorProfile(prof)
		} else {
			x.r.SetColorProfile(termenv.Ascii)
		}
	}
	p.styles, p.estyles = newStyles(p.r, p.Palette), newStyles(p.er, p.Palette)
}

// Fancy reports whether stdout gets the branded look (a colour terminal in
// human mode). Plain output is the same text without decoration.
func (p *Printer) Fancy() bool { return !p.Machine && p.TTY && p.Color }

// ErrFancy is Fancy for stderr.
func (p *Printer) ErrFancy() bool { return !p.Machine && p.ErrTTY && p.Color }

// S returns the stdout styles.
func (p *Printer) S() Styles { return p.styles }

// ES returns the stderr styles.
func (p *Printer) ES() Styles { return p.estyles }

// Result prints v: JSON (through --jq) in machine mode, else human() (or
// indented JSON when human is nil).
func (p *Printer) Result(v any, human func(io.Writer)) error {
	if p.Machine || human == nil {
		return p.JSON(v)
	}
	human(p.Out)
	return nil
}

// JSON prints v as indented JSON, through --jq when set.
func (p *Printer) JSON(v any) error {
	if p.jq != nil {
		return p.runJQ(v, false)
	}
	b, err := MarshalIndent(v)
	if err != nil {
		return err
	}
	_, err = p.Out.Write(append(b, '\n'))
	return err
}

// Line prints v as one compact JSON line (NDJSON), through --jq when set.
func (p *Printer) Line(v any) error {
	if p.jq != nil {
		return p.runJQ(v, true)
	}
	b, err := Marshal(v)
	if err != nil {
		return err
	}
	_, err = p.Out.Write(append(b, '\n'))
	return err
}

func (p *Printer) runJQ(v any, compact bool) error {
	iter := p.jq.Run(Normalize(v))
	for {
		x, ok := iter.Next()
		if !ok {
			return nil
		}
		if err, isErr := x.(error); isErr {
			if he, ok := err.(*gojq.HaltError); ok && he.Value() == nil {
				return nil
			}
			return api.UsageError("--jq: %v", err)
		}
		if s, isStr := x.(string); isStr {
			fmt.Fprintln(p.Out, s)
			continue
		}
		var b []byte
		var err error
		if compact || !p.TTY {
			b, err = Marshal(x)
		} else {
			b, err = MarshalIndent(x)
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(p.Out, string(b))
	}
}

// Marshal is json.Marshal without HTML escaping.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MarshalIndent is json.MarshalIndent without HTML escaping.
func MarshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Normalize converts any JSON-able value to the plain types gojq accepts.
func Normalize(v any) any {
	switch x := v.(type) {
	case nil, bool, string, int, float64:
		return x
	case json.Number:
		if i, err := x.Int64(); err == nil && i <= math.MaxInt && i >= math.MinInt {
			return int(i)
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = Normalize(e)
		}
		return m
	case []any:
		l := make([]any, len(x))
		for i, e := range x {
			l[i] = Normalize(e)
		}
		return l
	case json.RawMessage:
		var y any
		d := json.NewDecoder(bytes.NewReader(x))
		d.UseNumber()
		if d.Decode(&y) != nil {
			return string(x)
		}
		return Normalize(y)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return Normalize(json.RawMessage(b))
	}
}

// Error prints a failure: the {"error":{code,message,status}} envelope in
// machine mode, a red line (and hint) for humans. Always on stderr.
func (p *Printer) Error(e *api.Error) {
	if p.Machine {
		env := map[string]any{"code": e.Code, "message": e.Message}
		if e.Status != 0 {
			env["status"] = e.Status
		} else {
			env["status"] = nil
		}
		if e.Hint != "" {
			env["hint"] = e.Hint
		}
		if e.RetryAfter != "" {
			env["retry_after"] = e.RetryAfter
		}
		b, _ := Marshal(map[string]any{"error": env})
		fmt.Fprintln(p.Err, string(b))
		return
	}
	s := p.estyles
	if !p.ErrFancy() {
		// Plain: the classic one-liner, unchanged for log scrapers.
		line := "Error: " + e.Message
		if e.Status != 0 {
			line += fmt.Sprintf(" (HTTP %d, %s)", e.Status, e.Code)
		}
		fmt.Fprintln(p.Err, line)
		if e.Hint != "" {
			fmt.Fprintln(p.Err, e.Hint)
		}
		return
	}
	line := s.Err.Render("✗ " + e.Message)
	if e.Status != 0 {
		line += s.Dim.Render(fmt.Sprintf("  HTTP %d · %s", e.Status, e.Code))
	}
	fmt.Fprintln(p.Err, line)
	if e.Hint != "" {
		fmt.Fprintln(p.Err, s.Dim.Render("  → ")+p.Commands(s, e.Hint))
	}
}

// Commands renders a hint for people: `backticked` commands stand out in
// orange, the rest is muted. Plain styles leave the text as is.
func (p *Printer) Commands(s Styles, text string) string {
	parts := strings.Split(text, "`")
	var b strings.Builder
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i%2 == 1 && i < len(parts)-1 {
			b.WriteString(s.Accent.Render(part))
		} else if i%2 == 1 {
			b.WriteString(s.Dim.Render("`" + part))
		} else {
			b.WriteString(s.Dim.Render(part))
		}
	}
	return b.String()
}

// Info prints a status line on stderr for humans (silent in machine/quiet mode).
func (p *Printer) Info(format string, a ...any) {
	if p.Machine || p.Quiet {
		return
	}
	fmt.Fprintf(p.Err, format+"\n", a...)
}

// Warn prints a warning on stderr (also in machine mode, unless quiet).
func (p *Printer) Warn(format string, a ...any) {
	if p.Quiet {
		return
	}
	msg := fmt.Sprintf(format, a...)
	if p.Machine {
		b, _ := Marshal(map[string]any{"warning": msg})
		fmt.Fprintln(p.Err, string(b))
		return
	}
	if p.ErrFancy() {
		fmt.Fprintln(p.Err, p.estyles.Warn.Render("! ")+msg)
		return
	}
	fmt.Fprintln(p.Err, p.estyles.Warn.Render("Warning:")+" "+msg)
}

// Success prints a green check line on stdout for humans.
func (p *Printer) Success(format string, a ...any) {
	if p.Machine || p.Quiet {
		return
	}
	fmt.Fprintln(p.Out, p.styles.OK.Render("✓")+" "+fmt.Sprintf(format, a...))
}

// Table renders rows under headers with aligned columns.
func (p *Printer) Table(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = lipgloss.Width(h)
	}
	for _, r := range rows {
		for i := range headers {
			if i < len(r) {
				if wd := lipgloss.Width(r[i]); wd > widths[i] {
					widths[i] = wd
				}
			}
		}
	}
	pad := func(s string, n int) string {
		if d := n - lipgloss.Width(s); d > 0 {
			return s + strings.Repeat(" ", d)
		}
		return s
	}
	var hb strings.Builder
	for i, h := range headers {
		if i > 0 {
			hb.WriteString("  ")
		}
		if i == len(headers)-1 {
			hb.WriteString(strings.ToUpper(h))
		} else {
			hb.WriteString(pad(strings.ToUpper(h), widths[i]))
		}
	}
	fmt.Fprintln(w, p.styles.Header.Render(hb.String()))
	for _, r := range rows {
		var b strings.Builder
		for i := range headers {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			if i > 0 {
				b.WriteString("  ")
			}
			if i == len(headers)-1 {
				b.WriteString(cell)
			} else {
				b.WriteString(pad(cell, widths[i]))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

// KV renders key/value pairs (a single object) for humans.
func (p *Printer) KV(w io.Writer, m map[string]any, order []string) {
	keys := order
	if len(keys) == 0 {
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	width := 0
	for _, k := range keys {
		if _, ok := m[k]; ok && len(k) > width {
			width = len(k)
		}
	}
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		fmt.Fprintf(w, "%s  %s\n", p.styles.Dim.Render(fmt.Sprintf("%-*s", width, k)), Cell(v, 0))
	}
}

// Cell renders a JSON value in a table cell; max > 0 truncates.
func Cell(v any, max int) string {
	var s string
	switch x := v.(type) {
	case nil:
		s = "-"
	case string:
		s = x
		if s == "" {
			s = "-"
		}
	case bool, float64, int, json.Number:
		s = fmt.Sprint(x)
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			if _, scalar := e.(map[string]any); scalar {
				b, _ := Marshal(e)
				parts = append(parts, string(b))
			} else {
				parts = append(parts, Cell(e, 0))
			}
		}
		s = strings.Join(parts, ", ")
	default:
		b, _ := Marshal(x)
		s = string(b)
	}
	if s == "" {
		s = "-"
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	if max > 0 && len([]rune(s)) > max {
		s = string([]rune(s)[:max-1]) + "…"
	}
	return s
}

// HasJQ reports whether a --jq filter is set.
func (p *Printer) HasJQ() bool { return p.jq != nil }

// Paint renders text in an exact colour (hex "#RRGGBB") through the stdout
// (or stderr) renderer, so it degrades like every other style.
func (p *Printer) Paint(toStderr bool, hex, text string) string {
	r := p.r
	if toStderr {
		r = p.er
	}
	return r.NewStyle().Foreground(lipgloss.Color(hex)).Render(text)
}
