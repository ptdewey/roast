package chatgpt

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type codexFixture struct {
	*fixture
	omitID                bool
	tokenType, tokenScope string
	noRevoke              bool
}

func setupCodex(t *testing.T) *codexFixture {
	t.Helper()
	f := setup(t)
	f.s.Close()
	f.clientID = codexClientID
	f.workspace = "workspace-1"
	c := &codexFixture{fixture: f}
	f.s = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(f.s.Close)
	f.m = NewCodex(filepath.Join(t.TempDir(), "credentials"))
	f.m.origin = f.s.URL
	return c
}
func (f *codexFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/.well-known/openid-configuration" && f.noRevoke {
		json.NewEncoder(w).Encode(map[string]string{"issuer": f.s.URL, "jwks_uri": f.s.URL + "/jwks"})
		return
	}
	if r.URL.Path != "/api/accounts/oauth/token" {
		f.fixture.serve(w, r)
		return
	}
	r.ParseForm()
	if f.mode == "failure" {
		http.Error(w, "secret-response", 500)
		return
	}
	if r.Form.Get("client_id") != codexClientID || r.Form.Has("resource") || r.Form.Has("ext_agent_host_id") {
		http.Error(w, "wrong flow", 400)
		return
	}
	if r.Form.Get("grant_type") == "authorization_code" {
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge || r.Form.Get("redirect_uri") != codexRedirect || r.Form.Get("code") != "code" {
			http.Error(w, "bad PKCE", 400)
			return
		}
	} else {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Form.Get("refresh_token") != f.lastRefresh {
			http.Error(w, "rotated reuse", 400)
			return
		}
		f.refreshes.Add(1)
	}
	f.lastRefresh = fmt.Sprintf("refresh-%d", f.refreshes.Load())
	result := map[string]any{"access_token": fmt.Sprintf("access-%d", f.refreshes.Load()), "refresh_token": f.lastRefresh, "expires_in": 3600}
	if !f.omitID {
		result["id_token"] = f.jwt()
	}
	if f.tokenType != "" {
		result["token_type"] = f.tokenType
	}
	if f.tokenScope != "" {
		result["scope"] = f.tokenScope
	}
	json.NewEncoder(w).Encode(result)
}
func (f *codexFixture) visit(t *testing.T) func(string) error {
	t.Helper()
	return func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		expected := map[string]string{"client_id": codexClientID, "response_type": "code", "redirect_uri": codexRedirect, "scope": codexScopes, "code_challenge_method": "S256", "id_token_add_organizations": "true", "codex_cli_simplified_flow": "true", "originator": "roast"}
		if u.Path != "/api/accounts/authorize" || u.Scheme+"://"+u.Host != f.s.URL {
			t.Error("wrong authorization endpoint")
		}
		if len(q) != len(expected)+2 {
			t.Errorf("unexpected authorization fields: %v", q)
		}
		for key, value := range expected {
			if q.Get(key) != value {
				t.Errorf("%s = %q", key, q.Get(key))
			}
		}
		if len(q.Get("state")) < 32 || len(q.Get("code_challenge")) < 32 {
			t.Error("missing PKCE or state")
		}
		f.challenge = q.Get("code_challenge")
		// The allowlisted URI says localhost; reach only the actual IPv4 listener.
		cb := "http://127.0.0.1:1455/auth/callback?" + url.Values{"state": {q.Get("state")}, "code": {"code"}}.Encode()
		resp, err := http.Get(cb)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return errors.New("callback failed")
		}
		return nil
	}
}
func (f *codexFixture) loginCodex(t *testing.T) Account {
	t.Helper()
	a, err := f.m.Login(context.Background(), "", f.visit(t))
	if err != nil {
		t.Fatal(err)
	}
	if a.AccountID != "workspace-1" {
		t.Fatalf("workspace %q", a.AccountID)
	}
	return a
}
func expireCodex(t *testing.T, f *codexFixture) record {
	t.Helper()
	r, err := f.m.load("default")
	if err != nil {
		t.Fatal(err)
	}
	r.ExpiresAt = time.Now().Add(-time.Hour)
	if err := writeJSON(f.m.path("default"), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCodexLoginRefreshAndLogout(t *testing.T) {
	f := setupCodex(t)
	f.loginCodex(t)
	if _, err := os.Stat(filepath.Join(f.m.dir, ".host.json")); !os.IsNotExist(err) {
		t.Fatal("legacy flow created dynamic host")
	}
	f.loginCodex(t)
	r := expireCodex(t, f)
	if r.Provider != "codex" || r.ClientID != codexClientID {
		t.Fatal("wrong persisted provider")
	}
	f.omitID = true // Legacy refresh need not repeat ID, type or scope.
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			m := NewCodex(f.m.dir)
			m.origin = f.s.URL
			token, id, err := m.CodexCredentials(context.Background(), "")
			if err != nil || token != "access-1" || id != "workspace-1" {
				t.Errorf("credentials: %q %q %v", token, id, err)
			}
		})
	}
	wg.Wait()
	if f.refreshes.Load() != 1 {
		t.Fatal("refresh not serialized")
	}
	r, err := f.m.load("default")
	if err != nil || r.RefreshToken != "refresh-1" {
		t.Fatal("rotation not persisted", err)
	}
	if err := f.m.Logout(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if f.revokes.Load() != 1 {
		t.Fatal("missing revocation")
	}
	if _, err := f.m.Status(context.Background(), ""); err == nil {
		t.Fatal("still signed in")
	}
}

