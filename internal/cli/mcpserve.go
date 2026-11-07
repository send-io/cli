package cli

// sendio mcp serve: a local stdio MCP server (official MCP Go SDK) with the
// same tools as the hosted one (<API>/mcp): names, descriptions and input
// schemas come from internal/spec/tools.json, generated from the backend's
// registry. A call is executed exactly as the hosted server's mcpChild does
// (path parameters, camelCase → snake_case, Idempotency-Key, query for GET,
// multipart for uploads) against the REST API with the profile's auth, and
// answers with the API's response body as text (isError for 4xx/5xx).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/build"
	"send.io/cli/internal/spec"
)

func init() { Register(func(a *App, root *cobra.Command) { a.addMCPServeCommand(root) }) }

func (a *App) addMCPServeCommand(root *cobra.Command) {
	var readOnly bool
	var only []string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run a local MCP server over stdio with the send.io tools",
		Long: `Run send.io's MCP server locally over stdio, for AI clients that start
local servers (sendio mcp install --local writes the config). It offers the
same tools as the hosted server (` + "`sendio mcp url`" + `), and calls the API with
this CLI's credential (the profile, --api-key or SENDIO_API_KEY).

--read-only offers only the tools that read (GET). --tools limits the tools
to some families (emails, domains, templates, contacts, broadcasts,
free-inbox, …) or tool names, comma-separated. stdout is the protocol;
diagnostics go to stderr.

The free_inbox_* tools (create, list, wait for an email or code, read a
free anonymous inbox, make a link that connects it to a send.io workspace)
run on this machine and work without a sign-in; when signed out, only they
are offered.`,
		Example: "  sendio mcp serve\n  sendio mcp serve --read-only --tools emails,domains\n  sendio -p work mcp serve",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.Client(true)
			var ae *api.Error
			if errors.As(err, &ae) && ae.Code == "not_authenticated" {
				// Signed out: the free inbox tools still work.
				c, err = nil, nil
			}
			if err != nil {
				return err
			}
			srv, n, err := a.mcpServer(c, readOnly, splitList(only))
			if err != nil {
				return err
			}
			if c == nil {
				fmt.Fprintf(a.Stderr, "sendio mcp serve: %d tools (not signed in: free inbox tools only; run `sendio login` for the rest) (stdio)\n", n)
			} else {
				fmt.Fprintf(a.Stderr, "sendio mcp serve: %d tools, API %s (stdio)\n", n, c.BaseURL)
			}
			err = srv.Run(cmd.Context(), a.mcpTransport())
			if err != nil && cmd.Context().Err() == nil && !isEOF(err) {
				return api.Errorf(api.ExitAPI, "mcp_error", "%v", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "only offer tools that read (GET)")
	cmd.Flags().StringSliceVar(&only, "tools", nil, "only these tool families or names (comma-separated)")
	a.group(root, []string{"mcp"}).AddCommand(cmd)
}

func isEOF(err error) bool {
	return err == io.EOF || strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed")
}

// mcpTransport is stdio, or the App's streams when they are not the
// process's (tests).
func (a *App) mcpTransport() mcp.Transport {
	if a.Stdin == os.Stdin && a.Stdout == os.Stdout {
		return &mcp.StdioTransport{}
	}
	return &mcp.IOTransport{Reader: io.NopCloser(a.Stdin), Writer: nopWriteCloser{a.Stdout}}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func toolFamily(t spec.Tool) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(t.Family), " ", "-"))
}

// mcpTools selects the tools to offer.
func mcpTools(readOnly bool, only []string) ([]spec.Tool, error) {
	want := map[string]bool{}
	for _, o := range only {
		want[strings.ToLower(o)] = true
	}
	known := map[string]bool{}
	var out []spec.Tool
	for _, t := range spec.Tools() {
		fam := toolFamily(t)
		known[fam], known[t.Name] = true, true
		if readOnly && t.Method != "GET" {
			continue
		}
		if len(want) > 0 && !want[fam] && !want[strings.TrimSuffix(fam, "s")] && !want[t.Name] {
			continue
		}
		out = append(out, t)
	}
	for o := range want {
		if !known[o] && !known[o+"s"] {
			fams := []string{}
			seen := map[string]bool{}
			for _, t := range spec.Tools() {
				if f := toolFamily(t); !seen[f] {
					seen[f] = true
					fams = append(fams, f)
				}
			}
			return nil, api.UsageError("--tools: unknown family or tool %q (families: %s)", o, strings.Join(fams, ", "))
		}
	}
	return out, nil
}

