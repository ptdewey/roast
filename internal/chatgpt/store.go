// Package chatgpt manages isolated direct SIWC and legacy Codex OAuth credentials.
package chatgpt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Account contains display metadata, never credentials.
type Account struct {
	AccountID string    `json:"account_id,omitempty"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expires_at"`
}
type record struct {
	Account
	Provider     string `json:"provider,omitempty"`
	Subject      string `json:"subject"`
	ClientID     string `json:"client_id"`
	IDToken      string `json:"id_token,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Revision     string `json:"revision"`
}

var accountName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func name(s string) (string, error) {
	if s == "" {
		s = "default"
	}
	if !accountName.MatchString(s) {
		return "", errors.New("invalid ChatGPT account name")
	}
	return s, nil
}
func random() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("secure random source unavailable")
	}
	return hex.EncodeToString(b[:])
}
func (m *Manager) prepare() error {
	if err := checkParents(m.dir); err != nil {
		return err
	}
	if err := makePrivateDir(m.dir); err != nil {
		return errors.New("cannot create ChatGPT credential directory")
	}
	st, err := os.Lstat(m.dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || !private(m.dir, st) {
		return errors.New("ChatGPT credential directory must be private (0700) and not a symlink")
	}
	return nil
}
func safe(path string) error {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("cannot inspect ChatGPT credential file")
	}
	if !st.Mode().IsRegular() || !private(path, st) {
		return errors.New("ChatGPT credential file must be regular and private (0600)")
	}
	return nil
}
func readJSON(path string, v any) error {
	if err := safe(path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(b) > 1<<20 {
		return errors.New("cannot read ChatGPT credential file")
	}
	if json.Unmarshal(b, v) != nil {
		return errors.New("invalid ChatGPT credential file")
	}
	return nil
}
func writeJSON(path string, v any) error {
	if err := safe(path); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return errors.New("cannot encode ChatGPT credentials")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return errors.New("cannot create ChatGPT credential file")
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return errors.New("cannot save ChatGPT credentials")
	}
	return nil
}
func (m *Manager) path(n string) string { return filepath.Join(m.dir, n+".json") }
func (m *Manager) load(n string) (record, error) {
	var r record
	err := readJSON(m.path(n), &r)
	if err != nil {
		if m.codex {
			return r, errors.New("codex account unavailable; run roast auth login --provider codex")
		}
		return r, errors.New("ChatGPT account unavailable; sign in first")
	}
	if err := m.checkProvider(r); err != nil {
		return r, err
	}
	r.Name = n
	return r, nil
}
func (m *Manager) lock(ctx context.Context, n string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.prepare(); err != nil {
		return nil, err
	}
	p := filepath.Join(m.dir, n+".lock")
	if err := safe(p); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("cannot lock ChatGPT account")
	}
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, errors.New("cannot lock ChatGPT account")
		}
		if ok {
			return func() { unlock(f); f.Close() }, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
func (m *Manager) host(ctx context.Context) (string, error) {
	release, err := m.lock(ctx, ".host")
	if err != nil {
		return "", err
	}
	defer release()
	p := filepath.Join(m.dir, ".host.json")
	var h struct{ ID string }
	if err := readJSON(p, &h); err == nil {
		if h.ID == "" {
			return "", errors.New("invalid ChatGPT host identity")
		}
		return h.ID, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	// UUID v4 identifiers are explicitly supported by the SIWC protocol.
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("cannot generate ChatGPT host identity")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h.ID = fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	return h.ID, writeJSON(p, h)
}
