// Package theme holds the send.io ("Select All") brand tokens for the
// terminal: one place for the palette, light/dark resolution, the huh prompt
// theme and the motion frames. Everything else in the CLI styles through it.
package theme

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// Brand tokens.
const (
	SignalOrange  = "#FF4A1C" // primary accent, the selection block
	TerminalBlack = "#0A0A0A"
	Cream         = "#F2EFE6"
	Graphite      = "#1C1C1C"
	MutedGrey     = "#A19F99"
	Crimson       = "#C0143C" // errors: kept well apart from orange
)

// Palette is the brand resolved for one terminal background.
type Palette struct {
	Dark bool
	// Accent is Signal Orange for text and glyphs (a touch deeper on light
	// backgrounds so it stays legible); Block is the exact brand orange used
	// as a background (the selection highlight, the logo, the ticket).
	Accent, Block lipgloss.Color
	// OnBlock is text drawn on an orange block.
	OnBlock lipgloss.Color
	// Text is the body colour. It is the terminal's own foreground (empty),
	// so cream-on-white or black-on-black can never happen when the
	// background guess is wrong; Subtle is secondary text.
	Text, Subtle lipgloss.Color
	// Error is crimson, Warn an amber that does not read as orange.
	Error, Warn lipgloss.Color
	// Rule is a quiet line colour (borders, separators).
	Rule lipgloss.Color
}

// For returns the palette for a dark or light terminal.
func For(dark bool) Palette {
	if dark {
		return Palette{Dark: true,
			Accent: SignalOrange, Block: SignalOrange, OnBlock: TerminalBlack,
			Subtle: MutedGrey,
			Error:  "#E0284F", Warn: "#E9B44C",
			Rule: "#3A3936",
		}
	}
	return Palette{
		Accent: "#E5400F", Block: SignalOrange, OnBlock: TerminalBlack,
		Subtle: "#6B6963",
		Error:  Crimson, Warn: "#8F6200",
		Rule: "#D9D5CA",
	}
}

// DetectDark decides the terminal background: SENDIO_THEME=light|dark wins,
// then COLORFGBG ("15;0" is light text on a dark ground), then fallback
// (the CLI passes Lip Gloss's answer, which Bubble Tea already asked the
// terminal for at start-up), else dark.
func DetectDark(getenv func(string) string, fallback func() bool) bool {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	switch strings.ToLower(strings.TrimSpace(getenv("SENDIO_THEME"))) {
	case "light":
		return false
	case "dark":
		return true
	}
	if v := getenv("COLORFGBG"); v != "" {
		parts := strings.Split(v, ";")
		if bg, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			// 0-6 and 8 are dark ANSI colours; 7 and 9-15 are light.
			return bg <= 6 || bg == 8
		}
	}
	if fallback != nil {
		return fallback()
	}
	return true
}

// TrueColor reports whether the terminal advertises 24-bit colour.
func TrueColor(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	ct := strings.ToLower(getenv("COLORTERM"))
	return ct == "truecolor" || ct == "24bit"
}

// Liquid is the loader: orange liquid filling the s. square, one block
// character per frame (the dashboard loader, in a terminal cell).
var Liquid = []rune("▁▂▃▄▅▆▇█")

// Huh is the prompt theme: orange title and selector, the focused option
// drawn as a text selection (black on an orange block), crimson errors.
func Huh(p Palette) *huh.Theme {
	t := huh.ThemeBase()
	s := lipgloss.NewStyle
	t.Focused.Base = t.Focused.Base.BorderForeground(p.Block)
	t.Focused.Title = s().Foreground(p.Accent).Bold(true)
	t.Focused.NoteTitle = s().Foreground(p.Accent).Bold(true).MarginBottom(1)
	t.Focused.Directory = s().Foreground(p.Accent)
	t.Focused.Description = s().Foreground(p.Subtle)
	t.Focused.ErrorIndicator = s().Foreground(p.Error).SetString(" *")
	t.Focused.ErrorMessage = s().Foreground(p.Error)
	t.Focused.SelectSelector = s().Foreground(p.Accent).SetString("▌")
	t.Focused.NextIndicator = s().Foreground(p.Accent).MarginLeft(1).SetString("→")
	t.Focused.PrevIndicator = s().Foreground(p.Accent).MarginRight(1).SetString("←")
	t.Focused.Option = s().Foreground(p.Text)
	t.Focused.MultiSelectSelector = s().Foreground(p.Accent).SetString("▌")
	t.Focused.SelectedOption = s().Foreground(p.OnBlock).Background(p.Block).Bold(true).Padding(0, 1)
	t.Focused.SelectedPrefix = s().Foreground(p.Accent).SetString("■ ")
	t.Focused.UnselectedPrefix = s().Foreground(p.Subtle).SetString("□ ")
	t.Focused.UnselectedOption = s().Foreground(p.Text).Padding(0, 1)
	t.Focused.FocusedButton = s().Padding(0, 2).MarginRight(1).Foreground(p.OnBlock).Background(p.Block).Bold(true)
	t.Focused.BlurredButton = s().Padding(0, 2).MarginRight(1).Foreground(p.Text).Background(p.Rule)
	t.Focused.Next = t.Focused.FocusedButton
	t.Focused.TextInput.Cursor = s().Foreground(p.Accent)
	t.Focused.TextInput.Placeholder = s().Foreground(p.Subtle)
	t.Focused.TextInput.Prompt = s().Foreground(p.Accent)
	t.Focused.TextInput.Text = s().Foreground(p.Text)

	t.Blurred = t.Focused
	t.Blurred.Base = t.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
	t.Blurred.Title = s().Foreground(p.Subtle).Bold(true)
	t.Blurred.SelectSelector = s().SetString(" ")
	t.Blurred.NextIndicator = s()
	t.Blurred.PrevIndicator = s()

	t.Help.ShortKey = s().Foreground(p.Subtle)
	t.Help.ShortDesc = s().Foreground(p.Subtle).Faint(true)
	t.Help.ShortSeparator = s().Foreground(p.Rule)
	t.Help.FullKey = t.Help.ShortKey
	t.Help.FullDesc = t.Help.ShortDesc
	t.Help.FullSeparator = t.Help.ShortSeparator
	return t
}
