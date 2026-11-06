package chatgpt

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const issuer = "https://auth.openai.com"
const resource = "https://api.openai.com/v1"
const scopes = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"

// Manager stores isolated account registrations in a protected directory.
// Construct with New/Default (SIWC) or NewCodex/DefaultCodex (legacy Codex).
// Endpoints cannot be configured.
type Manager struct {
	dir    string
	origin string
	client *http.Client
	codex  bool
}

// New creates a credential manager. The directory is created privately on use.
func New(dir string) *Manager {
	return &Manager{dir: dir, origin: issuer, client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect denied") }}}
}

// Default returns the manager in the user's platform configuration directory.
func Default() (*Manager, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return nil, errors.New("cannot locate user configuration directory")
	}
	return New(filepath.Join(d, "roast", "auth", "chatgpt")), nil
}
func (m *Manager) request(ctx context.Context, endpoint string, form url.Values, out any) error {
	u, err := url.Parse(endpoint)
	base, _ := url.Parse(m.origin)
	if err != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" {
		return errors.New("untrusted ChatGPT OAuth endpoint")
	}
	method := http.MethodGet
	var body io.Reader
	if form != nil {
		method = http.MethodPost
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("invalid ChatGPT OAuth request")
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("ChatGPT OAuth request failed")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return errors.New("invalid ChatGPT OAuth response")
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New("ChatGPT OAuth request rejected; retry or sign in again")
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return errors.New("invalid ChatGPT OAuth response")
	}
	return nil
}

type discovery struct {
	Issuer string `json:"issuer"`
	JWKS   string `json:"jwks_uri"`
	Revoke string `json:"revocation_endpoint"`
}

func (m *Manager) discover(ctx context.Context) (discovery, error) {
	var d discovery
	err := m.request(ctx, m.origin+"/.well-known/openid-configuration", nil, &d)
	if err == nil && d.Issuer != m.origin {
		err = errors.New("invalid ChatGPT OAuth issuer")
	}
	return d, err
}

