package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"send.io/cli/internal/build"
	"send.io/cli/internal/config"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"1.2.0", "1.1.9", true}, {"v1.10.0", "1.9.0", true}, {"1.0.0", "1.0.0", false}, {"0.9", "1.0.0", false}, {"1.0.1", "dev", false}, {"", "1.0.0", false}} {
		if newer(c.a, c.b) != c.want {
			t.Errorf("newer(%q, %q) != %v", c.a, c.b, c.want)
		}
	}
}

func TestUpdateNotice(t *testing.T) {
	f := newFake(t)
	f.json("GET /releases/latest", 200, `{"tag_name":"v1.4.0"}`)
	h := newHarness(t, "")
	delete(h.env, "SENDIO_NO_UPDATE_NOTIFIER")
	h.env["SENDIO_UPDATE_URL"] = f.URL + "/releases/latest"
	a := h.newApp("")
	if v := a.latestVersion(context.Background(), time.Second); v != "1.4.0" {
		t.Fatalf("latest %q", v)
	}
	old := build.Version
	build.Version = "1.3.0"
	defer func() { build.Version = old }()
	st := &config.Store{Dir: h.dir}
	st.Save(&config.Config{Profiles: map[string]*config.Profile{}, LatestVersion: "1.4.0", LastUpdateCheck: h.clock.Now()})
	// Human mode: the cached newer release is announced on stderr.
	h.run("version", "--agent", "no")
	if !strings.Contains(h.errOut(), "1.3.0 → 1.4.0") {
		t.Fatalf("no notice: %q", h.errOut())
	}
	// Agent mode and the opt-out stay silent.
	h.run("version")
	if strings.Contains(h.errOut(), "release") {
		t.Fatalf("notice in agent mode: %q", h.errOut())
	}
	h.env["SENDIO_NO_UPDATE_NOTIFIER"] = "1"
	h.run("version", "--agent", "no")
	if strings.Contains(h.errOut(), "release") {
		t.Fatal("notice despite SENDIO_NO_UPDATE_NOTIFIER")
	}
}
