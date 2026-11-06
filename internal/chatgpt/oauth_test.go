package chatgpt

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixture struct {
	m                                                          *Manager
	s                                                          *httptest.Server
	key                                                        *rsa.PrivateKey
	nonce, clientID, subject, scope, mode, challenge, redirect string
	refreshes                                                  atomic.Int32
	revokes                                                    atomic.Int32
	lastRefresh                                                string
	workspace                                                  string
	mu                                                         sync.Mutex
}

func setup(t *testing.T) *fixture {
	t.Helper()
	k, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{key: k, clientID: "oaiapp_test", subject: "subject-1", scope: scopes}
	f.s = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.s.Close)
	f.m = New(filepath.Join(t.TempDir(), "credentials"))
	f.m.origin = f.s.URL
	return f
}
func (f *fixture) jwt() string {
	enc := base64.RawURLEncoding.EncodeToString
	h := enc([]byte(`{"alg":"RS256","kid":"key"}`))
	nonce := f.nonce
	aud := f.clientID
	iss := f.s.URL
	exp := time.Now().Add(time.Hour).Unix()
	if f.mode == "nonce" {
		nonce = "wrong"
	}
	if f.mode == "aud" {
		aud = "wrong"
	}
	if f.mode == "issuer" {
		iss = "https://evil.invalid"
	}
	if f.mode == "expired" {
		exp = 1
	}
	b, _ := json.Marshal(map[string]any{"iss": iss, "aud": aud, "sub": f.subject, "email": "test@example.com", "exp": exp, "nonce": nonce, "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": f.workspace}})
	p := h + "." + enc(b)
	sum := sha256.Sum256([]byte(p))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if f.mode == "signature" {
		sig[0] ^= 1
	}
	return p + "." + enc(sig)
}
func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		json.NewEncoder(w).Encode(map[string]string{"issuer": f.s.URL, "jwks_uri": f.s.URL + "/jwks", "revocation_endpoint": f.s.URL + "/revoke"})
	case "/jwks":
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "key", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()), "e": "AQAB"}}})
	case "/api/accounts/oauth/token":
		r.ParseForm()
		if f.mode == "failure" {
			http.Error(w, "secret-refresh-token-response", 500)
			return
		}
		if r.Form.Get("client_id") != f.clientID || r.Form.Get("resource") != resource {
			http.Error(w, "bad registration", 400)
			return
		}
		if r.Form.Get("grant_type") == "authorization_code" {
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge || r.Form.Get("redirect_uri") != f.redirect || r.Form.Get("code") != "code" {
				http.Error(w, "bad PKCE", 400)
				return
			}
		} else {
			f.mu.Lock()
			defer f.mu.Unlock()
			if r.Form.Get("refresh_token") != f.lastRefresh {
				http.Error(w, "rotated token reuse", 400)
				return
			}
			f.refreshes.Add(1)
		}
		f.lastRefresh = fmt.Sprintf("refresh-%d", f.refreshes.Load())
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("access-%d", f.refreshes.Load()), "refresh_token": f.lastRefresh, "id_token": f.jwt(), "token_type": "Bearer", "expires_in": 3600, "scope": f.scope})
	case "/revoke":
		r.ParseForm()
		f.revokes.Add(1)
		if f.mode == "failure" {
			http.Error(w, "secret-response", 500)
			return
		}
		if r.Form.Get("token") != f.lastRefresh || r.Form.Get("client_id") != f.clientID || r.Form.Get("token_type_hint") != "refresh_token" {
			http.Error(w, "invalid revocation", 400)
		}
	default:
		http.NotFound(w, r)
	}
}
func (f *fixture) visit(t *testing.T) func(string) error {
	return func(raw string) error {
		t.Helper()
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		f.nonce = q.Get("nonce")
		f.challenge = q.Get("code_challenge")
		f.redirect = q.Get("redirect_uri")
		if q.Get("code_challenge_method") != "S256" || q.Get("resource") != resource || q.Get("scope") != scopes || q.Get("ext_agent_host_id") == "" {
			t.Error("missing authorization parameters")
		}
		if !strings.HasPrefix(f.redirect, "http://127.0.0.1:") || !strings.HasSuffix(f.redirect, "/auth/callback") {
			t.Error("unsafe redirect URI")
		}
		if q.Get("id_token_hint") != "" {
			t.Error("sensitive URL")
		}
		cb := f.redirect + "?" + url.Values{"code": {"code"}, "client_id": {f.clientID}, "state": {q.Get("state")}}.Encode()
		resp, e := http.Get(cb)
		if e != nil {
			return e
		}
		resp.Body.Close()
		return nil
	}
}
func (f *fixture) login(t *testing.T) {
	t.Helper()
	a, e := f.m.Login(context.Background(), "default", f.visit(t))
	if e != nil {
		t.Fatal(e)
	}
	if a.Name != "default" || a.Email != "test@example.com" {
		t.Fatalf("bad account: %+v", a)
	}
}
func TestLoginPersistenceReturningAndPermissions(t *testing.T) {
	f := setup(t)
	f.login(t)
	ctx := context.Background()
	token, e := New(f.m.dir).Token(ctx, "")
	if e != nil || token != "access-0" {
		t.Fatalf("reused token: %q %v", token, e)
	}
	host, e := f.m.host(ctx)
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{f.m.dir, f.m.path("default"), filepath.Join(f.m.dir, ".host.json")} {
		s, e := os.Stat(path)
		if e != nil {
			t.Fatal(e)
		}
		want := os.FileMode(0600)
		if s.IsDir() {
			want = 0700
		}
		if !private(path, s) {
			t.Fatal("credential permissions are not private")
		}
		if runtime.GOOS != "windows" && s.Mode().Perm() != want {
			t.Fatalf("permissions %v", s.Mode())
		}
	}
	f.login(t)
	host2, _ := f.m.host(ctx)
	if host != host2 {
		t.Fatal("host identity changed")
	}
	if _, e := f.m.Status(ctx, ""); e != nil {
		t.Fatal(e)
	}
}
func TestInvalidTokensAndScopes(t *testing.T) {
	for _, mode := range []string{"nonce", "aud", "issuer", "expired", "signature", "scopes"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.mode = mode
			if mode == "scopes" {
				f.scope = "openid profile email"
			}
			_, e := f.m.Login(context.Background(), "default", f.visit(t))
			if e == nil {
				t.Fatal("accepted invalid authorization")
			}
			if _, e := os.Stat(f.m.path("default")); !os.IsNotExist(e) {
				t.Fatal("persisted invalid credentials")
			}
		})
	}
}
func TestCallbackStateDenialAndCancellation(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, e := f.m.Login(ctx, "default", func(raw string) error {
		u, _ := url.Parse(raw)
		q := u.Query()
		redirect := q.Get("redirect_uri")
		resp, e := http.Get(redirect + "?state=wrong&code=code&client_id=oaiapp_test")
		if e != nil {
			return e
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Error("accepted wrong state")
		}
		cancel()
		return nil
	})
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation: %v", e)
	}
	_, e = f.m.Login(context.Background(), "default", func(raw string) error {
		u, _ := url.Parse(raw)
		q := u.Query()
		resp, e := http.Get(q.Get("redirect_uri") + "?" + url.Values{"state": {q.Get("state")}, "error": {"access_denied"}}.Encode())
		if e == nil {
			resp.Body.Close()
		}
		return e
	})
	if e == nil {
		t.Fatal("accepted denied authorization")
	}
}
func TestPKCEAndRegistrationMismatch(t *testing.T) {
	for _, mode := range []string{"pkce", "client"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			visit := f.visit(t)
			_, e := f.m.Login(context.Background(), "default", func(raw string) error {
				if mode == "client" {
					u, _ := url.Parse(raw)
					q := u.Query()
					f.nonce = q.Get("nonce")
					f.challenge = q.Get("code_challenge")
					f.redirect = q.Get("redirect_uri")
					resp, e := http.Get(f.redirect + "?" + url.Values{"code": {"code"}, "state": {q.Get("state")}, "client_id": {"dynamic_agent_client"}}.Encode())
					if e == nil {
						resp.Body.Close()
					}
					return e
				}
				e := visit(raw)
				f.challenge = "not-pkce"
				return e
			})
			if e == nil {
				t.Fatal("accepted invalid exchange")
			}
		})
	}
}
func expire(t *testing.T, f *fixture) {
	t.Helper()
	r, e := f.m.load("default")
	if e != nil {
		t.Fatal(e)
	}
	r.ExpiresAt = time.Now().Add(-time.Minute)
	if e = writeJSON(f.m.path("default"), r); e != nil {
		t.Fatal(e)
	}
}
func TestConcurrentRefreshRotation(t *testing.T) {
	f := setup(t)
	f.login(t)
	expire(t, f)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			m := New(f.m.dir)
			m.origin = f.s.URL
			token, e := m.Token(context.Background(), "")
			if e != nil || token != "access-1" {
				t.Errorf("refresh: %q %v", token, e)
			}
		})
	}
	wg.Wait()
	if f.refreshes.Load() != 1 {
		t.Fatalf("refresh count %d", f.refreshes.Load())
	}
	r, _ := f.m.load("default")
	if r.RefreshToken != "refresh-1" {
		t.Fatal("rotation not persisted")
	}
	expire(t, f)
	if _, e := f.m.Token(context.Background(), ""); e != nil {
		t.Fatal(e)
	}
	if f.refreshes.Load() != 2 {
		t.Fatal("second refresh failed")
	}
}
func TestTransientRefreshAndRevocationRetainCredentials(t *testing.T) {
	f := setup(t)
	f.login(t)
	expire(t, f)
	before, _ := os.ReadFile(f.m.path("default"))
	f.mode = "failure"
	_, e := f.m.Token(context.Background(), "")
	if e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatalf("unredacted failure: %v", e)
	}
	after, _ := os.ReadFile(f.m.path("default"))
	if string(before) != string(after) {
		t.Fatal("lost refresh credentials")
	}
	if e = f.m.Logout(context.Background(), ""); e == nil {
		t.Fatal("ignored revocation failure")
	}
	after, _ = os.ReadFile(f.m.path("default"))
	if string(before) != string(after) {
		t.Fatal("lost credentials after failed revocation")
	}
	f.mode = ""
	if e = f.m.Logout(context.Background(), ""); e != nil {
		t.Fatal(e)
	}
	r, _ := f.m.load("default")
	if r.AccessToken != "" || r.RefreshToken != "" || r.IDToken != "" || r.ClientID != f.clientID || r.Subject != f.subject {
		t.Fatal("incorrect signout retention")
	}
	if _, e = f.m.Token(context.Background(), ""); e == nil {
		t.Fatal("token after logout")
	}
}
func TestUnsafeStorageAndNames(t *testing.T) {
	for _, n := range []string{"..", "../other", "/tmp/pwn", "a.b", "a/b", "a\\b", strings.Repeat("x", 65)} {
		m := New(t.TempDir())
		if _, e := m.Token(context.Background(), n); e == nil {
			t.Fatalf("unsafe account %q", n)
		}
	}
	f := setup(t)
	f.login(t)
	if runtime.GOOS != "windows" {
		os.Chmod(f.m.path("default"), 0644)
		if _, e := f.m.Token(context.Background(), ""); e == nil {
			t.Fatal("accepted public credentials")
		}
		os.Chmod(f.m.path("default"), 0600)
	}
	target := filepath.Join(t.TempDir(), "target")
	os.WriteFile(target, []byte("sensitive"), 0600)
	os.Remove(f.m.path("default"))
	if e := os.Symlink(target, f.m.path("default")); e != nil {
		t.Skip(e)
	}
	if _, e := f.m.Token(context.Background(), ""); e == nil {
		t.Fatal("followed symlink")
	}
}
func TestLoginDoesNotLockDuringVisitAndDetectsConflict(t *testing.T) {
	f := setup(t)
	f.login(t)
	visit := f.visit(t)
	_, e := f.m.Login(context.Background(), "default", func(raw string) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, e := f.m.Status(ctx, ""); e != nil {
			t.Fatal(e)
		}
		r, _ := f.m.load("default")
		r.Revision = random()
		if e := writeJSON(f.m.path("default"), r); e != nil {
			t.Fatal(e)
		}
		return visit(raw)
	})
	if e == nil || !strings.Contains(e.Error(), "changed during login") {
		t.Fatalf("missing conflict: %v", e)
	}
}
func TestHTTPBoundaries(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if e := f.m.request(ctx, "https://attacker.invalid/steal", url.Values{"token": {"secret"}}, nil); e == nil {
		t.Fatal("untrusted endpoint")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, f.s.URL+"/jwks", http.StatusFound) }))
	defer redirect.Close()
	m := New(t.TempDir())
	m.origin = redirect.URL
	if e := m.request(ctx, redirect.URL, nil, &map[string]any{}); e == nil {
		t.Fatal("followed redirect")
	}
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", (1<<20)+1)) }))
	defer large.Close()
	m.origin = large.URL
	if e := m.request(ctx, large.URL, nil, nil); e == nil {
		t.Fatal("accepted oversized body")
	}
}

