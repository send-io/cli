package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/auth"
	"send.io/cli/internal/config"
)

// oauthSource is the TokenSource of an OAuth profile: it refreshes the
// access token shortly before it expires and after a 401, and saves the
// rotated pair.
type oauthSource struct {
	app     *App
	profile *config.Profile
	secret  config.Secret
	base    string
	mu      sync.Mutex
}

func (o *oauthSource) Token(ctx context.Context) (string, error) {
	o.mu.Lock()
	expiring := !o.secret.ExpiresAt.IsZero() && o.app.now().Add(60*time.Second).After(o.secret.ExpiresAt) && o.secret.RefreshToken != ""
	o.mu.Unlock()
	if expiring {
		if _, err := o.Refresh(ctx); err != nil {
			return "", err
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.secret.AccessToken, nil
}

func (o *oauthSource) Refresh(ctx context.Context) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.secret.RefreshToken == "" {
		return false, nil
	}
	tok, err := auth.Refresh(ctx, o.app.baseHTTP(), o.base, o.secret.RefreshToken)
	if err != nil {
		var e *api.Error
		if errors.As(err, &e) && e.Exit != api.ExitNetwork {
			return false, &api.Error{Exit: api.ExitAuth, Code: "session_expired", Message: "Your sign-in expired or was revoked (" + e.Message + ").",
				Hint: "Run `sendio login` again."}
		}
		return false, err
	}
	o.secret.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		o.secret.RefreshToken = tok.RefreshToken
	}
	o.secret.ExpiresAt = tok.ExpiresAt(o.app.now())
	if _, err := o.app.Store().PutSecret(o.profile, o.secret); err != nil {
		o.app.P().Warn("could not save the refreshed token: %v", err)
	}
	return true, nil
}

// whoamiResult is GET /v0/cli/whoami (contract section 1).
type whoamiResult struct {
	Team struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	Permission string `json:"permission"`
	Auth       string `json:"auth"`
	KeyName    string `json:"key_name"`
	UserEmail  string `json:"user_email"`
	APIBase    string `json:"api_base"`
}

