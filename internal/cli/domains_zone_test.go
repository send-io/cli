package cli

import (
	"strings"
	"testing"
)

// SENDIO-341: a subdomain inside a parent zone shows each record's host
// relative to the zone (what to type into the DNS provider) and the zone.
func TestDomainsAddShowsZoneRelativeHost(t *testing.T) {
	f := newFake(t)
	f.json("POST /domains", 200, `{"id":"d1","name":"mail.example.com","status":"not_started","dns_zone":"example.com","records":[{"record":"SPF","type":"CNAME","name":"send","host":"send.mail","full_name":"send.mail.example.com","value":"rp.send.io","status":"not_started","ttl":"Auto"}]}`)
	h := newHarness(t, f.URL)
	withKey(h)
	if code := h.run("domains", "add", "mail.example.com", "--agent", "no"); code != 0 {
		t.Fatal(h.errOut())
	}
	o := h.out()
	if !strings.Contains(o, "DNS zone: example.com") || !strings.Contains(o, "HOST") || !strings.Contains(o, "send.mail") {
		t.Fatalf("output:\n%s", o)
	}
}

// An API that does not return host yet still shows the record name.
func TestDomainsAddFallsBackToName(t *testing.T) {
	f := newFake(t)
	f.json("POST /domains", 200, `{"id":"d1","name":"example.com","status":"not_started","records":[{"record":"SPF","type":"TXT","name":"send","value":"v=spf1 include:x ~all","status":"not_started","ttl":"Auto"}]}`)
	h := newHarness(t, f.URL)
	withKey(h)
	if code := h.run("domains", "add", "example.com", "--agent", "no"); code != 0 {
		t.Fatal(h.errOut())
	}
	if o := h.out(); !strings.Contains(o, "send") || strings.Contains(o, "DNS zone") {
		t.Fatalf("output:\n%s", o)
	}
}

func TestDomainsVerifyKeepsZoneNames(t *testing.T) {
	f := newFake(t)
	f.json("POST /domains/d1/verify", 200, `{"object":"domain","id":"d1"}`)
	f.json("GET /domains/d1", 200, `{"id":"d1","name":"mail.example.com","status":"verified","dns_zone":"example.com","records":[{"record":"SPF","type":"CNAME","name":"send","host":"send.mail","full_name":"send.mail.example.com","value":"rp.send.io","status":"verified","ttl":"Auto"}]}`)
	h := newHarness(t, f.URL)
	withKey(h)
	if code := h.run("domains", "verify", "d1"); code != 0 {
		t.Fatal(h.errOut())
	}
	m := h.outJSON()
	if m["dns_zone"] != "example.com" {
		t.Fatalf("json %v", m)
	}
	rec := m["records"].([]any)[0].(map[string]any)
	if rec["host"] != "send.mail" || rec["full_name"] != "send.mail.example.com" || rec["name"] != "send" {
		t.Fatalf("record %v", rec)
	}
}
