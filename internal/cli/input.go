package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"send.io/cli/internal/api"
)

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func jsonRaw(b []byte) json.RawMessage { return json.RawMessage(b) }

// readValue resolves a flag value: "@path" reads a file, "-" reads stdin,
// anything else is the literal value.
func (a *App) readValue(v string) (string, error) {
	switch {
	case v == "-":
		b, err := io.ReadAll(a.Stdin)
		if err != nil {
			return "", api.UsageError("reading stdin: %v", err)
		}
		return string(b), nil
	case strings.HasPrefix(v, "@"):
		b, err := os.ReadFile(a.path(v[1:]))
		if err != nil {
			return "", api.UsageError("reading %s: %v", v[1:], err)
		}
		return string(b), nil
	}
	return v, nil
}

// path resolves a user path against the working directory (and ~).
func (a *App) path(p string) string {
	if strings.HasPrefix(p, "~/") && a.HomeDir != "" {
		return filepath.Join(a.HomeDir, p[2:])
	}
	if filepath.IsAbs(p) || a.WorkDir == "" {
		return p
	}
	return filepath.Join(a.WorkDir, p)
}

// decodeJSON parses JSON keeping numbers exact.
func decodeJSON(s string) (any, error) {
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, fmt.Errorf("unexpected data after the JSON value")
	}
	return v, nil
}

// readJSON resolves a flag value (literal JSON, @file or -) and parses it.
func (a *App) readJSON(flag, v string) (any, error) {
	s, err := a.readValue(v)
	if err != nil {
		return nil, err
	}
	val, err := decodeJSON(s)
	if err != nil {
		return nil, api.UsageError("--%s: invalid JSON: %v", flag, err)
	}
	return val, nil
}

// parseKV splits "k=v" (string value) or "k:=json" (typed value).
func parseKV(flag, s string) (string, any, error) {
	if i := strings.Index(s, ":="); i > 0 && (strings.Index(s, "=") == -1 || strings.Index(s, "=") > i) {
		v, err := decodeJSON(s[i+2:])
		if err != nil {
			return "", nil, api.UsageError("--%s %s: the value after := must be JSON", flag, s)
		}
		return s[:i], v, nil
	}
	i := strings.Index(s, "=")
	if i <= 0 {
		return "", nil, api.UsageError("--%s expects key=value, got %q", flag, s)
	}
	return s[:i], s[i+1:], nil
}

// objectFlag merges repeated values of an object flag: each value is a JSON
// object, @file.json, or key=value / key:=json pairs.
func (a *App) objectFlag(flag string, vals []string) (map[string]any, error) {
	m := map[string]any{}
	for _, v := range vals {
		t := strings.TrimSpace(v)
		if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "@") || t == "-" {
			j, err := a.readJSON(flag, t)
			if err != nil {
				return nil, err
			}
			o, ok := j.(map[string]any)
			if !ok {
				return nil, api.UsageError("--%s: expected a JSON object", flag)
			}
			for k, x := range o {
				m[k] = x
			}
			continue
		}
		k, x, err := parseKV(flag, v)
		if err != nil {
			return nil, err
		}
		m[k] = x
	}
	return m, nil
}

// objectsFlag collects an array-of-objects flag: each value is a JSON object,
// a JSON array of objects, or @file holding either.
func (a *App) objectsFlag(flag string, vals []string) ([]any, error) {
	var outv []any
	for _, v := range vals {
		j, err := a.readJSON(flag, strings.TrimSpace(v))
		if err != nil {
			return nil, err
		}
		switch x := j.(type) {
		case []any:
			outv = append(outv, x...)
		case map[string]any:
			outv = append(outv, x)
		default:
			return nil, api.UsageError("--%s: expected a JSON object or array", flag)
		}
	}
	return outv, nil
}

// splitList splits comma-separated, repeatable list values.
func splitList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

var inRe = regexp.MustCompile(`(?i)^in\s+(\d+)\s*(s|secs?|seconds?|m|mins?|minutes?|h|hrs?|hours?|d|days?|w|weeks?)$`)

// parseWhen turns "in 1h", "in 30 minutes", "in 2 days" or "in 1h30m" into
// RFC 3339; RFC 3339 and any other text (the API understands natural
// language) pass through.
func parseWhen(s string, now time.Time) string {
	s = strings.TrimSpace(s)
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return s
	}
	if m := inRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := strings.ToLower(m[2])
		var d time.Duration
		switch unit[0] {
		case 's':
			d = time.Second
		case 'm':
			d = time.Minute
		case 'h':
			d = time.Hour
		case 'd':
			d = 24 * time.Hour
		case 'w':
			d = 7 * 24 * time.Hour
		}
		return now.Add(time.Duration(n) * d).UTC().Format(time.RFC3339)
	}
	if strings.HasPrefix(strings.ToLower(s), "in ") {
		if d, err := time.ParseDuration(strings.ReplaceAll(strings.TrimSpace(s[3:]), " ", "")); err == nil {
			return now.Add(d).UTC().Format(time.RFC3339)
		}
	}
	return s
}

func randomKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// readNDJSONOrArray parses a JSON array, a single object, or NDJSON lines.
func readNDJSONOrArray(s string) ([]any, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, fmt.Errorf("empty input")
	}
	if strings.HasPrefix(t, "[") {
		v, err := decodeJSON(t)
		if err != nil {
			return nil, err
		}
		return v.([]any), nil
	}
	var outv []any
	d := json.NewDecoder(bytes.NewReader([]byte(t)))
	d.UseNumber()
	for d.More() {
		var v any
		if err := d.Decode(&v); err != nil {
			return nil, err
		}
		outv = append(outv, v)
	}
	return outv, nil
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	switch v := m[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func kebab(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == '_':
			b.WriteByte('-')
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r + 32)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
