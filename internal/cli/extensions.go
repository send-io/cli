package cli

import "github.com/spf13/cobra"

// extensions add commands to the tree after the built-in ones. The streaming
// commands of the CLI contract (sections 2 to 5) plug in here:
//
//	sendio listen          WebSocket GET /v0/cli/listen       (contract §2)
//	sendio trigger         GET/POST /v0/cli/trigger[/events]  (contract §3)
//	sendio logs tail       SSE GET /v0/cli/logs/stream        (contract §4)
//	sendio events tail     SSE GET /v0/cli/events/stream      (contract §4)
//	sendio inboxes watch   agent-inbox WebSocket              (contract §5)
//	sendio inbox …         free anonymous inbox (contracts/free-inbox/PROTOCOL.md)
//	sendio templates dev   local preview server
//	sendio mcp serve       stdio MCP bridge
//
// An extension gets the App (Client(true) for an authenticated api.Client
// whose Stream method opens SSE/chunked responses with auth and refresh;
// P() for output in the human/agent modes) and the root command; use
// a.group(root, []string{"logs"}) to attach under an existing group.
var extensions []func(a *App, root *cobra.Command)

// Register adds an extension (call from an init function).
func Register(fn func(a *App, root *cobra.Command)) { extensions = append(extensions, fn) }
