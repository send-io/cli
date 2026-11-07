package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The repository's integrations/agent-skills/ directory is the package `npx skills add`
// installs from (see integrations/agent-skills/README.md). These tests keep its sendio-cli skill
// identical to the one this CLI embeds, and every skill's frontmatter valid.

var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func repoSkillsDir(t *testing.T) string {
	dir := filepath.Join("..", "..", "..", "integrations", "agent-skills")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no integrations/agent-skills/ directory (CLI-only checkout)")
	}
	return dir
}

// frontmatter returns the top-level "key: value" pairs of a SKILL.md header.
func frontmatter(t *testing.T, path, doc string) map[string]string {
	if !strings.HasPrefix(doc, "---\n") {
		t.Fatalf("%s: does not start with --- frontmatter", path)
	}
	end := strings.Index(doc[4:], "\n---\n")
	if end < 0 {
		t.Fatalf("%s: frontmatter is not closed with ---", path)
	}
	out := map[string]string{}
	for _, line := range strings.Split(doc[4:4+end], "\n") {
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "#") {
			continue // nested keys (metadata:) and comments
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("%s: frontmatter line %q is not key: value", path, line)
		}
		v = strings.TrimSpace(v)
		// A plain YAML scalar cannot hold ": " or " #", or start with a YAML indicator.
		if v != "" && !strings.HasPrefix(v, `"`) && !strings.HasPrefix(v, "'") {
			if strings.Contains(v, ": ") || strings.Contains(v, " #") || strings.ContainsAny(v[:1], "&*!|>%@`{[") {
				t.Fatalf("%s: %s needs quoting in YAML: %q", path, k, v)
			}
		}
		out[k] = strings.Trim(v, `"'`)
	}
	return out
}

func TestRepoSkillsFrontmatter(t *testing.T) {
	dir := repoSkillsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		fm := frontmatter(t, path, string(b))
		if fm["name"] != e.Name() || !skillName.MatchString(fm["name"]) {
			t.Errorf("%s: name %q must equal the folder name, lowercase with hyphens", path, fm["name"])
		}
		if d := fm["description"]; len(d) < 40 || len(d) > 1024 {
			t.Errorf("%s: description must be 40 to 1024 characters, got %d", path, len(d))
		}
		if strings.Contains(string(b), "sendio.run") {
			t.Errorf("%s: link send.io only, never the sendio.run development environment", path)
		}
		seen++
	}
	for _, want := range []string{"sendio-api", "sendio-cli", "sendio-mcp", "sendio-migrate", "sendio-agent-inboxes"} {
		if _, err := os.Stat(filepath.Join(dir, want, "SKILL.md")); err != nil {
			t.Errorf("missing skill %s", want)
		}
	}
	if seen == 0 {
		t.Fatal("no skills found")
	}
}

func TestRepoSkillMatchesEmbedded(t *testing.T) {
	dir := repoSkillsDir(t)
	b, err := os.ReadFile(filepath.Join(dir, "sendio-cli", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != SkillMD() {
		t.Fatal("integrations/agent-skills/sendio-cli/SKILL.md differs from cli/internal/cli/skill/SKILL.md: copy the CLI's file over it")
	}
}
