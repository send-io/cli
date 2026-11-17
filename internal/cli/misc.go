package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"send.io/cli/internal/api"
	"send.io/cli/internal/build"
	"send.io/cli/internal/out"
)

func jsonLine(v any) (string, error) {
	b, err := out.Marshal(v)
	return string(b), err
}

// openTargets are the pages `sendio open` knows (paths on the web app).
var openTargets = map[string]string{
	"dashboard": "/", "logs": "/logs", "domains": "/domains", "api-keys": "/api-keys", "docs": "/docs",
	"emails": "/emails", "templates": "/templates", "transactional": "/transactional", "contacts": "/contacts", "broadcasts": "/broadcasts",
	"webhooks": "/webhooks", "automations": "/automations", "settings": "/settings", "mcp": "/docs/mcp",
}

func (a *App) addMiscCommands(root *cobra.Command) {
	root.AddCommand(&cobra.Command{
		Use: "version", Short: "Print the CLI version", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v := map[string]any{"version": build.Version, "commit": build.Commit, "date": build.Date, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH}
			return a.P().Result(v, func(w io.Writer) {
				fmt.Fprintf(w, "sendio %s (commit %s, built %s, %s %s/%s)\n", build.Version, build.Commit, build.Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			})
		},
	})

	targets := sortedKeys(openTargets)
	root.AddCommand(&cobra.Command{
		Use:       "open [" + strings.Join(targets, "|") + "]",
		Short:     "Open a send.io dashboard page in the browser",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: targets,
		RunE: func(cmd *cobra.Command, args []string) error {
			t := "dashboard"
			if len(args) == 1 {
				t = args[0]
			}
			path, ok := openTargets[t]
			if !ok {
				return api.UsageError("unknown page %q (one of %s)", t, strings.Join(targets, ", "))
			}
			u := AppURL(a.BaseURL()) + path
			opened := false
			if !a.P().Machine && a.OpenURL != nil {
				opened = a.OpenURL(u) == nil
			}
			return a.P().Result(map[string]any{"url": u, "opened": opened}, func(w io.Writer) {
				if opened {
					a.P().Success("Opened %s", u)
				} else {
					fmt.Fprintln(w, u)
				}
			})
		},
	})

	var install bool
	comp := &cobra.Command{
		Use:       "completion bash|zsh|fish|powershell",
		Short:     "Print (or --install) the shell completion script",
		Args:      exactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var buf bytes.Buffer
			var err error
			var dest string
			switch args[0] {
			case "bash":
				err = root.GenBashCompletionV2(&buf, true)
				dest = a.home(".local", "share", "bash-completion", "completions", "sendio")
			case "zsh":
				err = root.GenZshCompletion(&buf)
				dest = a.home(".zfunc", "_sendio")
			case "fish":
				err = root.GenFishCompletion(&buf, true)
				dest = a.home(".config", "fish", "completions", "sendio.fish")
			case "powershell":
				err = root.GenPowerShellCompletionWithDesc(&buf)
			default:
				return api.UsageError("unknown shell %q (bash, zsh, fish, powershell)", args[0])
			}
			if err != nil {
				return err
			}
			if !install {
				_, err := a.Stdout.Write(buf.Bytes())
				return err
			}
			if dest == "" {
				return a.P().Result(map[string]any{"installed": false, "hint": "Add `sendio completion powershell | Out-String | Invoke-Expression` to your $PROFILE."}, func(w io.Writer) {
					fmt.Fprintln(w, "Add this line to your PowerShell $PROFILE:\n  sendio completion powershell | Out-String | Invoke-Expression")
				})
			}
			if a.g.dryRun {
				return a.P().Result(map[string]any{"installed": false, "path": dest, "dry_run": true}, func(w io.Writer) { fmt.Fprintf(w, "Would write %s\n", dest) })
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return api.Errorf(api.ExitAPI, "write_failed", "%v", err)
			}
			if err := os.WriteFile(dest, buf.Bytes(), 0o644); err != nil {
				return api.Errorf(api.ExitAPI, "write_failed", "%v", err)
			}
			hint := ""
			switch args[0] {
			case "zsh":
				hint = "Make sure ~/.zshrc has: fpath=(~/.zfunc $fpath); autoload -U compinit; compinit"
			case "bash":
				hint = "Needs the bash-completion package; open a new shell."
			}
			return a.P().Result(map[string]any{"installed": true, "path": dest, "hint": hint}, func(w io.Writer) {
				a.P().Success("Installed %s completion to %s", args[0], dest)
				if hint != "" {
					fmt.Fprintln(w, hint)
				}
			})
		},
	}
	comp.Flags().BoolVar(&install, "install", false, "write the script to the shell's completion folder")
	root.AddCommand(comp)

	root.AddCommand(&cobra.Command{
		Use:   "commands",
		Short: "List every command with its flags (use --json for agents)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			tree := commandTree(root)
			return a.P().Result(tree, func(w io.Writer) { printTree(w, a, root, 0) })
		},
	})
}

