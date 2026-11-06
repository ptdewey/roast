package scope

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// revisionWorkspace stops at the closest VCS boundary: a nested Git checkout
// must not accidentally resolve its revisions in an enclosing jj workspace.
func revisionWorkspace(start string) (root string, jj bool, err error) {
	root, err = filepath.Abs(start)
	if err != nil {
		return "", false, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", false, err
	}
	for {
		if _, err := os.Stat(filepath.Join(root, ".jj")); err == nil {
			return root, true, nil
		} else if !os.IsNotExist(err) {
			return "", false, err
		}
		if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
			return root, false, nil
		} else if !os.IsNotExist(err) {
			return "", false, err
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", false, fmt.Errorf("not inside a Git or jj workspace")
		}
		root = parent
	}
}

// RepositoryForRevision locates the workspace used for explicit revision
// reviews without changing GitRoot's existing discovery behavior.
func RepositoryForRevision(start string) (string, error) {
	root, isJJ, err := revisionWorkspace(start)
	if err != nil {
		return "", err
	}
	if isJJ {
		// All downstream content readers currently require a Git worktree. Do not
		// silently discover an unrelated enclosing Git repository for bare-backed jj.
		if _, err := jjOut(root, "git", "root", "--ignore-working-copy"); err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
			return "", fmt.Errorf("revision reviews currently require a colocated Git-backed jj workspace")
		}
	}
	return GitRoot(root)
}

