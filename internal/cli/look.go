package cli

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/build"
	"send.io/cli/internal/out"
	"send.io/cli/internal/theme"
)

// silentExit ends a run with an exit code whose error was already printed
// (a command the home screen ran on the user's behalf).
type silentExit int

func (e silentExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// applyLook gives the printer the brand palette and decides whether it may
// animate. Animation needs a person: stdout and stderr terminals, colour on,
// not CI and not TERM=dumb (the printer adds: not --json/--jq/--quiet/agent).
func (a *App) applyLook(p *out.Printer) {
	var fallback func() bool
	if a.StdoutTTY {
		fallback = lipgloss.HasDarkBackground
	}
	p.SetTheme(theme.DetectDark(a.env, fallback), theme.TrueColor(a.env))
	p.SetMotion(a.motionAllowed())
}

func (a *App) motionAllowed() bool {
	return a.StdoutTTY && a.StderrTTY && a.env("CI") == "" && a.env("TERM") != "dumb" &&
		a.env("SENDIO_NO_MOTION") == ""
}

// huhTheme is the brand prompt theme (plain when colours are off).
func (a *App) huhTheme() *huh.Theme {
	p := a.P()
	if !p.Color {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
	return theme.Huh(p.Palette)
}

// progress is the API client's hook: a spinner on stderr while a request
// is in flight, shown only if it takes longer than a blink.
func (a *App) progress(req *api.Request) func() {
	p := a.P()
	if !p.Motion() {
		return func() {}
	}
	return p.Spin(requestLabel(req), 150*time.Millisecond).Stop
}

// requestLabel names what a request is doing, for the spinner.
func requestLabel(req *api.Request) string {
	path := req.Path
	if i := strings.Index(path, "://"); i >= 0 {
		path = path[i+3:]
		if j := strings.Index(path, "/"); j >= 0 {
			path = path[j:]
		}
	}
	path = strings.SplitN(path, "?", 2)[0]
	segs := strings.Split(strings.Trim(path, "/"), "/")
	res := ""
	for _, s := range segs {
		if s != "" && s != "v0" && s != "v1" {
			res = strings.ReplaceAll(s, "-", " ")
			break
		}
	}
	switch strings.ToUpper(req.Method) {
	case "GET", "HEAD", "":
		if res == "" {
			return "Loading"
		}
		return "Loading " + res
	case "DELETE":
		return "Deleting"
	case "PATCH", "PUT":
		return "Saving"
	default:
		switch {
		case res == "emails" || strings.HasSuffix(path, "/send") || strings.HasSuffix(path, "/test"):
			return "Sending"
		case strings.HasSuffix(path, "/verify"):
			return "Verifying"
		case len(segs) == 1:
			return "Creating"
		}
		return "Working"
	}
}

// humanError adds the next step to common failures for people. Machine
// output is untouched (this only runs in human mode).
func (a *App) humanError(e *api.Error) *api.Error {
	c := *e
	msg := strings.ToLower(e.Message)
	switch {
	case e.Code == "insufficient_credits" || e.Status == 402:
		c.Hint = "You are out of credits. Check `sendio billing balance`, then top up at " + AppURL(a.BaseURL()) + "/settings/billing"
	case e.Status == 403 && strings.Contains(msg, "domain") && strings.Contains(msg, "not") && strings.Contains(msg, "verified"):
		c.Hint = "Send from a verified domain. See `sendio domains list`, then `sendio domains verify <domain>`."
	case e.Status == 403 && c.Hint == "":
		c.Hint = "This key may not be allowed to do that. Check it with `sendio whoami`."
	case e.Status == 401 || e.Code == "not_authenticated":
		if c.Hint == "" || strings.HasPrefix(c.Hint, "Run `sendio login`") {
			c.Hint = "Run `sendio login` (or set SENDIO_API_KEY)."
		}
		if e.Code == "not_authenticated" {
			c.Message = "Not signed in."
		}
	case e.Status == 429:
		if e.RetryAfter != "" {
			c.Hint = "Slow down a little: retry in " + e.RetryAfter + "s."
		} else {
			c.Hint = "Slow down a little and retry in a moment."
		}
	case e.Status >= 500:
		if c.Hint == "" {
			c.Hint = "send.io had a problem with that request. Retry, or run `sendio doctor`."
		}
	case e.Exit == api.ExitNetwork && e.Code == "network_error":
		c.Hint = "Could not reach send.io. Check your connection or --base-url, or run `sendio doctor`."
	}
	return &c
}

var (
	helpHeading = regexp.MustCompile(`^([A-Z][A-Za-z ]*):$`)
	helpEntry   = regexp.MustCompile(`^( +)(\S(?:\S| \S)*)(\s{2,}.*)$`)
)

// styleHelp keeps cobra's help text and dresses it for a colour terminal:
// orange headings, bold command and flag names. Plain output is untouched.
func (a *App) styleHelp(root *cobra.Command) {
	def := root.HelpFunc()
	root.SetHelpFunc(func(c *cobra.Command, args []string) {
		p := a.P()
		if !p.Fancy() {
			def(c, args)
			return
		}
		var buf bytes.Buffer
		c.SetOut(&buf)
		def(c, args)
		if c == root {
			c.SetOut(a.Stdout)
		} else {
			c.SetOut(nil)
		}
		s := p.S()
		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		section := ""
		for i, l := range lines {
			if m := helpHeading.FindStringSubmatch(l); m != nil {
				section = m[1]
				lines[i] = s.Accent.Render(m[1] + ":")
				continue
			}
			if section == "" || section == "Usage" || section == "Examples" {
				continue
			}
			if m := helpEntry.FindStringSubmatch(l); m != nil {
				lines[i] = m[1] + s.Bold.Render(m[2]) + s.Dim.Render(m[3])
			}
		}
		fmt.Fprintln(a.Stdout, strings.Join(lines, "\n"))
	})
}

// printVersionLine answers -v/--version: the compact wordmark and version on
// a colour terminal, else exactly "sendio X.Y.Z".
func (a *App) printVersionLine() error {
	p := a.P()
	if !p.Fancy() {
		fmt.Fprintf(a.Stdout, "sendio %s\n", build.Version)
		return nil
	}
	s := p.S()
	meta := ""
	if build.Commit != "none" && build.Commit != "" {
		meta = " · " + build.Commit
	}
	if build.Date != "unknown" && build.Date != "" {
		meta += " · " + build.Date
	}
	fmt.Fprintln(a.Stdout, a.wordmarkChip()+"  "+s.Bold.Render(build.Version)+s.Dim.Render(meta))
	return nil
}
