package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	sendio "github.com/send-io/sendio-go"
	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/out"
)

func (a *App) addDomainCommands(root *cobra.Command) {
	domains := a.group(root, []string{"domains"})

	var region, returnPath string
	var auto, receiving bool
	add := &cobra.Command{
		Use:   "add <domain>",
		Short: "Add a sending domain and show the DNS records to publish (--auto: one-click DNS setup)",
		Long: `Add a sending domain and print the DNS records to publish.

--auto asks the API for a one-click setup link (Domain Connect: the DNS
provider writes the records after you approve) and opens it when the
domain's DNS provider supports it; otherwise the records are printed.

Then run: sendio domains verify <id> --watch

API: POST /domains, GET /domains/{id}/domain-connect`,
		Example:     "  sendio domains add mail.example.com\n  sendio domains add example.com --region eu-west-1 --auto\n  sendio domains add inbound.example.com --receiving",
		Args:        exactArgs(1),
		Annotations: map[string]string{annTool: "create-domain", annMethod: "POST", annPath: "/domains", annWrite: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			body := map[string]any{"name": args[0]}
			if region != "" {
				body["region"] = region
			}
			if returnPath != "" {
				body["custom_return_path"] = returnPath
			}
			if receiving {
				body["receiving"] = true
			}
			c, err := a.Client(true)
			if err != nil {
				return err
			}
			resp, err := c.Do(ctx, &api.Request{Method: "POST", Path: "/domains", JSON: body})
			if err != nil {
				return err
			}
			v, _ := resp.Value()
			dom, _ := v.(map[string]any)
			res := map[string]any{"domain": dom}
			var dc map[string]any
			if auto {
				dc = a.domainConnect(ctx, c, str(dom, "id"))
				res["domain_connect"] = dc
				if dc["supported"] == true && str(dc, "url") != "" && a.OpenURL != nil && !a.P().Machine {
					res["opened"] = a.OpenURL(str(dc, "url")) == nil
				}
			}
			return a.P().Result(res, func(w io.Writer) {
				p := a.P()
				p.Success("Added %s (%s, status %s).", str(dom, "name"), str(dom, "id"), str(dom, "status"))
				if dc != nil {
					if dc["supported"] == true {
						fmt.Fprintf(w, "\nOne-click setup with %s:\n  %s\n", dash(str(dc, "provider")), str(dc, "url"))
						if res["opened"] == true {
							fmt.Fprintln(w, p.S().Dim.Render("Opened in your browser; approve the records there."))
						}
					} else {
						fmt.Fprintf(w, "\n%s\n", p.S().Dim.Render("One-click setup is not available for this domain ("+dash(str(dc, "reason"))+"); publish the records below."))
					}
				}
				fmt.Fprintln(w)
				a.printZone(w, str(dom, "dns_zone"))
				a.printRecords(w, dom["records"])
				fmt.Fprintf(w, "\nThen run: sendio domains verify %s --watch\n", str(dom, "id"))
			})
		},
	}
	add.Flags().StringVar(&region, "region", "", "sending region")
	add.Flags().StringVar(&returnPath, "custom-return-path", "", "Return-Path subdomain label (one-click setup needs the default)")
	add.Flags().BoolVar(&auto, "auto", false, "open the one-click DNS setup link when the DNS provider supports it")
	add.Flags().BoolVar(&receiving, "receiving", false, "also receive email for the domain (adds an MX record, which redirects mail it already receives elsewhere)")
	domains.AddCommand(add)

	var watch bool
	var timeout, interval time.Duration
	verify := &cobra.Command{
		Use:   "verify <id>",
		Short: "Check a domain's DNS records now (--watch: until verified)",
		Long: `Ask the API to check the domain's DNS records and show each record's status.
--watch repeats the check until the domain is verified (exit 0) or --timeout
passes (exit 7).

API: POST /domains/{id}/verify, GET /domains/{id}  (MCP tool: verify-domain)`,
		Args:        exactArgs(1),
		Annotations: map[string]string{annTool: "verify-domain", annMethod: "POST", annPath: "/domains/{id}/verify", annWrite: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.verifyDomain(cmd.Context(), args[0], watch, timeout, interval)
		},
	}
	verify.Flags().BoolVar(&watch, "watch", false, "keep checking until the domain is verified")
	verify.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long --watch waits")
	verify.Flags().DurationVar(&interval, "interval", 15*time.Second, "time between checks with --watch")
	domains.AddCommand(verify)

	domains.AddCommand(&cobra.Command{
		Use:         "connect <id>",
		Short:       "Get the one-click DNS setup (Domain Connect) link for a domain",
		Args:        exactArgs(1),
		Annotations: map[string]string{annMethod: "GET", annPath: "/domains/{id}/domain-connect"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.Client(true)
			if err != nil {
				return err
			}
			resp, err := c.Do(cmd.Context(), &api.Request{Method: "GET", Path: "/domains/" + url.PathEscape(args[0]) + "/domain-connect"})
			if err != nil {
				return err
			}
			v, _ := resp.Value()
			return a.P().Result(v, func(w io.Writer) { a.renderHuman(w, nil, v) })
		},
	})
}