func TestCodexRejectsInvalidSignedIdentity(t *testing.T) {
	for _, mode := range []string{"aud", "issuer", "expired", "signature", "subject", "workspace", "missing-id", "type", "scope"} {
		t.Run(mode, func(t *testing.T) {
			f := setupCodex(t)
			switch mode {
			case "subject":
				f.subject = ""
			case "workspace":
				f.workspace = ""
			case "missing-id":
				f.omitID = true
			case "type":
				f.tokenType = "Basic"
			case "scope":
				f.tokenScope = "openid"
			default:
				f.mode = mode
			}
			if _, err := f.m.Login(context.Background(), "", f.visit(t)); err == nil {
				t.Fatal("accepted invalid credentials")
			}
			if _, err := os.Stat(f.m.path("default")); !os.IsNotExist(err) {
				t.Fatal("invalid credentials persisted")
			}
		})
	}
}

func TestCodexPinsSubjectAndWorkspace(t *testing.T) {
	for _, mode := range []string{"subject", "workspace", "missing-workspace", "signature"} {
		for _, refresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/refresh=%v", mode, refresh), func(t *testing.T) {
				f := setupCodex(t)
				f.loginCodex(t)
				before, err := os.ReadFile(f.m.path("default"))
				if err != nil {
					t.Fatal(err)
				}
				if refresh {
					expireCodex(t, f)
					before, err = os.ReadFile(f.m.path("default"))
					if err != nil {
						t.Fatal(err)
					}
				}
				switch mode {
				case "subject":
					f.subject = "other"
				case "workspace":
					f.workspace = "other"
				case "missing-workspace":
					f.workspace = ""
				default:
					f.mode = mode
				}
				if refresh {
					_, _, err = f.m.CodexCredentials(context.Background(), "")
				} else {
					_, err = f.m.Login(context.Background(), "", f.visit(t))
				}
				if err == nil {
					t.Fatal("accepted identity/workspace change")
				}
				after, _ := os.ReadFile(f.m.path("default"))
				if string(before) != string(after) {
					t.Fatal("changed credentials on failed validation")
				}
			})
		}
	}
}

func TestCodexBusyPortAndCancellation(t *testing.T) {
	f := setupCodex(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:1455")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = f.m.Login(context.Background(), "", func(string) error { called = true; return nil })
	ln.Close()
	if err == nil || called {
		t.Fatal("busy port opened browser or succeeded")
	}
	if !strings.Contains(err.Error(), "Codex") || !strings.Contains(err.Error(), "1455") || !strings.Contains(err.Error(), "Codex/Pi") {
		t.Fatalf("unhelpful fixed-port error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = f.m.Login(ctx, "", func(raw string) error {
		resp, e := http.Get("http://127.0.0.1:1455/auth/callback?code=code&state=wrong")
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	ln, err = net.Listen("tcp4", "127.0.0.1:1455")
	if err != nil {
		t.Fatal("listener leaked", err)
	}
	ln.Close()
}

func TestOAuthProviderIsolation(t *testing.T) {
	ctx := context.Background()
	for _, provider := range []string{"", "chatgpt", "codex", "unknown"} {
		t.Run("record="+provider, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "credentials")
			cm, sm := NewCodex(dir), New(dir)
			if err := cm.prepare(); err != nil {
				t.Fatal(err)
			}
			client := "siwc-client"
			if provider == "codex" {
				client = codexClientID
			}
			r := record{Provider: provider, ClientID: client, Subject: "subject", Account: Account{AccountID: "workspace", ExpiresAt: time.Now().Add(time.Hour)}, AccessToken: "token", RefreshToken: "refresh", Scope: scopes}
			if err := writeJSON(cm.path("default"), r); err != nil {
				t.Fatal(err)
			}
			denied := cm
			if provider == "codex" {
				denied = sm
			}
			if _, err := denied.Status(ctx, ""); err == nil {
				t.Fatal("cross-mode status")
			}
			if _, err := denied.Token(ctx, ""); err == nil {
				t.Fatal("cross-mode token")
			}
			if err := denied.Logout(ctx, ""); err == nil {
				t.Fatal("cross-mode logout")
			}
			if _, err := denied.Login(ctx, "", func(string) error { t.Error("cross-mode browser"); return errors.New("stop") }); err == nil {
				t.Fatal("cross-mode login")
			}
			if provider == "" || provider == "chatgpt" {
				if _, err := sm.Status(ctx, ""); err != nil {
					t.Fatal("old SIWC unreadable", err)
				}
			}
		})
	}
	if _, _, err := New(t.TempDir()).CodexCredentials(ctx, ""); err == nil {
		t.Fatal("wrong manager accepted")
	}
}

func TestCodexLogoutFailureAndLocalFallback(t *testing.T) {
	f := setupCodex(t)
	f.loginCodex(t)
	before, _ := os.ReadFile(f.m.path("default"))
	f.mode = "failure"
	if err := f.m.Logout(context.Background(), ""); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("failed revocation not sanitized", err)
	}
	after, _ := os.ReadFile(f.m.path("default"))
	if string(before) != string(after) {
		t.Fatal("failed revocation erased credentials")
	}
	f.mode = ""
	f.noRevoke = true
	if err := f.m.Logout(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}
