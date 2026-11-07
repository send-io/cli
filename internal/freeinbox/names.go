package freeinbox

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var (
	nameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,30}[a-z0-9])?$`)
	orgRe  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
)

// reservedNames are never issued (PROTOCOL.md).
var reservedNames = map[string]bool{
	"admin": true, "administrator": true, "postmaster": true, "abuse": true, "security": true,
	"noreply": true, "no-reply": true, "hostmaster": true, "webmaster": true, "root": true,
	"support": true, "billing": true, "mailer-daemon": true, "info": true,
}

// Normalize applies the protocol's normalisation: NFKD, strip accents,
// lower-case, trim, spaces and "_" become "-".
func Normalize(s string) string {
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	r, _, err := transform.String(t, s)
	if err != nil {
		r = s
	}
	r = strings.ToLower(strings.TrimSpace(r))
	return strings.NewReplacer(" ", "-", "_", "-").Replace(r)
}

// CheckName validates a normalised name locally (the relay has the final say).
func CheckName(name string) error {
	if len(name) < 2 || !nameRe.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("name %q is not valid: 2-32 characters a-z, 0-9, '.' and '-', starting and ending with a letter or digit", name)
	}
	if reservedNames[name] {
		return fmt.Errorf("name %q is reserved", name)
	}
	return nil
}

// CheckOrg validates a normalised org label locally.
func CheckOrg(org string) error {
	if len(org) < 2 || !orgRe.MatchString(org) {
		return fmt.Errorf("org %q is not valid: one label of 2-32 characters a-z, 0-9 and '-', no dots", org)
	}
	return nil
}

// NormalizeAddress normalises a whole address (name, org and domain).
func NormalizeAddress(address string) string { return Normalize(address) }

// SplitAddress returns name, org and pool domain of name@org.pooldomain.
func SplitAddress(address string) (name, org, pool string, ok bool) {
	local, domain, found := strings.Cut(strings.ToLower(strings.TrimSpace(address)), "@")
	if !found || local == "" {
		return "", "", "", false
	}
	org, pool, found = strings.Cut(domain, ".")
	if !found || org == "" || pool == "" {
		return "", "", "", false
	}
	return local, org, pool, true
}
