# sendio: the send.io CLI

`sendio` is send.io's official command-line interface: one static Go binary
for sending email, managing domains, templates, contacts, broadcasts, webhooks
and API keys, and calling any API endpoint. It works for people (tables,
colours, prompts) and for coding agents and scripts (JSON, stable exit codes,
no prompts).

## Install

The web app serves the installers for its environment
(`frontend/console/scripts/gen-install-scripts.mjs`); binaries come from the
GitHub releases of `send-io/cli`, checked against `checksums.txt`:

```sh
curl -fsSL https://app.send.io/install.sh | sh              # macOS, Linux
irm https://app.send.io/install.ps1 | iex                   # Windows PowerShell
```

This works once a release is published there. From source
(Go 1.23+):

```sh
git clone <this repo> && cd <repo>/cli
CGO_ENABLED=0 go build -ldflags "-s -w \
  -X send.io/cli/internal/build.Version=0.1.0 \
  -X send.io/cli/internal/build.Commit=$(git rev-parse --short HEAD) \
  -X send.io/cli/internal/build.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o sendio ./cmd/sendio
sudo mv sendio /usr/local/bin/        # or anywhere on your PATH
sendio completion zsh --install       # bash | zsh | fish | powershell
```

## Quick start

```sh
sendio login                                   # browser sign-in (device code over SSH)
sendio whoami
sendio domains add mail.example.com --auto     # DNS records, or one-click setup
sendio domains verify <id> --watch
sendio emails send --from "Acme <hi@mail.example.com>" --to you@example.com \
  --subject "Hello" --text "It works" --wait delivered
sendio doctor
```

More:

```sh
sendio emails send --template welcome --var name=Ada --to ada@example.com
sendio emails send ... --html @mail.html --attach ./invoice.pdf --scheduled-at "in 1h" --idempotency-key auto
sendio emails batch --file emails.ndjson
sendio emails get <id>        sendio emails events <id>       sendio emails list --all
sendio templates pull ./emails && sendio templates push ./emails --dry-run
sendio contacts import ./contacts.csv --segment <id> --wait
sendio broadcasts send <id> --test-to me@example.com
sendio logs list --limit 20
sendio api GET /domains --jq '.data[].name'
sendio mcp install --client claude-code      # or cursor, vscode, codex, gemini, windsurf, zed, claude-desktop, claude-ai
sendio mcp install --client cursor --local   # the local stdio server (sendio mcp serve) instead of the hosted one
sendio agent setup                           # installs a SKILL.md for Claude Code / Codex / Cursor
sendio open logs
```

Every API operation has a command (`sendio <resource> <verb>`), generated from
the same registry as the send.io MCP server's tools, so the CLI, the MCP
server and the API cannot drift apart. Resources: `emails` (+`received`),
`templates` (+`versions`), `contacts` (+`segments`, `topics`, `imports`),
`contact-properties`, `segments`, `topics`, `broadcasts`, `automations`
(+`runs`, `templates`), `events`, `domains`, `api-keys`, `webhooks`, `suppressions`,
`logs`, `inboxes` (+`threads`, `messages`, `drafts`). Flags map to API fields
(`--first-name` sends `first_name`); object flags take `key=value`,
`key:=<json>` or a JSON object; list flags take repeats or commas; `--data
@body.json` (or `-` for stdin) sends a raw body that flags override. Run
`sendio commands` for the whole tree.

## Live and local tools

```sh
sendio listen --forward-to localhost:3000/webhooks   # webhook events → your app, signed with a session secret
sendio listen --format mailgun --forward-to :3000/mailgun --events email.delivered,email.bounced
sendio trigger email.bounced --override data.bounce.type=Permanent   # a test event to your listeners
sendio trigger --list
sendio logs tail --status 4xx --method POST --path /emails
sendio events tail --type email.bounced,email.complained
sendio inboxes watch agent@agents.example.com --exec ./handle.sh     # message JSON on stdin
sendio templates dev ./emails --open                 # local preview with hot reload
sendio mcp serve --read-only --tools emails,domains  # stdio MCP server, same tools as the hosted one
```

