package cli

// The declarative command table: every API operation in internal/spec
// (generated from the backend MCP registry) is listed here once, with the
// command path it lives at. TestEveryToolIsMapped fails when a tool is added
// to the registry without a row, or a row names a tool that no longer exists.

type resCmd struct {
	Tool    string
	Path    string // "domains list"
	Aliases []string
	// Confirm marks a destructive command (DELETE always is).
	Confirm bool
	// Columns for human list tables (default: picked from the items).
	Columns []string
	// Flags renames params (param → flag name).
	Flags map[string]string
	// Args takes these body params as positional arguments (after any path
	// params) instead of flags: automations templates use <slug>.
	Args []string
	// Hand marks a tool whose command is hand-written (the row only records
	// where it lives, for the mapping test).
	Hand bool
}

var resourceTable = []resCmd{
	// Emails (send and batch are hand-written experience commands).
	{Tool: "send-email", Path: "emails send", Hand: true},
	{Tool: "send-batch-emails", Path: "emails batch", Hand: true},
	{Tool: "list-emails", Path: "emails list", Aliases: []string{"ls"}, Columns: []string{"id", "to", "subject", "last_event", "created_at"}},
	{Tool: "get-email", Path: "emails get"},
	{Tool: "update-email", Path: "emails update", Flags: map[string]string{"scheduledAt": "scheduled-at"}},
	{Tool: "cancel-email", Path: "emails cancel", Confirm: true},
	{Tool: "list-email-events", Path: "emails events", Columns: []string{"type", "recipient", "created_at", "detail"}},
	{Tool: "list-email-attachments", Path: "emails attachments", Columns: []string{"id", "filename", "content_type", "size"}},
	{Tool: "get-email-attachment", Path: "emails attachment"},

	{Tool: "list-received-emails", Path: "emails received list", Aliases: []string{"ls"}, Columns: []string{"id", "from", "to", "subject", "created_at"}},
	{Tool: "get-received-email", Path: "emails received get"},
	{Tool: "list-received-email-attachments", Path: "emails received attachments", Columns: []string{"id", "filename", "content_type", "size"}},
	{Tool: "get-received-email-attachment", Path: "emails received attachment"},

	{Tool: "create-template", Path: "templates create"},
	{Tool: "list-templates", Path: "templates list", Aliases: []string{"ls"}, Columns: []string{"id", "name", "alias", "status", "updated_at"}},
	{Tool: "get-template", Path: "templates get"},
	{Tool: "update-template", Path: "templates update"},
	{Tool: "publish-template", Path: "templates publish"},
	{Tool: "duplicate-template", Path: "templates duplicate"},
	{Tool: "remove-template", Path: "templates delete", Aliases: []string{"rm", "remove"}},
	{Tool: "render-template", Path: "templates render"},
	{Tool: "send-test-template", Path: "templates send-test"},
	{Tool: "list-template-versions", Path: "templates versions list", Aliases: []string{"ls"}},
	{Tool: "get-template-version", Path: "templates versions get"},
	{Tool: "update-template-version", Path: "templates versions update"},
	{Tool: "remove-template-version", Path: "templates versions delete", Aliases: []string{"rm", "remove"}},
	{Tool: "activate-template-version", Path: "templates versions activate"},

	{Tool: "create-contact", Path: "contacts create"},
	{Tool: "list-contacts", Path: "contacts list", Aliases: []string{"ls"}, Columns: []string{"id", "email", "first_name", "last_name", "unsubscribed", "created_at"}},
	{Tool: "get-contact", Path: "contacts get"},
	{Tool: "update-contact", Path: "contacts update"},
	{Tool: "remove-contact", Path: "contacts delete", Aliases: []string{"rm", "remove"}},
	{Tool: "list-contact-segments", Path: "contacts segments list", Aliases: []string{"ls"}},
	{Tool: "add-contact-to-segment", Path: "contacts segments add"},
	{Tool: "remove-contact-from-segment", Path: "contacts segments remove", Aliases: []string{"rm"}},
	{Tool: "list-contact-topics", Path: "contacts topics list", Aliases: []string{"ls"}},
	{Tool: "update-contact-topics", Path: "contacts topics update"},

	{Tool: "create-contact-import", Path: "contacts imports create"},
	{Tool: "list-contact-imports", Path: "contacts imports list", Aliases: []string{"ls"}},
	{Tool: "get-contact-import", Path: "contacts imports get"},
	{Tool: "start-contact-import", Path: "contacts imports start"},

	{Tool: "create-contact-property", Path: "contact-properties create"},
	{Tool: "list-contact-properties", Path: "contact-properties list", Aliases: []string{"ls"}},
	{Tool: "get-contact-property", Path: "contact-properties get"},
	{Tool: "update-contact-property", Path: "contact-properties update"},
	{Tool: "remove-contact-property", Path: "contact-properties delete", Aliases: []string{"rm", "remove"}},

	{Tool: "create-segment", Path: "segments create"},
	{Tool: "list-segments", Path: "segments list", Aliases: []string{"ls"}},
	{Tool: "get-segment", Path: "segments get"},
	{Tool: "update-segment", Path: "segments update"},
	{Tool: "remove-segment", Path: "segments delete", Aliases: []string{"rm", "remove"}},
	{Tool: "list-segment-contacts", Path: "segments contacts"},

	{Tool: "create-topic", Path: "topics create"},
	{Tool: "list-topics", Path: "topics list", Aliases: []string{"ls"}},
	{Tool: "get-topic", Path: "topics get"},
	{Tool: "update-topic", Path: "topics update"},
	{Tool: "remove-topic", Path: "topics delete", Aliases: []string{"rm", "remove"}},

	{Tool: "create-broadcast", Path: "broadcasts create"},
	{Tool: "list-broadcasts", Path: "broadcasts list", Aliases: []string{"ls"}, Columns: []string{"id", "name", "subject", "status", "created_at"}},
	{Tool: "get-broadcast", Path: "broadcasts get"},
	{Tool: "update-broadcast", Path: "broadcasts update"},
	{Tool: "send-broadcast", Path: "broadcasts send"},
	{Tool: "cancel-broadcast", Path: "broadcasts cancel", Confirm: true},
	{Tool: "duplicate-broadcast", Path: "broadcasts duplicate"},
	{Tool: "remove-broadcast", Path: "broadcasts delete", Aliases: []string{"rm", "remove"}},
	{Tool: "estimate-broadcast", Path: "broadcasts estimate"},
	{Tool: "send-test-broadcast", Path: "broadcasts test"},
	{Tool: "get-broadcast-stats", Path: "broadcasts stats"},
	{Tool: "list-broadcast-recipients", Path: "broadcasts recipients", Columns: []string{"email", "contact_id", "count", "bounce_type"}},
	{Tool: "list-broadcast-clicked-links", Path: "broadcasts clicked-links", Aliases: []string{"links"}, Columns: []string{"url", "clicks", "unique_clicks"}},
	{Tool: "list-broadcast-versions", Path: "broadcasts versions list", Aliases: []string{"ls"}},
	{Tool: "get-broadcast-version", Path: "broadcasts versions get"},
	{Tool: "update-broadcast-version", Path: "broadcasts versions update"},
	{Tool: "restore-broadcast-version", Path: "broadcasts versions restore"},

	{Tool: "create-automation", Path: "automations create"},
	{Tool: "list-automations", Path: "automations list", Aliases: []string{"ls"}},
	{Tool: "import-automation-http-step", Path: "automations http import"},
	{Tool: "get-automation", Path: "automations get"},
	{Tool: "update-automation", Path: "automations update"},
	{Tool: "duplicate-automation", Path: "automations duplicate"},
	{Tool: "stop-automation", Path: "automations stop", Confirm: true},
	{Tool: "remove-automation", Path: "automations delete", Aliases: []string{"rm", "remove"}},
	{Tool: "list-automation-runs", Path: "automations runs list", Aliases: []string{"ls"}},
	{Tool: "get-automation-run", Path: "automations runs get"},
	{Tool: "test-automation-http", Path: "automations http test"},
	{Tool: "get-automation-signing-secret", Path: "automations signing-secret get"},
	{Tool: "rotate-automation-signing-secret", Path: "automations signing-secret rotate", Confirm: true},
	{Tool: "list-automation-templates", Path: "automations templates list", Aliases: []string{"ls"}, Columns: []string{"slug", "name", "category", "kind", "email_count", "in_use_count"}},
	{Tool: "get-automation-template", Path: "automations templates get"},
	{Tool: "create-automation-from-template", Path: "automations templates use", Args: []string{"slug"}},

	{Tool: "send-event", Path: "events send"},
	{Tool: "create-event", Path: "events create"},
	{Tool: "list-events", Path: "events list", Aliases: []string{"ls"}},
	{Tool: "get-event", Path: "events get"},
	{Tool: "update-event", Path: "events update"},
	{Tool: "remove-event", Path: "events delete", Aliases: []string{"rm", "remove"}},

	{Tool: "create-domain", Path: "domains create"},
	{Tool: "list-domains", Path: "domains list", Aliases: []string{"ls"}, Columns: []string{"id", "name", "status", "region", "created_at"}},
	{Tool: "get-domain", Path: "domains get"},
	{Tool: "update-domain", Path: "domains update"},
	{Tool: "verify-domain", Path: "domains verify", Hand: true},
	{Tool: "remove-domain", Path: "domains delete", Aliases: []string{"rm", "remove"}},

	{Tool: "create-api-key", Path: "api-keys create"},
	{Tool: "list-api-keys", Path: "api-keys list", Aliases: []string{"ls"}, Columns: []string{"id", "name", "permission", "token_prefix", "last_used_at", "created_at"}},
	{Tool: "get-api-key", Path: "api-keys get"},
	{Tool: "update-api-key", Path: "api-keys update"},
	{Tool: "remove-api-key", Path: "api-keys delete", Aliases: []string{"rm", "remove", "revoke"}},

	{Tool: "create-webhook", Path: "webhooks create"},
	{Tool: "list-webhooks", Path: "webhooks list", Aliases: []string{"ls"}, Columns: []string{"id", "endpoint", "events", "status", "format", "created_at"}},
	{Tool: "get-webhook", Path: "webhooks get"},
	{Tool: "update-webhook", Path: "webhooks update"},
	{Tool: "rotate-webhook-signing-secret", Path: "webhooks rotate-secret", Confirm: true},
	{Tool: "remove-webhook", Path: "webhooks delete", Aliases: []string{"rm", "remove"}},

	{Tool: "add-suppression", Path: "suppressions add", Aliases: []string{"create"}},
	{Tool: "list-suppressions", Path: "suppressions list", Aliases: []string{"ls"}, Columns: []string{"id", "email", "origin", "comment", "created_at"}},
	{Tool: "get-suppression", Path: "suppressions get"},
	{Tool: "remove-suppression", Path: "suppressions delete", Aliases: []string{"rm", "remove"}},

	{Tool: "list-logs", Path: "logs list", Aliases: []string{"ls"}, Columns: []string{"id", "method", "endpoint", "response_status", "created_at"}},
	{Tool: "get-log", Path: "logs get"},

	{Tool: "get-credit-balance", Path: "billing balance"},
	{Tool: "list-credit-transactions", Path: "billing transactions", Columns: []string{"id", "kind", "amount", "balance_after", "created_at"}},
	{Tool: "get-pricing", Path: "billing pricing"},

	{Tool: "list-mailboxes", Path: "mailboxes list", Aliases: []string{"ls"}, Columns: []string{"id", "address", "kind", "imap_smtp", "warmup_enabled", "health"}},
	{Tool: "create-mailbox", Path: "mailboxes create"},
	{Tool: "update-mailbox", Path: "mailboxes update"},
	{Tool: "reset-mailbox-password", Path: "mailboxes reset-password", Confirm: true},
	{Tool: "delete-mailbox", Path: "mailboxes delete", Aliases: []string{"rm", "remove"}},

	{Tool: "create-inbox", Path: "inboxes create"},
	{Tool: "list-inboxes", Path: "inboxes list", Aliases: []string{"ls"}, Columns: []string{"id", "address", "display_name", "created_at"}},
	{Tool: "get-inbox", Path: "inboxes get"},
	{Tool: "delete-inbox", Path: "inboxes delete", Aliases: []string{"rm", "remove"}},
	{Tool: "list-inbox-threads", Path: "inboxes threads list", Aliases: []string{"ls"}, Flags: map[string]string{"q": "query"}},
	{Tool: "get-inbox-thread", Path: "inboxes threads get"},
	{Tool: "reply-to-thread", Path: "inboxes threads reply"},
	{Tool: "forward-thread", Path: "inboxes threads forward"},
	{Tool: "update-thread", Path: "inboxes threads update"},
	{Tool: "list-inbox-messages", Path: "inboxes messages list", Aliases: []string{"ls"}},
	{Tool: "send-inbox-message", Path: "inboxes messages send"},
	{Tool: "create-draft", Path: "inboxes drafts create"},
	{Tool: "list-drafts", Path: "inboxes drafts list", Aliases: []string{"ls"}},
	{Tool: "send-draft", Path: "inboxes drafts send"},
}

