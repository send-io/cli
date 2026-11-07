package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	sendio "send.io/sendio-go"

	"send.io/cli/internal/api"
	"send.io/cli/internal/build"
)

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail, skip
	Detail string `json:"detail"`
}

func (a *App) addDoctorCommand(root *cobra.Command) {
	root.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "Check the CLI setup: auth, API reachability, domains (SPF/DKIM/DMARC), webhooks, agents, updates",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return a.doctor(cmd.Context()) },
	})
}

// recordKind buckets a DNS record into SPF, DKIM, DMARC, MX or other.
func recordKind(r sendio.Record) string {
	s := strings.ToUpper(string(r.Record) + " " + r.Name + " " + r.Value)
	switch {
	case strings.Contains(s, "DMARC"):
		return "DMARC"
	case strings.Contains(s, "DKIM") || strings.Contains(strings.ToLower(r.Name), "_domainkey"):
		return "DKIM"
	case strings.Contains(s, "SPF") || strings.Contains(strings.ToLower(r.Value), "v=spf1"):
		return "SPF"
	case strings.ToUpper(r.Type) == "MX":
		return "MX"
	}
	return "OTHER"
}

func worst(a, b string) string {
	rank := map[string]int{"": 0, "verified": 1, "not_started": 2, "pending": 3, "temporary_failure": 4, "failed": 5}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func (a *App) doctor(ctx context.Context) error {
	p := a.P()
	report := map[string]any{"version": build.Version, "commit": build.Commit, "os": runtime.GOOS + "/" + runtime.GOARCH,
		"profile": a.ProfileName(), "base_url": a.BaseURL(), "app_url": AppURL(a.BaseURL())}
	checks := []check{{Name: "version", Status: "ok", Detail: fmt.Sprintf("sendio %s (%s/%s)", build.Version, runtime.GOOS, runtime.GOARCH)}}
	var failure *api.Error

	cred, cerr := a.credential()
	switch {
	case cerr != nil:
		checks = append(checks, check{"credentials", "fail", cerr.Error()})
		failure = api.AsError(cerr)
	case cred.token == "":
		checks = append(checks, check{"credentials", "fail", "not signed in (run `sendio login` or set SENDIO_API_KEY)"})
		failure = &api.Error{Exit: api.ExitAuth, Code: "not_authenticated", Message: "not signed in"}
	default:
		detail := fmt.Sprintf("%s from %s", strings.ReplaceAll(cred.kind, "_", " "), cred.source)
		if cred.profile != nil {
			detail += fmt.Sprintf(" (profile %s, %s)", cred.profile.Name, cred.profile.Storage)
		}
		checks = append(checks, check{"credentials", "ok", detail})
	}

	var c *api.Client
	if failure == nil {
		c, _ = a.Client(true)
		start := time.Now()
		who, err := a.whoami(ctx, c, cred.token, cred.kind)
		lat := time.Since(start)
		if err != nil {
			e := api.AsError(err)
			if e.Exit == api.ExitNetwork {
				checks = append(checks, check{"api", "fail", a.BaseURL() + " unreachable: " + e.Message})
			} else {
				checks = append(checks, check{"api", "ok", fmt.Sprintf("%s reachable (%dms)", a.BaseURL(), lat.Milliseconds())})
				checks = append(checks, check{"auth", "fail", e.Message})
			}
			failure = e
		} else {
			report["whoami"] = who
			checks = append(checks, check{"api", "ok", fmt.Sprintf("%s reachable (%dms)", a.BaseURL(), lat.Milliseconds())})
			team := "unknown team"
			if t, ok := who["team"].(map[string]any); ok && str(t, "name") != "" {
				team = str(t, "name")
			}
			st := "ok"
			detail := fmt.Sprintf("%s, %s", team, dash(str(who, "permission")))
			if who["source"] == "fallback" {
				st = "warn"
				detail += " (inferred: the API has no /v0/cli/whoami yet)"
			}
			checks = append(checks, check{"auth", st, detail})
		}
	}

	if failure == nil {
		sdk := a.SDK(c)
		lim := 100
		list, err := sdk.Domains.ListWithOptions(ctx, &sendio.ListOptions{Limit: &lim})
		if err != nil {
			e := api.AsError(err)
			st := "warn"
			if e.Exit == api.ExitAuth {
				checks = append(checks, check{"domains", "skip", "not readable with this credential (" + e.Message + ")"})
			} else {
				checks = append(checks, check{"domains", st, e.Message})
			}
		} else {
			doms := []any{}
			verified := 0
			for i, d := range list.Data {
				entry := map[string]any{"id": d.Id, "name": d.Name, "status": d.Status}
				if d.Status == "verified" {
					verified++
				}
				if i < 20 {
					if full, err := sdk.Domains.GetWithContext(ctx, d.Id); err == nil {
						kinds := map[string]string{}
						for _, r := range full.Records {
							k := recordKind(r)
							kinds[k] = worst(kinds[k], r.Status)
						}
						for _, k := range []string{"SPF", "DKIM", "DMARC", "MX"} {
							if v, ok := kinds[k]; ok {
								entry[strings.ToLower(k)] = v
							}
						}
					}
				}
				doms = append(doms, entry)
			}
			report["domains"] = doms
			st := "ok"
			detail := fmt.Sprintf("%d domain(s), %d verified", len(list.Data), verified)
			if len(list.Data) == 0 {
				st, detail = "warn", "no domains yet (sendio domains add <domain>)"
			} else if verified == 0 {
				st = "warn"
			}
			checks = append(checks, check{"domains", st, detail})
		}
		if resp, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/webhooks", Query: map[string][]string{"limit": {"100"}}}); err == nil {
			var page struct {
				Data    []map[string]any `json:"data"`
				HasMore bool             `json:"has_more"`
			}
			_ = resp.Decode(&page)
			enabled := 0
			for _, w := range page.Data {
				if str(w, "status") != "disabled" {
					enabled++
				}
			}
			n := fmt.Sprint(len(page.Data))
			if page.HasMore {
				n += "+"
			}
			report["webhooks"] = len(page.Data)
			checks = append(checks, check{"webhooks", "ok", fmt.Sprintf("%s webhook(s), %d enabled", n, enabled)})
		} else {
			checks = append(checks, check{"webhooks", "skip", api.AsError(err).Message})
		}
	}

	agents := []string{}
	for _, t := range a.detectAgents() {
		if t.Detected {
			agents = append(agents, t.Name)
		}
	}
	report["agents"] = agents
	ad := "none detected"
	if len(agents) > 0 {
		ad = strings.Join(agents, ", ") + " (sendio agent setup installs the skill)"
	}
	if env := a.detectAgentEnv(); env != "" {
		ad += "; running inside an agent (" + env + ")"
	}
	checks = append(checks, check{"agents", "ok", ad})

	if latest := a.latestVersion(ctx, 3*time.Second); latest != "" {
		report["latest_version"] = latest
		if newer(latest, build.Version) {
			checks = append(checks, check{"update", "warn", "sendio " + latest + " is available"})
		} else {
			checks = append(checks, check{"update", "ok", "up to date"})
		}
	} else {
		checks = append(checks, check{"update", "skip", "update check disabled or unavailable"})
	}
	report["checks"] = checks
	ok := true
	for _, ch := range checks {
		if ch.Status == "fail" {
			ok = false
		}
	}
	report["ok"] = ok
	if err := p.Result(report, func(w io.Writer) { a.printDoctor(w, checks, report) }); err != nil {
		return err
	}
	if failure != nil {
		return failure
	}
	return nil
}

func (a *App) printDoctor(w io.Writer, checks []check, report map[string]any) {
	s := a.P().S()
	icon := map[string]string{"ok": s.OK.Render("✓"), "warn": s.Warn.Render("!"), "fail": s.Err.Render("✗"), "skip": s.Dim.Render("-")}
	for _, c := range checks {
		fmt.Fprintf(w, "%s %s %s\n", icon[c.Status], s.Bold.Render(fmt.Sprintf("%-12s", c.Name)), c.Detail)
	}
	if doms, ok := report["domains"].([]any); ok && len(doms) > 0 {
		fmt.Fprintln(w)
		rows := [][]string{}
		for _, d := range doms {
			m := d.(map[string]any)
			rows = append(rows, []string{str(m, "name"), a.statusCell(str(m, "status")), a.statusCell(str(m, "spf")), a.statusCell(str(m, "dkim")), a.statusCell(str(m, "dmarc"))})
		}
		a.P().Table(w, []string{"domain", "status", "spf", "dkim", "dmarc"}, rows)
	}
}