// whoami asks the API who the credential belongs to. When the API does not
// offer /v0/cli/whoami yet (404) and the credential is an API key, it falls
// back to GET /api-keys (full-access keys; also names the key by its
// prefix) and then GET /domains, and says so in "source"/"note".
func (a *App) whoami(ctx context.Context, c *api.Client, token, kind string) (map[string]any, error) {
	resp, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/v0/cli/whoami"})
	if err == nil {
		v, _ := resp.Value()
		m, _ := v.(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		m["source"] = "whoami"
		return m, nil
	}
	if !api.IsStatus(err, 404) || kind == config.AuthOAuth {
		return nil, err
	}
	res := map[string]any{"auth": "api_key", "team": nil, "permission": nil, "key_name": nil, "user_email": nil,
		"api_base": c.BaseURL, "source": "fallback",
		"note": "This API does not offer /v0/cli/whoami yet; identity was inferred from GET /api-keys and GET /domains."}
	keys, kerr := c.Do(ctx, &api.Request{Method: "GET", Path: "/api-keys", Query: map[string][]string{"limit": {"100"}}})
	if kerr == nil {
		res["permission"] = "full_access"
		var page struct {
			Data []map[string]any `json:"data"`
		}
		_ = keys.Decode(&page)
		best := ""
		for _, k := range page.Data {
			pfx := str(k, "token_prefix")
			if pfx != "" && strings.HasPrefix(token, pfx) && len(pfx) > len(best) {
				best = pfx
				res["key_name"] = k["name"]
				res["key_id"] = k["id"]
				if p := str(k, "permission"); p != "" {
					res["permission"] = p
				}
			}
		}
		return res, nil
	}
	if ae := api.AsError(kerr); ae.Exit == api.ExitNetwork {
		return nil, kerr
	}
	if _, derr := c.Do(ctx, &api.Request{Method: "GET", Path: "/domains", Query: map[string][]string{"limit": {"1"}}}); derr == nil {
		res["permission"] = "full_access"
		return res, nil
	} else if api.AsError(derr).Status == 401 {
		return nil, derr
	}
	res["permission"] = "sending_access"
	res["note"] = "This API does not offer /v0/cli/whoami yet and the key cannot read /api-keys or /domains: it is probably a sending-only key."
	return res, nil
}

func (a *App) addAuthCommands(root *cobra.Command) {
	var device, noBrowser bool
	var timeout time.Duration
	login := &cobra.Command{
		Use:   "login",
		Short: "Sign in (browser, device code over SSH, or --api-key mx_…)",
		Long: `Sign in to send.io and save the credential in a profile.

By default a browser opens on send.io (OAuth with PKCE on a 127.0.0.1
loopback port). Over SSH or without a display the device flow is used: you
get a code to approve at send.io/oauth/device from any browser. With
--api-key mx_… the key is stored instead.

Credentials go to the OS keyring, or to ~/.config/sendio/credentials.json
(mode 0600) when no keyring is available or with --insecure-storage.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.login(cmd.Context(), device, noBrowser, timeout)
		},
	}
	login.Flags().BoolVar(&device, "device", false, "use the device code flow (no local browser needed)")
	login.Flags().BoolVar(&noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	login.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait for the sign-in")
	root.AddCommand(login)

	root.AddCommand(&cobra.Command{
		Use:   "logout",
		Short: "Sign out of the selected profile (revokes OAuth tokens, deletes stored credentials)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.logout(cmd.Context(), a.ProfileName())
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "whoami",
		Short: "Show the team, permission and credential in use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.Client(true)
			if err != nil {
				return err
			}
			cred, _ := a.credential()
			m, err := a.whoami(cmd.Context(), c, cred.token, cred.kind)
			if err != nil {
				return err
			}
			m["profile"] = a.ProfileName()
			m["credential_source"] = cred.source
			return a.P().Result(m, func(w io.Writer) { a.printWhoami(w, m) })
		},
	})

	authCmd := a.group(root, []string{"auth"})
	authCmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List saved profiles", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.Config()
			if err != nil {
				return err
			}
			current := a.ProfileName()
			var items []any
			for _, n := range cfg.Names() {
				p := cfg.Profiles[n]
				items = append(items, map[string]any{"name": n, "current": n == current, "team": p.TeamName, "auth": p.Auth,
					"base_url": p.BaseURL, "storage": p.Storage, "permission": p.Permission})
			}
			if items == nil {
				items = []any{}
			}
			return a.P().Result(map[string]any{"current": current, "profiles": items}, func(w io.Writer) {
				if len(items) == 0 {
					fmt.Fprintln(w, "No profiles. Run `sendio login`.")
					return
				}
				rows := [][]string{}
				for _, it := range items {
					m := it.(map[string]any)
					mark := " "
					if m["current"] == true {
						mark = a.P().S().OK.Render("*")
					}
					rows = append(rows, []string{mark, str(m, "name"), dash(str(m, "team")), str(m, "auth"), str(m, "base_url"), str(m, "storage")})
				}
				a.P().Table(w, []string{" ", "profile", "team", "auth", "base url", "storage"}, rows)
			})
		},
	})
	authCmd.AddCommand(&cobra.Command{
		Use: "switch <profile>", Aliases: []string{"use"}, Short: "Make a profile the default", Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.Config()
			if err != nil {
				return err
			}
			if _, ok := cfg.Profiles[args[0]]; !ok {
				return api.Errorf(api.ExitNotFound, "profile_not_found", "no profile named %q (see `sendio auth list`)", args[0])
			}
			cfg.Current = args[0]
			if err := a.Store().Save(cfg); err != nil {
				return err
			}
			return a.P().Result(map[string]any{"current": args[0]}, func(w io.Writer) { a.P().Success("Now using profile %s.", args[0]) })
		},
	})
	authCmd.AddCommand(&cobra.Command{
		Use: "status", Short: "Show which credential is used and check it against the API", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cred, err := a.credential()
			if err != nil {
				return err
			}
			st := map[string]any{"profile": a.ProfileName(), "base_url": a.BaseURL(), "credential_source": cred.source, "auth": cred.kind, "authenticated": false}
			if cred.profile != nil {
				st["storage"] = cred.profile.Storage
				st["team"] = cred.profile.TeamName
				if !cred.secret.ExpiresAt.IsZero() {
					st["access_token_expires_at"] = cred.secret.ExpiresAt.UTC().Format(time.RFC3339)
				}
			}
			if cred.token == "" {
				st["error"] = "not signed in"
				_ = a.P().Result(st, func(w io.Writer) { a.printStatus(w, st) })
				return &api.Error{Exit: api.ExitAuth, Code: "not_authenticated", Message: "You are not signed in.", Hint: "Run `sendio login`."}
			}
			c, err := a.Client(true)
			if err != nil {
				return err
			}
			m, err := a.whoami(cmd.Context(), c, cred.token, cred.kind)
			if err != nil {
				return err
			}
			st["authenticated"] = true
			st["whoami"] = m
			return a.P().Result(st, func(w io.Writer) { a.printStatus(w, st) })
		},
	})
	authCmd.AddCommand(&cobra.Command{
		Use: "remove <profile>", Aliases: []string{"rm"}, Short: "Delete a saved profile and its credentials", Args: exactArgs(1),
		Annotations: map[string]string{annDestructive: "true", annWrite: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.Config()
			if err != nil {
				return err
			}
			if _, ok := cfg.Profiles[args[0]]; !ok {
				return api.Errorf(api.ExitNotFound, "profile_not_found", "no profile named %q", args[0])
			}
			if err := a.Confirm(fmt.Sprintf("Remove profile %s and its stored credentials?", args[0])); err != nil {
				return err
			}
			return a.logout(cmd.Context(), args[0])
		},
	})
	authCmd.AddCommand(&cobra.Command{
		Use: "rename <old> <new>", Aliases: []string{"mv"}, Short: "Rename a saved profile", Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.Config()
			if err != nil {
				return err
			}
			if !config.ValidName(args[1]) {
				return api.UsageError("profile names use letters, digits, '-', '_' and '.' (max 64)")
			}
			if err := a.Store().Rename(cfg, args[0], args[1]); err != nil {
				return api.Errorf(api.ExitUsage, "rename_failed", "%v", err)
			}
			return a.P().Result(map[string]any{"renamed": args[0], "to": args[1]}, func(w io.Writer) { a.P().Success("Renamed %s to %s.", args[0], args[1]) })
		},
	})
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *App) printWhoami(w io.Writer, m map[string]any) {
	team := ""
	if t, ok := m["team"].(map[string]any); ok {
		team = str(t, "name")
		if id := str(t, "id"); id != "" {
			team += a.P().S().Dim.Render(" (" + id + ")")
		}
	}
	rows := []struct{ k, v string }{
		{"team", dash(team)}, {"permission", dash(str(m, "permission"))}, {"auth", dash(str(m, "auth"))},
		{"key", dash(str(m, "key_name"))}, {"user", dash(str(m, "user_email"))}, {"api", dash(str(m, "api_base"))},
		{"profile", str(m, "profile")}, {"credential", str(m, "credential_source")},
	}
	for _, r := range rows {
		fmt.Fprintf(w, "%s  %s\n", a.P().S().Dim.Render(fmt.Sprintf("%-10s", r.k)), r.v)
	}
	if n := str(m, "note"); n != "" {
		fmt.Fprintln(w, a.P().S().Warn.Render("note: ")+n)
	}
}

func (a *App) printStatus(w io.Writer, st map[string]any) {
	s := a.P().S()
	if st["authenticated"] == true {
		fmt.Fprintln(w, s.OK.Render("✓")+" Signed in")
	} else {
		fmt.Fprintln(w, s.Err.Render("✗")+" Not signed in")
	}
	for _, k := range []string{"profile", "base_url", "credential_source", "auth", "storage", "team", "access_token_expires_at"} {
		if v := str(st, k); v != "" {
			fmt.Fprintf(w, "  %s  %s\n", s.Dim.Render(fmt.Sprintf("%-24s", k)), v)
		}
	}
	if m, ok := st["whoami"].(map[string]any); ok {
		fmt.Fprintln(w)
		a.printWhoami(w, m)
	}
}

// useDeviceFlow decides when the browser flow cannot work.
func (a *App) useDeviceFlow() (bool, string) {
	if a.env("SSH_CONNECTION") != "" || a.env("SSH_TTY") != "" || a.env("SSH_CLIENT") != "" {
		return true, "SSH session detected"
	}
	if runtime.GOOS == "linux" && a.env("DISPLAY") == "" && a.env("WAYLAND_DISPLAY") == "" && a.env("BROWSER") == "" {
		return true, "no display detected"
	}
	return false, ""
}

func (a *App) login(ctx context.Context, device, noBrowser bool, timeout time.Duration) error {
	name := a.ProfileName()
	if !config.ValidName(name) {
		return api.UsageError("invalid profile name %q", name)
	}
	a.printLogo(a.Stderr, "Sign in to send.io", "", false)
	cfg, err := a.Config()
	if err != nil {
		return err
	}
	base := a.BaseURL()
	p := &config.Profile{Name: name, BaseURL: base, CreatedAt: a.now().UTC().Format(time.RFC3339)}
	if old, ok := cfg.Profiles[name]; ok {
		a.Store().DeleteSecret(old)
	}
	var sec config.Secret
	key := a.g.apiKey
	pr := a.P()
	if key != "" {
		if !strings.HasPrefix(key, "mx_") {
			pr.Warn("send.io API keys start with mx_; storing it anyway")
		}
		p.Auth = config.AuthAPIKey
		sec.APIKey = key
	} else {
		host := "sendio-cli"
		if a.Hostname != nil {
			if h, err := a.Hostname(); err == nil && h != "" {
				host = h
			}
		}
		p.Device = auth.SanitizeDeviceName(host)
		flow := &auth.Flow{APIBase: base, HTTP: a.baseHTTP(), DeviceName: p.Device, Sleep: a.Sleep,
			Notify: func(s string) {
				if !pr.Machine {
					fmt.Fprintln(a.Stderr, s)
				}
			}}
		if !noBrowser && a.OpenURL != nil {
			flow.OpenBrowser = a.OpenURL
		}
		if a.env("SENDIO_LOGIN_LISTEN") != "" {
			flow.ListenAddr = a.env("SENDIO_LOGIN_LISTEN")
		}
		lctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		auto, why := a.useDeviceFlow()
		var tok *auth.Token
		if device || auto {
			if auto && !device {
				pr.Info("%s: using the device code flow.", why)
			}
			tok, err = flow.Device(lctx, func(dc auth.DeviceCode) {
				if pr.Machine {
					b, _ := jsonLine(map[string]any{"event": "device_code", "user_code": dc.UserCode, "verification_uri": dc.VerificationURI,
						"verification_uri_complete": dc.VerificationURIComplete, "expires_in": dc.ExpiresIn})
					fmt.Fprintln(a.Stderr, b)
					return
				}
				s := pr.ES()
				fmt.Fprintf(a.Stderr, "To sign in, open %s\nand enter the code %s\n", s.Link.Render(dc.VerificationURI), s.Bold.Render(dc.UserCode))
				if dc.VerificationURIComplete != "" {
					fmt.Fprintf(a.Stderr, "%s\n", s.Dim.Render("(or open "+dc.VerificationURIComplete+")"))
				}
				fmt.Fprintln(a.Stderr, s.Dim.Render("Waiting for approval…"))
			})
		} else {
			tok, err = flow.Browser(lctx)
		}
		if err != nil {
			return err
		}
		p.Auth = config.AuthOAuth
		sec.AccessToken, sec.RefreshToken, sec.ExpiresAt = tok.AccessToken, tok.RefreshToken, tok.ExpiresAt(a.now())
	}

	// Ask the API who this is before saving.
	c := &api.Client{BaseURL: base, HTTP: a.baseHTTP(), UserAgent: userAgent()}
	tokenNow := sec.APIKey
	if p.Auth == config.AuthOAuth {
		tokenNow = sec.AccessToken
	}
	c.Auth = api.StaticToken(tokenNow)
	who, werr := a.whoami(ctx, c, tokenNow, p.Auth)
	if werr != nil {
		if api.AsError(werr).Exit == api.ExitAuth {
			return &api.Error{Exit: api.ExitAuth, Code: "invalid_credentials", Message: "The API rejected this credential: " + werr.Error()}
		}
		pr.Warn("could not check the credential (%v); saving it anyway", werr)
	} else {
		if t, ok := who["team"].(map[string]any); ok {
			p.TeamID, p.TeamName = str(t, "id"), str(t, "name")
		}
		p.Permission, p.KeyName, p.UserEmail = str(who, "permission"), str(who, "key_name"), str(who, "user_email")
	}
	a.store = nil // re-read --insecure-storage
	fell, err := a.Store().PutSecret(p, sec)
	if err != nil {
		return api.Errorf(api.ExitAPI, "storage_error", "saving credentials: %v", err)
	}
	if fell {
		pr.Warn("no OS keyring available; credentials saved to %s (mode 0600)", a.Store().CredentialsPath())
	}
	cfg.Profiles[name] = p
	cfg.Current = name
	if err := a.Store().Save(cfg); err != nil {
		return api.Errorf(api.ExitAPI, "storage_error", "saving config: %v", err)
	}
	res := map[string]any{"profile": name, "auth": p.Auth, "base_url": base, "team": p.TeamName, "permission": p.Permission, "storage": p.Storage}
	if who != nil {
		res["whoami_source"] = who["source"]
	}
	return pr.Result(res, func(w io.Writer) {
		team := p.TeamName
		if team == "" {
			team = "send.io"
		}
		pr.Success("Logged in to %s as profile %s (%s, stored in %s).", team, name, strings.ReplaceAll(p.Auth, "_", " "), p.Storage)
		if pr.Fancy() && !pr.Quiet {
			s := pr.S()
			fmt.Fprintln(w, "\n"+a.wordmarkChip()+"  "+s.Bold.Render("You're in. Welcome to send.io.")+" "+s.OK.Render("✦"))
			fmt.Fprintln(w, "  "+pr.Commands(s, "Run `sendio` for the home screen, or `sendio emails send --help` to send your first email."))
		}
	})
}

func (a *App) logout(ctx context.Context, name string) error {
	cfg, err := a.Config()
	if err != nil {
		return err
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		return api.Errorf(api.ExitNotFound, "profile_not_found", "no profile named %q (nothing to log out of)", name)
	}
	revoked := false
	if p.Auth == config.AuthOAuth {
		if sec, err := a.Store().GetSecret(p); err == nil {
			hc := a.baseHTTP()
			if sec.RefreshToken != "" {
				revoked = auth.Revoke(ctx, hc, p.BaseURL, sec.RefreshToken) == nil
			}
			if sec.AccessToken != "" {
				_ = auth.Revoke(ctx, hc, p.BaseURL, sec.AccessToken)
			}
		}
	}
	a.Store().DeleteSecret(p)
	delete(cfg.Profiles, name)
	if cfg.Current == name {
		cfg.Current = ""
		if names := cfg.Names(); len(names) > 0 {
			cfg.Current = names[0]
		}
	}
	if err := a.Store().Save(cfg); err != nil {
		return err
	}
	return a.P().Result(map[string]any{"logged_out": name, "revoked": revoked}, func(w io.Writer) {
		a.P().Success("Logged out of profile %s.", name)
	})
}
