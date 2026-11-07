// Package cli wires the sendio command tree: global flags and modes (human
// vs agent), authentication, the generated resource commands and the
// hand-written experience commands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/pkg/browser"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"send.io/cli/internal/api"
	"send.io/cli/internal/build"
	"send.io/cli/internal/config"
	"send.io/cli/internal/out"
)

// DefaultBaseURL is the API origin this build talks to by default
// (build.BaseURL: https://api.send.io in production, the dev API in dev builds).
var DefaultBaseURL = build.BaseURL

// App is one CLI invocation with its environment. Everything the CLI touches
// outside itself goes through these fields so tests can replace them.
type App struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	Environ        func() []string
	StdinTTY       bool
	StdoutTTY      bool
	StderrTTY      bool
	// HTTP is the base HTTP client (nil: a default with timeouts).
	HTTP     *http.Client
	OpenURL  func(string) error
	Hostname func() (string, error)
	LookPath func(string) (string, error)
	// RunCmd runs an external program with stdio attached (claude mcp add).
	RunCmd func(name string, args ...string) error
	// Executable is this binary's path (mcp install --local writes it).
	Executable func() (string, error)
	HomeDir    string
	WorkDir    string
	Now        func() time.Time
	Sleep      func(context.Context, time.Duration) error
	Keyring    config.Keyring

	g       globals
	printer *out.Printer
	agent   bool
	store   *config.Store
	cfg     *config.Config
	update  chan string
	notice  string
	// showVersion is -v/--version.
	showVersion bool
}

type globals struct {
	profile, apiKey, baseURL, jq, agent string
	json, quiet, noColor, yes, dryRun   bool
	insecureStorage                     bool
}

