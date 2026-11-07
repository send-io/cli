package cli

// sendio inbox …: send.io's anonymous (free) agent inbox (contracts/free-inbox/PROTOCOL.md).
// No sign-in: an inbox is a key pair made on this machine, mail is sealed to
// it on arrival and fetched over a connection this CLI opens. Keys and mail
// live in ~/.sendio/free (SENDIO_HOME), shared with the Node SDK and the
// MCP tools.
//
// `sendio inbox watch <address>` still watches a send.io agent inbox
// (inboxes watch) when the address is not a free inbox on this machine.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/freeinbox"
	"send.io/cli/internal/out"
)

func init() { Register(func(a *App, root *cobra.Command) { a.addFreeInboxCommands(root) }) }

const freeGroupShort = "Free anonymous agent inbox: create, watch, wait for a code, connect to a workspace (no sign-in)"

func (a *App) freeStore() *freeinbox.Store {
	return &freeinbox.Store{Root: freeinbox.DefaultRoot(a.env, a.HomeDir)}
}

// freeRelay resolves the relay: --relay, SENDIO_FREE_RELAY, the inbox's
// key.json, then the default.
func (a *App) freeRelay(flag string, in *freeinbox.Inbox) string {
	switch {
	case strings.TrimSpace(flag) != "":
		return freeinbox.NormalizeRelay(flag)
	case strings.TrimSpace(a.env("SENDIO_FREE_RELAY")) != "":
		return freeinbox.NormalizeRelay(a.env("SENDIO_FREE_RELAY"))
	case in != nil && in.Relay != "":
		return freeinbox.NormalizeRelay(in.Relay)
	}
	return freeinbox.DefaultRelay
}

// freeAppURL is the web app that opens connect links: SENDIO_APP_URL, else
// freeinbox.DefaultAppURL.
func (a *App) freeAppURL() string {
	return freeinbox.NormalizeAppURL(a.env("SENDIO_APP_URL"))
}

// freeExplain turns a long-poll auth_failed (the HTTP API says only that)
// into the reason the stream gives: an *EndedError when the inbox was
// claimed (moved into a workspace), suspended or deleted. Other errors are
// returned unchanged.
func freeExplain(ctx context.Context, c *freeinbox.Client, in *freeinbox.Inbox, err error) error {
	var re *freeinbox.Error
	if !errors.As(err, &re) || re.Status != 401 || re.Code != "auth_failed" || strings.Contains(re.Message, "ts outside") {
		return err
	}
	code, perr := freeinbox.InboxStatus(ctx, c, in)
	if perr == nil && code != "active" && code != "auth_failed" {
		return &freeinbox.EndedError{Address: in.Address, Code: code}
	}
	return err
}

func (a *App) freeClient(relay string) *freeinbox.Client {
	return &freeinbox.Client{Relay: relay, HTTP: a.HTTP, UserAgent: userAgent()}
}

// freeErr maps freeinbox errors to CLI errors.
func freeErr(err error) error {
	if err == nil {
		return nil
	}
	var te *freeinbox.TakenError
	var re *freeinbox.Error
	var ee *freeinbox.EndedError
	switch {
	case errors.As(err, &ee):
		e := &api.Error{Exit: api.ExitAPI, Code: ee.Code, Message: ee.Error() + "."}
		if ee.Code == "claimed" {
			e.Hint = "It now lives in your send.io workspace (" + freeinbox.DefaultAppURL + "), where you can read and send from it. This machine's key no longer reads it."
		}
		return e
	case errors.As(err, &te):
		e := &api.Error{Exit: api.ExitAPI, Code: te.Reason, Message: fmt.Sprintf("%s is not available (%s).", te.Address, te.Reason)}
		if len(te.Suggestions) > 0 {
			e.Hint = "Available: " + strings.Join(te.Suggestions, ", ") + ". Create one with `sendio inbox create --address " + te.Suggestions[0] + "`."
		}
		return e
	case errors.As(err, &re):
		if re.Status == 0 {
			return &api.Error{Exit: api.ExitNetwork, Code: "network_error", Message: re.Error(), Hint: "Check --relay / SENDIO_FREE_RELAY and your connection."}
		}
		e := &api.Error{Exit: api.ExitAPI, Code: re.Code, Message: re.Error(), Status: re.Status, RetryAfter: re.RetryAfter}
		if re.Status == 429 {
			e.Exit = api.ExitRateLimited
		}
		return e
	case errors.Is(err, freeinbox.ErrNoInbox):
		return &api.Error{Exit: api.ExitNotFound, Code: "not_found", Message: err.Error() + ".", Hint: "List local inboxes with `sendio inbox list`, or restore one with `sendio inbox recover <phrase> <address>`."}
	case errors.Is(err, freeinbox.ErrAuthFailed):
		return &api.Error{Exit: api.ExitAuth, Code: "auth_failed", Message: err.Error()}
	case errors.Is(err, freeinbox.ErrReplaced):
		return &api.Error{Exit: api.ExitAPI, Code: "replaced", Message: err.Error()}
	case errors.Is(err, context.Canceled):
		return nil
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae
	}
	return &api.Error{Exit: api.ExitAPI, Code: "free_inbox_error", Message: err.Error()}
}

