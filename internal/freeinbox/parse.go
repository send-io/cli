package freeinbox

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"path"
	"regexp"
	"strings"
)

// riskyExt are the attachment extensions clients warn about (PROTOCOL.md).
var riskyExt = map[string]bool{
	"exe": true, "scr": true, "bat": true, "cmd": true, "com": true, "pif": true, "js": true, "jse": true,
	"vbs": true, "vbe": true, "wsf": true, "hta": true, "msi": true, "ps1": true, "jar": true, "lnk": true,
	"iso": true, "img": true, "docm": true, "xlsm": true, "pptm": true, "dotm": true,
}

func ext(name string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(strings.TrimSpace(name)), "."))
}

// RiskyName reports whether a filename has a risky extension.
func RiskyName(name string) bool { return riskyExt[ext(name)] }

var riskyNameInBytes = regexp.MustCompile(`(?i)[\w\-. ]+\.(exe|scr|bat|cmd|com|pif|js|jse|vbs|vbe|wsf|hta|msi|ps1|jar|lnk|iso|img|docm|xlsm|pptm|dotm)\x00`)

// Risky decides whether an attachment is risky: a risky extension, a
// Windows executable (starts with "MZ"), or an archive (zip, rar, 7z) that
// contains a risky name. zip entries are listed; rar and 7z are scanned for
// stored names (best effort: 7z headers may be compressed).
func Risky(filename string, data []byte) bool {
	if RiskyName(filename) || bytes.HasPrefix(data, []byte("MZ")) {
		return true
	}
	switch {
	case ext(filename) == "zip" || bytes.HasPrefix(data, []byte("PK\x03\x04")):
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return false
		}
		for _, f := range zr.File {
			if RiskyName(f.Name) {
				return true
			}
		}
	case ext(filename) == "rar" || bytes.HasPrefix(data, []byte("Rar!\x1a\x07")),
		ext(filename) == "7z" || bytes.HasPrefix(data, []byte("7z\xbc\xaf\x27\x1c")):
		if riskyNameInBytes.Match(data) {
			return true
		}
		// rar stores names without a terminator: look for ".ext" followed by
		// a non-name byte.
		low := bytes.ToLower(data)
		for e := range riskyExt {
			needle := []byte("." + e)
			for i := bytes.Index(low, needle); i >= 0; {
				end := i + len(needle)
				if end == len(low) || !isNameByte(low[end]) {
					return true
				}
				j := bytes.Index(low[end:], needle)
				if j < 0 {
					break
				}
				i = end + j
			}
		}
	}
	return false
}

func isNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

// Parsed is what the client reads out of a raw message.
type Parsed struct {
	From, To, Subject, Date string
	Text                    string // text/plain body (or HTML stripped to text)
	HTML                    string
	Attachments             []Attachment
}

var wordDecoder = mime.WordDecoder{CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
	return input, nil // best effort: pass other charsets through
}}

func decodeHeader(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

// Parse reads headers, bodies and attachments of a raw RFC 5322 message. It
// never fails: a message it cannot parse yields what it could read.
func Parse(raw []byte) *Parsed {
	p := &Parsed{}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return p
	}
	h := msg.Header
	p.From, p.To = decodeHeader(h.Get("From")), decodeHeader(h.Get("To"))
	p.Subject, p.Date = decodeHeader(h.Get("Subject")), h.Get("Date")
	if t, err := h.Date(); err == nil {
		p.Date = t.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	p.walk(h.Get("Content-Type"), h.Get("Content-Transfer-Encoding"), h.Get("Content-Disposition"), msg.Body, 0)
	if p.Text == "" && p.HTML != "" {
		p.Text = HTMLToText(p.HTML)
	}
	if p.Attachments == nil {
		p.Attachments = []Attachment{}
	}
	return p
}

func decodeBody(cte string, r io.Reader) []byte {
	var rd io.Reader = r
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		rd = base64.NewDecoder(base64.StdEncoding, &stripSpace{r: r})
	case "quoted-printable":
		rd = quotedprintable.NewReader(r)
	}
	b, _ := io.ReadAll(io.LimitReader(rd, 64<<20))
	return b
}

// stripSpace drops CR/LF/space so base64 lines decode as one stream.
type stripSpace struct{ r io.Reader }

