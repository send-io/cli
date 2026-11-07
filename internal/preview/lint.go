package preview

import (
	"fmt"
	"regexp"
	"strings"
)

// GmailClipBytes is where Gmail clips a message ("[Message clipped]  View
// entire message"): 102 KB of HTML.
const GmailClipBytes = 102 * 1024

var (
	hrefRe    = regexp.MustCompile(`(?is)<a\b[^>]*?\bhref\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	imgRe     = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	altRe     = regexp.MustCompile(`(?is)\balt\s*=`)
	srcRe     = regexp.MustCompile(`(?is)\bsrc\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
)

func attrValue(m []string) string {
	for _, s := range m[2:] {
		if s != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// Lint checks a rendered email: links that are not https, images without
// alt text, a body Gmail would clip, an empty subject.
func Lint(r Result) []Warning {
	var w []Warning
	add := func(level, code, part, format string, args ...any) {
		w = append(w, Warning{Level: level, Code: code, Part: part, Message: fmt.Sprintf(format, args...)})
	}
	if strings.TrimSpace(r.Subject) == "" {
		add("warning", "empty_subject", "subject", "The subject is empty (a send needs one).")
	}
	body := commentRe.ReplaceAllString(r.HTML, "")
	seen := map[string]bool{}
	for _, m := range hrefRe.FindAllStringSubmatch(body, -1) {
		u := attrValue(m)
		lu := strings.ToLower(u)
		if strings.HasPrefix(lu, "http://") && !seen[u] {
			seen[u] = true
			add("warning", "insecure_link", "html", "Link is not https: %s", u)
		}
	}
	missingAlt := 0
	example := ""
	for _, tag := range imgRe.FindAllString(body, -1) {
		if !altRe.MatchString(tag) {
			missingAlt++
			if example == "" {
				if m := srcRe.FindStringSubmatch(tag); m != nil {
					example = attrValue(m)
				}
			}
		}
		if m := srcRe.FindStringSubmatch(tag); m != nil && strings.HasPrefix(strings.ToLower(attrValue(m)), "http://") {
			add("warning", "insecure_image", "html", "Image is not loaded over https: %s", attrValue(m))
		}
	}
	if missingAlt > 0 {
		msg := fmt.Sprintf("%d image(s) without alt text", missingAlt)
		if example != "" {
			msg += " (e.g. " + example + ")"
		}
		add("warning", "image_alt", "html", "%s: screen readers and clients that block images show nothing.", msg)
	}
	if n := len(r.HTML); n > GmailClipBytes {
		add("warning", "gmail_clipping", "html", "The HTML is %.1f KB; Gmail clips messages over 102 KB (the rest, and its unsubscribe link and open pixel, is hidden).", float64(n)/1024)
	} else if n > GmailClipBytes*9/10 {
		add("info", "gmail_clipping_near", "html", "The HTML is %.1f KB, close to Gmail's 102 KB clipping limit.", float64(n)/1024)
	}
	if strings.TrimSpace(r.HTML) != "" && strings.TrimSpace(r.Text) == "" {
		add("info", "no_text_part", "text", "No plain-text part (template.txt): some clients and spam filters prefer one.")
	}
	return w
}
