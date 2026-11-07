package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/build"
)

// homeAction is one entry of the home screen menu. Every entry is a plain
// command, shown as you move so it is easy to remember for next time.
type homeAction struct {
	label string
	args  []string
	// form asks for input first and returns the final args.
	form func() ([]string, error)
}

// homeAvailable: the home screen is for a person at a terminal only. Agents,
// pipes, --json/--jq, --quiet, CI and TERM=dumb get the help text (or the
// command tree as JSON) as before.
func (a *App) homeAvailable() bool {
	return a.Interactive() && a.StderrTTY && !a.g.quiet && a.env("TERM") != "dumb"
}

// homeScreen is `sendio` with no arguments at a terminal: the wordmark, who you
// are, your credits, and a short menu of the things people do most.
func (a *App) homeScreen(cmd *cobra.Command) error {
	p := a.P()
	s := p.S()
	a.printLogo(a.Stdout, "The send.io CLI · "+build.Version, "Email from your terminal. `sendio --help` lists everything.", true)

	cred, _ := a.credential()
	signedIn := cred != nil && cred.token != ""
	if signedIn {
		who := a.homeIdentity(cred)
		fmt.Fprintln(a.Stdout, "  "+s.OK.Render("●")+" "+who)
		if bal := a.homeBalance(cmd.Context()); bal != "" {
			fmt.Fprintln(a.Stdout, "  "+a.ticket(bal))
		}
	} else {
		fmt.Fprintln(a.Stdout, "  "+s.Dim.Render("○ Not signed in. Pick “Sign in” below, or run ")+s.Accent.Render("sendio login"))
	}
	fmt.Fprintln(a.Stdout)

	actions := a.homeActions(signedIn)
	width := 0
	for _, x := range actions {
		if n := len([]rune(x.label)); n > width {
			width = n
		}
	}
	opts := make([]huh.Option[int], len(actions))
	for i, x := range actions {
		opts[i] = huh.NewOption(fmt.Sprintf("%-*s", width, x.label), i)
	}
	pick := 0
	sel := huh.NewSelect[int]().Title("What would you like to do?").Options(opts...).Value(&pick).
		DescriptionFunc(func() string {
			if pick < 0 || pick >= len(actions) || len(actions[pick].args) == 0 {
				return " "
			}
			return "› sendio " + strings.Join(actions[pick].args, " ")
		}, &pick)
	err := huh.NewForm(huh.NewGroup(sel)).WithTheme(a.huhTheme()).WithShowHelp(true).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return nil
	}
	if err != nil {
		return err
	}
	act := actions[pick]
	if act.args == nil && act.form == nil {
		return nil // Quit
	}
	args := act.args
	if act.form != nil {
		if args, err = act.form(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
	}
	if len(args) == 1 && args[0] == "--help" {
		return cmd.Help()
	}
	fmt.Fprintln(a.Stderr, s.Dim.Render("› ")+s.Accent.Render("sendio "+strings.Join(quoteArgs(args), " ")))
	return a.runSub(args)
}

func (a *App) homeActions(signedIn bool) []homeAction {
	if !signedIn {
		return []homeAction{
			{label: "Sign in", args: []string{"login"}},
			{label: "Read the docs", args: []string{"open", "docs"}},
			{label: "Check my setup", args: []string{"doctor"}},
			{label: "All commands", args: []string{"--help"}},
			{label: "Quit"},
		}
	}
	return []homeAction{
		{label: "Send a test email", args: []string{"emails", "send", "--to", "…"}, form: a.testEmailForm},
		{label: "Check domains", args: []string{"domains", "list"}},
		{label: "Tail logs", args: []string{"logs", "tail"}},
		{label: "Credit balance", args: []string{"billing", "balance"}},
		{label: "Open the dashboard", args: []string{"open", "dashboard"}},
		{label: "Read the docs", args: []string{"open", "docs"}},
		{label: "All commands", args: []string{"--help"}},
		{label: "Quit"},
	}
}