// groupShort describes the command groups.
var groupShort = map[string]string{
	"emails":                "Send, schedule and inspect emails",
	"emails received":       "Emails received on your domains",
	"templates":             "Email templates (and pull/push to a folder)",
	"templates versions":    "Saved template versions",
	"contacts":              "Contacts (and CSV imports)",
	"contacts segments":     "A contact's segment memberships",
	"contacts topics":       "A contact's topic subscriptions",
	"contacts imports":      "Contact imports",
	"contact-properties":    "Custom contact properties",
	"segments":              "Segments (static contact lists)",
	"topics":                "Subscription topics",
	"broadcasts":            "Broadcasts to segments",
	"broadcasts versions":   "Saved broadcast versions",
	"automations":           "Automations",
	"automations runs":      "Automation runs",
	"automations templates": "Ready-made workflow templates (list, preview, set up)",
	"events":                "Custom events (definitions and sends)",
	"domains":               "Sending domains and DNS verification",
	"api-keys":              "API keys",
	"webhooks":              "Webhook endpoints",
	"suppressions":          "Suppressed addresses",
	"logs":                  "API request logs",
	"billing":               "Credit balance, transactions and pricing",
	"mailboxes":             "Mailboxes (IMAP/SMTP accounts): create, warmup, password, delete",
	"inboxes":               "Agent inboxes, threads, messages and drafts",
	"inboxes threads":       "Inbox threads",
	"inboxes messages":      "Inbox messages",
	"inboxes drafts":        "Inbox drafts",
	"auth":                  "Manage saved profiles and credentials",
	"mcp":                   "Connect AI clients to the send.io MCP server",
	"agent":                 "Set up coding agents to use this CLI",
}
