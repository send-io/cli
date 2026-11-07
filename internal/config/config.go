// Package config stores the CLI's profiles (config.json) and their secrets
// (the OS keyring, or credentials.json with mode 0600 as a fallback).
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

// DefaultProfile is the profile used when none is named or selected.
const DefaultProfile = "default"

// Auth kinds.
const (
	AuthOAuth  = "oauth"
	AuthAPIKey = "api_key"
)

// Storage backends for a profile's secret.
const (
	StorageKeyring = "keyring"
	StorageFile    = "file"
)

// Profile is one saved identity: an API origin plus how to authenticate.
type Profile struct {
	Name       string `json:"-"`
	BaseURL    string `json:"base_url"`
	Auth       string `json:"auth"`
	TeamID     string `json:"team_id,omitempty"`
	TeamName   string `json:"team_name,omitempty"`
	Permission string `json:"permission,omitempty"`
	KeyName    string `json:"key_name,omitempty"`
	UserEmail  string `json:"user_email,omitempty"`
	Device     string `json:"device_name,omitempty"`
	Storage    string `json:"storage"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// Secret is what a profile authenticates with.
type Secret struct {
	APIKey       string    `json:"api_key,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// Config is config.json.
type Config struct {
	Current  string              `json:"current,omitempty"`
	Profiles map[string]*Profile `json:"profiles"`
	// LastUpdateCheck and LatestVersion back the daily update notice.
	LastUpdateCheck time.Time `json:"last_update_check,omitempty"`
	LatestVersion   string    `json:"latest_version,omitempty"`
}

// Names returns the profile names, sorted.
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Store reads and writes the config directory.
type Store struct {
	Dir string
	// ForceFile keeps every secret in credentials.json (no keyring).
	ForceFile bool
	// Keyring is the keyring backend; nil means the OS keyring.
	Keyring Keyring
}

// Keyring is the subset of go-keyring the store uses.
type Keyring interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Get(s, u string) (string, error) { return keyring.Get(s, u) }
func (osKeyring) Set(s, u, p string) error        { return keyring.Set(s, u, p) }
func (osKeyring) Delete(s, u string) error        { return keyring.Delete(s, u) }

const keyringService = "sendio-cli"

// DefaultDir resolves the config directory: SENDIO_CONFIG_DIR, then
// $XDG_CONFIG_HOME/sendio, then ~/.config/sendio (%AppData%\sendio on Windows).
func DefaultDir(getenv func(string) string) string {
	if d := getenv("SENDIO_CONFIG_DIR"); d != "" {
		return d
	}
	if d := getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "sendio")
	}
	if runtime.GOOS == "windows" {
		if d := getenv("AppData"); d != "" {
			return filepath.Join(d, "sendio")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".sendio")
	}
	return filepath.Join(home, ".config", "sendio")
}

func (s *Store) kr() Keyring {
	if s.Keyring != nil {
		return s.Keyring
	}
	return osKeyring{}
}

func (s *Store) configPath() string { return filepath.Join(s.Dir, "config.json") }

// CredentialsPath is the file-backend secret file.
func (s *Store) CredentialsPath() string { return filepath.Join(s.Dir, "credentials.json") }

// Load reads config.json (an empty config when it does not exist).
func (s *Store) Load() (*Config, error) {
	c := &Config{Profiles: map[string]*Profile{}}
	b, err := os.ReadFile(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", s.configPath(), err)
	}
	if c.Profiles == nil {
		c.Profiles = map[string]*Profile{}
	}
	for n, p := range c.Profiles {
		p.Name = n
	}
	return c, nil
}

// Save writes config.json.
func (s *Store) Save(c *Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.configPath(), append(b, '\n'), 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *Store) readFileSecrets() (map[string]Secret, error) {
	m := map[string]Secret{}
	b, err := os.ReadFile(s.CredentialsPath())
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", s.CredentialsPath(), err)
	}
	return m, nil
}