// freeAddress resolves the inbox a command works on: the argument, or the
// only local inbox (a picker when there are several and we may prompt).
func (a *App) freeAddress(args []string) (string, error) {
	st := a.freeStore()
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		addr := strings.ToLower(strings.TrimSpace(args[0]))
		if !st.Has(addr) {
			return "", freeErr(fmt.Errorf("%w: %s", freeinbox.ErrNoInbox, addr))
		}
		return addr, nil
	}
	list, err := st.List()
	if err != nil {
		return "", err
	}
	switch len(list) {
	case 0:
		return "", &api.Error{Exit: api.ExitNotFound, Code: "no_inbox", Message: "There is no free inbox on this machine.", Hint: "Create one with `sendio inbox create`."}
	case 1:
		return list[0].Address, nil
	}
	if a.Interactive() {
		var pick string
		var opts []huh.Option[string]
		for _, k := range list {
			opts = append(opts, huh.NewOption(k.Address, k.Address))
		}
		err := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Which inbox?").Options(opts...).Value(&pick))).
			WithTheme(a.huhTheme()).WithShowHelp(false).Run()
		if err != nil {
			return "", &api.Error{Exit: api.ExitUsage, Code: "canceled", Message: "Canceled."}
		}
		return pick, nil
	}
	var names []string
	for _, k := range list {
		names = append(names, k.Address)
	}
	return "", api.UsageError("several free inboxes on this machine; name one: %s", strings.Join(names, ", "))
}

func (a *App) openFreeInbox(args []string, relayFlag string) (*freeinbox.Inbox, *freeinbox.Client, error) {
	addr, err := a.freeAddress(args)
	if err != nil {
		return nil, nil, err
	}
	in, err := a.freeStore().OpenInbox(addr)
	if err != nil {
		return nil, nil, freeErr(err)
	}
	return in, a.freeClient(a.freeRelay(relayFlag, in)), nil
}

