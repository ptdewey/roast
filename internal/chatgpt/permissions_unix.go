//go:build !windows

package chatgpt

import (
	"errors"
	"os"
	"path/filepath"
)

func private(_ string, st os.FileInfo) bool { return st.Mode().Perm()&0077 == 0 }
func makePrivateDir(path string) error      { return os.MkdirAll(path, 0700) }

// Reject symlinked credential parents. System-owned top-level aliases (notably
// macOS /var and /tmp) are allowed; user-controlled links below them are not.
func checkParents(path string) error {
	p, err := filepath.Abs(path)
	if err != nil {
		return errors.New("invalid credential path")
	}
	for p != filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e == nil && st.Mode()&os.ModeSymlink != 0 {
			if p != "/tmp" && p != "/var" {
				return errors.New("ChatGPT credential parents must not be symlinks")
			}
		} else if e != nil && !os.IsNotExist(e) {
			return errors.New("cannot inspect ChatGPT credential parents")
		}
		p = filepath.Dir(p)
	}
	return nil
}
