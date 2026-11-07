package freeinbox

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

type vectors struct {
	Seeds []struct {
		SeedHex        string `json:"seed_hex"`
		SignPub        string `json:"sign_pub"`
		BoxPub         string `json:"box_pub"`
		BoxSK          string `json:"box_sk"`
		RecoveryPhrase string `json:"recovery_phrase"`
	} `json:"seeds"`
	Addresses []struct {
		Input, Normalised, Token string
	} `json:"addresses"`
	Auth struct {
		Address, Nonce, Signature string
	} `json:"auth"`
	Poll struct {
		Address, Signature string
		TS                 int64 `json:"ts"`
	} `json:"poll"`
	Ack struct {
		Address, Signature string
		TS                 int64  `json:"ts"`
		Seq                uint64 `json:"seq"`
	} `json:"ack"`
	OrgSig struct {
		Address, Signature string
	} `json:"org_sig"`
	Envelope struct {
		EnvelopeB64     string `json:"envelope_b64"`
		ESKHex          string `json:"esk_hex"`
		NonceHex        string `json:"nonce_hex"`
		Plaintext       string `json:"plaintext_utf8"`
		RecipientBoxPub string `json:"recipient_box_pub"`
	} `json:"envelope"`
	PoW struct {
		Challenge  string `json:"challenge"`
		Difficulty int    `json:"difficulty"`
		Nonce      string `json:"nonce"`
	} `json:"pow"`
}

