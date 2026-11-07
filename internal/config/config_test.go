package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type memKeyring map[string]string

func (m memKeyring) Get(s, u string) (string, error) {
	v, ok := m[s+"/"+u]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}
func (m memKeyring) Set(s, u, p string) error { m[s+"/"+u] = p; return nil }
func (m memKeyring) Delete(s, u string) error { delete(m, s+"/"+u); return nil }

func TestFileStore(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir, ForceFile: true}
	p := &Profile{Name: "default", BaseURL: "https://api.send.io", Auth: AuthOAuth}
	exp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.PutSecret(p, Secret{AccessToken: "a", RefreshToken: "r", ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	if p.Storage != StorageFile {
		t.Fatalf("storage %q", p.Storage)
	}
	st, _ := os.Stat(filepath.Join(dir, "credentials.json"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	got, err := s.GetSecret(p)
	if err != nil || got.AccessToken != "a" || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("got %+v %v", got, err)
	}
	c := &Config{Current: "default", Profiles: map[string]*Profile{"default": p}}
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(c, "default", "work"); err != nil {
		t.Fatal(err)
	}
	c2, _ := s.Load()
	if c2.Current != "work" || c2.Profiles["work"] == nil || c2.Profiles["default"] != nil {
		t.Fatalf("after rename %+v", c2)
	}
	if sec, err := s.GetSecret(c2.Profiles["work"]); err != nil || sec.RefreshToken != "r" {
		t.Fatalf("secret after rename %+v %v", sec, err)
	}
	s.DeleteSecret(c2.Profiles["work"])
	if _, err := s.GetSecret(c2.Profiles["work"]); err == nil {
		t.Fatal("secret still there")
	}
}

func TestKeyringStore(t *testing.T) {
	kr := memKeyring{}
	s := &Store{Dir: t.TempDir(), Keyring: kr}
	p := &Profile{Name: "p1", Auth: AuthAPIKey}
	fell, err := s.PutSecret(p, Secret{APIKey: "mx_1"})
	if err != nil || fell || p.Storage != StorageKeyring {
		t.Fatalf("fell=%v err=%v storage=%s", fell, err, p.Storage)
	}
	if _, err := os.Stat(s.CredentialsPath()); err == nil {
		t.Fatal("keyring secret also written to the file")
	}
	if sec, _ := s.GetSecret(p); sec.APIKey != "mx_1" {
		t.Fatal("keyring read")
	}
	s.DeleteSecret(p)
	if len(kr) != 0 {
		t.Fatal("keyring entry not deleted")
	}
}

func TestDefaultDirAndNames(t *testing.T) {
	env := map[string]string{"XDG_CONFIG_HOME": "/x"}
	if d := DefaultDir(func(k string) string { return env[k] }); d != "/x/sendio" {
		t.Fatal(d)
	}
	env["SENDIO_CONFIG_DIR"] = "/y"
	if d := DefaultDir(func(k string) string { return env[k] }); d != "/y" {
		t.Fatal(d)
	}
	for n, ok := range map[string]bool{"default": true, "prod-eu.1": true, "": false, "a b": false, "x/y": false} {
		if ValidName(n) != ok {
			t.Errorf("ValidName(%q) != %v", n, ok)
		}
	}
}
