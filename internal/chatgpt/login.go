package chatgpt

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// Login authorizes an account in the system browser. visit is invoked only after
// the loopback listener is ready. No account lock is held while visiting.
func (m *Manager) Login(ctx context.Context, account string, visit func(string) error) (Account, error) {
	n, err := name(account)
	if err != nil {
		return Account{}, err
	}
	if visit == nil {
		return Account{}, errors.New("ChatGPT login requires a browser callback")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var host string
	if !m.codex {
		host, err = m.host(ctx)
		if err != nil {
			return Account{}, err
		}
	}
	release, err := m.lock(ctx, n)
	if err != nil {
		return Account{}, err
	}
	var old record
	err = readJSON(m.path(n), &old)
	release()
	if err != nil && !os.IsNotExist(err) {
		return Account{}, err
	}
	if err == nil {
		if e := m.checkProvider(old); e != nil {
			return Account{}, e
		}
	}
	initial := old.ClientID == ""
	clientID := old.ClientID
	if initial {
		clientID = "dynamic_agent_client"
	}
	address := "127.0.0.1:0"
	if m.codex {
		clientID = codexClientID
		address = "127.0.0.1:1455"
	}
	ln, err := net.Listen("tcp4", address)
	if err != nil {
		if m.codex {
			return Account{}, errors.New("cannot start Codex callback: port 1455 is in use or unavailable; close other Codex/Pi login attempts and retry (the callback port cannot be changed)")
		}
		return Account{}, errors.New("cannot start ChatGPT loopback callback")
	}
	defer ln.Close()
	redirect := "http://" + ln.Addr().String() + "/auth/callback"
	if m.codex {
		redirect = codexRedirect
	}
	state, nonce, verifier := random(), random(), random()
	hash := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {clientID}, "ext_agent_host_id": {host}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {scopes}, "resource": {resource}, "state": {state}, "nonce": {nonce}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}}
	if initial {
		q.Set("agent_name_hint", "Roast")
	} else {
		if old.Email != "" {
			q.Set("login_hint", old.Email)
		}
		// Deliberately omit id_token_hint: CLI browser URLs are displayed publicly.
		// The optional email hint provides selection without putting an ID token in logs.
	}
	if m.codex {
		nonce = "" // Legacy Codex does not request or return an OIDC nonce. State and PKCE bind the exchange.
		q = url.Values{"client_id": {codexClientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {codexScopes}, "state": {state}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"}, "originator": {"roast"}}
	}
	type callback struct {
		code, client string
		err          error
	}
	result := make(chan callback, 1)
	var once sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.URL.Path != "/auth/callback" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		v, e := url.ParseQuery(r.URL.RawQuery)
		if e != nil {
			http.Error(w, "Invalid callback", 400)
			return
		}
		for _, key := range []string{"state", "code", "client_id", "error"} {
			if len(v[key]) > 1 {
				http.Error(w, "Invalid callback", 400)
				return
			}
		}
		if subtle.ConstantTimeCompare([]byte(v.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid authorization state", 400)
			return
		}
		c := callback{code: v.Get("code"), client: v.Get("client_id")}
		if v.Get("error") != "" {
			c.err = errors.New("ChatGPT authorization denied")
		}
		if c.err == nil {
			if c.code == "" {
				c.err = errors.New("ChatGPT callback missing authorization code")
			}
			if initial && !m.codex {
				if c.client == "" || c.client == "dynamic_agent_client" {
					c.err = errors.New("ChatGPT registration missing issued client ID")
				}
			} else {
				if c.client == "" {
					c.client = clientID
				}
				if c.client != clientID {
					c.err = errors.New("ChatGPT registration changed")
				}
			}
		}
		once.Do(func() { result <- c })
		_, _ = w.Write([]byte("Authorization received. You may close this window."))
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
	defer server.Close()
	go func() { _ = server.Serve(ln) }()
	if err = visit(m.origin + "/api/accounts/authorize?" + q.Encode()); err != nil {
		return Account{}, errors.New("cannot open ChatGPT authorization page")
	}
	var c callback
	select {
	case c = <-result:
	case <-ctx.Done():
		return Account{}, ctx.Err()
	}
	_ = server.Close()
	if c.err != nil {
		return Account{}, c.err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {c.client}, "code": {c.code}, "code_verifier": {verifier}, "redirect_uri": {redirect}, "resource": {resource}}
	if m.codex {
		form.Del("resource")
	}
	t, err := m.tokens(ctx, form)
	if err != nil {
		return Account{}, err
	}
	id, err := m.validateID(ctx, t.ID, c.client, nonce)
	if err != nil {
		return Account{}, err
	}
	if !initial && id.Subject != old.Subject {
		return Account{}, errors.New("ChatGPT account identity changed")
	}
	if m.codex && (id.Auth.AccountID == "" || (!initial && id.Auth.AccountID != old.AccountID)) {
		return Account{}, errors.New("codex account workspace missing or changed")
	}
	if t.Refresh == "" {
		return Account{}, errors.New("ChatGPT response missing renewable session")
	}
	r := record{Account: Account{Name: n, Email: id.Email}, ClientID: c.client, Subject: id.Subject}
	if m.codex {
		r.Provider = "codex"
		r.AccountID = id.Auth.AccountID
	}
	if err = m.apply(&r, t); err != nil {
		return Account{}, err
	}
	release, err = m.lock(ctx, n)
	if err != nil {
		return Account{}, err
	}
	defer release()
	var current record
	e := readJSON(m.path(n), &current)
	if e != nil && !os.IsNotExist(e) {
		return Account{}, e
	}
	if e == nil {
		if err := m.checkProvider(current); err != nil {
			return Account{}, err
		}
	}
	if current.Revision != old.Revision {
		return Account{}, errors.New("ChatGPT account changed during login; retry sign in")
	}
	if err = writeJSON(m.path(n), r); err != nil {
		return Account{}, err
	}
	return r.Account, nil
}