// NewApp returns an App bound to the real process environment.
func NewApp() *App {
	home, _ := os.UserHomeDir()
	wd, _ := os.Getwd()
	return &App{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getenv: os.Getenv, Environ: os.Environ,
		StdinTTY:  term.IsTerminal(int(os.Stdin.Fd())),
		StdoutTTY: term.IsTerminal(int(os.Stdout.Fd())),
		StderrTTY: term.IsTerminal(int(os.Stderr.Fd())),
		OpenURL: func(u string) error {
			browser.Stdout, browser.Stderr = io.Discard, io.Discard
			return browser.OpenURL(u)
		},
		Hostname: os.Hostname,
		LookPath: exec.LookPath,
		RunCmd: func(name string, args ...string) error {
			c := exec.Command(name, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stderr, os.Stderr
			return c.Run()
		},
		Executable: os.Executable,
		HomeDir:    home, WorkDir: wd,
		Now: time.Now,
	}
}

func (a *App) env(k string) string {
	if a.Getenv == nil {
		return ""
	}
	return a.Getenv(k)
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) sleep(ctx context.Context, d time.Duration) error {
	if a.Sleep != nil {
		return a.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// agentEnvVars are environment variables set by coding agents.
var agentEnvVars = []string{"CLAUDECODE", "CURSOR_AGENT", "GEMINI_CLI", "OPENCODE", "AI_AGENT"}

// detectAgentEnv names the agent whose environment variable is set.
func (a *App) detectAgentEnv() string {
	for _, k := range agentEnvVars {
		if a.env(k) != "" {
			return k
		}
	}
	if a.Environ != nil {
		for _, kv := range a.Environ() {
			if strings.HasPrefix(kv, "CODEX_") {
				return strings.SplitN(kv, "=", 2)[0]
			}
		}
	}
	return ""
}

// resolveModes computes agent mode and builds the printer from the flags.
func (a *App) resolveModes() error {
	mode := strings.ToLower(a.g.agent)
	if mode == "" {
		mode = strings.ToLower(a.env("SENDIO_AGENT"))
	}
	switch mode {
	case "", "auto":
		a.agent = !a.StdoutTTY || a.detectAgentEnv() != ""
	case "yes", "true", "1", "on":
		a.agent = true
	case "no", "false", "0", "off":
		a.agent = false
	default:
		return api.UsageError("--agent must be auto, yes or no")
	}
	color := !a.g.noColor && a.env("NO_COLOR") == "" && a.env("TERM") != "dumb"
	p, err := out.New(a.Stdout, a.Stderr, a.g.json, a.agent, a.g.quiet, a.StdoutTTY, a.StderrTTY, color, a.g.jq)
	if err != nil {
		// Still report the error in the right shape.
		a.printer, _ = out.New(a.Stdout, a.Stderr, a.g.json, a.agent, a.g.quiet, a.StdoutTTY, a.StderrTTY, color, "")
		a.applyLook(a.printer)
		return err
	}
	a.printer = p
	a.applyLook(p)
	return nil
}

// P returns the printer (building a fallback when flags were never parsed).
func (a *App) P() *out.Printer {
	if a.printer == nil {
		_ = a.resolveModes()
		if a.printer == nil {
			a.printer, _ = out.New(a.Stdout, a.Stderr, true, true, false, false, false, false, "")
		}
	}
	return a.printer
}

// Interactive reports whether the CLI may prompt.
func (a *App) Interactive() bool {
	return !a.agent && !a.P().Machine && a.StdinTTY && a.StdoutTTY && a.env("CI") == ""
}

// Store returns the config store.
func (a *App) Store() *config.Store {
	if a.store == nil {
		dir := config.DefaultDir(a.env)
		if a.env("SENDIO_CONFIG_DIR") == "" && a.env("XDG_CONFIG_HOME") == "" && a.HomeDir != "" && runtime.GOOS != "windows" {
			dir = a.HomeDir + "/.config/sendio"
		}
		a.store = &config.Store{Dir: dir, Keyring: a.Keyring,
			ForceFile: a.g.insecureStorage || strings.EqualFold(a.env("SENDIO_CREDENTIAL_STORE"), "file")}
	}
	return a.store
}

// Config loads config.json once.
func (a *App) Config() (*config.Config, error) {
	if a.cfg == nil {
		c, err := a.Store().Load()
		if err != nil {
			return nil, api.Errorf(api.ExitUsage, "config_error", "%v", err)
		}
		a.cfg = c
	}
	return a.cfg, nil
}

// ProfileName is the selected profile: -p, SENDIO_PROFILE, the current
// profile, or "default".
func (a *App) ProfileName() string {
	if a.g.profile != "" {
		return a.g.profile
	}
	if v := a.env("SENDIO_PROFILE"); v != "" {
		return v
	}
	if c, err := a.Config(); err == nil && c.Current != "" {
		return c.Current
	}
	return config.DefaultProfile
}

// profile returns the selected profile, or nil when it is not saved.
func (a *App) profile() *config.Profile {
	c, err := a.Config()
	if err != nil {
		return nil
	}
	return c.Profiles[a.ProfileName()]
}

// BaseURL resolves the API origin: --base-url, SENDIO_BASE_URL, the
// profile's, then the default.
func (a *App) BaseURL() string {
	v := a.g.baseURL
	if v == "" {
		v = a.env("SENDIO_BASE_URL")
	}
	if v == "" {
		if p := a.profile(); p != nil && p.BaseURL != "" {
			v = p.BaseURL
		}
	}
	if v == "" {
		v = DefaultBaseURL
	}
	return strings.TrimRight(strings.TrimSpace(v), "/")
}

// AppURL derives the web app origin from the API origin (api.X → X).
func AppURL(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "https://send.io"
	}
	host := strings.TrimPrefix(u.Host, "api.")
	return u.Scheme + "://" + host
}

func (a *App) baseHTTP() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

// credential describes where the token for this invocation comes from.
type credential struct {
	source  string // flag, env, profile, none
	kind    string // api_key, oauth
	token   string
	profile *config.Profile
	secret  config.Secret
}

func (a *App) credential() (*credential, error) {
	if a.g.apiKey != "" {
		return &credential{source: "flag", kind: config.AuthAPIKey, token: a.g.apiKey}, nil
	}
	if v := strings.Trim(strings.TrimSpace(a.env("SENDIO_API_KEY")), "'\""); v != "" {
		return &credential{source: "env", kind: config.AuthAPIKey, token: v}, nil
	}
	p := a.profile()
	if p == nil {
		return &credential{source: "none"}, nil
	}
	sec, err := a.Store().GetSecret(p)
	if err != nil {
		return nil, &api.Error{Exit: api.ExitAuth, Code: "credentials_unavailable", Message: err.Error(), Hint: "Run `sendio login` again."}
	}
	c := &credential{source: "profile", kind: p.Auth, profile: p, secret: sec, token: sec.APIKey}
	if p.Auth == config.AuthOAuth {
		c.token = sec.AccessToken
	}
	return c, nil
}

func userAgent() string {
	return fmt.Sprintf("sendio-cli/%s (%s/%s)", build.Version, runtime.GOOS, runtime.GOARCH)
}

// Client builds the API client for this invocation. requireAuth fails with
// exit 3 when there is no credential.
func (a *App) Client(requireAuth bool) (*api.Client, error) {
	c := &api.Client{BaseURL: a.BaseURL(), HTTP: a.baseHTTP(), UserAgent: userAgent(), DryRun: a.g.dryRun, OnDryRun: a.printDryRun, Progress: a.progress}
	cred, err := a.credential()
	if err != nil {
		return nil, err
	}
	switch {
	case cred.token == "" && requireAuth:
		return nil, &api.Error{Exit: api.ExitAuth, Code: "not_authenticated", Message: "You are not signed in.",
			Hint: "Run `sendio login` (or `sendio login --api-key mx_…`), or set SENDIO_API_KEY."}
	case cred.kind == config.AuthOAuth && cred.profile != nil:
		c.Auth = &oauthSource{app: a, profile: cred.profile, secret: cred.secret, base: c.BaseURL}
	case cred.token != "":
		c.Auth = api.StaticToken(cred.token)
	}
	return c, nil
}

func redact(tok string) string {
	if len(tok) <= 8 {
		return "…"
	}
	return tok[:7] + "…"
}

// printDryRun shows the request --dry-run did not send.
func (a *App) printDryRun(r *http.Request, body []byte) {
	headers := map[string]string{}
	for k := range r.Header {
		v := r.Header.Get(k)
		if strings.EqualFold(k, "Authorization") {
			v = "Bearer " + redact(strings.TrimPrefix(v, "Bearer "))
		}
		headers[k] = v
	}
	var bodyVal any
	if len(body) > 0 {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			bodyVal = out.Normalize(jsonRaw(body))
		} else {
			bodyVal = string(body)
		}
	}
	p := a.P()
	if p.Machine {
		_ = p.Line(map[string]any{"dry_run": true, "method": r.Method, "url": r.URL.String(), "headers": headers, "body": bodyVal})
		return
	}
	s := p.S()
	fmt.Fprintln(a.Stdout, s.Warn.Render("Dry run: this request was not sent."))
	fmt.Fprintln(a.Stdout, s.Bold.Render(r.Method+" "+r.URL.String()))
	for _, k := range sortedKeys(headers) {
		fmt.Fprintf(a.Stdout, "%s: %s\n", k, headers[k])
	}
	if bodyVal != nil {
		fmt.Fprintln(a.Stdout)
		if str, ok := bodyVal.(string); ok {
			fmt.Fprintln(a.Stdout, str)
		} else {
			b, _ := out.MarshalIndent(bodyVal)
			fmt.Fprintln(a.Stdout, string(b))
		}
	}
}

// Confirm guards destructive commands: --yes passes; agents and
// non-interactive runs must pass --yes; humans get a prompt.
func (a *App) Confirm(question string) error {
	if a.g.yes || a.g.dryRun {
		return nil
	}
	if !a.Interactive() {
		return &api.Error{Exit: api.ExitUsage, Code: "confirmation_required", Message: "This command is destructive; pass --yes to confirm.", Hint: question}
	}
	ok := false
	err := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title(question).Affirmative("Yes").Negative("No").Value(&ok))).
		WithTheme(a.huhTheme()).WithShowHelp(false).Run()
	if err != nil || !ok {
		return &api.Error{Exit: api.ExitUsage, Code: "canceled", Message: "Canceled."}
	}
	return nil
}