// testEmailForm asks for the three things a test needs and returns the
// `emails send` arguments.
func (a *App) testEmailForm() ([]string, error) {
	to, from, subject := "", "", "Hello from send.io"
	if pr := a.profile(); pr != nil {
		to = pr.UserEmail
	}
	need := func(what string) func(string) error {
		return func(v string) error {
			if !strings.Contains(v, "@") {
				return fmt.Errorf("%s needs an email address", what)
			}
			return nil
		}
	}
	err := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("From").Description("An address on one of your verified domains").Placeholder("hello@yourdomain.com").Value(&from).Validate(need("From")),
		huh.NewInput().Title("To").Placeholder("you@example.com").Value(&to).Validate(need("To")),
		huh.NewInput().Title("Subject").Value(&subject),
	)).WithTheme(a.huhTheme()).WithShowHelp(false).Run()
	if err != nil {
		return nil, err
	}
	return []string{"emails", "send", "--from", strings.TrimSpace(from), "--to", strings.TrimSpace(to),
		"--subject", subject, "--text", "Sent from the sendio CLI. It works!"}, nil
}

// homeIdentity is "Acme · you@acme.dev · profile default" from what login
// saved (no network call).
func (a *App) homeIdentity(cred *credential) string {
	s := a.P().S()
	if cred.profile == nil {
		return s.Bold.Render("API key") + s.Dim.Render(" from "+map[string]string{"flag": "--api-key", "env": "SENDIO_API_KEY"}[cred.source])
	}
	pr := cred.profile
	team := pr.TeamName
	if team == "" {
		team = "send.io"
	}
	meta := []string{}
	if pr.UserEmail != "" {
		meta = append(meta, pr.UserEmail)
	}
	meta = append(meta, "profile "+pr.Name)
	return s.Bold.Render(team) + s.Dim.Render("  "+strings.Join(meta, " · "))
}

// homeBalance fetches the credit balance ("" when it cannot, quickly).
func (a *App) homeBalance(ctx context.Context) string {
	c, err := a.Client(true)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	resp, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/billing/balance"})
	if err != nil {
		return ""
	}
	var m map[string]any
	if resp.Decode(&m) != nil {
		return ""
	}
	f, ok := m["balance"].(float64)
	if !ok {
		return ""
	}
	return formatCredits(f)
}

// formatCredits prints 1240.5 as "1,240.5".
func formatCredits(f float64) string {
	neg := f < 0
	if neg {
		f = -f
	}
	str := strconv.FormatFloat(f, 'f', -1, 64)
	if i := strings.IndexByte(str, '.'); i >= 0 && len(str)-i > 3 {
		str = strconv.FormatFloat(f, 'f', 2, 64)
	}
	intPart, frac := str, ""
	if i := strings.IndexByte(str, '.'); i >= 0 {
		intPart, frac = str[:i], str[i:]
	}
	var b strings.Builder
	for i, r := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	out := b.String() + frac
	if neg {
		out = "-" + out
	}
	return out
}

// runSub runs a command for the home screen in a fresh invocation that
// shares this one's terminal and environment (and its -p/--api-key/
// --base-url), so it behaves exactly as if typed.
func (a *App) runSub(args []string) error {
	var full []string
	if a.g.profile != "" {
		full = append(full, "--profile", a.g.profile)
	}
	if a.g.apiKey != "" {
		full = append(full, "--api-key", a.g.apiKey)
	}
	if a.g.baseURL != "" {
		full = append(full, "--base-url", a.g.baseURL)
	}
	if a.g.noColor {
		full = append(full, "--no-color")
	}
	full = append(full, args...)
	sub := *a
	sub.g, sub.printer, sub.agent, sub.store, sub.cfg, sub.update, sub.notice, sub.showVersion = globals{}, nil, false, nil, nil, nil, "", false
	parent := a.Getenv
	sub.Getenv = func(k string) string {
		if k == "SENDIO_NO_UPDATE_NOTIFIER" {
			return "1"
		}
		if parent == nil {
			return ""
		}
		return parent(k)
	}
	if code := sub.Run(full); code != 0 {
		return silentExit(code)
	}
	return nil
}

// quoteArgs shell-quotes arguments with spaces for display.
func quoteArgs(args []string) []string {
	q := make([]string, len(args))
	for i, s := range args {
		if strings.ContainsAny(s, " \t\"'$`!") {
			s = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
		q[i] = s
	}
	return q
}