// domainConnect fetches the one-click setup answer; failures become data.
func (a *App) domainConnect(ctx context.Context, c *api.Client, id string) map[string]any {
	resp, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/domains/" + url.PathEscape(id) + "/domain-connect"})
	if err != nil {
		return map[string]any{"supported": false, "reason": "unavailable: " + err.Error()}
	}
	var m map[string]any
	if resp.Decode(&m) != nil || m == nil {
		return map[string]any{"supported": false, "reason": "invalid response"}
	}
	return m
}

// printZone prints the DNS zone the record hosts are relative to (SENDIO-341).
func (a *App) printZone(w io.Writer, zone string) {
	if zone != "" {
		fmt.Fprintf(w, "DNS zone: %s\n", zone)
	}
}

// recordHost is what to type into the DNS provider: the record name relative
// to the domain's DNS zone. For mail.example.com in the zone example.com the
// return path is "send.mail", not "send". An API that does not return host
// yet falls back to name (relative to the domain).
func recordHost(m map[string]any) string {
	if h := str(m, "host"); h != "" {
		return h
	}
	return str(m, "name")
}

func (a *App) printRecords(w io.Writer, records any) {
	l, _ := records.([]any)
	rows := [][]string{}
	for _, r := range l {
		m, _ := r.(map[string]any)
		rows = append(rows, []string{str(m, "record"), str(m, "type"), recordHost(m), out.Cell(m["value"], 0), dash(str(m, "priority")), a.statusCell(str(m, "status"))})
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, a.P().S().Dim.Render("No DNS records returned."))
		return
	}
	a.P().Table(w, []string{"record", "type", "host", "value", "priority", "status"}, rows)
}

func (a *App) statusCell(s string) string {
	st := a.P().S()
	switch s {
	case "verified", "valid", "ok", "enabled", "delivered", "active", "sent", "completed", "succeeded", "success", "opened", "clicked":
		return st.OK.Render(s)
	case "failed", "invalid", "temporary_failure", "bounced", "complained", "error", "errored", "disabled":
		return st.Err.Render(s)
	case "":
		return "-"
	case "draft", "canceled", "cancelled", "archived", "inactive", "stopped":
		return st.Dim.Render(s)
	default:
		// pending, not_started, queued, scheduled, delivery_delayed, ...
		return st.Warn.Render(s)
	}
}

func domainView(d sendio.Domain) map[string]any {
	recs := []any{}
	for _, r := range d.Records {
		m := map[string]any{"record": string(r.Record), "type": r.Type, "name": r.Name, "value": r.Value, "status": r.Status, "ttl": r.Ttl}
		if r.Priority != "" {
			m["priority"] = r.Priority.String()
		}
		if r.Host != "" {
			m["host"] = r.Host
		}
		if r.FullName != "" {
			m["full_name"] = r.FullName
		}
		recs = append(recs, m)
	}
	v := map[string]any{"id": d.Id, "name": d.Name, "status": d.Status, "region": d.Region, "records": recs}
	if d.DnsZone != "" {
		v["dns_zone"] = d.DnsZone
	}
	return v
}

func recordSignature(d map[string]any) string {
	var b strings.Builder
	b.WriteString(str(d, "status"))
	l, _ := d["records"].([]any)
	for _, r := range l {
		m, _ := r.(map[string]any)
		b.WriteString("|" + str(m, "name") + str(m, "type") + str(m, "status"))
	}
	return b.String()
}

func (a *App) verifyDomain(ctx context.Context, id string, watch bool, timeout, interval time.Duration) error {
	c, err := a.Client(true)
	if err != nil {
		return err
	}
	p := a.P()
	sdk := a.SDK(c)
	start := a.now()
	last := ""
	for {
		if _, err := c.Do(ctx, &api.Request{Method: "POST", Path: "/domains/" + url.PathEscape(id) + "/verify"}); err != nil {
			return err
		}
		d, err := sdk.Domains.GetWithContext(ctx, id)
		if err != nil {
			return api.AsError(err)
		}
		view := domainView(d)
		verified := d.Status == "verified"
		if !watch || verified {
			return p.Result(view, func(w io.Writer) {
				if verified {
					p.Success("%s is verified.", d.Name)
				} else {
					fmt.Fprintf(w, "%s is %s.\n", d.Name, a.statusCell(d.Status))
				}
				a.printZone(w, d.DnsZone)
				a.printRecords(w, view["records"])
			})
		}
		if sig := recordSignature(view); sig != last {
			last = sig
			if p.Machine {
				view["checked_at"] = a.now().UTC().Format(time.RFC3339)
				if !p.Quiet {
					b, _ := out.Marshal(map[string]any{"event": "domain_status", "domain": view})
					fmt.Fprintln(a.Stderr, string(b))
				}
			} else if !p.Quiet {
				fmt.Fprintf(a.Stdout, "\n%s %s is %s (%s elapsed)\n", p.S().Dim.Render(a.now().Format("15:04:05")), d.Name, a.statusCell(d.Status), a.now().Sub(start).Round(time.Second))
				a.printZone(a.Stdout, d.DnsZone)
				a.printRecords(a.Stdout, view["records"])
			}
		}
		if a.now().Sub(start) >= timeout {
			return &api.Error{Exit: api.ExitTimeout, Code: "wait_timeout", Message: fmt.Sprintf("%s is still %s after %s", d.Name, d.Status, timeout),
				Hint: "DNS changes can take a while to propagate; run the command again later."}
		}
		if err := a.sleep(ctx, interval); err != nil {
			return api.NetworkError(err)
		}
	}
}