- `listen` opens a WebSocket (`/v0/cli/listen`) and replays every webhook
  event against `--forward-to` with the exact method, headers, body and
  signature a real endpoint in that `--format` would get (sendio/resend =
  Svix headers; mailgun, mailchimp/Mandrill and sendgrid sign with their own
  schemes; postmark, mailjet, brevo do not sign). The secret is made per
  session, so a reconnect prints a new one. `--print-secret` prints it and
  exits; `--skip-verify` accepts a self-signed https target; `--header`
  adds headers; `--print-json` prints bodies. Each forward is acked.
- The streaming commands (`listen`, `logs tail`, `events tail`, `inboxes
  watch`) print one NDJSON record per line in agent mode (`--jq` applies per
  line), reconnect with backoff after a drop or a server restart (close 1001;
  the tails resume with `Last-Event-ID`), refresh the OAuth token before
  reconnecting, and exit 0 on Ctrl-C. The first connection's failure exits
  with the usual code.
- `templates dev` renders with a copy of the backend's engine
  (`internal/tmpl`, kept identical by `go generate ./internal/tmpl` and
  `TestEngineInSync`; `internal/preview` mirrors the send path and
  `TestParityWithBackend` compiles the backend's own renderer to compare).
  Data comes from `<dir>/fixtures/<alias>.json`; the page shows desktop and
  mobile widths, a dark-mode preview, the plain-text part, checks (missing
  variables, non-https links, images without alt, HTML over Gmail's 102 KB
  clip) and a confirmed "Send test".

## Free agent inbox

A free, anonymous, receive-only inbox for agents and scripts at
`name@org.sendioinbox.com`, with no account and no sign-in (the commands work
signed out). The inbox key is generated on this machine and never leaves it;
mail is sealed to that key on arrival, held up to 72 hours (100 messages a
day, 10 MB each), and fetched over a connection the CLI opens. Protocol:
`contracts/free-inbox/PROTOCOL.md`.

```sh
sendio inbox create                                # prompts for name and org, offers suggestions when taken
sendio inbox create --name signup-bot --org acme --json
code=$(sendio inbox wait --code --timeout 2m)      # prints the 6-8 digit code; exit 1 on timeout
sendio inbox wait --from github.com --subject verify --json
sendio inbox watch                                 # live: time, from, subject; risky attachments flagged
sendio inbox poll --wait 25                        # one HTTP long poll (cron, serverless)
sendio inbox read --limit 5 · sendio inbox read --id 42
sendio inbox list
sendio inbox recover <phrase> <address>            # restore on another machine
sendio inbox remove <address>                      # local key and mail only (confirm / --yes)
sendio inbox connect [address] [--open]            # one-time link: move the inbox into your send.io workspace
```

- `create` prints the **recovery phrase once**: base32 of the inbox seed, the
  only way to use the inbox elsewhere. send.io cannot recover it.
- Keys and mail live in `~/.sendio/free/<address>/` (`key.json`, `state.json`,
  `mail/<seq>.eml` and `.json`, all 0600), shared with the Node SDK and the
  MCP tools. `SENDIO_HOME` moves `~/.sendio`. A message is written to disk
  before it is acknowledged; the relay then deletes it.
- The relay is `https://relay.sendioinbox.com`; override with `--relay` or
  `SENDIO_FREE_RELAY` (an inbox remembers the relay it was created on).
- `watch` keeps one WebSocket per relay, reconnects with backoff (1 s to 60 s,
  jittered) and resumes from the last acknowledged message. One live
  connection per inbox: a newer `watch`/`wait` for the same inbox takes over.
- `wait` counts only mail that reaches this machine during the wait (it may
  have reached the relay just before); `--since 10m` also accepts mail saved
  here in the last 10 minutes. With `--code` it prints only the code (JSON
  with `--json`).
- Attachments are never opened. Executables, scripts, macro documents, disk
  images and archives containing them are marked `risky` and warned about.
- `connect` prints a link signed with the inbox key
  (`https://app.send.io/connect#a=…&t=…&n=…&s=…`; `SENDIO_APP_URL`
  overrides the origin). Whoever opens it and signs in attaches the inbox to
  their workspace, where it is read in the dashboard and can send with the
  workspace's credits (up to 50 a day). It works once and expires in 30
  minutes; `--json` gives `{address, link, expires_at}`. Afterwards the key
  no longer reads the inbox: `watch`, `wait` and `poll` stop with
  "<address> was moved into a send.io workspace" (exit 1, code `claimed`)
  instead of reconnecting.
- MCP: `sendio mcp serve` offers `free_inbox_create`, `free_inbox_list`,
  `free_inbox_wait_for_email`, `free_inbox_read` and `free_inbox_connect`,
  also when signed out (then only these). `--tools free-inbox` limits the
  server to them.
- `sendio inbox watch <address>` for an address that is not a free inbox on
  this machine watches a send.io agent inbox, as `sendio inboxes watch`.

## Look and feel

At a terminal the CLI wears the send.io brand ("Select All"): Signal Orange
`#FF4A1C` for accents and success, crimson for errors, muted grey for
secondary text, and your terminal's own foreground for body text (so a wrong
light/dark guess never hides anything). The brand gesture is a text
selection, so the wordmark is "send" on an orange block, and the selected
item in every prompt is drawn the same way.

