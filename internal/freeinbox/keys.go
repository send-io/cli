// Package freeinbox is the client side of send.io's anonymous agent inbox
// (contracts/free-inbox/PROTOCOL.md, protocol v1): keys derived from one client-held seed,
// sealed envelopes opened locally, the relay's HTTP and WebSocket APIs,
// and the local storage shared with the Node SDK (~/.sendio/free).
package freeinbox

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

// Protocol constants (PROTOCOL.md).
const (
	boxKeyContext  = "sendio-free-box-v1"
	authContext    = "sendio-free-auth-v1:"
	pollContext    = "sendio-free-poll-v1:"
	ackContext     = "sendio-free-ack-v1:"
	orgContext     = "sendio-free-org-v1:"
	connectContext = "sendio-free-connect-v1:"
	envelopeMagic  = "SB1"
)

// Keys are one inbox's key material, all derived from Seed.
type Keys struct {
	Seed     []byte
	SignPriv ed25519.PrivateKey
	SignPub  ed25519.PublicKey
	BoxSK    [32]byte
	BoxPub   [32]byte
}

// NewKeys generates a fresh random seed.
func NewKeys() (*Keys, error) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return KeysFromSeed(seed)
}

// KeysFromSeed derives the signing key (Ed25519 from the seed) and the box
// key (box_sk = sha256("sendio-free-box-v1" || seed), box_pub = X25519(box_sk, G)).
func KeysFromSeed(seed []byte) (*Keys, error) {
	if len(seed) != 32 {
		return nil, fmt.Errorf("seed must be 32 bytes, got %d", len(seed))
	}
	k := &Keys{Seed: append([]byte(nil), seed...)}
	k.SignPriv = ed25519.NewKeyFromSeed(k.Seed)
	k.SignPub = k.SignPriv.Public().(ed25519.PublicKey)
	h := sha256.New()
	h.Write([]byte(boxKeyContext))
	h.Write(k.Seed)
	copy(k.BoxSK[:], h.Sum(nil))
	pub, err := curve25519.X25519(k.BoxSK[:], curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	copy(k.BoxPub[:], pub)
	return k, nil
}

var phraseEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// RecoveryPhrase is the seed as lower-case RFC 4648 base32 without padding,
// in groups of 4 joined by "-".
func RecoveryPhrase(seed []byte) string {
	s := strings.ToLower(phraseEnc.EncodeToString(seed))
	var groups []string
	for len(s) > 4 {
		groups = append(groups, s[:4])
		s = s[4:]
	}
	if s != "" {
		groups = append(groups, s)
	}
	return strings.Join(groups, "-")
}

// ParsePhrase reads a recovery phrase back into the seed. Case, dashes and
// whitespace are ignored.
func ParsePhrase(phrase string) ([]byte, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(phrase) {
		switch r {
		case '-', ' ', '\t', '\n', '\r', '=':
		default:
			b.WriteRune(r)
		}
	}
	seed, err := phraseEnc.DecodeString(b.String())
	if err != nil {
		return nil, errors.New("not a recovery phrase (expected groups of base32 letters like abcd-efgh-...)")
	}
	if len(seed) != 32 {
		return nil, fmt.Errorf("recovery phrase decodes to %d bytes, want 32", len(seed))
	}
	return seed, nil
}

// Token is the inbox token: hex(sha256(lower-case address))[0:32].
func Token(address string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(address)))
	return hex.EncodeToString(sum[:])[:32]
}

func (k *Keys) sign(msg string) []byte { return ed25519.Sign(k.SignPriv, []byte(msg)) }

// SignAuth signs the WebSocket auth: "sendio-free-auth-v1:" + nonce + ":" + address.
func (k *Keys) SignAuth(nonce, address string) []byte {
	return k.sign(authContext + nonce + ":" + address)
}

// SignPoll signs a long-poll request: "sendio-free-poll-v1:" + ts + ":" + address.
func (k *Keys) SignPoll(ts int64, address string) []byte {
	return k.sign(pollContext + strconv.FormatInt(ts, 10) + ":" + address)
}

// SignAck signs an HTTP ack: "sendio-free-ack-v1:" + ts + ":" + address + ":" + seq.
func (k *Keys) SignAck(ts int64, address string, seq uint64) []byte {
	return k.sign(ackContext + strconv.FormatInt(ts, 10) + ":" + address + ":" + strconv.FormatUint(seq, 10))
}

// SignOrg lets the owner of an org label add another name under it:
// "sendio-free-org-v1:" + address.
func (k *Keys) SignOrg(address string) []byte {
	return k.sign(orgContext + address)
}

// SignConnect signs a connect link: "sendio-free-connect-v1:" + ts + ":" +
// address + ":" + nonce (nonce is the base64url text that goes in the link).
func (k *Keys) SignConnect(ts int64, address, nonce string) []byte {
	return k.sign(ConnectString(ts, address, nonce))
}

// ConnectString is the connect-link signature input.
func ConnectString(ts int64, address, nonce string) string {
	return connectContext + strconv.FormatInt(ts, 10) + ":" + address + ":" + nonce
}

// ErrEnvelope is returned for an envelope that cannot be opened.
var ErrEnvelope = errors.New("sealed envelope could not be opened (wrong key or corrupted)")

// Open decrypts a sealed envelope: "SB1" || epk(32) || nonce(24) || NaCl box.
func (k *Keys) Open(envelope []byte) ([]byte, error) {
	const hdr = len(envelopeMagic) + 32 + 24
	if len(envelope) < hdr+box.Overhead || string(envelope[:3]) != envelopeMagic {
		return nil, ErrEnvelope
	}
	var epk [32]byte
	var nonce [24]byte
	copy(epk[:], envelope[3:35])
	copy(nonce[:], envelope[35:59])
	msg, ok := box.Open(nil, envelope[hdr:], &nonce, &epk, &k.BoxSK)
	if !ok {
		return nil, ErrEnvelope
	}
	return msg, nil
}

// Seal makes an envelope for boxPub the way free-mx does (tests and tools).
func Seal(msg []byte, boxPub *[32]byte) ([]byte, error) {
	epk, esk, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	out := append([]byte(envelopeMagic), epk[:]...)
	out = append(out, nonce[:]...)
	return box.Seal(out, msg, &nonce, boxPub, esk), nil
}
