// Package build carries values set at link time:
//
//	go build -ldflags "-X send.io/cli/internal/build.Version=1.2.3 -X send.io/cli/internal/build.Commit=abc -X send.io/cli/internal/build.Date=2026-10-08"
package build

var (
	// Version is the release version ("dev" for local builds).
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "none"
	// Date is the build date (RFC 3339).
	Date = "unknown"
	// UpdateRepo is the GitHub "owner/name" whose releases the update check
	// reads. Placeholder until the CLI is published.
	UpdateRepo = "send-io/cli"
	// Each environment's release sets these three:
	// BaseURL is the API origin used when no --base-url / SENDIO_BASE_URL.
	BaseURL = "https://api.send.io"
	// LatestURL serves the newest version as plain text; empty: GitHub.
	LatestURL = ""
	// InstallURL is the install script the update notice points to.
	InstallURL = ""
)