type claims struct {
	Issuer          string          `json:"iss"`
	Audience        json.RawMessage `json:"aud"`
	Subject         string          `json:"sub"`
	Email           string          `json:"email"`
	Exp             int64           `json:"exp"`
	Nonce           string          `json:"nonce"`
	AuthorizedParty string          `json:"azp"`
	Auth            struct {
		AccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
}

func (m *Manager) validateID(ctx context.Context, token, clientID, nonce string) (claims, error) {
	var c claims
	bad := errors.New("invalid ChatGPT ID token")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, bad
	}
	decode := base64.RawURLEncoding.DecodeString
	hb, e := decode(parts[0])
	if e != nil {
		return c, bad
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hb, &h) != nil || h.Alg != "RS256" || h.Kid == "" {
		return c, bad
	}
	d, e := m.discover(ctx)
	if e != nil {
		return c, e
	}
	var keys struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if e = m.request(ctx, d.JWKS, nil, &keys); e != nil {
		return c, e
	}
	sig, e := decode(parts[2])
	if e != nil {
		return c, bad
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	valid := false
	for _, k := range keys.Keys {
		if k.Kid != h.Kid || k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, e1 := decode(k.N)
		eb, e2 := decode(k.E)
		if e1 != nil || e2 != nil || len(eb) > 4 {
			continue
		}
		exp := 0
		for _, b := range eb {
			exp = exp*256 + int(b)
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
		if pub.N.BitLen() < 2048 {
			continue
		}
		if rsa.VerifyPKCS1v15(pub, crypto.SHA256, hash[:], sig) == nil {
			valid = true
			break
		}
	}
	if !valid {
		return c, bad
	}
	cb, e := decode(parts[1])
	if e != nil || json.Unmarshal(cb, &c) != nil {
		return c, bad
	}
	var aud []string
	var single string
	if json.Unmarshal(c.Audience, &single) == nil {
		aud = []string{single}
	} else if json.Unmarshal(c.Audience, &aud) != nil {
		return c, bad
	}
	found := false
	for _, a := range aud {
		if a == clientID {
			found = true
		}
	}
	if !found || c.Issuer != m.origin || c.Subject == "" || c.Exp <= time.Now().Unix() || (nonce != "" && c.Nonce != nonce) || (len(aud) > 1 && c.AuthorizedParty != clientID) || (c.AuthorizedParty != "" && c.AuthorizedParty != clientID) {
		return c, bad
	}
	return c, nil
}

type tokenResponse struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	ID      string `json:"id_token"`
	Type    string `json:"token_type"`
	Scope   string `json:"scope"`
	Expires int64  `json:"expires_in"`
}

func permitted(s string) bool {
	have := map[string]bool{}
	for _, v := range strings.Fields(s) {
		have[v] = true
	}
	for _, v := range strings.Fields(scopes) {
		if !have[v] {
			return false
		}
	}
	return true
}
func (m *Manager) tokens(ctx context.Context, form url.Values) (tokenResponse, error) {
	var t tokenResponse
	err := m.request(ctx, m.origin+"/api/accounts/oauth/token", form, &t)
	return t, err
}
func apply(r *record, t tokenResponse) error {
	if t.Access == "" || !strings.EqualFold(t.Type, "Bearer") || t.Expires <= 0 || t.Expires > 86400*365 || !permitted(t.Scope) {
		return errors.New("ChatGPT token response lacks required credentials or permissions")
	}
	return applyCredentials(r, t)
}

func applyCredentials(r *record, t tokenResponse) error {
	r.AccessToken = t.Access
	r.ExpiresAt = time.Now().Add(time.Duration(t.Expires) * time.Second)
	r.Scope = t.Scope
	if t.Refresh != "" {
		r.RefreshToken = t.Refresh
	}
	if t.ID != "" {
		r.IDToken = t.ID
	}
	r.Revision = random()
	return nil
}

// Status returns local account metadata without refreshing or exposing tokens.
func (m *Manager) Status(ctx context.Context, account string) (Account, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	n, err := name(account)
	if err != nil {
		return Account{}, err
	}
	release, err := m.lock(ctx, n)
	if err != nil {
		return Account{}, err
	}
	defer release()
	r, err := m.load(n)
	if err == nil && r.AccessToken == "" {
		if m.codex {
			err = errors.New("codex account is signed out; run roast auth login --provider codex")
		} else {
			err = errors.New("ChatGPT account is signed out")
		}
	}
	return r.Account, err
}

// Token returns a current access token, serializing rotating refreshes across processes.
func (m *Manager) Token(ctx context.Context, account string) (string, error) {
	if m.codex {
		token, _, err := m.CodexCredentials(ctx, account)
		return token, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	n, err := name(account)
	if err != nil {
		return "", err
	}
	release, err := m.lock(ctx, n)
	if err != nil {
		return "", err
	}
	defer release()
	r, err := m.load(n)
	if err != nil {
		return "", err
	}
	if r.AccessToken != "" && time.Until(r.ExpiresAt) > time.Minute && permitted(r.Scope) {
		return r.AccessToken, nil
	}
	if r.RefreshToken == "" {
		return "", errors.New("ChatGPT account requires sign in")
	}
	t, err := m.tokens(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {r.ClientID}, "refresh_token": {r.RefreshToken}, "resource": {resource}})
	if err != nil {
		return "", err
	}
	if t.Scope == "" {
		t.Scope = r.Scope
	}
	if t.ID != "" {
		c, e := m.validateID(ctx, t.ID, r.ClientID, "")
		if e != nil {
			return "", e
		}
		if c.Subject != r.Subject {
			return "", errors.New("ChatGPT account identity changed")
		}
		r.Email = c.Email
	}
	if err = apply(&r, t); err != nil {
		return "", err
	}
	if err = writeJSON(m.path(n), r); err != nil {
		return "", err
	}
	return r.AccessToken, nil
}

// Logout confirms advertised remote revocation before clearing local credentials.
// On failure, credentials are retained so revocation can be retried. Registration
// and host persist. Legacy Codex signs out locally if discovery advertises no
// revocation endpoint; this does not invalidate the session on other devices.
func (m *Manager) Logout(ctx context.Context, account string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	n, err := name(account)
	if err != nil {
		return err
	}
	release, err := m.lock(ctx, n)
	if err != nil {
		return err
	}
	defer release()
	r, err := m.load(n)
	if err != nil {
		return err
	}
	if r.RefreshToken != "" {
		d, e := m.discover(ctx)
		if e != nil {
			return e
		}
		if d.Revoke == "" && !m.codex {
			return errors.New("ChatGPT revocation endpoint unavailable")
		}
		if d.Revoke != "" {
			if e = m.request(ctx, d.Revoke, url.Values{"token": {r.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {r.ClientID}}, nil); e != nil {
				return e
			}
		}
	}
	r.AccessToken = ""
	r.RefreshToken = ""
	r.IDToken = ""
	r.Scope = ""
	r.ExpiresAt = time.Time{}
	r.Revision = random()
	return writeJSON(m.path(n), r)
}

// Avoid accidental credentials in diagnostic formatting of internal records.
func (r record) String() string { return "ChatGPT credential record (redacted)" }