func (s *Store) writeFileSecrets(m map[string]Secret) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.CredentialsPath(), append(b, '\n'), 0o600)
}

// GetSecret reads a profile's secret from the backend recorded on it.
func (s *Store) GetSecret(p *Profile) (Secret, error) {
	if p.Storage == StorageKeyring {
		v, err := s.kr().Get(keyringService, p.Name)
		if err != nil {
			return Secret{}, fmt.Errorf("reading the keyring entry for profile %q: %w", p.Name, err)
		}
		var sec Secret
		if err := json.Unmarshal([]byte(v), &sec); err != nil {
			return Secret{}, fmt.Errorf("the keyring entry for profile %q is corrupted: %w", p.Name, err)
		}
		return sec, nil
	}
	m, err := s.readFileSecrets()
	if err != nil {
		return Secret{}, err
	}
	sec, ok := m[p.Name]
	if !ok {
		return Secret{}, fmt.Errorf("no stored credentials for profile %q", p.Name)
	}
	return sec, nil
}

// PutSecret stores a profile's secret. A profile that has no backend yet
// tries the keyring first (unless ForceFile) and falls back to the file;
// fellBack reports that fallback so the caller can say so. The chosen
// backend is recorded on p.
func (s *Store) PutSecret(p *Profile, sec Secret) (fellBack bool, err error) {
	b, _ := json.Marshal(sec)
	if p.Storage == "" {
		p.Storage = StorageKeyring
		if s.ForceFile {
			p.Storage = StorageFile
		}
	}
	if p.Storage == StorageKeyring {
		if kerr := s.kr().Set(keyringService, p.Name, string(b)); kerr == nil {
			s.deleteFileSecret(p.Name)
			return false, nil
		}
		p.Storage = StorageFile
		fellBack = true
	}
	m, err := s.readFileSecrets()
	if err != nil {
		return fellBack, err
	}
	m[p.Name] = sec
	return fellBack, s.writeFileSecrets(m)
}

func (s *Store) deleteFileSecret(name string) {
	m, err := s.readFileSecrets()
	if err != nil {
		return
	}
	if _, ok := m[name]; ok {
		delete(m, name)
		_ = s.writeFileSecrets(m)
	}
}

// DeleteSecret removes a profile's secret from every backend.
func (s *Store) DeleteSecret(p *Profile) {
	if p.Storage == StorageKeyring {
		_ = s.kr().Delete(keyringService, p.Name)
	}
	s.deleteFileSecret(p.Name)
}

// Rename moves a profile and its secret to a new name.
func (s *Store) Rename(c *Config, from, to string) error {
	p, ok := c.Profiles[from]
	if !ok {
		return fmt.Errorf("no profile named %q", from)
	}
	if _, exists := c.Profiles[to]; exists {
		return fmt.Errorf("a profile named %q already exists", to)
	}
	sec, err := s.GetSecret(p)
	if err != nil {
		return err
	}
	s.DeleteSecret(p)
	delete(c.Profiles, from)
	np := *p
	np.Name = to
	if _, err := s.PutSecret(&np, sec); err != nil {
		return err
	}
	c.Profiles[to] = &np
	if c.Current == from {
		c.Current = to
	}
	return s.Save(c)
}

// ValidName reports whether a profile name is usable.
func ValidName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// DeviceID returns this machine's id for `sendio listen` (created on first
// use, stored as device_id in dir). The server derives the session signing
// secret from it, so the secret stays the same across reconnects and runs.
func DeviceID(dir string) string {
	p := filepath.Join(dir, "device_id")
	if b, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(b)); len(id) >= 8 {
			return id
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	id := "dev_" + hex.EncodeToString(raw)
	if err := os.MkdirAll(dir, 0o700); err == nil {
		_ = os.WriteFile(p, []byte(id+"\n"), 0o600)
	}
	return id
}