func (a *App) addFreeInboxCommands(root *cobra.Command) {
	g := &cobra.Command{
		Use:   "inbox",
		Short: freeGroupShort,
		Long: `A free, anonymous inbox for agents and scripts: name@org.sendioinbox.com.
No account or sign-in. The inbox key is made on this machine and never
leaves it; mail is sealed to that key on arrival, held up to 72 hours, and
fetched by this CLI over a connection it opens. Inboxes are receive-only
(100 messages a day, 10 MB each); "sendio inbox connect" makes a one-time
link that moves one into your send.io workspace, where it can also send.

Keys and mail are stored in ~/.sendio/free/<address>/ (SENDIO_HOME moves
~/.sendio). The relay is ` + freeinbox.DefaultRelay + ` (SENDIO_FREE_RELAY or --relay).

For send.io agent inboxes on your own domain, see "sendio inboxes".`,
		Example: `  sendio inbox create --name signup-bot --org acme
  sendio inbox wait --code --timeout 2m
  sendio inbox watch
  sendio inbox read --limit 5
  sendio inbox connect --open`,
	}
	var relay string
	g.PersistentFlags().StringVar(&relay, "relay", "", "relay URL (env SENDIO_FREE_RELAY, default "+freeinbox.DefaultRelay+")")
	root.AddCommand(g)

	// create
	var name, org, address string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a free inbox (prompts for the name and org on a terminal)",
		Long: `Create a free inbox name@org.sendioinbox.com. On a terminal, missing
--name/--org are asked for; a taken address offers up to three available
alternatives. --address creates an exact address (one of the suggestions).

The recovery phrase is printed once. It is the only way to read the inbox on
another machine (sendio inbox recover); send.io cannot recover it.`,
		Example: `  sendio inbox create
  sendio inbox create --name signup-bot --org acme
  sendio inbox create --name signup-bot --org acme --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.freeCreate(cmd.Context(), relay, name, org, address)
		},
	}
	create.Flags().StringVar(&name, "name", "", "the part before @ (e.g. signup-bot)")
	create.Flags().StringVar(&org, "org", "", "the org label (e.g. acme for @acme.sendioinbox.com)")
	create.Flags().StringVar(&address, "address", "", "create this exact address (e.g. a suggestion)")
	g.AddCommand(create)

	// check
	check := &cobra.Command{
		Use:   "check",
		Short: "Check whether a free inbox name is available",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || org == "" {
				return api.UsageError("--name and --org are required")
			}
			res, err := a.freeClient(a.freeRelay(relay, nil)).Check(cmd.Context(), freeinbox.Normalize(name), freeinbox.Normalize(org))
			if err != nil {
				return freeErr(err)
			}
			return a.P().Result(res, func(w io.Writer) {
				s := a.P().S()
				if res.Available {
					fmt.Fprintf(w, "%s %s is available\n", s.OK.Render("✓"), s.Bold.Render(res.Address))
					return
				}
				fmt.Fprintf(w, "%s %s is not available (%s)\n", s.Warn.Render("✗"), s.Bold.Render(res.Address), res.Reason)
				for _, sug := range res.Suggestions {
					fmt.Fprintf(w, "  %s\n", sug)
				}
			})
		},
	}
	check.Flags().StringVar(&name, "name", "", "the part before @")
	check.Flags().StringVar(&org, "org", "", "the org label")
	g.AddCommand(check)

	// list
	g.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the free inboxes on this machine",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.freeList()
		},
	})

	// watch
	var wo inboxWatchOpts
	watch := &cobra.Command{
		Use:   "watch [address]",
		Short: "Print mail as it arrives in a free inbox (all local inboxes by default)",
		Long: `Watch free inboxes live: each message is decrypted, saved, acknowledged
and printed (time, from, subject); risky attachments are flagged. One NDJSON
line per message in agent mode or with --json. Without an address every
free inbox on this machine is watched. The connection is kept up and
resumes where it left off; Ctrl-C stops (exit 0).

An address that is not a free inbox here is watched as a send.io agent
inbox (as sendio inboxes watch, sign-in required).`,
		Example: `  sendio inbox watch
  sendio inbox watch signup-bot@acme.sendioinbox.com --json | jq -r .subject`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && !a.freeStore().Has(args[0]) && !isPoolAddress(args[0]) {
				return a.inboxWatch(cmd.Context(), args[0], wo)
			}
			return a.freeWatch(cmd.Context(), args, relay)
		},
	}
	watch.Flags().StringVar(&wo.exec, "exec", "", "send.io agent inboxes only: shell command per message")
	watch.Flags().DurationVar(&wo.execTimeout, "exec-timeout", time.Minute, "send.io agent inboxes only: time limit of each --exec run")
	watch.Flags().BoolVar(&wo.spam, "include-spam", false, "send.io agent inboxes only: also print spam")
	for _, f := range []string{"exec", "exec-timeout", "include-spam"} {
		_ = watch.Flags().MarkHidden(f)
	}
	g.AddCommand(watch)

	// read
	var limit int
	var id uint64
	var noSync bool
	read := &cobra.Command{
		Use:   "read [address]",
		Short: "Show the newest mail of a free inbox (fetches waiting mail first)",
		Long: `Show the newest messages of a free inbox, or one message in full with --id.
Mail waiting on the relay is fetched, saved and acknowledged first
(--no-sync reads only what is on disk).`,
		Example: `  sendio inbox read
  sendio inbox read --limit 3 --json
  sendio inbox read --id 42`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.freeRead(cmd.Context(), args, relay, limit, id, noSync)
		},
	}
	read.Flags().IntVar(&limit, "limit", 10, "number of messages")
	read.Flags().Uint64Var(&id, "id", 0, "show this message (its seq) in full")
	read.Flags().BoolVar(&noSync, "no-sync", false, "do not fetch waiting mail from the relay")
	g.AddCommand(read)

	// wait
	var wt freeWaitOpts
	wait := &cobra.Command{
		Use:   "wait [address]",
		Short: "Wait for an email (or a verification code) and print it: for scripts and agents",
		Long: `Wait until an email arrives that matches --from/--subject (substrings,
case-insensitive) and print it; with --code, print only the 6-8 digit code it
contains (messages without a code are skipped). Exits 1 when nothing matched
before --timeout.

Only mail that reaches this machine during the wait counts (it may have
arrived at the relay just before). --since 10m also accepts mail already
saved here in the last 10 minutes.`,
		Example: `  code=$(sendio inbox wait --code --timeout 2m)
  sendio inbox wait --from github.com --subject "verify" --json
  sendio inbox wait signup-bot@acme.sendioinbox.com --code --since 5m`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.freeWait(cmd.Context(), args, relay, wt)
		},
	}
	wait.Flags().DurationVar(&wt.timeout, "timeout", 120*time.Second, "how long to wait")
	wait.Flags().StringVar(&wt.from, "from", "", "only mail whose From contains this")
	wait.Flags().StringVar(&wt.subject, "subject", "", "only mail whose Subject contains this")
	wait.Flags().BoolVar(&wt.code, "code", false, "print only the 6-8 digit code from the mail")
	wait.Flags().DurationVar(&wt.since, "since", 0, "also accept mail saved here within this window")
	g.AddCommand(wait)

	// poll
	var pollWait int
	poll := &cobra.Command{
		Use:   "poll [address]",
		Short: "Fetch waiting mail once over HTTP long polling (for cron and serverless)",
		Long: `Fetch the mail waiting for a free inbox with one long-poll request (waiting
up to --wait seconds when there is none), save and acknowledge it, and print
the new messages.`,
		Example: "  sendio inbox poll\n  sendio inbox poll --wait 0 --json",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.freePoll(cmd.Context(), args, relay, pollWait)
		},
	}
	poll.Flags().IntVar(&pollWait, "wait", 25, "seconds to wait for mail (0-25)")
	g.AddCommand(poll)

	// connect
	var openLink bool
	connect := &cobra.Command{
		Use:   "connect [address]",
		Short: "Make a one-time link that moves a free inbox into your send.io workspace (to send from it)",
		Long: `Print a link that attaches a free inbox to a send.io workspace. Open it in a
browser, sign in, and the inbox moves into that workspace: you read it in the
dashboard and can send from it with the workspace's credits (up to 50 emails
a day). The link is signed with the inbox key on this machine, works once and
expires in 30 minutes. Anyone holding it can take the inbox, so give it only
to the person who should own it.

