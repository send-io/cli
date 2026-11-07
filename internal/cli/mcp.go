package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
	"send.io/cli/internal/config"
	"send.io/cli/internal/out"
)

// mcpClients mirrors frontend/console/src/modules/docs/mcp/mcp-clients.ts (the dashboard's setup
// instructions): the same server URL and config shapes per client.
var mcpClients = []string{"claude-code", "cursor", "vscode", "codex", "gemini", "windsurf", "zed", "claude-desktop", "claude-ai"}

type mcpPlan struct {
	Client  string   `json:"client"`
	Action  string   `json:"action"` // run, write, manual
	Path    string   `json:"path,omitempty"`
	Command []string `json:"command,omitempty"`
	Snippet string   `json:"snippet,omitempty"`
	Steps   []string `json:"next_steps,omitempty"`
	Auth    string   `json:"auth"`
	// merge sets a JSON value at a key path in Path.
	merge  []string
	value  any
	toml   string
	secret string
}

func (a *App) addMCPCommands(root *cobra.Command) {
	mcp := a.group(root, []string{"mcp"})
	var client, scope string
	var withKey, project, local, localRO bool
	install := &cobra.Command{
		Use:   "install --client <client>",
		Short: "Add the hosted send.io MCP server to an AI client",
		Long: `Configure an AI client to use the hosted send.io MCP server (<API>/mcp).

Clients: ` + strings.Join(mcpClients, ", ") + `

By default the config holds only the URL and the client signs in with
send.io (OAuth) on first use. --with-api-key (or --api-key mx_…) writes the
API key variant instead; most clients then read SENDIO_API_KEY from the
environment so no key lands in the file. Claude Code is configured with
"claude mcp add" when the claude binary is installed. claude.ai and Claude
Desktop's connector screen take the URL by hand: the steps are printed.

--local configures the local stdio server instead ("sendio mcp serve", this
binary by its full path), which calls the API with this CLI's sign-in (the
selected profile; with --with-api-key the key goes in the config's env).
--read-only adds --read-only to it.`,
		Example:     "  sendio mcp install --client claude-code\n  sendio mcp install --client cursor --with-api-key\n  sendio mcp install --client claude-ai\n  sendio mcp install --client claude-desktop --local --read-only",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annWrite: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if client == "" {
				return api.UsageError("pass --client (%s)", strings.Join(mcpClients, ", "))
			}
			c := strings.ToLower(client)
			if c == "gemini-cli" {
				c = "gemini"
			}
			if c == "claude" {
				c = "claude-code"
			}
			if !contains(mcpClients, c) {
				return api.UsageError("unknown client %q (one of %s)", client, strings.Join(mcpClients, ", "))
			}
			useKey := withKey || a.g.apiKey != ""
			key := "mx_your_api_key"
			if useKey {
				if cred, err := a.credential(); err == nil && cred.kind == config.AuthAPIKey && cred.token != "" {
					key = cred.token
				}
			}
			var plan *mcpPlan
			var err error
			if local {
				plan, err = a.mcpLocalPlan(c, useKey, key, scope, project, localRO)
			} else {
				if localRO {
					return api.UsageError("--read-only applies to --local (the hosted server's tools follow the key's permission)")
				}
				plan, err = a.mcpPlan(c, useKey, key, scope, project)
			}
			if err != nil {
				return err
			}
			return a.applyMCPPlan(plan)
		},
	}
	f := install.Flags()
	f.StringVar(&client, "client", "", "AI client: "+strings.Join(mcpClients, ", "))
	f.BoolVar(&withKey, "with-api-key", false, "configure with an API key header instead of sign-in (uses --api-key, SENDIO_API_KEY or the profile's key)")
	f.StringVar(&scope, "scope", "user", "Claude Code scope: user, project or local")
	f.BoolVar(&project, "project", false, "write the project config (./.cursor, ./.gemini) instead of the user one")
	f.BoolVar(&local, "local", false, "configure the local stdio server (sendio mcp serve) instead of the hosted one")
	f.BoolVar(&localRO, "read-only", false, "with --local: only the tools that read")
	mcp.AddCommand(install)

	mcp.AddCommand(&cobra.Command{
		Use: "url", Short: "Print the hosted MCP server URL", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			u := a.BaseURL() + "/mcp"
			return a.P().Result(map[string]any{"url": u}, func(w io.Writer) { fmt.Fprintln(w, u) })
		},
	})
}

