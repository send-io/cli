package cli

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"send.io/cli/internal/api"
)

//go:embed skill/SKILL.md
var skillMD string

// SkillMD is the agent skill describing this CLI.
func SkillMD() string { return skillMD }

type agentTarget struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Detected bool   `json:"detected"`
	Reason   string `json:"reason,omitempty"`
	Path     string `json:"path"`
}

func (a *App) dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// detectAgents finds coding agents from env vars, config dirs and binaries.
func (a *App) detectAgents() []agentTarget {
	have := func(bin string) bool {
		if a.LookPath == nil {
			return false
		}
		_, err := a.LookPath(bin)
		return err == nil
	}
	codexEnv := false
	if a.Environ != nil {
		for _, kv := range a.Environ() {
			if strings.HasPrefix(kv, "CODEX_") {
				codexEnv = true
			}
		}
	}
	cursorRules := filepath.Join(a.WorkDir, ".cursor", "rules", "sendio-cli.mdc")
	targets := []agentTarget{
		{ID: "claude-code", Name: "Claude Code", Path: a.home(".claude", "skills", "sendio-cli", "SKILL.md")},
		{ID: "codex", Name: "Codex", Path: a.home(".codex", "skills", "sendio-cli", "SKILL.md")},
		{ID: "cursor", Name: "Cursor", Path: cursorRules},
	}
	for i := range targets {
		t := &targets[i]
		switch t.ID {
		case "claude-code":
			switch {
			case a.env("CLAUDECODE") != "":
				t.Detected, t.Reason = true, "CLAUDECODE is set"
			case a.dirExists(a.home(".claude")):
				t.Detected, t.Reason = true, "~/.claude exists"
			case have("claude"):
				t.Detected, t.Reason = true, "claude is on PATH"
			}
		case "codex":
			switch {
			case codexEnv:
				t.Detected, t.Reason = true, "CODEX_* is set"
			case a.dirExists(a.home(".codex")):
				t.Detected, t.Reason = true, "~/.codex exists"
			case have("codex"):
				t.Detected, t.Reason = true, "codex is on PATH"
			}
		case "cursor":
			switch {
			case a.env("CURSOR_AGENT") != "":
				t.Detected, t.Reason = true, "CURSOR_AGENT is set"
			case a.dirExists(a.home(".cursor")):
				t.Detected, t.Reason = true, "~/.cursor exists"
			case have("cursor"):
				t.Detected, t.Reason = true, "cursor is on PATH"
			}
		}
	}
	return targets
}

func cursorRule(skill string) string {
	body := skill
	if strings.HasPrefix(body, "---") {
		if i := strings.Index(body[3:], "---"); i >= 0 {
			body = strings.TrimLeft(body[i+6:], "\n")
		}
	}
	return "---\ndescription: Use the sendio CLI for send.io email tasks (send, verify domains, templates, contacts, logs)\nalwaysApply: false\n---\n\n" + body
}

func (a *App) addAgentCommands(root *cobra.Command) {
	agent := a.group(root, []string{"agent"})
	var print bool
	var targets []string
	setup := &cobra.Command{
		Use:   "setup",
		Short: "Install the sendio skill into detected coding agents (Claude Code, Codex, Cursor)",
		Long: `Detect coding agents (environment variables, config folders, binaries) and
install a skill that teaches them this CLI: when to use which command, the
agent-mode contract and examples.

  Claude Code  ~/.claude/skills/sendio-cli/SKILL.md
  Codex        ~/.codex/skills/sendio-cli/SKILL.md
  Cursor       ./.cursor/rules/sendio-cli.mdc (project rule)

--print shows the skill instead. --target picks agents explicitly.`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annWrite: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if print {
				if a.P().Machine {
					return a.P().JSON(map[string]any{"name": "sendio-cli", "content": skillMD})
				}
				_, err := io.WriteString(a.Stdout, skillMD)
				return err
			}
			all := a.detectAgents()
			var chosen []agentTarget
			for _, t := range all {
				if len(targets) > 0 {
					if contains(splitList(targets), t.ID) {
						chosen = append(chosen, t)
					}
				} else if t.Detected {
					chosen = append(chosen, t)
				}
			}
			for _, want := range splitList(targets) {
				found := false
				for _, t := range all {
					found = found || t.ID == want
				}
				if !found {
					return api.UsageError("unknown --target %q (claude-code, codex, cursor)", want)
				}
			}
			results := []any{}
			for _, t := range chosen {
				content := skillMD
				if t.ID == "cursor" {
					content = cursorRule(skillMD)
				}
				status := "installed"
				if old, err := os.ReadFile(t.Path); err == nil {
					status = "updated"
					if string(old) == content {
						status = "unchanged"
					}
				}
				if a.g.dryRun {
					status = "would install"
				} else if status != "unchanged" {
					if err := os.MkdirAll(filepath.Dir(t.Path), 0o755); err != nil {
						return api.Errorf(api.ExitAPI, "write_failed", "%v", err)
					}
					if err := os.WriteFile(t.Path, []byte(content), 0o644); err != nil {
						return api.Errorf(api.ExitAPI, "write_failed", "%v", err)
					}
				}
				results = append(results, map[string]any{"agent": t.ID, "name": t.Name, "path": t.Path, "status": status, "reason": t.Reason})
			}
			return a.P().Result(map[string]any{"detected": all, "installed": results}, func(w io.Writer) {
				p := a.P()
				if len(results) == 0 {
					fmt.Fprintln(w, "No coding agents detected. Use --target claude-code|codex|cursor, or --print to copy the skill.")
					return
				}
				for _, r := range results {
					m := r.(map[string]any)
					p.Success("%s: %s %s", m["name"], m["status"], m["path"])
				}
			})
		},
	}
	setup.Flags().BoolVar(&print, "print", false, "print the skill instead of installing it")
	setup.Flags().StringSliceVar(&targets, "target", nil, "agents to install into: claude-code, codex, cursor (default: detected)")
	agent.AddCommand(setup)
}
