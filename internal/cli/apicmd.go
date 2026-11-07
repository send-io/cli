package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/out"
)

var httpMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

// typedField parses a -F value: true/false/null, numbers, @file (contents),
// otherwise a string.
func (a *App) typedField(v string) (any, error) {
	switch v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	if strings.HasPrefix(v, "@") || v == "-" {
		return a.readValue(v)
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n, nil
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && !strings.ContainsAny(v, "xXeE") {
		return f, nil
	}
	if strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") {
		if j, err := decodeJSON(v); err == nil {
			return j, nil
		}
	}
	return v, nil
}

func (a *App) addAPICommand(root *cobra.Command) {
	var rawFields, typedFields, headers []string
	var input string
	var paginate, include bool
	cmd := &cobra.Command{
		Use:   "api [METHOD] <path>",
		Short: "Make an authenticated request to any send.io API path (raw passthrough)",
		Long: `Call any API path with your credentials, like "gh api".

-f key=value adds a string field; -F key=value adds a typed field (true,
false, null, numbers, JSON, or @file for a file's contents). Fields go to
the query string for GET and to a JSON body otherwise. --input sends a raw
body (@file or - for stdin). --paginate follows has_more/after cursors and
prints one item per line. Works with /compat/* provider-compatible paths.`,
		Example: `  sendio api GET /domains --jq '.data[].name'
  sendio api POST /emails -f from=hi@acme.dev -f to=ada@example.com -f subject=Hi -f text=Hello
  sendio api /contacts --paginate --jq .email
  sendio api POST /compat/postmark/email --input @message.json -H 'X-Postmark-Server-Token: …'`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			method, path := "", args[0]
			if len(args) == 2 {
				method, path = strings.ToUpper(args[0]), args[1]
			} else if contains(httpMethods, strings.ToUpper(args[0])) {
				return api.UsageError("missing <path> after %s", args[0])
			}
			if method != "" && !contains(httpMethods, method) {
				return api.UsageError("unknown HTTP method %q", method)
			}
			fields := map[string]any{}
			for _, kv := range rawFields {
				i := strings.Index(kv, "=")
				if i <= 0 {
					return api.UsageError("-f expects key=value, got %q", kv)
				}
				fields[kv[:i]] = kv[i+1:]
			}
			for _, kv := range typedFields {
				i := strings.Index(kv, "=")
				if i <= 0 {
					return api.UsageError("-F expects key=value, got %q", kv)
				}
				v, err := a.typedField(kv[i+1:])
				if err != nil {
					return err
				}
				fields[kv[:i]] = v
			}
			if method == "" {
				method = "GET"
				if len(fields) > 0 || input != "" {
					method = "POST"
				}
			}
			req := &api.Request{Method: method, Path: path, Header: http.Header{}}
			if strings.Contains(path, "?") && !strings.HasPrefix(path, "http") {
				if u, err := url.Parse(path); err == nil {
					req.Path, req.Query = u.Path, u.Query()
				}
			}
			for _, h := range headers {
				i := strings.Index(h, ":")
				if i <= 0 {
					return api.UsageError("-H expects 'Name: value', got %q", h)
				}
				req.Header.Add(strings.TrimSpace(h[:i]), strings.TrimSpace(h[i+1:]))
			}
			switch {
			case input != "":
				s, err := a.readValue(input)
				if err != nil {
					return err
				}
				req.Raw = []byte(s)
				if ct := req.Header.Get("Content-Type"); ct != "" {
					req.ContentType = ct
				} else if _, err := decodeJSON(s); err != nil {
					req.ContentType = "text/plain"
				}
				if len(fields) > 0 {
					req.Query = mergeQuery(req.Query, fields)
				}
			case method == "GET" || method == "HEAD":
				req.Query = mergeQuery(req.Query, fields)
			case len(fields) > 0:
				req.JSON = fields
				if ct := req.Header.Get("Content-Type"); ct != "" {
					req.ContentType = ct
				}
			}
			c, err := a.Client(true)
			if err != nil {
				return err
			}
			p := a.P()
			if paginate {
				if method != "GET" {
					return api.UsageError("--paginate needs GET")
				}
				return c.Paginate(cmd.Context(), req, 100, 0, func(raw json.RawMessage) error { return p.Line(out.Normalize(raw)) })
			}
			resp, err := c.Do(cmd.Context(), req)
			if err != nil {
				return err
			}
			v, _ := resp.Value()
			if include {
				hs := map[string]any{}
				keys := make([]string, 0, len(resp.Header))
				for k := range resp.Header {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					hs[k] = resp.Header.Get(k)
				}
				if p.Machine {
					return p.JSON(map[string]any{"status": resp.Status, "headers": hs, "body": v})
				}
				fmt.Fprintf(a.Stdout, "HTTP %d %s\n", resp.Status, http.StatusText(resp.Status))
				for _, k := range keys {
					fmt.Fprintf(a.Stdout, "%s: %s\n", k, hs[k])
				}
				fmt.Fprintln(a.Stdout)
			}
			if v == nil {
				if !p.Machine {
					p.Success("HTTP %d", resp.Status)
					return nil
				}
				return p.JSON(map[string]any{"status": resp.Status})
			}
			if s, ok := v.(string); ok && !p.HasJQ() {
				_, err := io.WriteString(a.Stdout, s+"\n")
				return err
			}
			return p.JSON(v)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&rawFields, "raw-field", "f", nil, "add a string field key=value")
	f.StringArrayVarP(&typedFields, "field", "F", nil, "add a typed field key=value (true/false/null/number/JSON/@file)")
	f.StringVar(&input, "input", "", "raw request body: @file or - for stdin")
	f.StringArrayVarP(&headers, "header", "H", nil, "add a request header 'Name: value'")
	f.BoolVar(&paginate, "paginate", false, "follow cursor pagination, printing one item per line")
	f.BoolVarP(&include, "include", "i", false, "include the response status and headers")
	root.AddCommand(cmd)
}

func mergeQuery(q url.Values, fields map[string]any) url.Values {
	if q == nil {
		q = url.Values{}
	}
	for k, v := range fields {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				q.Add(k, fmt.Sprint(e))
			}
		case nil:
			q.Set(k, "")
		default:
			q.Set(k, fmt.Sprint(x))
		}
	}
	return q
}