func loadVectors(t *testing.T) *vectors {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "free-inbox", "testvectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Seeds) == 0 {
		t.Fatal("no seeds in testvectors.json")
	}
	return &v
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func seedKeys(t *testing.T, v *vectors, i int) *Keys {
	t.Helper()
	seed, _ := hex.DecodeString(v.Seeds[i].SeedHex)
	k, err := KeysFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeyVectors(t *testing.T) {
	v := loadVectors(t)
	for i, s := range v.Seeds {
		k := seedKeys(t, v, i)
		if got := b64(k.SignPub); got != s.SignPub {
			t.Errorf("seed %d sign_pub = %s, want %s", i, got, s.SignPub)
		}
		if got := b64(k.BoxPub[:]); got != s.BoxPub {
			t.Errorf("seed %d box_pub = %s, want %s", i, got, s.BoxPub)
		}
		if got := b64(k.BoxSK[:]); got != s.BoxSK {
			t.Errorf("seed %d box_sk = %s, want %s", i, got, s.BoxSK)
		}
		if got := RecoveryPhrase(k.Seed); got != s.RecoveryPhrase {
			t.Errorf("seed %d phrase = %s, want %s", i, got, s.RecoveryPhrase)
		}
		back, err := ParsePhrase(strings.ToUpper(s.RecoveryPhrase))
		if err != nil || !bytes.Equal(back, k.Seed) {
			t.Errorf("seed %d phrase round trip: %v", i, err)
		}
	}
}

func TestSignatureVectors(t *testing.T) {
	v := loadVectors(t)
	k := seedKeys(t, v, 0)
	if got := b64(k.SignAuth(v.Auth.Nonce, v.Auth.Address)); got != v.Auth.Signature {
		t.Errorf("auth sig = %s, want %s", got, v.Auth.Signature)
	}
	if got := b64(k.SignPoll(v.Poll.TS, v.Poll.Address)); got != v.Poll.Signature {
		t.Errorf("poll sig = %s, want %s", got, v.Poll.Signature)
	}
	if got := b64(k.SignAck(v.Ack.TS, v.Ack.Address, v.Ack.Seq)); got != v.Ack.Signature {
		t.Errorf("ack sig = %s, want %s", got, v.Ack.Signature)
	}
	if got := b64(k.SignOrg(v.OrgSig.Address)); got != v.OrgSig.Signature {
		t.Errorf("org sig = %s, want %s", got, v.OrgSig.Signature)
	}
}

func TestAddressVectors(t *testing.T) {
	v := loadVectors(t)
	for _, a := range v.Addresses {
		n := NormalizeAddress(a.Input)
		if n != a.Normalised {
			t.Errorf("Normalize(%q) = %q, want %q", a.Input, n, a.Normalised)
		}
		if got := Token(n); got != a.Token {
			t.Errorf("Token(%q) = %s, want %s", n, got, a.Token)
		}
	}
}

func TestEnvelopeVector(t *testing.T) {
	v := loadVectors(t)
	k := seedKeys(t, v, 0)
	if b64(k.BoxPub[:]) != v.Envelope.RecipientBoxPub {
		t.Fatal("recipient is not seeds[0]")
	}
	env, _ := base64.StdEncoding.DecodeString(v.Envelope.EnvelopeB64)
	msg, err := k.Open(env)
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != v.Envelope.Plaintext {
		t.Fatalf("plaintext = %q", msg)
	}
	// Rebuild the envelope from esk and nonce: byte for byte.
	eskB, _ := hex.DecodeString(v.Envelope.ESKHex)
	nonceB, _ := hex.DecodeString(v.Envelope.NonceHex)
	var esk [32]byte
	var nonce [24]byte
	copy(esk[:], eskB)
	copy(nonce[:], nonceB)
	epk, _ := curve25519.X25519(esk[:], curve25519.Basepoint)
	re := append([]byte("SB1"), epk...)
	re = append(re, nonce[:]...)
	re = box.Seal(re, []byte(v.Envelope.Plaintext), &nonce, &k.BoxPub, &esk)
	if !bytes.Equal(re, env) {
		t.Fatal("rebuilt envelope differs")
	}
	// Wrong key and tampering fail.
	other := seedKeys(t, v, 1)
	if _, err := other.Open(env); err == nil {
		t.Fatal("opened with the wrong key")
	}
	env[len(env)-1] ^= 1
	if _, err := k.Open(env); err == nil {
		t.Fatal("opened a tampered envelope")
	}
	// Seal round trip.
	sealed, err := Seal([]byte("hi"), &k.BoxPub)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := k.Open(sealed); err != nil || string(got) != "hi" {
		t.Fatalf("seal round trip: %q %v", got, err)
	}
}

func TestPoWVector(t *testing.T) {
	v := loadVectors(t)
	if !VerifyPoW(v.PoW.Challenge, v.PoW.Nonce, v.PoW.Difficulty) {
		t.Fatal("vector nonce does not verify")
	}
	n, err := SolvePoW(context.Background(), v.PoW.Challenge, 12)
	if err != nil || !VerifyPoW(v.PoW.Challenge, n, 12) {
		t.Fatalf("SolvePoW: %s %v", n, err)
	}
	if LeadingZeroBits([]byte{0, 0x1f}) != 11 {
		t.Fatal("LeadingZeroBits")
	}
}

func TestPhraseRoundTrip(t *testing.T) {
	for i := 0; i < 20; i++ {
		k, err := NewKeys()
		if err != nil {
			t.Fatal(err)
		}
		p := RecoveryPhrase(k.Seed)
		if strings.ToLower(p) != p || strings.Contains(p, "=") {
			t.Fatalf("phrase not lower-case/unpadded: %s", p)
		}
		for _, g := range strings.Split(p, "-")[:12] {
			if len(g) != 4 {
				t.Fatalf("group %q in %s", g, p)
			}
		}
		for _, variant := range []string{p, strings.ToUpper(p), strings.ReplaceAll(p, "-", " "), " " + p + "\n"} {
			s, err := ParsePhrase(variant)
			if err != nil || !bytes.Equal(s, k.Seed) {
				t.Fatalf("ParsePhrase(%q): %v", variant, err)
			}
		}
	}
	if _, err := ParsePhrase("not a phrase!"); err == nil {
		t.Fatal("accepted junk")
	}
	if _, err := ParsePhrase("aaaa-bbbb"); err == nil {
		t.Fatal("accepted a short phrase")
	}
}

func TestNames(t *testing.T) {
	if Normalize("  Ágent_Smith ") != "agent-smith" {
		t.Fatal(Normalize("  Ágent_Smith "))
	}
	for _, bad := range []string{"a", "-ab", "ab-", "a..b", "admin", strings.Repeat("a", 33)} {
		if CheckName(bad) == nil {
			t.Errorf("CheckName(%q) passed", bad)
		}
	}
	for _, good := range []string{"ab", "agent.smith", "x-1", strings.Repeat("a", 32)} {
		if err := CheckName(good); err != nil {
			t.Errorf("CheckName(%q): %v", good, err)
		}
	}
	if CheckOrg("my.org") == nil || CheckOrg("my-org") != nil {
		t.Fatal("CheckOrg")
	}
	n, o, p, ok := SplitAddress("praveen@instantly.sendioinbox.com")
	if !ok || n != "praveen" || o != "instantly" || p != "sendioinbox.com" {
		t.Fatal(n, o, p, ok)
	}
}

func TestExtractCode(t *testing.T) {
	cases := []struct{ subject, body, want string }{
		{"Your code", "Your verification code is 482913.", "482913"},
		{"Welcome", "Order 20261011 shipped.\n\nYour one-time passcode: 1234 5678? no: 73920115", "73920115"},
		{"Sign in to Acme", "Use 123 456 to sign in.", "123456"},
		{"482913 is your Acme code", "Thanks", "482913"},
		{"Hello", "Visit https://x.test/verify?token=99887766 or call. Code: 556677", "556677"},
		{"Colors", "<p style=\"color:#123456\">no code here</p>", ""},
		{"Hi", "Nothing to see 12345 or 123456789", ""},
		{"Invoice 445566", "Your login code is 908172", "908172"},
	}
	for _, c := range cases {
		body := c.body
		if strings.Contains(body, "<p") {
			body = HTMLToText(body)
		}
		if got := ExtractCode(c.subject, body); got != c.want {
			t.Errorf("ExtractCode(%q, %q) = %q, want %q", c.subject, c.body, got, c.want)
		}
	}
}

func TestRisky(t *testing.T) {
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("invoice.pdf.exe")
	w.Write([]byte("x"))
	zw.Close()
	var safe bytes.Buffer
	zw = zip.NewWriter(&safe)
	w, _ = zw.Create("photo.jpg")
	w.Write([]byte("x"))
	zw.Close()
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"report.pdf", []byte("%PDF"), false},
		{"setup.EXE", nil, true},
		{"macro.docm", nil, true},
		{"notes.txt", []byte("MZ\x90\x00"), true},
		{"bundle.zip", zb.Bytes(), true},
		{"photos.zip", safe.Bytes(), false},
		{"a.rar", []byte("Rar!\x1a\x07\x01\x00....run.vbs...."), true},
		{"b.rar", []byte("Rar!\x1a\x07\x01\x00....readme.txt...."), false},
		{"run.js", nil, true},
	}
	for _, c := range cases {
		if got := Risky(c.name, c.data); got != c.want {
			t.Errorf("Risky(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

const sampleMIME = "From: =?utf-8?q?Acme_Bot?= <bot@acme.test>\r\n" +
	"To: agent@org.sendioinbox.test\r\n" +
	"Subject: Your code\r\n" +
	"Date: Sat, 11 Oct 2026 10:00:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=XX\r\n\r\n" +
	"--XX\r\nContent-Type: multipart/alternative; boundary=YY\r\n\r\n" +
	"--YY\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nYour code is 6=\r\n61234.\r\n" +
	"--YY\r\nContent-Type: text/html\r\n\r\n<p>Your code is <b>661234</b></p>\r\n--YY--\r\n" +
	"--XX\r\nContent-Type: application/octet-stream; name=\"tool.exe\"\r\nContent-Disposition: attachment; filename=\"tool.exe\"\r\nContent-Transfer-Encoding: base64\r\n\r\nTVqQAAMAAAAEAAAA\r\n" +
	"--XX\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"r.pdf\"\r\n\r\n%PDF-1.4\r\n--XX--\r\n"

func TestParseAndIndex(t *testing.T) {
	idx := Index([]byte(sampleMIME), 7, "m7", "2026-10-11T10:00:01Z")
	if idx.From != "Acme Bot <bot@acme.test>" || idx.Subject != "Your code" || idx.To != "agent@org.sendioinbox.test" {
		t.Fatalf("headers: %+v", idx)
	}
	if idx.Date != "2026-10-11T10:00:00Z" || idx.Seq != 7 || idx.ID != "m7" {
		t.Fatalf("date/seq: %+v", idx)
	}
	if len(idx.Attachments) != 2 || !idx.Attachments[0].Risky || idx.Attachments[0].Filename != "tool.exe" || idx.Attachments[0].Size != 12 || idx.Attachments[1].Risky {
		t.Fatalf("attachments: %+v", idx.Attachments)
	}
	p := Parse([]byte(sampleMIME))
	if !strings.Contains(p.Text, "661234") || ExtractCode(p.Subject, p.Text) != "661234" {
		t.Fatalf("text: %q", p.Text)
	}
	if !idx.Risky() {
		t.Fatal("Risky()")
	}
}

func TestStoreLayout(t *testing.T) {
	root := t.TempDir()
	if got := DefaultRoot(func(k string) string {
		if k == "SENDIO_HOME" {
			return root
		}
		return ""
	}, "/home/x"); got != filepath.Join(root, "free") {
		t.Fatal(got)
	}
	if got := DefaultRoot(func(string) string { return "" }, "/home/x"); got != "/home/x/.sendio/free" {
		t.Fatal(got)
	}
	st := &Store{Root: filepath.Join(root, "free")}
	k, _ := NewKeys()
	addr := "agent@org.sendioinbox.test"
	if err := st.SaveKey(NewKeyFile(addr, k, "2026-10-11T10:00:00Z", "http://relay.test")); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveMail(addr, []byte(sampleMIME), Index([]byte(sampleMIME), 3, "m3", "2026-10-11T10:00:01Z")); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveState(addr, State{Cursor: 3}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "free", addr)
	for _, f := range []string{"key.json", "state.json", "mail/3.eml", "mail/3.json"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", f, fi.Mode().Perm())
		}
	}
	var kj map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "key.json"))
	json.Unmarshal(b, &kj)
	for _, f := range []string{"address", "seed", "sign_pub", "box_pub", "created_at", "relay"} {
		if kj[f] == nil {
			t.Errorf("key.json lacks %s", f)
		}
	}
	var sj map[string]any
	b, _ = os.ReadFile(filepath.Join(dir, "state.json"))
	json.Unmarshal(b, &sj)
	if sj["cursor"] != float64(3) {
		t.Errorf("state.json = %s", b)
	}
	var mj map[string]any
	b, _ = os.ReadFile(filepath.Join(dir, "mail", "3.json"))
	json.Unmarshal(b, &mj)
	for _, f := range []string{"seq", "id", "received_at", "from", "to", "subject", "date", "attachments"} {
		if _, ok := mj[f]; !ok {
			t.Errorf("3.json lacks %s", f)
		}
	}
	att := mj["attachments"].([]any)[0].(map[string]any)
	for _, f := range []string{"filename", "content_type", "size", "risky"} {
		if _, ok := att[f]; !ok {
			t.Errorf("attachment lacks %s", f)
		}
	}
	in, err := st.OpenInbox(addr)
	if err != nil || !bytes.Equal(in.Keys.Seed, k.Seed) {
		t.Fatal(err)
	}
	list, _ := st.List()
	if len(list) != 1 || list[0].Address != addr {
		t.Fatal(list)
	}
	mail, _ := st.Mail(addr)
	if len(mail) != 1 || mail[0].Seq != 3 {
		t.Fatal(mail)
	}
	if err := st.Remove(addr); err != nil || st.Has(addr) {
		t.Fatal(err)
	}
}

func TestNormalizeRelay(t *testing.T) {
	for in, want := range map[string]string{
		"":                                      DefaultRelay,
		"https://relay.sendioinbox.com/":        "https://relay.sendioinbox.com",
		"wss://relay.sendioinbox.com/v1/stream": "https://relay.sendioinbox.com",
		"ws://127.0.0.1:18080/v1/stream":        "http://127.0.0.1:18080",
		"relay.example.com":                     "https://relay.example.com",
	} {
		if got := NormalizeRelay(in); got != want {
			t.Errorf("NormalizeRelay(%q) = %q, want %q", in, got, want)
		}
	}
	c := &Client{Relay: "http://127.0.0.1:1"}
	if c.StreamURL() != "ws://127.0.0.1:1/v1/stream" {
		t.Fatal(c.StreamURL())
	}
}

func TestBackoff(t *testing.T) {
	for n := 0; n < 10; n++ {
		d := Backoff(n)
		if d < time.Second || d > 75*time.Second {
			t.Fatalf("Backoff(%d) = %s", n, d)
		}
	}
	if Backoff(20) < 60*time.Second {
		t.Fatal("cap")
	}
}