func (a *App) home(rel ...string) string {
	return filepath.Join(append([]string{a.HomeDir}, rel...)...)
}

func (a *App) claudeDesktopConfig() string {
	switch runtime.GOOS {
	case "darwin":
		return a.home("Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		if d := a.env("APPDATA"); d != "" {
			return filepath.Join(d, "Claude", "claude_desktop_config.json")
		}
	}
	return a.home(".config", "Claude", "claude_desktop_config.json")
}

func (a *App) mcpPlan(client string, useKey bool, key, scope string, project bool) (*mcpPlan, error) {
	u := a.BaseURL() + "/mcp"
	auth := "oauth"
	if useKey {
		auth = "api_key"
	}
	p := &mcpPlan{Client: client, Auth: auth}
	if useKey && key != "mx_your_api_key" {
		p.secret = key
	}
	local := func(rel ...string) string { return filepath.Join(append([]string{a.WorkDir}, rel...)...) }
	switch client {
	case "claude-code":
		p.Command = []string{"claude", "mcp", "add", "--transport", "http", "sendio", u, "--scope", scope}
		if useKey {
			p.Command = append(p.Command, "--header", "Authorization: Bearer "+key)
		}
		p.Action = "run"
		p.Steps = []string{"Run /mcp in Claude Code, pick sendio and sign in (a browser opens on send.io where you choose the team and permission)."}
		if useKey {
			p.Steps = []string{"Run /mcp in Claude Code to check the connection."}
		}
	case "cursor":
		p.Path = a.home(".cursor", "mcp.json")
		if project {
			p.Path = local(".cursor", "mcp.json")
		}
		p.merge = []string{"mcpServers", "sendio"}
		p.value = map[string]any{"url": u}
		if useKey {
			p.value = map[string]any{"url": u, "headers": map[string]any{"Authorization": "Bearer ${env:SENDIO_API_KEY}"}}
			p.Steps = []string{"Export SENDIO_API_KEY in the environment Cursor starts from."}
		} else {
			p.Steps = []string{"Cursor offers to connect in its MCP settings; approve on send.io in the browser that opens."}
		}
	case "vscode":
		p.Path = local(".vscode", "mcp.json")
		p.merge = []string{"servers", "sendio"}
		p.value = map[string]any{"type": "http", "url": u}
		if useKey {
			p.value = map[string]any{"type": "http", "url": u, "headers": map[string]any{"Authorization": "Bearer ${input:sendio-key}"}}
			p.Steps = []string{"VS Code asks for the key (input sendio-key) the first time the server starts."}
		} else {
			p.Steps = []string{"When the server starts, VS Code asks to sign in and opens send.io in your browser."}
		}
	case "windsurf":
		p.Path = a.home(".codeium", "windsurf", "mcp_config.json")
		p.merge = []string{"mcpServers", "sendio"}
		p.value = map[string]any{"serverUrl": u}
		if useKey {
			p.value = map[string]any{"serverUrl": u, "headers": map[string]any{"Authorization": "Bearer ${env:SENDIO_API_KEY}"}}
		}
		p.Steps = []string{"Refresh MCP servers in Windsurf; it opens send.io to sign in."}
	case "gemini":
		p.Path = a.home(".gemini", "settings.json")
		if project {
			p.Path = local(".gemini", "settings.json")
		}
		p.merge = []string{"mcpServers", "sendio"}
		p.value = map[string]any{"httpUrl": u}
		if useKey {
			p.value = map[string]any{"httpUrl": u, "headers": map[string]any{"Authorization": "Bearer $SENDIO_API_KEY"}}
		} else {
			p.Steps = []string{"Run /mcp auth sendio in Gemini CLI to sign in (it needs a local browser)."}
		}
	case "zed":
		p.Path = a.home(".config", "zed", "settings.json")
		p.merge = []string{"context_servers", "sendio"}
		p.value = map[string]any{"url": u}
		if useKey {
			p.value = map[string]any{"url": u, "headers": map[string]any{"Authorization": "Bearer " + key}}
		} else {
			p.Steps = []string{"With no Authorization header, Zed asks you to sign in and opens send.io in your browser."}
		}
	case "codex":
		p.Path = a.home(".codex", "config.toml")
		p.toml = "[mcp_servers.sendio]\nurl = \"" + u + "\"\n"
		if useKey {
			p.toml += "bearer_token_env_var = \"SENDIO_API_KEY\"\n"
			p.Steps = []string{"Codex sends the key from SENDIO_API_KEY as a bearer token."}
		} else {
			p.Steps = []string{"Run `codex mcp login sendio`: it opens send.io in your browser to sign in."}
		}
	case "claude-desktop":
		if useKey {
			p.Path = a.claudeDesktopConfig()
			p.merge = []string{"mcpServers", "sendio"}
			p.value = map[string]any{"command": "npx", "args": []any{"-y", "mcp-remote", u, "--header", "Authorization:${AUTH_HEADER}"},
				"env": map[string]any{"AUTH_HEADER": "Bearer " + key}}
			p.Steps = []string{"Restart Claude Desktop. mcp-remote needs Node.js."}
			break
		}
		fallthrough
	case "claude-ai":
		p.Action = "manual"
		p.Snippet = "Name: send.io\nRemote MCP server URL: " + u
		p.Steps = []string{
			"Open claude.ai (or Claude Desktop) › Customize › Connectors › Add › Add custom connector.",
			"Name: send.io; Remote MCP server URL: " + u,
			"Click Add, then Connect, and sign in with send.io.",
			"On Team and Enterprise plans an owner adds the connector under Organization settings, then each member connects.",
		}
	}
	if p.Action == "" {
		p.Action = "write"
	}
	return p, nil
}