```text
$ sendio
 ▄▄▄▄▄▄
  send .io   The send.io CLI · 1.4.0
 ▀▀▀▀▀▀      Email from your terminal. sendio --help lists everything.

  ● Acme Inc  you@acme.dev · profile default
  ◗ 1,240.5 ┆ credits ◖                      ← the send ticket: your balance

┃ What would you like to do?
┃ › sendio domains list                     ← the command, to remember next time
┃   Send a test email
┃ ▌ Check domains                           ← orange selection block
┃   Tail logs
┃   …

$ sendio emails send --from hi@acme.dev --to you@example.com --subject Hi --text Hello
▕▃▄▏ Sen[din]g…  1.2s                        ← liquid filling + a sweeping selection (stderr)
✓ Sent in 412 ms  em_8f2c41d07a

$ sendio emails send … --to someone@example.com
✗ Your team has no credits left.  HTTP 402 · insufficient_credits
  → You are out of credits. Check sendio billing balance, then top up at https://app.send.io/settings/billing
```

- `sendio` with no arguments opens the home screen (wordmark, account,
  credit balance and a short arrow-key menu of common actions, each just a
  regular command). `sendio --version` shows the compact wordmark.
- Network calls longer than ~150 ms show the loader on stderr; it clears
  itself when the response arrives. Errors get a one-line cause and the next
  command to run (401 → `sendio login`, 402 → credits and the top-up link,
  403 unverified domain → `sendio domains`, 429 → when to retry).
- Light terminals: detected from `COLORFGBG` or the terminal's answer to the
  background query; force with `SENDIO_THEME=light|dark`. 24-bit colour is
  used when `COLORTERM=truecolor`, otherwise 256 colours.
- None of this reaches scripts: colours are off with `NO_COLOR`,
  `--no-color`, `TERM=dumb` or a non-terminal stdout; animation and the home
  screen also need stdout and stderr terminals, no `CI`, no `--json`/`--jq`/
  `--quiet` (`SENDIO_NO_MOTION=1` turns animation off). Without colour the
  output is the classic plain text (`Error: … (HTTP 402, …)`,
  `sendio 1.4.0`), and agent mode is unchanged.

## Agent mode (the contract)

Agent mode is on when stdout is not a terminal or a coding agent is detected
(`CLAUDECODE`, `CODEX_*`, `CURSOR_AGENT`, `GEMINI_CLI`, `OPENCODE`,
`AI_AGENT`). Force it with `--agent yes|no` (default `auto`).

- Results are JSON on stdout. `--all` on list commands streams NDJSON, one item per line.
- Errors are one line on stderr: `{"error":{"code":"not_found","message":"…","status":404}}`
  (`status` is `null` for errors that are not HTTP responses).
- Never prompts. Destructive commands (delete, cancel, stop, rotate-secret,
  `auth remove`) exit 2 unless `--yes` is passed.
- `--jq '<expr>'` filters any JSON output (string results print raw); `--json`
  forces JSON in human mode.
- `--dry-run` on any write prints the exact HTTP request (method, URL,
  headers with the token redacted, body) and exits 0 without sending it.
- `sendio commands --json` describes every command: flags, the API method
  and path, the MCP tool, and whether it writes, is destructive or paginates.
- No update notices, colours or spinners in agent mode.

### Exit codes

