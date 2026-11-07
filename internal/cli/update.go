package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"send.io/cli/internal/build"
)

// The update notice: at most one GitHub request a day, in the background,
// never blocking a command, never in CI or agent mode, and off with
// SENDIO_NO_UPDATE_NOTIFIER=1.

func (a *App) updatesDisabled() bool {
	return a.env("SENDIO_NO_UPDATE_NOTIFIER") != "" || a.env("CI") != "" || a.agent || a.P().Machine
}

func (a *App) updateURL() string {
	if u := a.env("SENDIO_UPDATE_URL"); u != "" {
		return u
	}
	if build.LatestURL != "" {
		return build.LatestURL
	}
	return "https://api.github.com/repos/" + build.UpdateRepo + "/releases/latest"
}

// latestVersion asks GitHub for the latest release ("" when unknown).
func (a *App) latestVersion(ctx context.Context, timeout time.Duration) string {
	if a.env("SENDIO_NO_UPDATE_NOTIFIER") != "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.updateURL(), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent())
	hc := &http.Client{Timeout: timeout}
	if a.HTTP != nil {
		hc.Transport = a.HTTP.Transport
	}
	resp, err := hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return ""
	}
	// GitHub answers JSON ({"tag_name":"v1.2.3"}); a site's /cli/download/latest
	// answers the bare version.
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if json.Unmarshal(body, &rel) == nil && rel.TagName != "" {
		return strings.TrimPrefix(rel.TagName, "v")
	}
	if v := strings.TrimSpace(string(body)); v != "" && !strings.ContainsAny(v, " {<") {
		return strings.TrimPrefix(v, "v")
	}
	return ""
}

// newer reports whether version a is newer than b (semver-ish).
func newer(a, b string) bool {
	if b == "dev" || a == "" {
		return false
	}
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(strings.SplitN(pa[i], "-", 2)[0])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(strings.SplitN(pb[i], "-", 2)[0])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func (a *App) startUpdateCheck(cmd *cobra.Command) {
	if a.updatesDisabled() || build.Version == "dev" {
		return
	}
	cfg, err := a.Store().Load()
	if err != nil {
		return
	}
	if newer(cfg.LatestVersion, build.Version) {
		if build.InstallURL != "" {
			a.notice = fmt.Sprintf("A new sendio release is available: %s → %s. Update: curl -fsSL %s | sh", build.Version, cfg.LatestVersion, build.InstallURL)
		} else {
			a.notice = fmt.Sprintf("A new sendio release is available: %s → %s (https://github.com/%s/releases)", build.Version, cfg.LatestVersion, build.UpdateRepo)
		}
	}
	if a.now().Sub(cfg.LastUpdateCheck) < 24*time.Hour {
		return
	}
	a.update = make(chan string, 1)
	go func() { a.update <- a.latestVersion(context.Background(), 2*time.Second) }()
}

// finishUpdateCheck records a finished background check without waiting.
func (a *App) finishUpdateCheck() {
	if a.notice != "" {
		fmt.Fprintln(a.Stderr, a.P().ES().Dim.Render(a.notice))
	}
	if a.update == nil {
		return
	}
	select {
	case v := <-a.update:
		cfg, err := a.Store().Load()
		if err != nil {
			return
		}
		cfg.LastUpdateCheck = a.now()
		if v != "" {
			cfg.LatestVersion = v
		}
		_ = a.Store().Save(cfg)
	default:
	}
}
