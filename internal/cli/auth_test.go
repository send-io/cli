package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"send.io/cli/internal/config"
)

// fakeOAuth implements the CLI contract's section 1 endpoints.
type fakeOAuth struct {
	*fakeAPI
	t           *testing.T
	mu          sync.Mutex
	challenge   string
	redirect    string
	deviceName  string
	access      string // currently valid access token
	refresh     string // currently valid refresh token
	refreshes   int
	devicePolls int
	revoked     []string
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	o := &fakeOAuth{fakeAPI: newFake(t), t: t, access: "", refresh: ""}
	o.on("GET /v0/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "sendio-cli" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
			q.Get("scope") != "cli" || q.Get("resource") != o.URL || !strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") {
			t.Errorf("bad authorize request: %s", r.URL.RawQuery)
			w.WriteHeader(400)
			return
		}
		o.mu.Lock()
		o.challenge, o.redirect, o.deviceName = q.Get("code_challenge"), q.Get("redirect_uri"), q.Get("device_name")
		o.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=code1&state="+url.QueryEscape(q.Get("state"))+"&iss="+url.QueryEscape(o.URL), http.StatusFound)
	})
	o.on("POST /v0/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		o.mu.Lock()
		defer o.mu.Unlock()
		fail := func(code string) {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"error":%q,"error_description":"%s"}`, code, code)
		}
		if r.FormValue("client_id") != "sendio-cli" {
			fail("invalid_client")
			return
		}
		if res := r.FormValue("resource"); res != "" && res != o.URL {
			fail("invalid_target")
			return
		}
		issue := func(n int) {
			o.access, o.refresh = fmt.Sprintf("mxo_access_%d", n), fmt.Sprintf("mxr_refresh_%d", n)
			fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":3600,"refresh_token":%q,"scope":"cli"}`, o.access, o.refresh)
		}
		switch r.FormValue("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
			if r.FormValue("code") != "code1" || base64.RawURLEncoding.EncodeToString(sum[:]) != o.challenge || r.FormValue("redirect_uri") != o.redirect {
				fail("invalid_grant")
				return
			}
			issue(1)
		case "refresh_token":
			if r.FormValue("refresh_token") != o.refresh {
				fail("invalid_grant")
				return
			}
			o.refreshes++
			issue(10 + o.refreshes)
		case "urn:ietf:params:oauth:grant-type:device_code":
			if r.FormValue("device_code") != "dev123" {
				fail("invalid_grant")
				return
			}
			o.devicePolls++
			switch o.devicePolls {
			case 1, 2:
				fail("authorization_pending")
			case 3:
				fail("slow_down")
			default:
				issue(2)
			}
		default:
			fail("unsupported_grant_type")
		}
	})
	o.on("POST /v0/oauth/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		o.mu.Lock()
		o.deviceName = r.FormValue("device_name")
		o.mu.Unlock()
		if r.FormValue("client_id") != "sendio-cli" || r.FormValue("resource") != o.URL || r.FormValue("scope") != "cli" {
			w.WriteHeader(400)
			return
		}
		fmt.Fprintf(w, `{"device_code":"dev123","user_code":"ABCD-EFGH","verification_uri":"%s/oauth/device","verification_uri_complete":"%s/oauth/device?code=ABCD-EFGH","expires_in":600,"interval":5}`, o.URL, o.URL)
	})
	o.on("POST /v0/revoke", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		o.mu.Lock()
		o.revoked = append(o.revoked, r.FormValue("token"))
		o.mu.Unlock()
	})
	authed := func(r *http.Request) bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.access != "" && r.Header.Get("Authorization") == "Bearer "+o.access || r.Header.Get("Authorization") == "Bearer mx_key_abcdef"
	}
	o.on("GET /v0/cli/whoami", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(401)
			io.WriteString(w, `{"statusCode":401,"name":"invalid_access_token","message":"Invalid token."}`)
			return
		}
		auth := "oauth"
		if strings.Contains(r.Header.Get("Authorization"), "mx_key") {
			auth = "api_key"
		}
		fmt.Fprintf(w, `{"team":{"id":"team_1","name":"Acme"},"permission":"full_access","auth":%q,"key_name":null,"user_email":"ada@acme.dev","api_base":%q}`, auth, o.URL)
	})
	o.on("GET /domains", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(401)
			io.WriteString(w, `{"statusCode":401,"name":"invalid_access_token","message":"Expired."}`)
			return
		}
		io.WriteString(w, `{"object":"list","has_more":false,"data":[{"id":"d1","name":"acme.dev","status":"verified"}]}`)
	})
	return o
}