| code | meaning |
|-----:|---------|
| 0 | success |
| 1 | API error (4xx/5xx other than below), or a `--wait` that ended in failure (bounce) |
| 2 | usage error (bad flags or arguments, missing `--yes`) |
| 3 | authentication or permission (401/403, not signed in, expired sign-in) |
| 4 | network error |
| 5 | not found (404) |
| 6 | rate limited (429) |
| 7 | `--wait` / `--watch` timeout |

## Authentication

- `sendio login` signs in with OAuth (PKCE) through a browser and a loopback
  callback on `127.0.0.1` with a random port. Over SSH (`SSH_CONNECTION` /
  `SSH_TTY`), without a display, or with `--device` it uses the device flow:
  open the printed URL anywhere and enter the code. The device shows up under
  *Connected AI apps* as "send.io CLI" with your host name.
- `sendio login --api-key mx_…` stores an API key instead.
- Access tokens are refreshed automatically (refresh tokens rotate) and a
  request that gets 401 is retried once after a refresh.
- `sendio logout` revokes the OAuth tokens and deletes the stored credentials.
- Per command: `--api-key mx_…` or `SENDIO_API_KEY` (no saved login needed).
  `--base-url` / `SENDIO_BASE_URL` points at another API origin (default
  `https://api.send.io`).
- `sendio whoami` shows the team, permission and credential in use.

Credentials are stored in the OS keyring (macOS Keychain, Windows Credential
Manager, Secret Service on Linux). When no keyring is available, or with
`--insecure-storage` / `SENDIO_CREDENTIAL_STORE=file`, they go to
`~/.config/sendio/credentials.json` with mode 0600 (and the CLI says so).
Profile metadata lives in `~/.config/sendio/config.json` (override the folder
with `SENDIO_CONFIG_DIR`).

## Profiles

A profile is an API origin plus a credential (and the team name, for display).

```sh
sendio login -p work                    # sign in and make "work" the default
sendio login -p staging --base-url https://api.staging.example --api-key mx_…
sendio auth list                        # * marks the default
sendio auth switch work
sendio auth status                      # which credential, checked against the API
sendio auth rename staging stage
sendio auth remove stage --yes
sendio -p stage domains list            # one command with another profile (or SENDIO_PROFILE)
```

Precedence: `--api-key` > `SENDIO_API_KEY` > the profile (`-p`, then
`SENDIO_PROFILE`, then the default). Base URL: `--base-url` >
`SENDIO_BASE_URL` > the profile's > `https://api.send.io`.

## Telemetry

None. The CLI sends nothing anywhere except the API requests you ask for and,
at most once a day in human mode, a request for the latest release on GitHub
to print an update notice. Turn that off with `SENDIO_NO_UPDATE_NOTIFIER=1`;
it is always off in CI and in agent mode.

## Development

```sh
go generate ./...     # refresh internal/spec/tools.json from backend/core/internal/transport/mcp/mcp.go
go vet ./... && gofmt -l . && go test ./...
```

`internal/spec/tools.json` is generated from the backend's MCP tool registry;
`internal/cli/restable.go` maps every tool to a command path and a test fails
when a tool has no command. Hand-written experience commands (`emails send`,
`domains add`, `templates pull/push`, `contacts import`, `login`, …) live
next to it in `internal/cli`. New top-level commands (streaming ones such as
`listen` or `logs tail`) register through `cli.Register` in
`internal/cli/extensions.go`.

## Packaging

Nothing is published to package registries yet (Homebrew, Scoop, npm, ghcr). `cli/.goreleaser.yaml` builds darwin/linux/windows
× amd64/arm64 (CGO off, version/commit/date ldflags), archives, checksums,
deb/rpm, a Homebrew cask (`send-io/homebrew-tap`), a Scoop manifest and a
`ghcr.io/send-io/cli` image (owners are placeholders; signing and
notarization are commented out until there are credentials):

```sh
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean
node scripts/npm-packages.mjs   # dist/ → dist/npm: @sendio/cli + @sendio/cli-<os>-<arch>
```

- `install/install.sh` (POSIX, checksum-verified, installs to
  `~/.sendio/bin`) and `install/install.ps1` (Windows).
- `npm/`: the `@sendio/cli` wrapper (the npm name `sendio` is the Node SDK)
  with esbuild-style per-platform optional dependencies and a bin shim.
- `action/`: the `setup-sendio` GitHub Action skeleton.
