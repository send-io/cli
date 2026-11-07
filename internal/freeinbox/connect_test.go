package freeinbox

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Golden connect link: seeds[0] of testvectors.json, ts 1760000000, nonce =
// bytes 0x80..0x8f. The signature was cross-checked with an independent
// Ed25519 implementation (Python cryptography); the Node SDK tests use the
// same values.
const (
	goldenConnectAddr  = "praveen@instantly.sendioinbox.com"
	goldenConnectTS    = 1760000000
	goldenConnectNonce = "gIGCg4SFhoeIiYqLjI2Ojw"
	goldenConnectSig   = "gSnJs3MhDEZjbjszN63ZTGF5oNwf9Qk318EM66kbunPxAoBaZBAhJJAbaD4j6H-5UCmYvcRdLb67dnSNR9cADQ"
	goldenConnectLink  = "https://app.send.io/connect#a=praveen@instantly.sendioinbox.com&t=1760000000&n=gIGCg4SFhoeIiYqLjI2Ojw&s=" + goldenConnectSig
)

func goldenNonce() []byte {
	n := make([]byte, 16)
	for i := range n {
		n[i] = byte(0x80 + i)
	}
	return n
}

func TestConnectString(t *testing.T) {
	got := ConnectString(goldenConnectTS, goldenConnectAddr, goldenConnectNonce)
	want := "sendio-free-connect-v1:1760000000:praveen@instantly.sendioinbox.com:gIGCg4SFhoeIiYqLjI2Ojw"
	if got != want {
		t.Fatalf("ConnectString = %q", got)
	}
}

func TestConnectLinkGolden(t *testing.T) {
	v := loadVectors(t)
	k := seedKeys(t, v, 0)
	cl := BuildConnectLink("", k, "  Praveen@Instantly.SendioInbox.com ", goldenConnectTS, goldenNonce())
	if cl.Link != goldenConnectLink {
		t.Fatalf("link\n got %s\nwant %s", cl.Link, goldenConnectLink)
	}
	if cl.Address != goldenConnectAddr || cl.Nonce != goldenConnectNonce || cl.Sig != goldenConnectSig || cl.TS != goldenConnectTS {
		t.Fatalf("%+v", cl)
	}
	if !cl.ExpiresAt.Equal(time.Unix(goldenConnectTS, 0).Add(30*time.Minute)) || cl.ExpiresAt.Location() != time.UTC {
		t.Fatalf("expires_at %v", cl.ExpiresAt)
	}
	// The signature verifies with the vector's public key (what the relay does).
	pub, _ := base64.StdEncoding.DecodeString(v.Seeds[0].SignPub)
	sig, err := base64.RawURLEncoding.DecodeString(goldenConnectSig)
	if err != nil || !ed25519.Verify(pub, []byte(ConnectString(goldenConnectTS, goldenConnectAddr, goldenConnectNonce)), sig) {
		t.Fatalf("signature does not verify: %v", err)
	}
	// The fragment parses back into the claim fields.
	u, err := url.Parse(cl.Link)
	if err != nil || u.Path != "/connect" || u.RawQuery != "" {
		t.Fatalf("%v %v", err, u)
	}
	q, err := url.ParseQuery(u.Fragment)
	if err != nil || q.Get("a") != goldenConnectAddr || q.Get("t") != "1760000000" || q.Get("n") != goldenConnectNonce || q.Get("s") != goldenConnectSig {
		t.Fatalf("fragment %v %v", err, q)
	}
}

func TestConnectLinkFresh(t *testing.T) {
	k, _ := NewKeys()
	in := &Inbox{Address: "bot@acme.sendioinbox.com", Keys: k}
	now := time.Unix(1760001234, 0)
	a, err := NewConnectLink("https://app.example.test/", in, now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewConnectLink("app.example.test", in, now)
	if a.Nonce == b.Nonce {
		t.Fatal("nonce reused")
	}
	for _, cl := range []*ConnectLink{a, b} {
		if !strings.HasPrefix(cl.Link, "https://app.example.test/connect#a=bot@acme.sendioinbox.com&t=1760001234&n=") {
			t.Fatalf("link %s", cl.Link)
		}
		// base64url without padding: 16 bytes -> 22 chars, 64 bytes -> 86 chars.
		if len(cl.Nonce) != 22 || len(cl.Sig) != 86 || strings.ContainsAny(cl.Nonce+cl.Sig, "+/=") {
			t.Fatalf("encoding %q %q", cl.Nonce, cl.Sig)
		}
		sig, _ := base64.RawURLEncoding.DecodeString(cl.Sig)
		if !ed25519.Verify(k.SignPub, []byte(ConnectString(cl.TS, in.Address, cl.Nonce)), sig) {
			t.Fatal("fresh link does not verify")
		}
		if cl.ExpiresAt.Sub(now) != 30*time.Minute {
			t.Fatalf("expires %v", cl.ExpiresAt)
		}
	}
	// Characters outside the address alphabet are escaped, "@" stays.
	odd := BuildConnectLink("", k, "a+b@x.test", 1, goldenNonce())
	if !strings.Contains(odd.Link, "#a=a%2Bb@x.test&") {
		t.Fatalf("escape %s", odd.Link)
	}
}

func TestNormalizeAppURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                           DefaultAppURL,
		"  ":                         DefaultAppURL,
		"https://app.sendio.run/":    "https://app.sendio.run",
		"localhost:3000":             "https://localhost:3000",
		"http://localhost:3000":      "http://localhost:3000",
		"https://dev.app.sendio.run": "https://dev.app.sendio.run",
	} {
		if got := NormalizeAppURL(in); got != want {
			t.Errorf("NormalizeAppURL(%q) = %q, want %q", in, got, want)
		}
	}
}