type flagInfo struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage"`
}

type cmdInfo struct {
	Name        string     `json:"name"`
	Path        string     `json:"path"`
	Use         string     `json:"usage"`
	Short       string     `json:"description"`
	Aliases     []string   `json:"aliases,omitempty"`
	Flags       []flagInfo `json:"flags,omitempty"`
	Method      string     `json:"method,omitempty"`
	APIPath     string     `json:"api_path,omitempty"`
	MCPTool     string     `json:"mcp_tool,omitempty"`
	Writes      bool       `json:"writes,omitempty"`
	Destructive bool       `json:"destructive,omitempty"`
	Paginated   bool       `json:"paginated,omitempty"`
	Commands    []cmdInfo  `json:"commands,omitempty"`
}

func flagsOf(fs *pflag.FlagSet) []flagInfo {
	var outf []flagInfo
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		d := f.DefValue
		if d == "[]" || d == "false" || d == "0" || d == "0s" {
			d = ""
		}
		outf = append(outf, flagInfo{Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(), Default: d, Usage: f.Usage})
	})
	return outf
}

func describe(c *cobra.Command) cmdInfo {
	ci := cmdInfo{Name: c.Name(), Path: c.CommandPath(), Use: c.UseLine(), Short: c.Short, Aliases: c.Aliases,
		Flags: flagsOf(c.LocalNonPersistentFlags()), Method: c.Annotations[annMethod], APIPath: c.Annotations[annPath],
		MCPTool: c.Annotations[annTool], Writes: c.Annotations[annWrite] == "true", Destructive: c.Annotations[annDestructive] == "true",
		Paginated: c.Annotations[annPaginated] == "true"}
	subs := c.Commands()
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
	for _, s := range subs {
		if s.Hidden || s.Name() == "help" {
			continue
		}
		ci.Commands = append(ci.Commands, describe(s))
	}
	return ci
}

// commandTree describes the whole CLI for agents.
func commandTree(root *cobra.Command) map[string]any {
	d := describe(root)
	return map[string]any{
		"name": "sendio", "version": build.Version,
		"global_flags": flagsOf(root.PersistentFlags()),
		"exit_codes": map[string]string{"0": "ok", "1": "API error", "2": "usage error", "3": "auth", "4": "network",
			"5": "not found", "6": "rate limited", "7": "--wait/--watch timeout"},
		"agent_mode": "JSON on stdout, errors as {\"error\":{code,message,status}} on stderr, no prompts, destructive commands need --yes",
		"commands":   d.Commands,
	}
}

func printTree(w io.Writer, a *App, c *cobra.Command, depth int) {
	subs := c.Commands()
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
	for _, s := range subs {
		if s.Hidden || s.Name() == "help" {
			continue
		}
		name := strings.Repeat("  ", depth) + s.Use
		fmt.Fprintf(w, "%-52s %s\n", name, a.P().S().Dim.Render(s.Short))
		printTree(w, a, s, depth+1)
	}
}