// setPath sets v at keys inside m, creating objects on the way.
func setPath(m map[string]any, keys []string, v any) error {
	cur := m
	for _, k := range keys[:len(keys)-1] {
		next, ok := cur[k]
		if !ok || next == nil {
			nm := map[string]any{}
			cur[k] = nm
			cur = nm
			continue
		}
		nm, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("%q is not an object", k)
		}
		cur = nm
	}
	cur[keys[len(keys)-1]] = v
	return nil
}

var tomlSection = regexp.MustCompile(`(?ms)^\[mcp_servers\.sendio\]\s*\n.*?(^\[|\z)`)

func (a *App) applyMCPPlan(p *mcpPlan) error {
	pr := a.P()
	dry := a.g.dryRun
	switch p.Action {
	case "run":
		if a.LookPath != nil {
			if _, err := a.LookPath(p.Command[0]); err != nil {
				p.Action = "manual"
				p.Snippet = shellJoin(p.Command)
				p.Steps = append([]string{"The claude binary was not found; run this in your terminal:"}, p.Steps...)
				break
			}
		}
		if dry {
			p.Action = "would run"
			break
		}
		if err := a.RunCmd(p.Command[0], p.Command[1:]...); err != nil {
			return api.Errorf(api.ExitAPI, "command_failed", "%s: %v", shellJoin(redactCmd(p.Command)), err)
		}
	case "write":
		var content []byte
		if p.toml != "" {
			old, _ := os.ReadFile(p.Path)
			s := string(old)
			if tomlSection.MatchString(s) {
				s = tomlSection.ReplaceAllStringFunc(s, func(m string) string {
					if strings.HasSuffix(m, "[") {
						return p.toml + "\n["
					}
					return p.toml
				})
			} else {
				if s != "" && !strings.HasSuffix(s, "\n") {
					s += "\n"
				}
				if s != "" {
					s += "\n"
				}
				s += p.toml
			}
			content = []byte(s)
			p.Snippet = p.toml
		} else {
			m := map[string]any{}
			if old, err := os.ReadFile(p.Path); err == nil && len(strings.TrimSpace(string(old))) > 0 {
				if err := json.Unmarshal(old, &m); err != nil {
					// JSONC or hand-edited: never clobber it.
					snippet := map[string]any{}
					_ = setPath(snippet, p.merge, p.value)
					b, _ := out.MarshalIndent(snippet)
					p.Action = "manual"
					p.Snippet = string(b)
					p.Steps = append([]string{fmt.Sprintf("%s is not plain JSON (comments?); merge this into it by hand:", p.Path)}, p.Steps...)
					break
				}
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return api.Errorf(api.ExitAPI, "read_failed", "%v", err)
			}
			if err := setPath(m, p.merge, p.value); err != nil {
				return api.Errorf(api.ExitAPI, "config_conflict", "%s: %v", p.Path, err)
			}
			b, _ := out.MarshalIndent(m)
			content = append(b, '\n')
			sn := map[string]any{}
			_ = setPath(sn, p.merge, p.value)
			sb, _ := out.MarshalIndent(sn)
			p.Snippet = string(sb)
		}
		if p.Action != "write" {
			break
		}
		if dry {
			p.Action = "would write"
			break
		}
		if err := os.MkdirAll(filepath.Dir(p.Path), 0o755); err != nil {
			return api.Errorf(api.ExitAPI, "write_failed", "%v", err)
		}
		mode := os.FileMode(0o644)
		if p.Auth == "api_key" {
			mode = 0o600
		}
		if err := os.WriteFile(p.Path, content, mode); err != nil {
			return api.Errorf(api.ExitAPI, "write_failed", "%v", err)
		}
	}
	view := *p
	view.Command = redactCmd(p.Command)
	if p.secret != "" {
		view.Snippet = strings.ReplaceAll(view.Snippet, p.secret, redact(p.secret))
	}
	return pr.Result(view, func(w io.Writer) {
		s := pr.S()
		switch p.Action {
		case "run":
			pr.Success("Added send.io to Claude Code (%s).", shellJoin(view.Command))
		case "write":
			pr.Success("Wrote the sendio server to %s.", p.Path)
		case "would write", "would run":
			fmt.Fprintln(w, s.Warn.Render("Dry run: nothing was changed."))
			if p.Path != "" {
				fmt.Fprintf(w, "Would merge into %s:\n%s\n", p.Path, view.Snippet)
			} else {
				fmt.Fprintf(w, "Would run: %s\n", shellJoin(view.Command))
			}
		case "manual":
			fmt.Fprintln(w, s.Bold.Render("Set it up by hand:"))
			if p.Snippet != "" {
				fmt.Fprintln(w, view.Snippet)
				fmt.Fprintln(w)
			}
		}
		for _, st := range p.Steps {
			fmt.Fprintln(w, "• "+st)
		}
	})
}

func redactCmd(c []string) []string {
	outc := make([]string, len(c))
	for i, s := range c {
		if strings.HasPrefix(s, "Authorization: Bearer ") {
			s = "Authorization: Bearer " + redact(strings.TrimPrefix(s, "Authorization: Bearer "))
		}
		outc[i] = s
	}
	return outc
}

func shellJoin(c []string) string {
	parts := make([]string, len(c))
	for i, s := range c {
		if strings.ContainsAny(s, " \"'$") {
			s = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
		parts[i] = s
	}
	return strings.Join(parts, " ")
}