// mcpServer builds the server: the REST tools (when c is not nil) and the
// free inbox tools (always; they need no sign-in).
func (a *App) mcpServer(c *api.Client, readOnly bool, only []string) (*mcp.Server, int, error) {
	freeOnly, rest := splitFreeTools(only)
	var tools []spec.Tool
	if c != nil && (len(only) == 0 || len(rest) > 0) {
		var err error
		if tools, err = mcpTools(readOnly, rest); err != nil {
			return nil, 0, err
		}
	} else if len(rest) > 0 {
		if _, err := mcpTools(readOnly, rest); err != nil {
			return nil, 0, err
		}
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "sendio", Title: "send.io", Version: build.Version}, &mcp.ServerOptions{
		Instructions: "send.io tools: send email, manage domains, templates, contacts, broadcasts, webhooks and agent inboxes. Each tool calls the send.io REST API and returns its JSON response. " +
			"The free_inbox_* tools create and read free anonymous inboxes on this machine (no send.io account needed), e.g. to receive a sign-up verification code; free_inbox_connect makes a one-time link the user opens to move the inbox into their send.io workspace so it can also send.",
	})
	nFree := 0
	if len(only) == 0 || len(freeOnly) > 0 {
		nFree = a.addFreeTools(srv, readOnly, freeOnly)
	}
	for _, t := range tools {
		t := t
		var schema any = json.RawMessage(t.InputSchema)
		if len(t.InputSchema) == 0 {
			schema = map[string]any{"type": "object"}
		}
		ann := &mcp.ToolAnnotations{ReadOnlyHint: t.Method == "GET"}
		if t.Method == "DELETE" {
			yes := true
			ann.DestructiveHint = &yes
		} else if t.Method != "GET" {
			no := false
			ann.DestructiveHint = &no
		}
		srv.AddTool(&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: schema, Annotations: ann},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				args := map[string]any{}
				if len(req.Params.Arguments) > 0 {
					if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
						return toolText("invalid arguments: "+err.Error(), true), nil
					}
				}
				return a.callTool(ctx, c, t, args), nil
			})
	}
	return srv, len(tools) + nFree, nil
}

func toolText(s string, isErr bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}, IsError: isErr}
}

var mcpPathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// toolRequest is the backend's mcpChild: the REST request for a tools/call.
func toolRequest(t spec.Tool, args map[string]any) (*api.Request, error) {
	var missing []string
	path := mcpPathParam.ReplaceAllStringFunc(t.Path, func(p string) string {
		name := p[1 : len(p)-1]
		v := fmt.Sprint(args[name])
		if args[name] == nil || v == "" {
			missing = append(missing, name)
		}
		delete(args, name)
		return url.PathEscape(v)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required argument(s): %s", strings.Join(missing, ", "))
	}
	for from, to := range map[string]string{"replyTo": "reply_to", "scheduledAt": "scheduled_at"} {
		if v, ok := args[from]; ok {
			args[to] = v
			delete(args, from)
		}
	}
	req := &api.Request{Method: t.Method, Path: path, Header: map[string][]string{"X-Sendio-Origin": {"mcp"}}}
	if k, ok := args["idempotencyKey"].(string); ok && k != "" {
		req.Header["Idempotency-Key"] = []string{k}
	}
	delete(args, "idempotencyKey")
	// GET and DELETE take their arguments in the query string (delete-inbox's
	// keep_mailbox); other methods send a JSON body.
	if t.Method == "GET" || t.Method == "DELETE" {
		q := url.Values{}
		for k, v := range args {
			if l, ok := v.([]any); ok {
				for _, x := range l {
					q.Add(k, jsonScalar(x))
				}
				continue
			}
			q.Set(k, jsonScalar(v))
		}
		req.Query = q
		return req, nil
	}
	if file, ok := args[t.Upload].(string); t.Upload != "" && ok && file != "" {
		delete(args, t.Upload)
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, v := range args {
			if s, isStr := v.(string); isStr {
				_ = mw.WriteField(k, s)
			} else {
				b, _ := json.Marshal(v)
				_ = mw.WriteField(k, string(b))
			}
		}
		fw, _ := mw.CreateFormFile("file", "contacts.csv")
		_, _ = fw.Write([]byte(file))
		_ = mw.Close()
		req.Raw, req.ContentType = buf.Bytes(), mw.FormDataContentType()
		return req, nil
	}
	var payload any = args
	if t.Path == "/emails/batch" {
		payload = args["emails"]
	}
	b, _ := json.Marshal(payload)
	req.Raw, req.ContentType = b, "application/json"
	return req, nil
}

// jsonScalar is strings.Trim(json(v), `"`), as the hosted server writes
// query values.
func jsonScalar(v any) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

func (a *App) callTool(ctx context.Context, c *api.Client, t spec.Tool, args map[string]any) *mcp.CallToolResult {
	req, err := toolRequest(t, args)
	if err != nil {
		return toolText(err.Error(), true)
	}
	resp, err := c.Do(ctx, req)
	if err != nil {
		e := api.AsError(err)
		body := map[string]any{"statusCode": e.Status, "name": e.Code, "message": e.Message}
		if e.Status == 0 {
			body["statusCode"] = nil
		}
		b, _ := json.Marshal(body)
		return toolText(string(b), true)
	}
	return toolText(string(resp.Body), false)
}
