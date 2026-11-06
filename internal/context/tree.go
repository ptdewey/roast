package context

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Tree names the corpus every repository reader in one review must agree on.
// The zero value means the live working tree. A non-empty OID is an immutable
// Git tree object, never a mutable ref or commit.
type Tree struct {
	// OID is the pinned tree object. Empty means the live working tree.
	OID string
	// Worktree distinguishes a captured checkout or selected revision from
	// the pinned index used by staged reviews. For compatibility, a non-empty OID with this
	// false is the index corpus.
	Worktree bool
	// Revision identifies a selected historical commit rather than a captured checkout.
	Revision bool
}

// Pinned reports whether t names immutable Git tree content.
func (t Tree) Pinned() bool {
	return t.OID != ""
}

// Staged reports whether t is the pinned index corpus for a staged review.
func (t Tree) Staged() bool {
	return t.Pinned() && !t.Worktree
}

// PinnedWorktree returns an immutable captured-worktree corpus.
func PinnedWorktree(oid string) Tree {
	return Tree{OID: oid, Worktree: oid != ""}
}

// PinnedRevision returns the immutable corpus of an explicitly selected commit.
func PinnedRevision(oid string) Tree {
	return Tree{OID: oid, Worktree: oid != "", Revision: oid != ""}
}

// PinIndexTree snapshots the CURRENT index into an immutable tree object and
// returns it as a Tree. This is what closes the one-review-one-tree race: a
// concurrent `git add` after this call cannot change what OID's readers see,
// because the tree was already written before they run.
//
// It copies the live index file and runs write-tree over the copy under a
// private GIT_INDEX_FILE, but touches no ref
// and no commit — this is a read, not a capture. The live index is never
// written. Deliberately duplicates rather than imports snapshot's helpers: a
// content-reading package pulling in the run-recording package is a new
// dependency this refactor doesn't need.
//
// GIT_INDEX_FILE wins when set, for the same reason snapshot.Capture honors
// it: under the native pre-commit gate git points it at a temporary index
// holding exactly the commit's content, and pinning gitDir/index instead
// would review a DIFFERENT state than the one being committed.
func PinIndexTree(repo string) (Tree, error) {
	gitDir, err := gitOut(repo, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return Tree{}, fmt.Errorf("resolve git dir: %w", err)
	}

	live := os.Getenv("GIT_INDEX_FILE")
	if live == "" {
		live = filepath.Join(gitDir, "index")
	} else if !filepath.IsAbs(live) {
		// git may hand it over relative to the repo root.
		live = filepath.Join(repo, live)
	}

	tmp, err := os.MkdirTemp("", "roast-pin-")
	if err != nil {
		return Tree{}, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	idxCopy := filepath.Join(tmp, "index")
	if err := copyIndexFile(live, idxCopy); err != nil {
		return Tree{}, err
	}

	oid, err := gitOut(repo, []string{"GIT_INDEX_FILE=" + idxCopy}, "write-tree")
	if err != nil {
		return Tree{}, fmt.Errorf("write-tree: %w", err)
	}
	return Tree{OID: oid}, nil
}

// copyIndexFile copies the live index to dst. A missing index (a repo where
// nothing was ever staged) is not an error: git treats an absent
// GIT_INDEX_FILE as an empty index.
func copyIndexFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(f, in); err != nil {
		_ = f.Close()
		return fmt.Errorf("copy index: %w", err)
	}
	return f.Close()
}

// gitOut runs a git command in dir with extra environment, returning trimmed
// stdout. Extra env is appended to the process environment so a caller-set
// GIT_INDEX_FILE wins over an inherited one.
func gitOut(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	raw, err := cmd.Output()
	if err != nil {
		return "", withStderr(err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// withStderr folds a failed command's stderr into the error, which is where
// git puts every message worth reading.
func withStderr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}