After the move this machine's key no longer reads the inbox: watch, wait and
poll stop and say so.

The app origin is ` + freeinbox.DefaultAppURL + ` (SENDIO_APP_URL overrides it).`,
		Example: `  sendio inbox connect
  sendio inbox connect signup-bot@acme.sendioinbox.com --open
  sendio inbox connect --json | jq -r .link`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.freeConnect(args, openLink)
		},
	}
	connect.Flags().BoolVar(&openLink, "open", false, "also try to open the link in a browser")
	g.AddCommand(connect)

	// recover
	g.AddCommand(&cobra.Command{
		Use:     "recover <phrase> <address>",
		Short:   "Restore a free inbox on this machine from its recovery phrase",
		Example: "  sendio inbox recover aaaq-eaye-auda-… signup-bot@acme.sendioinbox.com",
		Args:    exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := a.freeClient(a.freeRelay(relay, nil))
			kf, err := freeinbox.Recover(cmd.Context(), c, a.freeStore(), args[0], args[1])
			if err != nil {
				return freeErr(err)
			}
			path := a.freeStore().KeyPath(kf.Address)
			return a.P().Result(map[string]any{"address": kf.Address, "key_path": path, "relay": kf.Relay}, func(w io.Writer) {
				a.P().Success("Recovered %s", a.P().S().Bold.Render(kf.Address))
				fmt.Fprintln(w, a.P().S().Dim.Render("  Key saved to "+path))
			})
		},
	})

	// remove
	g.AddCommand(&cobra.Command{
		Use:     "remove <address>",
		Aliases: []string{"rm"},
		Short:   "Delete a free inbox's key and mail from this machine (local only)",
		Long: `Delete the local key, state and saved mail of a free inbox. The inbox
