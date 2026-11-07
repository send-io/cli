package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"net/url"
	"regexp"
	"strings"

	"send.io/cli/internal/api"
)

// The server needs column_map.email; `contacts import` promises to match
// columns by header, so without --column the CLI builds the map from the
// file's header row itself.

// headerFields: the contact fields a header name maps to, compared after
// importNorm (case, spaces, dashes and underscores ignored), as the
// dashboard's import dialog guesses them.
var headerFields = map[string]string{
	"email": "email", "emailaddress": "email", "mail": "email",
	"firstname": "first_name", "givenname": "first_name",
	"lastname": "last_name", "surname": "last_name", "familyname": "last_name",
	"unsubscribed": "unsubscribed",
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]`)

// propertyKeyRE: a valid contact property key (the server's rule).
var propertyKeyRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,50}$`)

func importNorm(s string) string { return nonAlnum.ReplaceAllString(strings.ToLower(s), "") }

// csvHeader reads the first row of data with the delimiter the server would
// use: the given one, or the first of , ; tab | on the first line.
func csvHeader(data []byte, delimiter string) []string {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	switch delimiter {
	case "tab", `\t`:
		delimiter = "\t"
	case "":
		first, _, _ := bytes.Cut(data, []byte("\n"))
		delimiter = ","
		for _, d := range []string{",", ";", "\t", "|"} {
			if bytes.Contains(first, []byte(d)) {
				delimiter = d
				break
			}
		}
	}
	rd := csv.NewReader(bytes.NewReader(data))
	rd.Comma = rune(delimiter[0])
	rd.FieldsPerRecord, rd.LazyQuotes = -1, true
	rec, err := rd.Read()
	if err != nil {
		return nil
	}
	for i := range rec {
		rec[i] = strings.TrimSpace(rec[i])
	}
	return rec
}

// headerColumnMap maps header columns to contact fields by name, and other
// columns to contact properties: an existing property with that key (props),
// or any valid key when create is set (--create-missing-properties).
func headerColumnMap(header []string, props map[string]bool, create bool) map[string]any {
	cm := map[string]any{}
	pm := map[string]any{}
	for _, h := range header {
		if h == "" {
			continue
		}
		if f, ok := headerFields[importNorm(h)]; ok {
			if _, taken := cm[f]; !taken {
				cm[f] = h
			}
			continue
		}
		if propertyKeyRE.MatchString(h) && (props[h] || create) {
			pm[h] = map[string]any{"column": h}
		}
	}
	if len(pm) > 0 {
		cm["properties"] = pm
	}
	return cm
}

// propertyKeys lists the team's contact property keys (every page).
func propertyKeys(ctx context.Context, c *api.Client) (map[string]bool, error) {
	keys := map[string]bool{}
	after := ""
	for {
		q := url.Values{"limit": {"100"}}
		if after != "" {
			q.Set("after", after)
		}
		resp, err := c.Do(ctx, &api.Request{Method: "GET", Path: "/contact-properties?" + q.Encode()})
		if err != nil {
			return nil, err
		}
		var page struct {
			Data []struct {
				ID  string `json:"id"`
				Key string `json:"key"`
			} `json:"data"`
			HasMore bool `json:"has_more"`
		}
		if err := resp.Decode(&page); err != nil {
			return nil, err
		}
		for _, p := range page.Data {
			keys[p.Key] = true
		}
		if !page.HasMore || len(page.Data) == 0 {
			return keys, nil
		}
		after = page.Data[len(page.Data)-1].ID
	}
}

// needsPropertyKeys: whether any header column could be an existing
// property (so the keys are worth fetching).
func needsPropertyKeys(header []string) bool {
	for _, h := range header {
		if _, ok := headerFields[importNorm(h)]; !ok && propertyKeyRE.MatchString(h) {
			return true
		}
	}
	return false
}
