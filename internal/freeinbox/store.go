package freeinbox

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultRelay is the production relay.
const DefaultRelay = "https://relay.sendioinbox.com"

// KeyFile is key.json.
type KeyFile struct {
	Address   string `json:"address"`
	Seed      string `json:"seed"`
	SignPub   string `json:"sign_pub"`
	BoxPub    string `json:"box_pub"`
	CreatedAt string `json:"created_at"`
	Relay     string `json:"relay"`
}

// Keys derives the key material from the stored seed.
func (k *KeyFile) Keys() (*Keys, error) {
	seed, err := base64.StdEncoding.DecodeString(k.Seed)
	if err != nil {
		return nil, fmt.Errorf("key.json for %s: bad seed: %v", k.Address, err)
	}
	return KeysFromSeed(seed)
}

// NewKeyFile builds key.json for an inbox.
func NewKeyFile(address string, keys *Keys, createdAt, relay string) *KeyFile {
	b64 := base64.StdEncoding.EncodeToString
	return &KeyFile{Address: strings.ToLower(address), Seed: b64(keys.Seed), SignPub: b64(keys.SignPub),
		BoxPub: b64(keys.BoxPub[:]), CreatedAt: createdAt, Relay: relay}
}

// State is state.json.
type State struct {
	Cursor uint64 `json:"cursor"`
}

// Attachment is one attachment in a mail index.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Risky       bool   `json:"risky"`
}

// MailIndex is mail/<seq>.json.
type MailIndex struct {
	Seq         uint64       `json:"seq"`
	ID          string       `json:"id"`
	ReceivedAt  string       `json:"received_at"`
	From        string       `json:"from"`
	To          string       `json:"to"`
	Subject     string       `json:"subject"`
	Date        string       `json:"date"`
	Attachments []Attachment `json:"attachments"`
}

// Risky reports whether any attachment is risky.
func (m *MailIndex) Risky() bool {
	for _, a := range m.Attachments {
		if a.Risky {
			return true
		}
	}
	return false
}

// Store is the local storage root: $SENDIO_HOME/free or ~/.sendio/free.
type Store struct{ Root string }

// DefaultRoot resolves the storage root from SENDIO_HOME (which replaces
// ~/.sendio) and the home directory.
func DefaultRoot(getenv func(string) string, home string) string {
	if getenv != nil {
		if h := strings.TrimSpace(getenv("SENDIO_HOME")); h != "" {
			return filepath.Join(h, "free")
		}
	}
	return filepath.Join(home, ".sendio", "free")
}

func (s *Store) dir(address string) string {
	return filepath.Join(s.Root, strings.ToLower(strings.TrimSpace(address)))
}

// KeyPath is the path of an inbox's key.json.
func (s *Store) KeyPath(address string) string { return filepath.Join(s.dir(address), "key.json") }

// MailDir is an inbox's mail directory.
func (s *Store) MailDir(address string) string { return filepath.Join(s.dir(address), "mail") }

// writeFile writes atomically (temp file, fsync, rename) with mode 0600.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'))
}

// SaveKey writes key.json (0600) and an empty state.json when there is none.
func (s *Store) SaveKey(k *KeyFile) error {
	if err := writeJSON(s.KeyPath(k.Address), k); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(s.dir(k.Address), "state.json")); errors.Is(err, os.ErrNotExist) {
		return s.SaveState(k.Address, State{})
	}
	return nil
}

// ErrNoInbox is returned for an address without a local key.
var ErrNoInbox = errors.New("no local key for this inbox")

// LoadKey reads key.json.
func (s *Store) LoadKey(address string) (*KeyFile, error) {
	b, err := os.ReadFile(s.KeyPath(address))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoInbox, address)
	}
	if err != nil {
		return nil, err
	}
	var k KeyFile
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, fmt.Errorf("%s: %v", s.KeyPath(address), err)
	}
	if k.Address == "" {
		k.Address = strings.ToLower(address)
	}
	return &k, nil
}

// Has reports whether a local key exists for address.
func (s *Store) Has(address string) bool {
	_, err := os.Stat(s.KeyPath(address))
	return err == nil
}

// List returns the local inboxes, sorted by address.
func (s *Store) List() ([]*KeyFile, error) {
	ents, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*KeyFile
	for _, e := range ents {
		if !e.IsDir() || !strings.Contains(e.Name(), "@") {
			continue
		}
		k, err := s.LoadKey(e.Name())
		if err != nil {
			continue
		}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// Remove deletes an inbox's local directory (key, state and mail).
func (s *Store) Remove(address string) error {
	if !s.Has(address) {
		return fmt.Errorf("%w: %s", ErrNoInbox, address)
	}
	return os.RemoveAll(s.dir(address))
}

// LoadState reads state.json (zero when absent).
func (s *Store) LoadState(address string) (State, error) {
	var st State
	b, err := os.ReadFile(filepath.Join(s.dir(address), "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}

// SaveState writes state.json.
func (s *Store) SaveState(address string, st State) error {
	return writeJSON(filepath.Join(s.dir(address), "state.json"), st)
}

// HasMail reports whether mail/<seq>.json exists.
func (s *Store) HasMail(address string, seq uint64) bool {
	_, err := os.Stat(filepath.Join(s.MailDir(address), strconv.FormatUint(seq, 10)+".json"))
	return err == nil
}

// SaveMail writes mail/<seq>.eml first, then mail/<seq>.json (both 0600,
// fsynced): once SaveMail returns, the message may be acked.
func (s *Store) SaveMail(address string, raw []byte, idx *MailIndex) error {
	base := filepath.Join(s.MailDir(address), strconv.FormatUint(idx.Seq, 10))
	if err := writeFile(base+".eml", raw); err != nil {
		return err
	}
	return writeJSON(base+".json", idx)
}

// Mail returns the index entries, newest (highest seq) first.
func (s *Store) Mail(address string) ([]*MailIndex, error) {
	ents, err := os.ReadDir(s.MailDir(address))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*MailIndex
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".json") || strings.HasPrefix(n, ".") {
			continue
		}
		if _, err := strconv.ParseUint(strings.TrimSuffix(n, ".json"), 10, 64); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.MailDir(address), n))
		if err != nil {
			continue
		}
		var m MailIndex
		if json.Unmarshal(b, &m) == nil {
			out = append(out, &m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	return out, nil
}

// RawMail reads mail/<seq>.eml.
func (s *Store) RawMail(address string, seq uint64) ([]byte, error) {
	return os.ReadFile(s.EMLPath(address, seq))
}

// EMLPath is the path of mail/<seq>.eml.
func (s *Store) EMLPath(address string, seq uint64) string {
	return filepath.Join(s.MailDir(address), strconv.FormatUint(seq, 10)+".eml")
}

// NowRFC3339 is the creation time format used in key.json.
func NowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
