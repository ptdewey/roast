package chatgpt

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
const codexScopes = "openid profile email offline_access"
const codexRedirect = "http://localhost:1455/auth/callback"

// NewCodex creates an isolated legacy Codex OAuth manager, not a SIWC manager.
func NewCodex(dir string) *Manager { m := New(dir); m.codex = true; return m }

// DefaultCodex returns the legacy Codex manager in its separate configuration directory.
func DefaultCodex() (*Manager, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return nil, errors.New("cannot locate user configuration directory")
	}
	return NewCodex(filepath.Join(d, "roast", "auth", "codex")), nil
}

func (m *Manager) checkProvider(r record) error {
	if (m.codex && (r.Provider != "codex" || r.ClientID != codexClientID)) || (!m.codex && r.Provider != "" && r.Provider != "chatgpt") {
		return errors.New("OAuth credential provider mismatch; use a separate account directory")
	}
	return nil
}

func (m *Manager) apply(r *record, t tokenResponse) error {
	if !m.codex {
		return apply(r, t)
	}
	// Legacy responses omit token_type and scope, including during refresh.
	if t.Type != "" && !strings.EqualFold(t.Type, "Bearer") {
		return errors.New("invalid Codex token type")
	}
	if t.Scope != "" {
		have := map[string]bool{}
		for _, scope := range strings.Fields(t.Scope) {
			have[scope] = true
		}
		for _, scope := range strings.Fields(codexScopes) {
			if !have[scope] {
				return errors.New("codex token lacks identity permissions")
			}
		}
	}
	// Reuse atomic credential application, with a separate legacy permission policy.
	if t.Access == "" || t.Expires <= 0 || t.Expires > 86400*365 {
		return errors.New("codex token response lacks required credentials or lifetime")
	}
	if err := applyCredentials(r, t); err != nil {
		return err
	}
	r.Scope = codexScopes
	return nil
}

// CodexCredentials atomically returns the refreshed access token and its signed workspace.
// Identity and workspace are pinned at login; a refresh cannot silently switch either.
func (m *Manager) CodexCredentials(ctx context.Context, account string) (string, string, error) {
	if !m.codex {
		return "", "", errors.New("codex credentials require a Codex manager")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	n, err := name(account)
	if err != nil {
		return "", "", err
	}
	release, err := m.lock(ctx, n)
	if err != nil {
		return "", "", err
	}
	defer release()
	r, err := m.load(n)
	if err != nil {
		return "", "", err
	}
	if r.AccountID == "" || r.Subject == "" {
		return "", "", errors.New("codex account requires sign in")
	}
	if r.AccessToken != "" && time.Until(r.ExpiresAt) > time.Minute {
		return r.AccessToken, r.AccountID, nil
	}
	if r.RefreshToken == "" {
		return "", "", errors.New("codex account requires sign in")
	}
	t, err := m.tokens(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {codexClientID}, "refresh_token": {r.RefreshToken}})
	if err != nil {
		return "", "", err
	}
	if t.ID != "" {
		c, e := m.validateID(ctx, t.ID, codexClientID, "")
		if e != nil {
			return "", "", e
		}
		if c.Subject != r.Subject || c.Auth.AccountID == "" || c.Auth.AccountID != r.AccountID {
			return "", "", errors.New("codex account identity or workspace changed")
		}
		r.Email = c.Email
	}
	if err = m.apply(&r, t); err != nil {
		return "", "", err
	}
	if err = writeJSON(m.path(n), r); err != nil {
		return "", "", err
	}
	return r.AccessToken, r.AccountID, nil
}