// Run executes the CLI with args and returns the exit code.
func (a *App) Run(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	root := a.Root()
	root.SetArgs(args)
	root.SetIn(a.Stdin)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)
	err := root.ExecuteContext(ctx)
	a.finishUpdateCheck()
	if err == nil {
		return api.ExitOK
	}
	if errors.Is(err, api.ErrDryRun) {
		return api.ExitOK
	}
	var se silentExit
	if errors.As(err, &se) {
		return int(se)
	}
	var e *api.Error
	if !errors.As(err, &e) {
		// cobra's own errors: unknown command/flag, wrong arg count.
		e = &api.Error{Exit: api.ExitUsage, Code: "usage_error", Message: err.Error()}
	}
	if e.Exit == api.ExitUsage && e.Hint == "" && e.Code == "usage_error" {
		e.Hint = "Run `sendio --help` (or `sendio <command> --help`) for usage."
	}
	if ctx.Err() != nil && e.Exit == api.ExitNetwork {
		e = &api.Error{Exit: 130, Code: "canceled", Message: "Canceled."}
	}
	if !a.P().Machine {
		e = a.humanError(e)
	}
	a.P().Error(e)
	if e.Exit == 0 {
		return api.ExitAPI
	}
	return e.Exit
}

// Root builds the command tree.
func (a *App) Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "sendio",
		Short: "send.io from your terminal: send email, manage domains, templates, contacts and more",
		Long: `sendio is the official send.io CLI.

It works for humans (tables, colours, prompts) and for agents and scripts:
when stdout is not a terminal, or a coding agent is detected, it switches to
agent mode: JSON on stdout, structured errors on stderr, no prompts, and
destructive commands require --yes. See "sendio commands --json".`,
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if err := a.resolveModes(); err != nil {
				return err
			}
			if !a.showVersion {
				a.startUpdateCheck(cmd)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.showVersion {
				return a.printVersionLine()
			}
			if a.P().Machine {
				return a.P().JSON(commandTree(cmd.Root()))
			}
			if a.homeAvailable() {
				return a.homeScreen(cmd)
			}
			a.printLogo(a.Stdout, "The send.io CLI · "+build.Version, "Run `sendio login` to get started", false)
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	// -v/--version is handled in RunE (not cobra's template) so it can show
	// the compact wordmark on a terminal and stay "sendio X.Y.Z" elsewhere.
	root.Flags().BoolVarP(&a.showVersion, "version", "v", false, "version for sendio")
	a.styleHelp(root)
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return &api.Error{Exit: api.ExitUsage, Code: "usage_error", Message: err.Error(), Hint: "Run `" + c.CommandPath() + " --help` for usage."}
	})
	pf := root.PersistentFlags()
	pf.StringVarP(&a.g.profile, "profile", "p", "", "profile to use (env SENDIO_PROFILE)")
	pf.StringVar(&a.g.apiKey, "api-key", "", "API key for this command (env SENDIO_API_KEY)")
	pf.StringVar(&a.g.baseURL, "base-url", "", "API origin (env SENDIO_BASE_URL, default "+DefaultBaseURL+")")
	pf.BoolVar(&a.g.json, "json", false, "print JSON")
	pf.StringVar(&a.g.jq, "jq", "", "filter JSON output with a jq expression (implies --json)")
	pf.StringVar(&a.g.agent, "agent", "auto", "agent mode: auto, yes or no (auto: when stdout is not a TTY or an agent is detected)")
	pf.BoolVarP(&a.g.quiet, "quiet", "q", false, "print only results, no status messages")
	pf.BoolVar(&a.g.noColor, "no-color", false, "disable colours (also NO_COLOR)")
	pf.BoolVarP(&a.g.yes, "yes", "y", false, "confirm destructive actions without prompting")
	pf.BoolVar(&a.g.dryRun, "dry-run", false, "print write requests instead of sending them")
	pf.BoolVar(&a.g.insecureStorage, "insecure-storage", false, "store credentials in a 0600 file instead of the OS keyring")

	a.addResourceCommands(root)
	a.addAuthCommands(root)
	a.addEmailCommands(root)
	a.addDomainCommands(root)
	a.addTemplateCommands(root)
	a.addContactCommands(root)
	a.addAPICommand(root)
	a.addMCPCommands(root)
	a.addAgentCommands(root)
	a.addDoctorCommand(root)
	a.addMiscCommands(root)
	for _, ext := range extensions {
		ext(a, root)
	}
	setGroupRunE(root)
	return root
}

// setGroupRunE makes bare group commands print help (or the subtree as JSON
// in machine mode) instead of silently succeeding.
func setGroupRunE(c *cobra.Command) {
	for _, sub := range c.Commands() {
		if sub.HasSubCommands() {
			if sub.RunE == nil && sub.Run == nil {
				s := sub
				s.Args = cobra.NoArgs
				s.RunE = func(cmd *cobra.Command, args []string) error { return cmd.Help() }
			}
			setGroupRunE(sub)
		}
	}
}

// Main runs the CLI on the real process and returns the exit code.
func Main() int { return NewApp().Run(os.Args[1:]) }