func (s *stripSpace) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	j := 0
	for i := 0; i < n; i++ {
		if c := p[i]; c != '\r' && c != '\n' && c != ' ' && c != '\t' {
			p[j] = c
			j++
		}
	}
	if j == 0 && err == nil && n > 0 {
		return s.Read(p)
	}
	return j, err
}

func (p *Parsed) walk(ctype, cte, disp string, body io.Reader, depth int) {
	if depth > 10 {
		return
	}
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil || mt == "" {
		mt, params = "text/plain", map[string]string{}
	}
	if strings.HasPrefix(mt, "multipart/") && params["boundary"] != "" {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextRawPart()
			if err != nil {
				return
			}
			p.walk(part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"),
				part.Header.Get("Content-Disposition"), part, depth+1)
		}
	}
	dt, dparams, _ := mime.ParseMediaType(disp)
	filename := decodeHeader(dparams["filename"])
	if filename == "" {
		filename = decodeHeader(params["name"])
	}
	data := decodeBody(cte, body)
	if dt == "attachment" || filename != "" || (mt != "text/plain" && mt != "text/html" && mt != "message/rfc822" && depth > 0) {
		p.Attachments = append(p.Attachments, Attachment{Filename: filename, ContentType: mt, Size: len(data), Risky: Risky(filename, data)})
		return
	}
	switch mt {
	case "text/plain":
		if p.Text == "" {
			p.Text = string(data)
		}
	case "text/html":
		if p.HTML == "" {
			p.HTML = string(data)
		}
	case "message/rfc822":
		inner := Parse(data)
		p.Attachments = append(p.Attachments, inner.Attachments...)
		if p.Text == "" {
			p.Text = inner.Text
		}
	}
}

// Index builds the mail/<seq>.json record of a message.
func Index(raw []byte, seq uint64, id, receivedAt string) *MailIndex {
	p := Parse(raw)
	return &MailIndex{Seq: seq, ID: id, ReceivedAt: receivedAt, From: p.From, To: p.To, Subject: p.Subject, Date: p.Date, Attachments: p.Attachments}
}

var (
	tagRe     = regexp.MustCompile(`(?s)<(script|style|head)[^>]*>.*?</(script|style|head)>|<[^>]+>`)
	blockRe   = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/li|/h[1-6])[^>]*>`)
	spacesRe  = regexp.MustCompile(`[ \t\x{a0}]+`)
	newlineRe = regexp.MustCompile(`\n{3,}`)
)

// HTMLToText strips an HTML body to readable text.
func HTMLToText(s string) string {
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = spacesRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(newlineRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

var (
	codeRe    = regexp.MustCompile(`(?:^|[^0-9A-Za-z#&=/._-])(\d{6,8}|\d{3}[ -]\d{3}(?:\d{1,2})?)(?:$|[^0-9A-Za-z])`)
	urlRe     = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)
	keywordRe = regexp.MustCompile(`(?i)\b(code|otp|one[- ]time|passcode|pass code|verification|verify|pin|token|security|login|sign[- ]in|confirm)`)
)

// ExtractCode finds a 6-8 digit one-time code in a subject and body: a code
// near a keyword (code, OTP, verification, ...) wins, then the first one in
// the subject, then the first in the body. URLs are ignored. It returns ""
// when there is none.
func ExtractCode(subject, body string) string {
	type cand struct {
		code  string
		score int
		pos   int
	}
	var best *cand
	consider := func(text string, base int) {
		text = urlRe.ReplaceAllString(text, " ")
		for _, m := range codeRe.FindAllStringSubmatchIndex(text, -1) {
			code := strings.NewReplacer(" ", "", "-", "").Replace(text[m[2]:m[3]])
			if len(code) < 6 || len(code) > 8 {
				continue
			}
			score := base
			lo := m[2] - 80
			if lo < 0 {
				lo = 0
			}
			hi := m[3] + 40
			if hi > len(text) {
				hi = len(text)
			}
			if keywordRe.MatchString(text[lo:m[2]]) {
				score += 10 // "your code is 123456"
			} else if keywordRe.MatchString(text[m[3]:hi]) {
				score += 4 // "123456 is your code"
			}
			c := &cand{code: code, score: score, pos: m[2]}
			if best == nil || c.score > best.score {
				best = c
			}
		}
	}
	consider(subject, 5)
	consider(body, 0)
	if best == nil {
		return ""
	}
	return best.code
}
