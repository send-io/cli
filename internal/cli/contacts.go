package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
)

func (a *App) addContactCommands(root *cobra.Command) {
	contacts := a.group(root, []string{"contacts"})
	var segments, topics, columns []string
	var onConflict, delimiter string
	var noHeader, wait, createProps bool
	var timeout time.Duration
	imp := &cobra.Command{
		Use:   "import <file.csv>",
		Short: "Import contacts from a CSV file (--wait: until the import finishes)",
		Long: `Upload a CSV of contacts. Columns are matched by header (email,
first_name, last_name, unsubscribed, or any contact property; with
--create-missing-properties every other column becomes a property) unless
--column maps them: --column email=E-mail --column properties.plan=Plan.

API: POST /contacts/imports (multipart), GET /contacts/imports/{id}
(MCP tools: create-contact-import, get-contact-import)`,
		Example:     "  sendio contacts import ./contacts.csv --segment seg_123 --wait",
		Args:        exactArgs(1),
		Annotations: map[string]string{annTool: "create-contact-import", annMethod: "POST", annPath: "/contacts/imports", annWrite: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			data, err := a.readValue("@" + args[0])
			if err != nil {
				return err
			}
			fields := map[string]any{}
			if len(segments) > 0 {
				l := []any{}
				for _, s := range splitList(segments) {
					l = append(l, map[string]any{"id": s})
				}
				fields["segments"] = l
			}
			if len(topics) > 0 {
				l := []any{}
				for _, s := range splitList(topics) {
					l = append(l, map[string]any{"id": s, "subscription": "opt_in"})
				}
				fields["topics"] = l
			}
			c, err := a.Client(true)
			if err != nil {
				return err
			}
			cm := map[string]any{}
			props := map[string]any{}
			for _, kv := range columns {
				k, v, err := parseKV("column", kv)
				if err != nil {
					return err
				}
				if len(k) > 11 && k[:11] == "properties." {
					props[k[11:]] = map[string]any{"column": v}
				} else {
					cm[k] = v
				}
			}
			// Columns --column does not map are matched by header name.
			if !noHeader {
				header := csvHeader([]byte(data), delimiter)
				keys := map[string]bool{}
				if !createProps && needsPropertyKeys(header) {
					if keys, err = propertyKeys(ctx, c); err != nil {
						return err
					}
				}
				auto := headerColumnMap(header, keys, createProps)
				for k, v := range auto {
					if k == "properties" {
						for pk, pv := range v.(map[string]any) {
							if _, set := props[pk]; !set {
								props[pk] = pv
							}
						}
					} else if _, set := cm[k]; !set {
						cm[k] = v
					}
				}
			}
			if len(props) > 0 {
				cm["properties"] = props
			}
			if cm["email"] == nil {
				hint := "The header has no email column: name it email, or map it with --column email=<column>."
				if noHeader {
					hint = "Without a header row, map columns by position: --column email:=0."
				}
				return &api.Error{Exit: api.ExitUsage, Code: "usage_error", Message: "No column holds the email address.", Hint: hint}
			}
			fields["column_map"] = cm
			if onConflict != "" {
				fields["on_conflict"] = onConflict
			}
			if delimiter != "" {
				fields["delimiter"] = delimiter
			}
			if noHeader {
				fields["has_header"] = false
			}
			if createProps {
				fields["create_missing_properties"] = true
			}
			raw, ct, err := multipartBody(fields, "file", filepath.Base(args[0]), []byte(data))
			if err != nil {
				return err
			}
			resp, err := c.Do(ctx, &api.Request{Method: "POST", Path: "/contacts/imports", Raw: raw, ContentType: ct})
			if err != nil {
				return err
			}
			var im map[string]any
			_ = resp.Decode(&im)
			if !wait {
				return a.P().Result(im, func(w io.Writer) {
					a.P().Success("Import %s %s.", str(im, "id"), dash(str(im, "status")))
					fmt.Fprintf(w, "Follow it with: sendio contacts imports get %s\n", str(im, "id"))
				})
			}
			return a.waitImport(ctx, c, str(im, "id"), timeout)
		},
	}
	f := imp.Flags()
	f.StringSliceVar(&segments, "segment", nil, "add imported contacts to this segment id (repeatable)")
	f.StringSliceVar(&topics, "topic", nil, "opt imported contacts in to this topic id (repeatable)")
	f.StringArrayVar(&columns, "column", nil, "map a field to a CSV column: email=Email, properties.plan=Plan (repeatable)")
	f.StringVar(&onConflict, "on-conflict", "", "existing contacts: upsert (default) or skip")
	f.StringVar(&delimiter, "delimiter", "", "column delimiter: , ; | or tab (default: guessed)")
	f.BoolVar(&noHeader, "no-header", false, "the first row is data, not a header")
	f.BoolVar(&createProps, "create-missing-properties", false, "create contact properties that do not exist yet")
	f.BoolVar(&wait, "wait", false, "wait until the import completes or fails")
	f.DurationVar(&timeout, "timeout", 10*time.Minute, "how long --wait waits")
	contacts.AddCommand(imp)
}

func (a *App) waitImport(ctx context.Context, c *api.Client, id string, timeout time.Duration) error {
	start := a.now()
	interval := time.Second
	p := a.P()
	for {
		resp, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/contacts/imports/" + url.PathEscape(id)})
		if err != nil {
			return err
		}
		var im map[string]any
		_ = resp.Decode(&im)
		switch str(im, "status") {
		case "completed":
			return p.Result(im, func(w io.Writer) {
				p.Success("Import %s completed.", id)
				a.renderObject(w, im)
			})
		case "failed":
			_ = p.Result(im, func(w io.Writer) { a.renderObject(w, im) })
			return &api.Error{Exit: api.ExitAPI, Code: "import_failed", Message: fmt.Sprintf("import %s failed: %s", id, dash(str(im, "error")))}
		}
		if a.now().Sub(start) >= timeout {
			return &api.Error{Exit: api.ExitTimeout, Code: "wait_timeout", Message: fmt.Sprintf("import %s is still %s after %s", id, str(im, "status"), timeout)}
		}
		if !p.Machine && !p.Quiet && a.StderrTTY {
			fmt.Fprintf(a.Stderr, "\r\033[2KImport %s: %s…", id, str(im, "status"))
		}
		if err := a.sleep(ctx, interval); err != nil {
			return api.NetworkError(err)
		}
		if interval < 5*time.Second {
			interval *= 2
		}
	}
}