func jjOut(repo string, args ...string) (string, error) {
	cmd := exec.Command("jj", append([]string{"--repository", repo, "--no-pager", "--color=never"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("jj: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("jj: %w", err)
	}
	return string(out), nil
}

var revisionOID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func buildRevision(start string, opts BuildOpts) (Scope, error) {
	for _, name := range []string{"GIT_INDEX_FILE", "GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		if os.Getenv(name) != "" {
			return Scope{}, fmt.Errorf("revision reviews cannot run with %s set; unset custom Git routing variables to preserve the caller repository", name)
		}
	}
	if strings.TrimSpace(opts.Revision) == "" {
		return Scope{}, fmt.Errorf("revision must not be blank")
	}
	if opts.Base != "" || opts.StagedOnly || opts.UncommittedOnly || opts.StagedTreeOID != "" || opts.IncludeWorktree || (opts.Scope != "" && opts.Scope != "diff") {
		return Scope{}, fmt.Errorf("revision requires diff scope and cannot be combined with base, staged, uncommitted, or include-worktree options")
	}
	root, isJJ, err := revisionWorkspace(start)
	if err != nil {
		return Scope{}, err
	}
	repo, err := RepositoryForRevision(root)
	if err != nil {
		return Scope{}, err
	}
	target := opts.Revision
	if isJJ {
		// Intentionally snapshot the working copy here: @ means today's dirty @,
		// not the stale commit before the user's most recent edits.
		out, err := jjOut(root, "log", "--no-graph", "--revisions", opts.Revision, "--template", `commit_id ++ " " ++ conflict ++ "\n"`)
		if err != nil {
			return Scope{}, fmt.Errorf("resolve revision %q: %w", opts.Revision, err)
		}
		lines := nonEmptyLines(out)
		if len(lines) != 1 {
			return Scope{}, fmt.Errorf("revision %q must resolve to exactly one commit (got %d)", opts.Revision, len(lines))
		}
		fields := strings.Fields(lines[0])
		if len(fields) != 2 || !revisionOID.MatchString(fields[0]) || fields[1] != "false" {
			return Scope{}, fmt.Errorf("revision %q is conflicted or cannot be materialized as a Git commit", opts.Revision)
		}
		target = fields[0]
	}
	// --end-of-options prevents revision strings from becoming rev-parse flags;
	// ^{commit} rejects trees/blobs and --verify rejects ranges/multiple objects.
	out, err := resolveRevisionCommit(repo, target)
	if err != nil {
		return Scope{}, fmt.Errorf("resolve revision %q: %w", opts.Revision, err)
	}
	head := strings.TrimSpace(out)
	if !revisionOID.MatchString(head) {
		return Scope{}, fmt.Errorf("revision did not resolve to a single commit")
	}
	treeOut, err := gitOut(repo, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return Scope{}, fmt.Errorf("materialize revision tree: %w", err)
	}
	tree := strings.TrimSpace(treeOut)
	parentsOut, err := gitOut(repo, "cat-file", "commit", head)
	if err != nil {
		return Scope{}, fmt.Errorf("read revision parents: %w", err)
	}
	var parents []string
	for _, line := range strings.Split(parentsOut, "\n") {
		if line == "" {
			break
		}
		if parent, ok := strings.CutPrefix(line, "parent "); ok {
			if !revisionOID.MatchString(parent) {
				return Scope{}, fmt.Errorf("invalid revision parent")
			}
			parents = append(parents, parent)
		}
	}
	base := ""
	notes := []string{fmt.Sprintf("reviewing revision %s only; context pinned to tree %s", head, tree)}
	if len(parents) > 0 {
		base = parents[0]
	} else {
		empty, err := gitOut(repo, "hash-object", "-w", "-t", "tree", "--stdin")
		if err != nil {
			return Scope{}, fmt.Errorf("resolve empty tree: %w", err)
		}
		base = strings.TrimSpace(empty)
		notes = append(notes, "root commit: comparing against the empty tree")
	}
	if len(parents) > 1 {
		notes = append(notes, "merge commit: comparing against its first parent only")
	}
	maxSection := opts.MaxFileDiffBytes
	if maxSection <= 0 {
		maxSection = DefaultMaxFileDiffBytes
	}
	excludes := append([]string{}, opts.Excludes...)
	if !opts.IncludeVendored {
		excludes = append(excludes, DefaultIgnores...)
	}
	// Pin the private index and attribute-only worktree to the selected tree
	// for every diff, including rename probes.
	diffRepo, cleanupRepo, err := revisionDiffRepository(repo, tree)
	if err != nil {
		return Scope{}, fmt.Errorf("pin revision diff attributes: %w", err)
	}
	defer cleanupRepo()
	repo = diffRepo
	flags := append([]string{"--no-renames"}, PatchFlags...)
	refs := []string{base, head}
	// Resolve attributes only for the actual changed paths, using the target's
	// private index. This covers deletions too without consulting mutable HEAD.
	nameArgs := append(append(append([]string{"diff"}, flags...), "--name-only", "-z"), refs...)
	changed, err := gitOut(repo, append(append([]string{}, nameArgs...), "--")...)
	if err != nil {
		return Scope{}, fmt.Errorf("read revision paths: %w", err)
	}
	var marked []string
	if !opts.IncludeVendored {
		env, cleanup, err := pinnedIndexEnv(repo, true, tree)
		if err != nil {
			return Scope{}, fmt.Errorf("materialize revision index: %w", err)
		}
		defer cleanup()
		marked = linguistMarked(repo, splitNUL(changed), true, env)
		if len(marked) > 0 {
			notes = append(notes, fmt.Sprintf("excluding %d path(s) marked linguist-generated/-vendored in target .gitattributes (--include-vendored to review)", len(marked)))
		}
	}
	spec := pathspecArgs(opts.Paths, excludes, marked)
	args := append(append(append([]string{"diff"}, flags...), refs...), spec...)
	diff, cut, err := gitPatchOut(repo, maxSection, args...)
	if err != nil {
		return Scope{}, fmt.Errorf("git diff (revision): %w", err)
	}
	diff, renamed := compactRenames(repo, refs, spec, diff, maxSection)
	notes = renameNote(truncationNote(notes, cut, maxSection), renamed)
	names, err := gitOut(repo, append(append([]string{}, nameArgs...), spec...)...)
	if err != nil {
		return Scope{}, fmt.Errorf("git diff --name-only (revision): %w", err)
	}
	paths := splitNUL(names)
	sort.Strings(paths)
	return Scope{Diff: diff, Files: paths, Base: base, BaseOID: base, HeadOID: head, RevisionTreeOID: tree, Notes: notes}, nil
}

// rev-parse can exit successfully while warning that it chose one of several
// colliding refs. A revision review must never accept that implicit choice.
func resolveRevisionCommit(repo, target string) (string, error) {
	cmd := exec.Command("git", "-C", repo, "-c", "core.warnAmbiguousRefs=true", "rev-parse", "--verify", "--end-of-options", target+"^{commit}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stderr.Len() > 0 {
		return "", fmt.Errorf("revision resolution was not unambiguous: %s", strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// revisionDiffRepository borrows immutable objects, not refs/config/hooks,
// from the source repository. Only regular .gitattributes files are materialized;
// no source checkout or executable content is written.
func revisionDiffRepository(repo, tree string) (string, func(), error) {
	noop := func() {}
	objects, err := gitOut(repo, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return "", noop, err
	}
	format, err := gitOut(repo, "rev-parse", "--show-object-format")
	if err != nil {
		return "", noop, err
	}
	tmp, err := os.MkdirTemp("", "roast-revision-")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	if _, err := gitOut(tmp, "-c", "init.templateDir=", "init", "--quiet", "--object-format="+strings.TrimSpace(format)); err != nil {
		cleanup()
		return "", noop, err
	}
	if err := os.WriteFile(filepath.Join(tmp, ".git", "objects", "info", "alternates"), []byte(strconv.Quote(strings.TrimSuffix(objects, "\n"))+"\n"), 0600); err != nil {
		cleanup()
		return "", noop, err
	}
	if _, err := gitOut(tmp, "read-tree", tree); err != nil {
		cleanup()
		return "", noop, err
	}
	// Only regular attribute blobs are materialized; never follow tree symlinks.
	listing, err := gitOut(repo, "ls-tree", "-r", "-z", tree)
	if err != nil {
		cleanup()
		return "", noop, err
	}
	for _, entry := range splitNUL(listing) {
		header, path, ok := strings.Cut(entry, "\t")
		if !ok || filepath.Base(path) != ".gitattributes" {
			continue
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" {
			continue
		}
		clean := filepath.Clean(path)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			cleanup()
			return "", noop, fmt.Errorf("invalid attribute path")
		}
		content, err := gitOut(repo, "cat-file", "blob", fields[2])
		if err != nil {
			cleanup()
			return "", noop, err
		}
		full := filepath.Join(tmp, clean)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			cleanup()
			return "", noop, err
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			cleanup()
			return "", noop, err
		}
	}
	return tmp, cleanup, nil
}