func TestProcessTokenHelper(t *testing.T) {
	if os.Getenv("ROAST_CHATGPT_LOCK_HELPER") != "1" {
		return
	}
	m := New(os.Getenv("ROAST_CHATGPT_TEST_DIR"))
	m.origin = os.Getenv("ROAST_CHATGPT_TEST_ORIGIN")
	token, err := m.Token(context.Background(), "default")
	if err != nil || token != "access-1" {
		t.Fatalf("child refresh failed: %v", err)
	}
}
func TestRefreshSerializedAcrossProcesses(t *testing.T) {
	f := setup(t)
	f.login(t)
	expire(t, f)
	var commands []*exec.Cmd
	for range 4 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTokenHelper$")
		cmd.Env = append(os.Environ(), "ROAST_CHATGPT_LOCK_HELPER=1", "ROAST_CHATGPT_TEST_DIR="+f.m.dir, "ROAST_CHATGPT_TEST_ORIGIN="+f.s.URL)
		commands = append(commands, cmd)
	}
	var wg sync.WaitGroup
	for _, cmd := range commands {
		wg.Go(func() {
			if out, e := cmd.CombinedOutput(); e != nil {
				t.Errorf("child: %v %s", e, out)
			}
		})
	}
	wg.Wait()
	if f.refreshes.Load() != 1 {
		t.Fatalf("cross-process refreshes: %d", f.refreshes.Load())
	}
}
func TestReturningIdentityAndRegistrationProtected(t *testing.T) {
	for _, change := range []string{"identity", "registration"} {
		t.Run(change, func(t *testing.T) {
			f := setup(t)
			f.login(t)
			before, _ := os.ReadFile(f.m.path("default"))
			if change == "identity" {
				f.subject = "different-user"
			} else {
				f.clientID = "different-client"
			}
			_, err := f.m.Login(context.Background(), "default", f.visit(t))
			if err == nil {
				t.Fatal("replaced selected account")
			}
			after, _ := os.ReadFile(f.m.path("default"))
			if string(before) != string(after) {
				t.Fatal("mutated selected account")
			}
		})
	}
}
func TestCanceledLockAndUnrelatedAccounts(t *testing.T) {
	f := setup(t)
	f.login(t)
	release, err := f.m.lock(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err = f.m.lock(ctx, "default"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock cancellation %v", err)
	}
	other, err := f.m.lock(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	other()
}

func TestHostAccountDoesNotClobberIdentity(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	before, err := f.m.host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.m.Login(ctx, "host", f.visit(t)); err != nil {
		t.Fatal(err)
	}
	after, err := f.m.host(ctx)
	if err != nil || before != after {
		t.Fatalf("host account changed host identity: %v", err)
	}
	if _, err = f.m.Status(ctx, "host"); err != nil {
		t.Fatal(err)
	}
}
func TestRejectSymlinkParent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	m := New(filepath.Join(link, "auth"))
	if _, err := m.Status(context.Background(), "default"); err == nil {
		t.Fatal("accepted symlinked parent")
	}
}
func TestReturningLoginDoesNotOverwriteConcurrentRefresh(t *testing.T) {
	f := setup(t)
	f.login(t)
	expire(t, f)
	visit := f.visit(t)
	_, err := f.m.Login(context.Background(), "default", func(raw string) error {
		if _, e := f.m.Token(context.Background(), "default"); e != nil {
			return e
		}
		return visit(raw)
	})
	if err == nil || !strings.Contains(err.Error(), "changed during login") {
		t.Fatalf("missing refresh conflict: %v", err)
	}
	r, e := f.m.load("default")
	if e != nil || r.RefreshToken != "refresh-1" {
		t.Fatalf("lost refresh token: %v", e)
	}
}

func TestUnsolicitedStateDoesNotConsumeLogin(t *testing.T) {
	f := setup(t)
	visit := f.visit(t)
	_, err := f.m.Login(context.Background(), "default", func(raw string) error {
		u, _ := url.Parse(raw)
		q := u.Query()
		resp, e := http.Get(q.Get("redirect_uri") + "?state=unsolicited&code=bad&client_id=wrong")
		if e != nil {
			return e
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Error("accepted unsolicited callback")
		}
		return visit(raw)
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestRefreshInvalidIdentityOrScopeRetainsRecord(t *testing.T) {
	for _, mode := range []string{"identity", "scopes", "signature"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.login(t)
			expire(t, f)
			before, _ := os.ReadFile(f.m.path("default"))
			switch mode {
			case "identity":
				f.subject = "other-user"
			case "scopes":
				f.scope = "openid"
			default:
				f.mode = mode
			}
			if _, err := f.m.Token(context.Background(), "default"); err == nil {
				t.Fatal("accepted invalid refresh")
			}
			after, _ := os.ReadFile(f.m.path("default"))
			if string(before) != string(after) {
				t.Fatal("replaced account on invalid refresh")
			}
		})
	}
}

func TestDefaultUsesIsolatedUserConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("APPDATA", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	m, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, m.dir)
	if err != nil || strings.HasPrefix(rel, "..") || !strings.HasSuffix(m.dir, filepath.Join("roast", "auth", "chatgpt")) {
		t.Fatal("default escaped isolated config directory")
	}
}
