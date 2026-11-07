package cli

import (
	"path/filepath"

	"send.io/cli/internal/api"
)

// sendioCommand is the command line of the local MCP server: this binary by
// absolute path (GUI clients do not have the shell's PATH), the selected
// profile and base URL when set, then "mcp serve".
func (a *App) sendioCommand(readOnly bool) (string, []string) {
	bin := "sendio"
	if a.Executable != nil {
		if p, err := a.Executable(); err == nil && p != "" {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				p = r
			}
			bin = p
		}
	}
	var args []string
	if a.g.profile != "" {
		args = append(args, "--profile", a.g.profile)
	}
	if a.g.baseURL != "" {
		args = append(args, "--base-url", a.g.baseURL)
	}
	args = append(args, "mcp", "serve")
	if readOnly {
		args = append(args, "--read-only")
	}
	return bin, args
}

func anyList(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

func tomlList(l []string) string {
	s := "["
	for i, x := range l {
		if i > 0 {
			s += ", "
		}
		s += tomlQuote(x)
	}
	return s + "]"
}

func tomlQuote(s string) string {
	b, _ := jsonLine(s)
	return b
}

// mcpLocalPlan configures a client to start "sendio mcp serve" over stdio.
func (a *App) mcpLocalPlan(client string, useKey bool, key, scope string, project, readOnly bool) (*mcpPlan, error) {
	bin, args := a.sendioCommand(readOnly)
	auth := "cli"
	if useKey {
		auth = "api_key"
	}
	p := &mcpPlan{Client: client, Auth: auth}
	if useKey && key != "mx_your_api_key" {
		p.secret = key
	}
	env := map[string]any{}
	if useKey {
		env["SENDIO_API_KEY"] = key
	}
	server := map[string]any{"command": bin, "args": anyList(args)}
	if len(env) > 0 {
		server["env"] = env
	}
	local := func(rel ...string) string { return filepath.Join(append([]string{a.WorkDir}, rel...)...) }
	signIn := "The server uses this CLI's sign-in (run `sendio login` first, or pass --with-api-key)."
	switch client {
	case "claude-code":
		p.Action = "run"
		p.Command = []string{"claude", "mcp", "add", "--transport", "stdio", "sendio", "--scope", scope}
		if useKey {
			p.Command = append(p.Command, "--env", "SENDIO_API_KEY="+key)
		}
		p.Command = append(append(p.Command, "--", bin), args...)
		p.Steps = []string{"Run /mcp in Claude Code to check that sendio is connected.", signIn}
	case "cursor":
		p.Path = a.home(".cursor", "mcp.json")
		if project {
			p.Path = local(".cursor", "mcp.json")
		}
		p.merge, p.value = []string{"mcpServers", "sendio"}, server
		p.Steps = []string{"Enable sendio in Cursor's MCP settings.", signIn}
	case "vscode":
		p.Path = local(".vscode", "mcp.json")
		v := map[string]any{"type": "stdio", "command": bin, "args": anyList(args)}
		if len(env) > 0 {
			v["env"] = env
		}
		p.merge, p.value = []string{"servers", "sendio"}, v
		p.Steps = []string{"Start the sendio server from VS Code's MCP view.", signIn}
	case "windsurf":
		p.Path = a.home(".codeium", "windsurf", "mcp_config.json")
		p.merge, p.value = []string{"mcpServers", "sendio"}, server
		p.Steps = []string{"Refresh MCP servers in Windsurf.", signIn}
	case "gemini":
		p.Path = a.home(".gemini", "settings.json")
		if project {
			p.Path = local(".gemini", "settings.json")
		}
		p.merge, p.value = []string{"mcpServers", "sendio"}, server
		p.Steps = []string{"Run /mcp in Gemini CLI to check the connection.", signIn}
	case "zed":
		p.Path = a.home(".config", "zed", "settings.json")
		v := map[string]any{"source": "custom", "command": bin, "args": anyList(args)}
		if len(env) > 0 {
			v["env"] = env
		}
		p.merge, p.value = []string{"context_servers", "sendio"}, v
		p.Steps = []string{signIn}
	case "codex":
		p.Path = a.home(".codex", "config.toml")
		p.toml = "[mcp_servers.sendio]\ncommand = " + tomlQuote(bin) + "\nargs = " + tomlList(args) + "\n"
		if useKey {
			p.toml += "env = { SENDIO_API_KEY = " + tomlQuote(key) + " }\n"
		}
		p.Steps = []string{"Restart Codex; sendio starts as a local server.", signIn}
	case "claude-desktop":
		p.Path = a.claudeDesktopConfig()
		p.merge, p.value = []string{"mcpServers", "sendio"}, server
		p.Steps = []string{"Restart Claude Desktop.", signIn}
	case "claude-ai":
		return nil, api.UsageError("claude.ai cannot start local servers; use the hosted server (sendio mcp install --client claude-ai)")
	}
	if p.Action == "" {
		p.Action = "write"
	}
	return p, nil
}