func readCreds(t *testing.T, h *harness) map[string]config.Secret {
	t.Helper()
	p := filepath.Join(h.dir, "credentials.json")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("credentials.json: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials.json mode %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(p)
	m := map[string]config.Secret{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLoginBrowserFlow(t *testing.T) {
	o := newFakeOAuth(t)
	h := newHarness(t, o.URL)
	a := h.newApp("")
	a.OpenURL = func(u string) error {
		go func() {
			resp, err := http.Get(u) // the "browser": follows the redirect to the loopback callback
			if err != nil {
				t.Errorf("browser: %v", err)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(body), "Signed in") {
				t.Errorf("callback page: %s", body)
			}
		}()
		return nil
	}
	if code := a.Run([]string{"login", "--timeout", "10s"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	m := h.outJSON()
	if m["auth"] != "oauth" || m["team"] != "Acme" || m["profile"] != "default" || m["storage"] != "file" {
		t.Fatalf("login result %v", m)
	}
	if o.deviceName != "test-host" {
		t.Fatalf("device_name %q", o.deviceName)
	}
	sec := readCreds(t, h)["default"]
	if sec.AccessToken != "mxo_access_1" || sec.RefreshToken != "mxr_refresh_1" || sec.ExpiresAt.IsZero() {
		t.Fatalf("stored secret %+v", sec)
	}
	// The saved profile works and whoami reports the team.
	if code := h.run("whoami"); code != 0 {
		t.Fatal(h.errOut())
	}
	if w := h.outJSON(); w["team"].(map[string]any)["name"] != "Acme" || w["source"] != "whoami" || w["credential_source"] != "profile" {
		t.Fatalf("whoami %v", w)
	}
	// Logout revokes and deletes.
	if code := h.run("logout"); code != 0 {
		t.Fatal(h.errOut())
	}
	if len(o.revoked) != 2 {
		t.Fatalf("revoked %v", o.revoked)
	}
	if len(readCreds(t, h)) != 0 {
		t.Fatal("credentials left after logout")
	}
	if code := h.run("whoami"); code != 3 {
		t.Fatalf("whoami after logout: exit %d", code)
	}
}

func TestLoginBrowserDenied(t *testing.T) {
	o := newFakeOAuth(t)
	o.on("GET /v0/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?error=access_denied&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	h := newHarness(t, o.URL)
	a := h.newApp("")
	a.OpenURL = func(u string) error { go http.Get(u); return nil }
	if code := a.Run([]string{"login", "--timeout", "10s"}); code != 3 {
		t.Fatalf("denied exit %d: %s", code, h.errOut())
	}
	if e := h.errJSON(); e["code"] != "access_denied" {
		t.Fatalf("envelope %v", e)
	}
}

func TestLoginDeviceFlowOverSSH(t *testing.T) {
	o := newFakeOAuth(t)
	h := newHarness(t, o.URL)
	h.env["SSH_CONNECTION"] = "10.0.0.1 5000 10.0.0.2 22"
	if code := h.run("login", "-p", "work"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	if o.devicePolls != 4 {
		t.Fatalf("device polls %d", o.devicePolls)
	}
	if !strings.Contains(h.errOut(), `"user_code":"ABCD-EFGH"`) {
		t.Fatalf("device code not shown: %s", h.errOut())
	}
	if len(h.opened) != 0 {
		t.Fatal("browser opened over SSH")
	}
	// slow_down added 5s to the 5s interval: 5+5+5+10 seconds of (fake) waiting.
	if got := h.clock.Now().Sub(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)); got != 25*time.Second {
		t.Fatalf("waited %v", got)
	}
	cfg, _ := (&config.Store{Dir: h.dir}).Load()
	if cfg.Current != "work" || cfg.Profiles["work"].Auth != "oauth" || cfg.Profiles["work"].Device != "test-host" {
		t.Fatalf("config %+v", cfg.Profiles["work"])
	}
	// Human mode prints the code.
	h2 := newHarness(t, o.URL)
	o.devicePolls = 0
	if code := h2.run("login", "--device", "--agent", "no"); code != 0 {
		t.Fatal(h2.errOut())
	}
	if !strings.Contains(h2.errOut(), "ABCD-EFGH") || !strings.Contains(h2.out(), "Logged in to Acme") {
		t.Fatalf("human device flow:\n%s\n%s", h2.errOut(), h2.out())
	}
}

func TestRefreshOn401AndRotation(t *testing.T) {
	o := newFakeOAuth(t)
	h := newHarness(t, o.URL)
	// A saved OAuth profile whose access token the server no longer accepts.
	st := &config.Store{Dir: h.dir, ForceFile: true}
	p := &config.Profile{Name: "default", BaseURL: o.URL, Auth: config.AuthOAuth}
	st.PutSecret(p, config.Secret{AccessToken: "mxo_stale", RefreshToken: "mxr_refresh_1", ExpiresAt: h.clock.Now().Add(time.Hour)})
	st.Save(&config.Config{Current: "default", Profiles: map[string]*config.Profile{"default": p}})
	o.refresh = "mxr_refresh_1"
	o.access = "mxo_valid_unknown_to_client"

	if code := h.run("domains", "list"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	if o.refreshes != 1 {
		t.Fatalf("refreshes %d", o.refreshes)
	}
	reqs := o.requests()
	var domainCalls []string
	for _, r := range reqs {
		if r.Path == "/domains" {
			domainCalls = append(domainCalls, r.Auth)
		}
	}
	if len(domainCalls) != 2 || domainCalls[0] != "Bearer mxo_stale" || domainCalls[1] != "Bearer mxo_access_11" {
		t.Fatalf("domain calls %v", domainCalls)
	}
	sec := readCreds(t, h)["default"]
	if sec.AccessToken != "mxo_access_11" || sec.RefreshToken != "mxr_refresh_11" {
		t.Fatalf("rotated secret not saved: %+v", sec)
	}

	// Proactive refresh when the token is about to expire.
	st.PutSecret(p, config.Secret{AccessToken: o.access, RefreshToken: o.refresh, ExpiresAt: h.clock.Now().Add(10 * time.Second)})
	if code := h.run("domains", "list"); code != 0 {
		t.Fatal(h.errOut())
	}
	if o.refreshes != 2 {
		t.Fatalf("expected a proactive refresh, got %d", o.refreshes)
	}

	// A revoked refresh token: exit 3 with a re-login hint.
	o.refresh = "something-else"
	o.access = "nope"
	if code := h.run("domains", "list"); code != 3 {
		t.Fatalf("revoked exit %d", code)
	}
	if e := h.errJSON(); e["code"] != "session_expired" {
		t.Fatalf("envelope %v", e)
	}
}

func TestLoginAPIKeyAndProfiles(t *testing.T) {
	o := newFakeOAuth(t)
	h := newHarness(t, o.URL)
	if code := h.run("login", "--api-key", "mx_key_abcdef", "-p", "prod"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut())
	}
	if sec := readCreds(t, h)["prod"]; sec.APIKey != "mx_key_abcdef" {
		t.Fatalf("stored %+v", sec)
	}
	if code := h.run("login", "--api-key", "mx_bad", "-p", "bad"); code != 3 {
		t.Fatalf("bad key exit %d", code)
	}
	h.run("login", "--api-key", "mx_key_abcdef", "-p", "staging")
	h.run("auth", "list")
	m := h.outJSON()
	if m["current"] != "staging" || len(m["profiles"].([]any)) != 2 {
		t.Fatalf("auth list %v", m)
	}
	if code := h.run("auth", "switch", "prod"); code != 0 {
		t.Fatal(h.errOut())
	}
	if code := h.run("auth", "switch", "nope"); code != 5 {
		t.Fatalf("switch unknown exit %d", code)
	}
	if code := h.run("auth", "rename", "staging", "stage"); code != 0 {
		t.Fatal(h.errOut())
	}
	if _, ok := readCreds(t, h)["stage"]; !ok {
		t.Fatal("secret not moved on rename")
	}
	h.run("auth", "status")
	if s := h.outJSON(); s["authenticated"] != true || s["profile"] != "prod" {
		t.Fatalf("status %v", s)
	}
	// -p selects without switching; env SENDIO_PROFILE too.
	h.env["SENDIO_PROFILE"] = "stage"
	h.run("whoami")
	if w := h.outJSON(); w["profile"] != "stage" {
		t.Fatalf("whoami profile %v", w["profile"])
	}
	delete(h.env, "SENDIO_PROFILE")
	if code := h.run("auth", "remove", "stage"); code != 2 {
		t.Fatalf("remove without --yes exit %d", code)
	}
	if code := h.run("auth", "remove", "stage", "--yes"); code != 0 {
		t.Fatal(h.errOut())
	}
	if _, ok := readCreds(t, h)["stage"]; ok {
		t.Fatal("secret left after remove")
	}
	// --api-key flag beats the profile.
	h.run("whoami", "--api-key", "mx_key_abcdef")
	if w := h.outJSON(); w["credential_source"] != "flag" {
		t.Fatalf("source %v", w["credential_source"])
	}
}

func TestWhoamiFallbackWithoutEndpoint(t *testing.T) {
	f := newFake(t) // no /v0/cli/whoami: 404
	f.json("GET /api-keys", 200, `{"object":"list","data":[{"id":"k1","name":"other","token_prefix":"mx_zzz","permission":"full_access"},{"id":"k2","name":"ci-key","token_prefix":"mx_test_","permission":"full_access"}]}`)
	h := newHarness(t, f.URL)
	withKey(h)
	if code := h.run("whoami"); code != 0 {
		t.Fatal(h.errOut())
	}
	m := h.outJSON()
	if m["source"] != "fallback" || m["key_name"] != "ci-key" || m["permission"] != "full_access" || m["note"] == nil {
		t.Fatalf("fallback %v", m)
	}
	// A sending-only key: /api-keys and /domains are forbidden.
	f.json("GET /api-keys", 403, `{"statusCode":403,"name":"restricted_api_key","message":"no"}`)
	f.json("GET /domains", 403, `{"statusCode":403,"name":"restricted_api_key","message":"no"}`)
	h.run("whoami")
	if m := h.outJSON(); m["permission"] != "sending_access" {
		t.Fatalf("sending fallback %v", m)
	}
}

type failingKeyring struct{}

func (failingKeyring) Get(s, u string) (string, error) { return "", fmt.Errorf("no keyring") }
func (failingKeyring) Set(s, u, p string) error        { return fmt.Errorf("no keyring") }
func (failingKeyring) Delete(s, u string) error        { return fmt.Errorf("no keyring") }

func TestKeyringFallbackWarns(t *testing.T) {
	o := newFakeOAuth(t)
	h := newHarness(t, o.URL)
	delete(h.env, "SENDIO_CREDENTIAL_STORE")
	a := h.newApp("")
	a.Keyring = failingKeyring{}
	if code := a.Run([]string{"login", "--api-key", "mx_key_abcdef"}); code != 0 {
		t.Fatal(h.errOut())
	}
	if !strings.Contains(h.errOut(), "no OS keyring available") {
		t.Fatalf("no fallback warning: %s", h.errOut())
	}
	if readCreds(t, h)["default"].APIKey != "mx_key_abcdef" {
		t.Fatal("not stored in the file")
	}
}