itself stays on the relay; without its recovery phrase it cannot be read
again. Requires confirmation (--yes in scripts).`,
		Args:        exactArgs(1),
		Annotations: map[string]string{annDestructive: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			addr := strings.ToLower(strings.TrimSpace(args[0]))
			st := a.freeStore()
			if !st.Has(addr) {
				return freeErr(fmt.Errorf("%w: %s", freeinbox.ErrNoInbox, addr))
			}
			if err := a.Confirm(fmt.Sprintf("Delete the key and mail of %s from this machine? Without the recovery phrase it cannot be read again.", addr)); err != nil {
				return err
			}
			if err := st.Remove(addr); err != nil {
				return freeErr(err)
			}
			return a.P().Result(map[string]any{"removed": addr}, func(w io.Writer) { a.P().Success("Removed %s from this machine", addr) })
		},
	})
}

// isPoolAddress reports whether address is on a free pool domain.
func isPoolAddress(address string) bool {
	_, _, pool, ok := freeinbox.SplitAddress(address)
	return ok && strings.HasPrefix(pool, "sendioinbox.")
}

func (a *App) freeCreate(ctx context.Context, relayFlag, name, org, address string) error {
	p := a.P()
	c := a.freeClient(a.freeRelay(relayFlag, nil))
	st := a.freeStore()
	opt := freeinbox.CreateOptions{Name: name, Org: org, Address: address}
	if address == "" && (name == "" || org == "") {
		if !a.Interactive() {
			return api.UsageError("--name and --org are required (or --address)")
		}
		if err := a.promptNameOrg(&opt.Name, &opt.Org); err != nil {
			return err
		}
	}
	// On a terminal, check first so a taken name can be swapped for a suggestion.
	if a.Interactive() && opt.Address == "" {
		for {
			n, o := freeinbox.Normalize(opt.Name), freeinbox.Normalize(opt.Org)
			sp := p.Spin("Checking "+n+"@"+o, 150*time.Millisecond)
			chk, err := c.Check(ctx, n, o)
			sp.Stop()
			if err != nil {
				return freeErr(err)
			}
			s := p.ES()
			if chk.Available || (chk.Reason == "org_owned" && freeinbox.OwnsOrg(st, chk.Address)) {
				fmt.Fprintf(a.Stderr, "%s %s is available\n", s.OK.Render("✓"), s.Bold.Render(chk.Address))
				opt.Address, opt.HoldToken = chk.Address, chk.HoldToken
				break
			}
			fmt.Fprintf(a.Stderr, "%s %s is not available (%s)\n", s.Warn.Render("✗"), s.Bold.Render(chk.Address), chk.Reason)
			const other = "\x00other"
			var pick string
			var opts []huh.Option[string]
			for _, sug := range chk.Suggestions {
				opts = append(opts, huh.NewOption(sug, sug))
			}
			opts = append(opts, huh.NewOption("Try another name", other))
			err = huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Pick an available address").Options(opts...).Value(&pick))).
				WithTheme(a.huhTheme()).WithShowHelp(false).Run()
			if err != nil {
				return &api.Error{Exit: api.ExitUsage, Code: "canceled", Message: "Canceled."}
			}
			if pick != other {
				opt.Address = pick
				break
			}
			if err := a.promptNameOrg(&opt.Name, &opt.Org); err != nil {
				return err
			}
		}
	}
	var sp *out.Spinner
	opt.OnPoW = func(bits int) { sp = p.Spin("Solving proof of work", 100*time.Millisecond) }
	res, err := freeinbox.Create(ctx, c, st, opt)
	if sp != nil {
		sp.Stop()
	}
	if err != nil {
		return freeErr(err)
	}
	machine := map[string]any{
		"address": res.Address, "created_at": res.CreatedAt, "hold_hours": res.HoldHours, "daily_limit": res.DailyLimit,
		"max_message_bytes": res.MaxMessageBytes, "relay": c.Relay, "key_path": res.KeyPath, "recovery_phrase": res.RecoveryPhrase,
		"warning": "The recovery phrase is shown once. It is the only way to read this inbox elsewhere; send.io cannot recover it.",
	}
	return p.Result(machine, func(w io.Writer) {
		s := p.S()
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s %s\n", s.OK.Render("✓"), s.Bold.Render(res.Address))
		fmt.Fprintln(w, s.Dim.Render(fmt.Sprintf("  Mail is held %d h until fetched · %d messages a day · %s each · receive only",
			orDefault(res.HoldHours, 72), orDefault(res.DailyLimit, 100), humanBytes(orDefault(res.MaxMessageBytes, 10485760)))))
		fmt.Fprintln(w, s.Dim.Render("  Key: "+res.KeyPath))
		fmt.Fprintln(w)
		fmt.Fprintln(w, s.Accent.Render("Recovery phrase")+s.Dim.Render(" (shown once)"))
		if p.Fancy() {
			fmt.Fprintln(w, "  "+s.Select.Render(" "+res.RecoveryPhrase+" "))
		} else {
			fmt.Fprintln(w, "  "+res.RecoveryPhrase)
		}
		fmt.Fprintln(w, s.Warn.Render("  ! Save it somewhere safe. It is the only way to read this inbox on another machine; send.io cannot recover it."))
		fmt.Fprintln(w)
		fmt.Fprintln(w, s.Dim.Render("Next: ")+p.Commands(s, "`sendio inbox watch` · `sendio inbox wait --code`"))
	})
}

func orDefault(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

func humanBytes(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%d MB", n>>20)
	}
	return fmt.Sprintf("%d KB", n>>10)
}

func (a *App) promptNameOrg(name, org *string) error {
	n, o := *name, *org
	err := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Name").Description("The part before @, e.g. signup-bot").Value(&n).
			Validate(func(s string) error { return freeinbox.CheckName(freeinbox.Normalize(s)) }),
		huh.NewInput().Title("Org").Description("Your org label: name@org.sendioinbox.com").Value(&o).
			Validate(func(s string) error { return freeinbox.CheckOrg(freeinbox.Normalize(s)) }),
	)).WithTheme(a.huhTheme()).WithShowHelp(false).Run()
	if err != nil {
		return &api.Error{Exit: api.ExitUsage, Code: "canceled", Message: "Canceled."}
	}
	*name, *org = n, o
	return nil
}

func (a *App) freeList() error {
	st := a.freeStore()
	list, err := st.List()
	if err != nil {
		return freeErr(err)
	}
	rows := []map[string]any{}
	for _, k := range list {
		mail, _ := st.Mail(k.Address)
		state, _ := st.LoadState(k.Address)
		rows = append(rows, map[string]any{"address": k.Address, "created_at": k.CreatedAt, "messages": len(mail), "cursor": state.Cursor, "relay": k.Relay})
	}
	return a.P().Result(map[string]any{"data": rows, "root": st.Root}, func(w io.Writer) {
		if len(rows) == 0 {
			fmt.Fprintln(w, "No free inboxes on this machine. Create one with "+a.P().Commands(a.P().S(), "`sendio inbox create`")+".")
			return
		}
		var tr [][]string
		for _, r := range rows {
			tr = append(tr, []string{fmt.Sprint(r["address"]), a.clock(fmt.Sprint(r["created_at"])), fmt.Sprint(r["messages"]), fmt.Sprint(r["relay"])})
		}
		a.P().Table(w, []string{"ADDRESS", "CREATED", "MAIL", "RELAY"}, tr)
	})
}

// freeRecord is the JSON shape of a message: the index plus where it is.
func freeRecord(d freeinbox.Delivered, withText bool) map[string]any {
	idx := d.Index
	m := map[string]any{
		"address": d.Address, "seq": idx.Seq, "id": idx.ID, "received_at": idx.ReceivedAt, "from": idx.From, "to": idx.To,
		"subject": idx.Subject, "date": idx.Date, "attachments": idx.Attachments, "risky": idx.Risky(), "path": d.Path,
	}
	if withText {
		p := freeinbox.Parse(d.Raw)
		m["text"] = p.Text
		if c := freeinbox.ExtractCode(idx.Subject, p.Text); c != "" {
			m["code"] = c
		}
	}
	return m
}

// printFreeLine prints one message for people: time, from, subject, and a
// warning per risky attachment.
func (a *App) printFreeLine(d freeinbox.Delivered, showAddr bool) {
	p := a.P()
	s := p.S()
	idx := d.Index
	ts := idx.ReceivedAt
	line := fmt.Sprintf("%s  %s  %s", a.clock(ts), s.Bold.Render(out.Cell(idx.From, 40)), out.Cell(idx.Subject, 60))
	if showAddr {
		line += s.Dim.Render("  → " + d.Address)
	}
	if n := len(idx.Attachments); n > 0 {
		line += s.Dim.Render(fmt.Sprintf("  [%d attachment%s]", n, plural(n)))
	}
	fmt.Fprintln(a.Stdout, line)
	a.warnRisky(d)
}

// warnRisky warns (stderr) about each risky attachment of a message.
func (a *App) warnRisky(d freeinbox.Delivered) {
	for _, at := range d.Index.Attachments {
		if at.Risky {
			a.P().Warn("risky attachment %q (%s) in message %d from %s: saved in %s, not opened. Do not run it.", at.Filename, at.ContentType, d.Index.Seq, d.Index.From, d.Path)
		}
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (a *App) freeWatch(ctx context.Context, args []string, relayFlag string) error {
	st := a.freeStore()
	var inboxes []*freeinbox.Inbox
	if len(args) == 1 {
		in, err := st.OpenInbox(strings.ToLower(strings.TrimSpace(args[0])))
		if err != nil {
			return freeErr(err)
		}
		inboxes = append(inboxes, in)
	} else {
		list, err := st.List()
		if err != nil {
			return freeErr(err)
		}
		for _, k := range list {
			in, err := st.OpenInbox(k.Address)
			if err != nil {
				return freeErr(err)
			}
			inboxes = append(inboxes, in)
		}
		if len(inboxes) == 0 {
			return &api.Error{Exit: api.ExitNotFound, Code: "no_inbox", Message: "There is no free inbox on this machine.", Hint: "Create one with `sendio inbox create`."}
		}
	}
	// One connection per relay.
	byRelay := map[string][]*freeinbox.Inbox{}
	var order []string
	for _, in := range inboxes {
		r := a.freeRelay(relayFlag, in)
		if byRelay[r] == nil {
			order = append(order, r)
		}
		byRelay[r] = append(byRelay[r], in)
	}
	p := a.P()
	multi := len(inboxes) > 1
	recv := &freeinbox.Receiver{Store: st, OnWarn: func(f string, args ...any) { p.Warn(f, args...) }, OnMessage: func(d freeinbox.Delivered) {
		if !d.New {
			return
		}
		if p.Machine {
			_ = p.Line(freeRecord(d, false))
			a.warnRisky(d)
			return
		}
		a.printFreeLine(d, multi)
	}}
	errc := make(chan error, len(order))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, r := range order {
		ins := byRelay[r]
		names := make([]string, len(ins))
		for i, in := range ins {
			names[i] = in.Address
		}
		s := &freeinbox.Stream{Client: a.freeClient(r), Receiver: recv, Inboxes: ins, Sleep: a.sleep, OnEvent: func(e freeinbox.Event) {
			switch e.Kind {
			case "ready":
				if e.Attempt == 0 {
					p.Info("Watching %s for new mail… (^C to quit)", strings.Join(names, ", "))
				}
			case "reconnecting":
				p.Warn("inbox watch: connection lost (%v); reconnecting in %s", e.Err, e.Delay.Round(100*time.Millisecond))
			case "replaced":
				p.Warn("inbox watch: another client connected to %s; this watch stopped receiving it", e.Address)
			case "ended":
				if len(names) > 1 {
					p.Warn("inbox watch: %s; still watching the others", (&freeinbox.EndedError{Address: e.Address, Code: e.Code}).Error())
				}
			}
		}}
		go func() { errc <- s.Run(ctx) }()
	}
	var first error
	for range order {
		if err := <-errc; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return freeErr(first)
}

func (a *App) freeRead(ctx context.Context, args []string, relayFlag string, limit int, id uint64, noSync bool) error {
	in, c, err := a.openFreeInbox(args, relayFlag)
	if err != nil {
		return err
	}
	st := a.freeStore()
	p := a.P()
	if !noSync {
		recv := &freeinbox.Receiver{Store: st, OnWarn: func(f string, args ...any) { p.Warn(f, args...) }}
		sp := p.Spin("Fetching mail", 200*time.Millisecond)
		for {
			ds, err := recv.PollOnce(ctx, c, in, 0, 100)
			if err != nil {
				sp.Stop()
				var ee *freeinbox.EndedError
				if errors.As(freeExplain(ctx, c, in, err), &ee) {
					p.Warn("%s (showing what is saved here)", ee.Error())
					break
				}
				p.Warn("could not fetch waiting mail from %s: %v (showing what is saved here)", c.Relay, err)
				break
			}
			if len(ds) < 100 {
				sp.Stop()
				break
			}
		}
	}
	if id != 0 {
		raw, err := st.RawMail(in.Address, id)
		if err != nil {
			return &api.Error{Exit: api.ExitNotFound, Code: "not_found", Message: fmt.Sprintf("No message %d in %s.", id, in.Address)}
		}
		d := freeinbox.Delivered{Address: in.Address, Index: freeinbox.Index(raw, id, "", ""), Raw: raw, Path: st.EMLPath(in.Address, id)}
		if list, _ := st.Mail(in.Address); list != nil {
			for _, m := range list {
				if m.Seq == id {
					d.Index = m
				}
			}
		}
		rec := freeRecord(d, true)
		return p.Result(rec, func(w io.Writer) {
			s := p.S()
			fmt.Fprintf(w, "%s %s\n%s %s\n%s %s\n%s %s\n", s.Dim.Render("From:   "), d.Index.From, s.Dim.Render("To:     "), d.Index.To,
				s.Dim.Render("Subject:"), s.Bold.Render(d.Index.Subject), s.Dim.Render("Date:   "), a.clock(d.Index.ReceivedAt))
			for _, at := range d.Index.Attachments {
				mark := ""
				if at.Risky {
					mark = s.Warn.Render("  ! risky: do not open")
				}
				fmt.Fprintf(w, "%s %s (%s, %d bytes)%s\n", s.Dim.Render("Attach: "), at.Filename, at.ContentType, at.Size, mark)
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w, strings.TrimSpace(fmt.Sprint(rec["text"])))
			fmt.Fprintln(w)
			fmt.Fprintln(w, s.Dim.Render("Raw message: "+d.Path))
		})
	}
	list, err := st.Mail(in.Address)
	if err != nil {
		return freeErr(err)
	}
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	recs := []map[string]any{}
	for _, idx := range list {
		raw, _ := st.RawMail(in.Address, idx.Seq)
		recs = append(recs, freeRecord(freeinbox.Delivered{Address: in.Address, Index: idx, Raw: raw, Path: st.EMLPath(in.Address, idx.Seq)}, true))
	}
	return p.Result(map[string]any{"address": in.Address, "data": recs}, func(w io.Writer) {
		if len(recs) == 0 {
			fmt.Fprintf(w, "No mail in %s yet.\n", in.Address)
			return
		}
		var rows [][]string
		for _, r := range recs {
			att := ""
			if as, _ := r["attachments"].([]freeinbox.Attachment); len(as) > 0 {
				att = strconv.Itoa(len(as))
				if r["risky"] == true {
					att += " !"
				}
			}
			rows = append(rows, []string{fmt.Sprint(r["seq"]), a.clock(fmt.Sprint(r["received_at"])), out.Cell(r["from"], 36), out.Cell(r["subject"], 50), att})
		}
		p.Table(w, []string{"ID", "RECEIVED", "FROM", "SUBJECT", "ATTACH"}, rows)
		fmt.Fprintln(w, p.S().Dim.Render("Show one: ")+p.Commands(p.S(), "`sendio inbox read --id <ID>`"))
	})
}

type freeWaitOpts struct {
	timeout       time.Duration
	from, subject string
	code          bool
	since         time.Duration
}

func (a *App) freeWait(ctx context.Context, args []string, relayFlag string, o freeWaitOpts) error {
	in, c, err := a.openFreeInbox(args, relayFlag)
	if err != nil {
		return err
	}
	if o.timeout <= 0 {
		o.timeout = 120 * time.Second
	}
	p := a.P()
	label := "Waiting for mail at " + in.Address
	if o.code {
		label = "Waiting for a code at " + in.Address
	}
	sp := p.Spin(label, 0)
	wctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	m, err := freeinbox.Wait(wctx, c, a.freeStore(), in, freeinbox.WaitOptions{From: o.from, Subject: o.subject, Code: o.code, Since: o.since},
		func(e freeinbox.Event) {
			if e.Kind == "reconnecting" {
				p.Warn("inbox wait: connection lost (%v); reconnecting in %s", e.Err, e.Delay.Round(100*time.Millisecond))
			}
		}, func(f string, args ...any) { p.Warn(f, args...) })
	sp.Stop()
	if errors.Is(err, freeinbox.ErrWaitTimeout) {
		if ctx.Err() != nil {
			return nil // Ctrl-C
		}
		what := "email"
		if o.code {
			what = "email with a code"
		}
		return &api.Error{Exit: api.ExitAPI, Code: "timeout", Message: fmt.Sprintf("No matching %s arrived at %s within %s.", what, in.Address, o.timeout)}
	}
	if err != nil {
		return freeErr(err)
	}
	rec := freeRecord(m.Delivered, true)
	if o.code {
		rec["code"] = m.Code
		if !a.g.json && a.g.jq == "" {
			fmt.Fprintln(a.Stdout, m.Code)
			return nil
		}
	}
	return p.Result(rec, func(w io.Writer) {
		a.printFreeLine(m.Delivered, false)
		if t := strings.TrimSpace(fmt.Sprint(rec["text"])); t != "" {
			fmt.Fprintln(w)
			fmt.Fprintln(w, t)
		}
	})
}

func (a *App) freePoll(ctx context.Context, args []string, relayFlag string, wait int) error {
	if wait < 0 || wait > 25 {
		return api.UsageError("--wait must be 0 to 25 seconds")
	}
	in, c, err := a.openFreeInbox(args, relayFlag)
	if err != nil {
		return err
	}
	p := a.P()
	recv := &freeinbox.Receiver{Store: a.freeStore(), OnWarn: func(f string, args ...any) { p.Warn(f, args...) }}
	sp := p.Spin("Polling "+in.Address, 200*time.Millisecond)
	ds, err := recv.PollOnce(ctx, c, in, wait, 100)
	sp.Stop()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return freeErr(freeExplain(ctx, c, in, err))
	}
	recs := []map[string]any{}
	for _, d := range ds {
		if d.New {
			recs = append(recs, freeRecord(d, false))
		}
	}
	return p.Result(map[string]any{"address": in.Address, "messages": recs}, func(w io.Writer) {
		if len(recs) == 0 {
			p.Info("No new mail.")
			return
		}
		for _, d := range ds {
			if d.New {
				a.printFreeLine(d, false)
			}
		}
	})
}

// connectInstructions is the human message printed above a connect link.
func connectInstructions(address string) string {
	return "Open this link in a browser to attach " + address + " to your send.io workspace and enable sending. It works once and expires in 30 minutes."
}

func (a *App) freeConnect(args []string, open bool) error {
	addr, err := a.freeAddress(args)
	if err != nil {
		return err
	}
	in, err := a.freeStore().OpenInbox(addr)
	if err != nil {
		return freeErr(err)
	}
	cl, err := freeinbox.NewConnectLink(a.freeAppURL(), in, a.now())
	if err != nil {
		return freeErr(err)
	}
	opened := false
	if open && a.OpenURL != nil {
		opened = a.OpenURL(cl.Link) == nil // best effort
	}
	p := a.P()
	machine := map[string]any{"address": cl.Address, "link": cl.Link, "expires_at": cl.ExpiresAt.Format(time.RFC3339)}
	return p.Result(machine, func(w io.Writer) {
		fmt.Fprintln(w, connectInstructions(cl.Address))
		fmt.Fprintln(w)
		fmt.Fprintln(w, cl.Link)
		fmt.Fprintln(w)
		if opened {
			fmt.Fprintln(w, p.S().Dim.Render("Opened in your browser."))
		} else if open {
			fmt.Fprintln(w, p.S().Dim.Render("Could not open a browser here; copy the link instead."))
		}
		fmt.Fprintln(w, p.S().Dim.Render("Anyone with the link can take this inbox: share it only with its owner."))
	})
}
