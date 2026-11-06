// Package scope owns diff production: deciding exactly what gets reviewed.
//
// Owns ref resolution, pathspec scoping, default ignores, and untracked-file
// synthesis. Invariant: the returned Files list is derived from the same
// pathspec-filtered git queries that produced the diff text — a file outside
// the post-exclude diff can never reach the reviewer or the output.
//
// Diffs are built with --no-renames: a rename becomes a plain delete+add, so
// pathspec excludes apply uniformly to both sides (rename detection across an
// exclude boundary was the root of the v1 vendor-escape bug). compactRenames
// then collapses delete+add pairs whose two paths both survived the excludes
// into git's rename form, so a large move costs a header per file instead of
// every line twice.
package scope

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bluesky-social/roast/internal/snapshot"
)

// DEFAULT_IGNORES are reviewed only with IncludeVendored. Applied as git
// :(exclude) pathspecs and to untracked-file filtering. Deliberately
// conservative: only paths that are essentially never human-authored.
var DefaultIgnores = []string{
	"vendor/**",
	"node_modules/**",
	"**/node_modules/**",
	"*.lock",
	"package-lock.json",
	"yarn.lock",
	"pnpm-lock.yaml",
	"go.sum",
	"*.pb.go",
	"*_pb2.py",
	"*_pb2_grpc.py",
	"*.min.js",
	"*.min.css",
}

// PatchFlags go on every git invocation that produces a patch. A reviewed
// repository's .gitattributes can route any path to a diff driver by name,
// and the user's own config decides what that driver runs; neither the
// textconv program nor an external diff may run on, or rewrite, reviewed
// content. Color and prefix settings in user config would otherwise change
// the text roast parses (`+`/`-` sides, `a/`/`b/` paths).
var PatchFlags = []string{"--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/"}

// BaseCandidates are tried in order when no explicit base is requested.
var BaseCandidates = []string{"origin/main", "origin/master", "main", "master", "HEAD~1"}

// EmptyTree is git's well-known empty-tree object. Lets git diff work in a
// repo with no commits yet (everything shows as added).
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Scope is the result of one diff-production pass.
type Scope struct {
	Diff   string   // combined diff text; empty in survey mode (see Manifest)
	Files  []string // sorted list of files in scope (derived from same queries as Diff/Manifest)
	Base   string   // resolved base ref; empty for uncommitted/staged/repo scope
	Notes  []string // human-facing scoping decisions
	Staged bool     // file context must come from the index, not the worktree

	// StagedTreeOID is the pinned tree-ish this diff was measured against when
	// Staged is true and BuildOpts.StagedTreeOID was set (empty when the
	// caller didn't pin one, in which case Staged content readers fall back to
	// the live index). Callers construct their context.Tree from this, not
	// from Staged alone, so every content reader in the review resolves
	// against the exact same OID Build itself diffed against.
	StagedTreeOID string

	// RevisionTreeOID pins all context reads for an explicitly selected commit.
	RevisionTreeOID string

	// Survey-mode fields (Scope == "branch" or "repo"). Diff stays empty;
	// Manifest carries the small map handed to the finder instead. BaseOID/
	// MergeBaseOID/HeadOID are the immutable snapshot identity — rev-parse of
	// the resolved refs at Build time — so later phases (git_diff tool) can
	// pin all reads to one snapshot instead of the mutable worktree/refs.
	Manifest     string // survey map (diffstat+name-status+log for branch; tracked listing for repo); empty for diff scope
	Survey       bool   // true for branch/repo scope
	BaseOID      string // branch scope: rev-parse of the resolved base
	MergeBaseOID string // branch scope: rev-parse of merge-base(base, HEAD)
	HeadOID      string // rev-parse HEAD; empty if the repo has no commits
}

// Empty reports whether there is nothing to review. Survey scope has no Diff
// to inspect (by design — see Manifest), so it is judged by Files instead: a
// branch with no commits since base, or a repo with no tracked files, is
// empty; a non-empty manifest around zero changed files must not read as
// "no diff" the way a blank Diff string would.
func (s *Scope) Empty() bool {
	if s.Survey {
		return len(s.Files) == 0
	}
	return strings.TrimSpace(s.Diff) == ""
}

// BuildOpts controls what Build produces.
type BuildOpts struct {
	Revision        string   // exactly one commit/revset target, diffed against its first parent
	Base            string   // explicit base ref; empty = try BaseCandidates (diff scope) or the remote default branch (branch scope)
	UncommittedOnly bool     // skip committed-since-base; uncommitted + untracked only (diff scope)
	StagedOnly      bool     // exactly the staged set (git diff --cached) (diff scope)
	Paths           []string // limit to these pathspec patterns
	Excludes        []string // extra exclude patterns (prepended before DefaultIgnores)
	IncludeVendored bool     // skip DefaultIgnores
	Scope           string   // "" / "diff" (default, today's behavior) / "branch" / "repo"
	IncludeWorktree bool     // branch scope: allow a dirty tree instead of failing fast (worktree synthesis is a follow-on)
	// StagedTreeOID, when set (StagedOnly only), is a tree object already
	// pinned from the live index — e.g. via context.PinIndexTree, run once by
	// the caller before Build. The staged diff is then taken against this
	// fixed tree-ish (`git diff <head> <T>`) instead of the mutable
	// `--cached` index, so a concurrent `git add` while the review is running
	// cannot move what Build itself measured out from under the other
	// context readers that resolve against the same pinned OID. Empty falls
	// back to today's `--cached` behavior.
	StagedTreeOID string
	// MaxFileDiffBytes caps each file's section of the diff (diff scope);
	// zero means DefaultMaxFileDiffBytes. A cut section ends in a marker
	// line the finder sees, and Notes names the files. Callers pass their
	// whole-diff budget, since no single file can be shown beyond it anyway.
	MaxFileDiffBytes int
}

// Build produces the diff under review.
//
// Default: committed-since-base + uncommitted + untracked, minus excluded
// paths. StagedOnly: exactly the staged set (git diff --cached) — what a
// commit would contain — with no untracked synthesis and no working-tree
// noise; this is the pre-commit gate's scope.
func Build(repo string, opts BuildOpts) (Scope, error) {
	if opts.Revision != "" {
		return buildRevision(repo, opts)
	}
	var notes []string
	maxSection := opts.MaxFileDiffBytes
	if maxSection <= 0 {
		maxSection = DefaultMaxFileDiffBytes
	}
	var cutSections []string
	allExcludes := append([]string{}, opts.Excludes...)
	// linguistExcludes are resolved literal file paths (not globs) — kept
	// separate from allExcludes so pathspecArgs can emit them with :(literal)
	// magic. A tracked file literally named `*.go` marked generated must
	// exclude only itself, not every Go file; without literal magic its own
	// name would be interpreted as a wildcard and blow the review scope open
	// (the exact vendor-escape class this package guards against).
	var linguistExcludes []string
	if !opts.IncludeVendored {
		allExcludes = append(allExcludes, DefaultIgnores...)
		// Honor the repo's own .gitattributes generated/vendored markers.
		// This is the standard, tool-agnostic channel a repo uses to say
		// "these files aren't authored source" (GitHub collapses them in
		// diffs and drops them from language stats); roast treats them like
		// DefaultIgnores. Unlike repository-local configuration, deliberately
		// absent, see config package doc — an in-tree .gitattributes marker is
		// a committed, PR-reviewable claim bounded to "is this source", so
		// honoring it is not a review-suppression trust hole. linguistMarked
		// deliberately suppresses the global/system attribute sources (not
		// committed, not PR-reviewable) so only in-tree/index attributes can
		// narrow scope. Same --include-vendored escape.
		//
		// Staged-only resolves attributes from the index (--cached), not the
		// working tree, so an unstaged .gitattributes edit cannot narrow the
		// pre-commit gate's scope below what the commit actually contains.
		linguistExcludes = linguistIgnores(repo, opts.StagedOnly, opts.StagedTreeOID)
		if len(linguistExcludes) > 0 {
			notes = append(notes, fmt.Sprintf("excluding %d path(s) marked linguist-generated/-vendored in .gitattributes (--include-vendored to review)", len(linguistExcludes)))
		}
	}
	spec := pathspecArgs(opts.Paths, allExcludes, linguistExcludes)
	// --no-renames on every diff invocation; see package doc.
	diffFlags := append([]string{"--no-renames"}, PatchFlags...)

	switch opts.Scope {
	case "branch":
		return buildSurveyBranch(repo, opts, spec, allExcludes, notes)
	case "repo":
		return buildSurveyRepo(repo, opts, allExcludes, linguistExcludes, notes)
	case "", "diff":
		// fall through to diff-mode below
	default:
		return Scope{}, fmt.Errorf("unsupported scope %q: expected \"\", \"diff\", \"branch\", or \"repo\"", opts.Scope)
	}

	// A repo with no commits has no HEAD; diff staged content against the
	// empty tree instead so brand-new repos are still reviewable.
	head := "HEAD"
	if err := git(repo, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		head = EmptyTree
		opts.UncommittedOnly = true
		notes = append(notes, "repository has no commits; reviewing staged + untracked files")
	}

	if opts.StagedOnly {
		// A pre-pinned tree (StagedTreeOID) replaces `--cached` with the
		// equivalent two-tree-ish form: `git diff --cached X` already means
		// "X's tree vs the index", so `git diff X <pinned tree>` measures the
		// exact same comparison against an OID that cannot move mid-review,
		// instead of the live index that can move during a review.
		stagedRef := []string{"--cached", head}
		if opts.StagedTreeOID != "" {
			stagedRef = []string{head, opts.StagedTreeOID}
		}

		diffArgs := append([]string{"diff"}, diffFlags...)
		diffArgs = append(diffArgs, stagedRef...)
		diffArgs = append(diffArgs, spec...)
		diff, cut, err := gitPatchOut(repo, maxSection, diffArgs...)
		if err != nil {
			return Scope{}, fmt.Errorf("git diff (staged): %w", err)
		}
		notes = truncationNote(notes, cut, maxSection)
		diff, renamed := compactRenames(repo, stagedRef, spec, diff, maxSection)
		notes = renameNote(notes, renamed)

		nameArgs := append([]string{"diff"}, diffFlags...)
		nameArgs = append(nameArgs, "--name-only")
		nameArgs = append(nameArgs, stagedRef...)
		nameArgs = append(nameArgs, spec...)
		nameOut, err := gitOut(repo, nameArgs...)
		if err != nil {
			return Scope{}, fmt.Errorf("git diff --name-only (staged): %w", err)
		}
		files := nonEmptyLines(nameOut)
		sort.Strings(files)
		return Scope{Diff: diff, Files: files, Notes: notes, Staged: true, StagedTreeOID: opts.StagedTreeOID}, nil
	}

	var resolvedBase string
	var committed, committedFiles string

	if !opts.UncommittedOnly {
		var err error
		resolvedBase, err = resolveBase(repo, opts.Base)
		if err != nil || resolvedBase == "" {
			tried := opts.Base
			if tried == "" {
				tried = strings.Join(BaseCandidates, ", ")
			}
			notes = append(notes, fmt.Sprintf("no usable base ref (tried %s); reviewing uncommitted changes only", tried))
		} else {
			if opts.Base == "" && resolvedBase != BaseCandidates[0] {
				notes = append(notes, fmt.Sprintf("using base '%s'", resolvedBase))
			}
			rangeArgs := buildRangeArgs(repo, resolvedBase)

			args := append([]string{"diff"}, diffFlags...)
			args = append(args, rangeArgs...)
			args = append(args, spec...)
			var cut []string
			committed, cut, err = gitPatchOut(repo, maxSection, args...)
			if err != nil {
				return Scope{}, fmt.Errorf("git diff (committed): %w", err)
			}
			cutSections = append(cutSections, cut...)
			var renamed int
			committed, renamed = compactRenames(repo, rangeArgs, spec, committed, maxSection)
			notes = renameNote(notes, renamed)

			nameArgs := append([]string{"diff"}, diffFlags...)
			nameArgs = append(nameArgs, "--name-only")
			nameArgs = append(nameArgs, rangeArgs...)
			nameArgs = append(nameArgs, spec...)
			committedFiles, err = gitOut(repo, nameArgs...)
			if err != nil {
				return Scope{}, fmt.Errorf("git diff --name-only (committed): %w", err)
			}
		}
	}

	uncommittedArgs := append([]string{"diff"}, diffFlags...)
	uncommittedArgs = append(uncommittedArgs, head)
	uncommittedArgs = append(uncommittedArgs, spec...)
	uncommitted, cut, err := gitPatchOut(repo, maxSection, uncommittedArgs...)
	if err != nil {
		return Scope{}, fmt.Errorf("git diff (uncommitted): %w", err)
	}
	cutSections = append(cutSections, cut...)
	uncommitted, renamedUncommitted := compactRenames(repo, []string{head}, spec, uncommitted, maxSection)
	notes = renameNote(notes, renamedUncommitted)

	uncommittedNameArgs := append([]string{"diff"}, diffFlags...)
	uncommittedNameArgs = append(uncommittedNameArgs, "--name-only", head)
	uncommittedNameArgs = append(uncommittedNameArgs, spec...)
	uncommittedFiles, err := gitOut(repo, uncommittedNameArgs...)
	if err != nil {
		return Scope{}, fmt.Errorf("git diff --name-only (uncommitted): %w", err)
	}

	diff := committed
	if strings.TrimSpace(uncommitted) != "" {
		diff += "\n\n### Uncommitted changes ###\n" + uncommitted
	}

	fileSet := make(map[string]struct{})
	for _, f := range nonEmptyLines(committedFiles) {
		fileSet[f] = struct{}{}
	}
	for _, f := range nonEmptyLines(uncommittedFiles) {
		fileSet[f] = struct{}{}
	}

	// Untracked files — git ls-files --others --exclude-standard. git pathspecs
	// behave subtly differently for ls-files, so filter with our own matcher.
	untrackedOut, err := gitOut(repo, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return Scope{}, fmt.Errorf("git ls-files --others: %w", err)
	}
	// Untracked candidates that survive the pathspec-style exclude filter and
	// the secret denylist. The denylist is the snapshot's: a credential file
	// nobody gitignored must not be sent to a provider any more than it is
	// captured. Unlike DefaultIgnores, it applies even with --include-vendored.
	var untrackedCandidates, denied []string
	for _, u := range nonEmptyLines(untrackedOut) {
		if !untrackedPasses(u, opts.Paths, allExcludes) {
			continue
		}
		if snapshot.Denylisted(u) {
			denied = append(denied, u)
			continue
		}
		untrackedCandidates = append(untrackedCandidates, u)
	}
	if len(denied) > 0 {
		notes = append(notes, fmt.Sprintf("skipped %d untracked file(s) matching the secret denylist: %s", len(denied), listSome(denied, 10)))
	}
	// linguist-generated/-vendored markers apply to untracked paths too (git
	// check-attr resolves .gitattributes for any path), matching how
	// DefaultIgnores suppresses untracked files. The tracked-path excludes
	// above don't cover a not-yet-committed file that a glob marker would
	// catch, so re-check the candidates here. Skipped under IncludeVendored.
	if !opts.IncludeVendored && len(untrackedCandidates) > 0 {
		markedSet := make(map[string]struct{})
		// Untracked files aren't in the index, so attributes always resolve
		// from the working tree (cached=false), regardless of review mode — and
		// therefore no pinned index applies either (nil env). An untracked file
		// is worktree state by definition; there is no tree to pin it to.
		for _, p := range linguistMarked(repo, untrackedCandidates, false, nil) {
			markedSet[p] = struct{}{}
		}
		if len(markedSet) > 0 {
			kept := untrackedCandidates[:0]
			for _, u := range untrackedCandidates {
				if _, skip := markedSet[u]; !skip {
					kept = append(kept, u)
				}
			}
			untrackedCandidates = kept
		}
	}
	var synthBlocks []string
	for _, u := range untrackedCandidates {
		block, wasCut := synthesizeNewFileDiff(repo, u, maxSection)
		if block == "" {
			continue
		}
		if wasCut {
			cutSections = append(cutSections, u)
		}
		synthBlocks = append(synthBlocks, block)
		fileSet[u] = struct{}{}
	}
	notes = truncationNote(notes, cutSections, maxSection)
	if len(synthBlocks) > 0 {
		if strings.TrimSpace(diff) != "" {
			diff += "\n\n### Untracked files (treated as new-file additions) ###\n"
		}
		diff += strings.Join(synthBlocks, "\n")
	}

	files := make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}
	sort.Strings(files)

	return Scope{Diff: diff, Files: files, Base: resolvedBase, Notes: notes}, nil
}

// buildSurveyBranch produces the branch-scope manifest: a diffstat +
// name-status + commit log over mergebase..HEAD, with the same
// excludes/linguist filtering as a diff. It never falls back to HEAD~1 — an
// unresolved base fails loudly rather than silently reviewing one commit
// (roast finding #7; the whole point of survey mode is reviewing the whole
// branch).
func buildSurveyBranch(repo string, opts BuildOpts, spec, allExcludes []string, notes []string) (Scope, error) {
	if err := git(repo, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return Scope{}, fmt.Errorf("branch scope requires a commit at HEAD; repository has no commits")
	}

	base, err := resolveBranchBase(repo, opts.Base)
	if err != nil {
		return Scope{}, err
	}

	if !opts.IncludeWorktree {
		dirty, derr := gitOut(repo, "status", "--porcelain")
		if derr != nil {
			return Scope{}, fmt.Errorf("git status --porcelain: %w", derr)
		}
		if strings.TrimSpace(dirty) != "" {
			return Scope{}, fmt.Errorf("branch scope reviews committed HEAD, but the index or worktree is dirty; commit/stash first, or pass IncludeWorktree to proceed anyway (worktree changes are NOT yet folded into the manifest/git_diff snapshot — see the note this adds)")
		}
	} else {
		// Worktree synthesis into the manifest is not implemented yet (a
		// follow-on, per the survey-mode plan): this only bypasses the
		// dirty-tree guard above. Manifest/Files/OIDs below still describe
		// committed HEAD exclusively — dirty state is silently invisible to
		// them, not reviewed. Do not word this note as "worktree included."
		notes = append(notes, "IncludeWorktree set: dirty-tree guard bypassed, but worktree changes are NOT included anywhere — the manifest, Files, and OIDs below describe committed HEAD only")
	}

	baseOID, err := gitOut(repo, "rev-parse", base)
	if err != nil {
		return Scope{}, fmt.Errorf("git rev-parse %s: %w", base, err)
	}
	baseOID = strings.TrimSpace(baseOID)
	headOID, err := gitOut(repo, "rev-parse", "HEAD")
	if err != nil {
		return Scope{}, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	headOID = strings.TrimSpace(headOID)
	mergeBaseOID, err := gitOut(repo, "merge-base", baseOID, headOID)
	if err != nil {
		return Scope{}, fmt.Errorf("no merge base between %s and HEAD; the histories are unrelated", base)
	}
	mergeBaseOID = strings.TrimSpace(mergeBaseOID)

	diffFlags := append([]string{"--no-renames"}, PatchFlags...)
	// Pin the range to the captured headOID, not the mutable "HEAD" ref: a
	// concurrent commit/checkout between the rev-parse above and these git
	// invocations would otherwise make the manifest describe a different
	// commit than the returned HeadOID, defeating the immutable-snapshot
	// guarantee documented on Scope.
	rangeSpec := mergeBaseOID + ".." + headOID

	statArgs := append([]string{"diff"}, diffFlags...)
	statArgs = append(statArgs, "--stat", rangeSpec)
	statArgs = append(statArgs, spec...)
	stat, err := gitOut(repo, statArgs...)
	if err != nil {
		return Scope{}, fmt.Errorf("git diff --stat: %w", err)
	}

	nameStatusArgs := append([]string{"diff"}, diffFlags...)
	nameStatusArgs = append(nameStatusArgs, "--name-status", rangeSpec)
	nameStatusArgs = append(nameStatusArgs, spec...)
	nameStatus, err := gitOut(repo, nameStatusArgs...)
	if err != nil {
		return Scope{}, fmt.Errorf("git diff --name-status: %w", err)
	}

	logOut, err := gitOut(repo, "log", "--oneline", "--no-color", "--no-show-signature", rangeSpec)
	if err != nil {
		return Scope{}, fmt.Errorf("git log --oneline: %w", err)
	}

	var files []string
	for _, line := range nonEmptyLines(nameStatus) {
		// name-status is "<STATUS>\t<path>" (or "<STATUS>\t<old>\t<new>" for
		// copies, but --no-renames never emits those). Take the last field.
		fields := strings.Split(line, "\t")
		if len(fields) >= 2 {
			path := fields[len(fields)-1]
			// git's default core.quotepath C-quotes paths containing a tab,
			// newline, quote, backslash, or non-ASCII byte (e.g. "café.go"
			// becomes "\"caf\\303\\251.go\""), since --name-status isn't run
			// with -z here. Undo that so Files holds real, openable paths —
			// every other filename-producing query in this package uses -z
			// instead, for the same reason.
			if strings.HasPrefix(path, `"`) {
				decoded, derr := strconv.Unquote(path)
				if derr != nil {
					return Scope{}, fmt.Errorf("decode git name-status path %q: %w", path, derr)
				}
				path = decoded
			}
			files = append(files, path)
		}
	}
	sort.Strings(files)

	var sb strings.Builder
	sb.WriteString("### Diffstat (" + base + ".." + "HEAD) ###\n")
	sb.WriteString(stat)
	sb.WriteString("\n### Changed files (name-status) ###\n")
	sb.WriteString(nameStatus)
	sb.WriteString("\n### Commits (" + base + ".." + "HEAD) ###\n")
	sb.WriteString(logOut)

	if len(allExcludes) > 0 {
		notes = append(notes, fmt.Sprintf("manifest excludes applied (%d pattern(s))", len(allExcludes)))
	}

	return Scope{
		Manifest:     sb.String(),
		Survey:       true,
		Files:        files,
		Base:         base,
		BaseOID:      baseOID,
		MergeBaseOID: mergeBaseOID,
		HeadOID:      headOID,
		Notes:        notes,
	}, nil
}

// buildSurveyRepo produces the repo-scope manifest: a tracked-file listing
// grouped/counted by top-level directory, plus HEAD info. No base; reviews
// the current tree as one snapshot (HeadOID).
//
// The file listing is read from the HeadOID tree itself (`git ls-tree`), not
// the mutable index (`git ls-files`): the latter would disagree with the
// returned HeadOID whenever anything is staged (a staged-but-uncommitted add
// would appear despite not existing at HeadOID; a staged deletion would
// vanish despite still existing there), breaking the immutable-snapshot
// guarantee later phases pin their reads to. -z / splitNUL keep this safe for
// filenames containing spaces or newlines, matching the ls-files -z pattern
// used elsewhere in this package for the same reason. ls-tree doesn't
// support git's :(exclude) pathspec magic, so filtering is done in-process
// with the same glob matcher untracked-file filtering uses.
func buildSurveyRepo(repo string, opts BuildOpts, allExcludes, linguistExcludes []string, notes []string) (Scope, error) {
	// linguistExcludes was computed by the caller from the mutable index and
	// working-tree .gitattributes (linguistIgnores runs uncached for repo
	// scope) — not from HeadOID, since git has no "resolve attributes as of
	// this historical commit" query. Filtering the HeadOID-pinned ls-tree
	// listing through that mutable set would let an unstaged .gitattributes
	// edit change which committed files the manifest reports, contradicting
	// the immutable-snapshot guarantee. Repo scope is spec'd to note dirty
	// state rather than fail (unlike branch scope's dirty guard), so instead
	// of failing here, linguist-attribute filtering is skipped whenever the
	// tree is dirty: DefaultIgnores/opts.Excludes still apply (they're static
	// globs, not attribute-derived, so they can't drift), only the
	// .gitattributes-sourced excludes are dropped. Worst case a file that
	// HeadOID's own .gitattributes would exclude leaks into the manifest —
	// safe direction (more visibility, never a silently-hidden committed
	// file).
	dirty, derr := gitOut(repo, "status", "--porcelain")
	if derr != nil {
		return Scope{}, fmt.Errorf("git status --porcelain: %w", derr)
	}
	if strings.TrimSpace(dirty) != "" {
		if len(linguistExcludes) > 0 {
			notes = append(notes, "working tree is dirty: skipping .gitattributes-based excludes for repo scope (they were resolved from the mutable index/worktree, not HeadOID)")
		}
		linguistExcludes = nil
		notes = append(notes, "working tree has uncommitted changes not reflected in this repo-scope snapshot (pinned to HeadOID)")
	}

	var headOID string
	if err := git(repo, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		out, rerr := gitOut(repo, "rev-parse", "HEAD")
		if rerr != nil {
			return Scope{}, fmt.Errorf("git rev-parse HEAD: %w", rerr)
		}
		headOID = strings.TrimSpace(out)
	} else {
		notes = append(notes, "repository has no commits; manifest is empty")
	}

	var files []string
	if headOID != "" {
		lsOut, err := gitOut(repo, "ls-tree", "-r", "--name-only", "-z", headOID)
		if err != nil {
			return Scope{}, fmt.Errorf("git ls-tree: %w", err)
		}
		// linguistExcludes are exact resolved paths, filtered by set
		// membership rather than through the glob matcher: a file literally
		// named e.g. "weird*name.go" must not have its own name reinterpreted
		// as a wildcard (the vendor-escape class this package guards
		// against — see pathspecArgs' :(literal) handling for diffs).
		literalSet := make(map[string]struct{}, len(linguistExcludes))
		for _, p := range linguistExcludes {
			literalSet[p] = struct{}{}
		}
		for _, f := range splitNUL(lsOut) {
			if _, excluded := literalSet[f]; excluded {
				continue
			}
			if untrackedPasses(f, opts.Paths, allExcludes) {
				files = append(files, f)
			}
		}
	}
	sort.Strings(files)

	var headLine string
	if headOID != "" {
		out, lerr := gitOut(repo, "log", "--oneline", "--no-color", "--no-show-signature", "-1")
		if lerr == nil {
			headLine = strings.TrimSpace(out)
		}
	}

	counts := make(map[string]int)
	var dirOrder []string
	for _, f := range files {
		top := f
		if idx := strings.IndexByte(f, '/'); idx >= 0 {
			top = f[:idx]
		}
		if _, ok := counts[top]; !ok {
			dirOrder = append(dirOrder, top)
		}
		counts[top]++
	}
	sort.Strings(dirOrder)

	// Every name and the commit subject below are repository content, and
	// the finder reads this inside <manifest> tags. A raw path holding a
	// newline could end the tag and append text that reads as instructions,
	// so any name that could is written Go-quoted instead.
	quoted := false
	line := func(s string) string {
		q, changed := manifestQuote(s)
		quoted = quoted || changed
		return q
	}
	var sb strings.Builder
	sb.WriteString("### Repository snapshot ###\n")
	if headLine != "" {
		sb.WriteString("HEAD: " + line(headLine) + "\n")
	} else {
		sb.WriteString("HEAD: (no commits)\n")
	}
	_, _ = fmt.Fprintf(&sb, "%d tracked file(s) across %d top-level director(ies)\n\n", len(files), len(dirOrder))
	sb.WriteString("### Files by top-level directory ###\n")
	for _, d := range dirOrder {
		_, _ = fmt.Fprintf(&sb, "%s (%d)\n", line(d), counts[d])
	}
	sb.WriteString("\n### Tracked files ###\n")
	for _, f := range files {
		sb.WriteString(line(f))
		sb.WriteByte('\n')
	}
	if quoted {
		sb.WriteString("\n(Names in double quotes contain control or markup characters and are Go-escaped; unescape one before passing it to a tool.)\n")
	}

	return Scope{
		Manifest: sb.String(),
		Survey:   true,
		Files:    files,
		HeadOID:  headOID,
		Notes:    notes,
	}, nil
}

// manifestQuote returns s unchanged when it is safe on one manifest line, or
// Go-quoted with '<' and '>' escaped when it holds anything unicode.IsPrint
// rejects (controls, line separators, invisible format characters), invalid
// UTF-8, a quote or backslash (which would make quoting ambiguous), or angle
// brackets (which could spell the manifest's closing tag).
func manifestQuote(s string) (string, bool) {
	safe := utf8.ValidString(s) && !strings.ContainsAny(s, "\"\\<>")
	for _, r := range s {
		if !safe {
			break
		}
		safe = unicode.IsPrint(r)
	}
	if safe {
		return s, false
	}
	return strings.NewReplacer("<", `\x3c`, ">", `\x3e`).Replace(strconv.Quote(s)), true
}

// resolveBranchBase resolves the branch-scope base ref: an explicit request,
// else the remote default branch. Deliberately does NOT fall through to
// BaseCandidates' HEAD~1 tail — reviewing "the whole branch" must never
// silently narrow to one commit (roast finding #7). Returns an error, never
// an empty string, so callers can't mistake "unresolved" for "no base".
func resolveBranchBase(repo, requested string) (string, error) {
	if requested != "" {
		if err := git(repo, "rev-parse", "--verify", "--quiet", requested); err != nil {
			return "", fmt.Errorf("base ref %q does not resolve in this repository", requested)
		}
		return requested, nil
	}

	if out, err := gitOut(repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		ref := strings.TrimSpace(out)
		if ref != "" {
			if verr := git(repo, "rev-parse", "--verify", "--quiet", ref); verr == nil {
				return ref, nil
			}
		}
	}

	for _, candidate := range []string{"origin/main", "origin/master"} {
		if err := git(repo, "rev-parse", "--verify", "--quiet", candidate); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("branch scope could not resolve a base ref: no --base given, refs/remotes/origin/HEAD is not set, and neither origin/main nor origin/master exist; pass --base explicitly")
}

// SizeEntry is one file's approximate contribution to the diff, used by Fit.
type SizeEntry struct {
	Path  string
	Bytes int
}

// Fit truncates the diff to maxBytes. Returns per-file byte breakdown
// (largest first) when truncation occurred, so the operator knows what to
// --exclude next time.
func Fit(s *Scope, maxBytes int) []SizeEntry {
	if len(s.Diff) <= maxBytes {
		return nil
	}
	breakdown := make([]SizeEntry, 0, len(s.Files))
	for _, f := range s.Files {
		breakdown = append(breakdown, SizeEntry{Path: f, Bytes: fileDiffBytes(s, f)})
	}
	sort.Slice(breakdown, func(i, j int) bool {
		return breakdown[i].Bytes > breakdown[j].Bytes
	})
	s.Diff = s.Diff[:maxBytes] + "\n... (diff truncated)\n"
	s.Notes = append(s.Notes, fmt.Sprintf("diff truncated to %d bytes", maxBytes))
	return breakdown
}

// GitRoot returns the root of the git repository containing start.
func GitRoot(start string) (string, error) {
	out, err := exec.Command("git", "-C", start, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not inside a git repository")
	}
	return strings.TrimSpace(string(out)), nil
}

// --- internal helpers -------------------------------------------------------

func git(repo string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func gitOut(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// linguistAttrs are the .gitattributes markers roast honors as "not authored
// source". These are GitHub Linguist's standard overrides; a repo setting them
// already gets diff-collapse and language-stat exclusion from GitHub.
var linguistAttrs = []string{"linguist-generated", "linguist-vendored"}

// linguistIgnores returns the tracked paths a repo has marked
// linguist-generated or linguist-vendored in .gitattributes, as literal
// pathspec strings to exclude from the diff. Best-effort: any git error yields
// no excludes (the review proceeds unfiltered rather than failing).
//
// cached selects the attribute source: true reads .gitattributes from the
// index (staged-only review, so an unstaged attribute edit can't change the
// gate's scope); false reads the working tree (every other mode).
//
// treeOID, when set (cached mode only), pins every one of those index reads to
// that tree instead of the LIVE index. Without it this function is the hole in
// "one review sees one tree" (roast R-65db13): the diff and every content
// reader resolve against the pinned tree while the exclusion set that decides
// WHICH PATHS ENTER THE REVIEW AT ALL is read from an index another process can
// still write. A `.gitattributes` staged after the pin would then mark files
// that exist in the pinned tree as generated/vendored and silently drop them —
// a scope-narrowing race, which is the worse direction: the review shrinks and
// nothing says so.
//
// Pinning works by materializing the tree into a private index
// (`GIT_INDEX_FILE=<tmp> git read-tree <OID>`) and running ls-files/diff/
// check-attr against that. Verified: `check-attr --cached` over a temp index
// holding a tree WITHOUT .gitattributes reports `unspecified` for a path the
// live index reports `set` for. check-attr has no tree-ish argument of its own
// (git 2.39), so the temp index is the mechanism, not a workaround.
func linguistIgnores(repo string, cached bool, treeOID string) []string {
	env, cleanup, err := pinnedIndexEnv(repo, cached, treeOID)
	if err != nil {
		// Best-effort, like every other failure here — but NOT by falling back
		// to the live index: that is exactly the read this pin exists to
		// prevent, and a wrong exclusion set silently changes what is
		// reviewed. No excludes means the review proceeds UNFILTERED, which
		// errs toward reviewing too much rather than too little.
		return nil
	}
	defer cleanup()

	// ls-files -z: tracked paths, NUL-delimited so spaces/newlines survive.
	filesOut, err := gitEnvOut(repo, env, "ls-files", "-z")
	if err != nil {
		return nil
	}
	paths := splitNUL(filesOut)
	// A staged deletion removes the path from the index, so ls-files no longer
	// lists it — but `git diff --cached` still shows the deletion, which would
	// re-enter a staged-only review uncaught. Union the staged diff's paths so
	// a deleted generated/vendored file's removal-diff is still excluded. Only
	// needed in cached mode (staged-only); best-effort, and check-attr --cached
	// still resolves the index attributes for a path no longer on disk.
	if cached {
		if changed, cerr := gitEnvOut(repo, env, append(append([]string{"diff"}, PatchFlags...), "--cached", "--name-only", "-z")...); cerr == nil {
			paths = append(paths, splitNUL(changed)...)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	marked := linguistMarked(repo, paths, cached, env)
	sort.Strings(marked)
	return marked
}

// pinnedIndexEnv returns the environment additions that point git at a private
// index holding treeOID, plus a cleanup. When treeOID is empty (or this is not
// a cached read) it returns no additions and a no-op cleanup, so every
// non-staged caller keeps reading exactly what it read before.
func pinnedIndexEnv(repo string, cached bool, treeOID string) ([]string, func(), error) {
	noop := func() {}
	if !cached || treeOID == "" {
		return nil, noop, nil
	}
	tmp, err := os.MkdirTemp("", "roast-scope-idx-")
	if err != nil {
		return nil, noop, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	idx := filepath.Join(tmp, "index")
	env := []string{"GIT_INDEX_FILE=" + idx}
	if _, err := gitEnvOut(repo, env, "read-tree", treeOID); err != nil {
		cleanup()
		return nil, noop, err
	}
	return env, cleanup, nil
}

// gitEnvOut is gitOut with extra environment appended, so a caller-supplied
// GIT_INDEX_FILE wins over an inherited one.
func gitEnvOut(repo string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// linguistMarked returns the subset of paths that carry a linguist-generated
// or linguist-vendored attribute set to "set" (bare form) or "true". The
// explicit "=false" and "unspecified" values do not count, so a repo can
// un-mark a subtree with `foo/bar linguist-generated=false`. Empty input or
// any git error yields nil (best-effort). check-attr applies .gitattributes
// rules to any path, tracked or not, so this works for untracked candidates
// too. cached passes --cached so attributes resolve from the index; see
// linguistIgnores.
func linguistMarked(repo string, paths []string, cached bool, env []string) []string {
	if len(paths) == 0 {
		return nil
	}
	// Only repository-controlled attributes may narrow review scope. git
	// check-attr otherwise also honors the user's global attributes file
	// (core.attributesFile) and the system attributes file — machine-level
	// config that is not part of the repo and not PR-reviewable, so a stray
	// `* linguist-generated` there must not blank a review. -c
	// core.attributesFile=/dev/null drops the global source; GIT_ATTR_NOSYSTEM
	// drops the system source. NOT closed:
	// $GIT_DIR/info/attributes, which git always reads with no override flag;
	// it is local and untracked, but writing it already requires the same
	// checkout access as .git/hooks or .git/config, so it grants no new reach.
	args := []string{"-c", "core.attributesFile=" + os.DevNull, "check-attr", "--stdin", "-z"}
	if cached {
		args = append(args, "--cached")
	}
	args = append(args, linguistAttrs...)
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	// env carries the pinned GIT_INDEX_FILE in staged scope (see
	// pinnedIndexEnv), so --cached resolves .gitattributes from the tree the
	// review is actually reading rather than from whatever the live index holds
	// by the time this runs. Appended last so it wins.
	cmd.Env = append(append(os.Environ(), "GIT_ATTR_NOSYSTEM=1"), env...)
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	// check-attr -z emits NUL-separated (path, attr, value) triples. A short
	// trailing group (truncated output) is ignored by the i+2 bound.
	fields := splitNUL(string(out))
	seen := make(map[string]struct{})
	var marked []string
	for i := 0; i+2 < len(fields); i += 3 {
		path, value := fields[i], fields[i+2]
		if value != "set" && value != "true" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		marked = append(marked, path)
	}
	return marked
}

// splitNUL splits NUL-delimited git output, dropping the trailing empty field
// after the final NUL.
func splitNUL(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\x00") //nolint:stringsseq // indexed access needed downstream
	// Drop a single trailing empty element from the final delimiter.
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// pathspecArgs builds the `-- <paths> :(exclude)<glob>...` argument tail.
// excludes are glob patterns (default/user excludes); literalExcludes are
// resolved file paths emitted with :(literal) magic so pathspec metacharacters
// in a filename can't widen the exclusion to unrelated files.
func pathspecArgs(paths []string, excludes []string, literalExcludes []string) []string {
	if len(paths) == 0 && len(excludes) == 0 && len(literalExcludes) == 0 {
		return nil
	}
	args := []string{"--"}
	args = append(args, paths...)
	for _, pat := range excludes {
		args = append(args, ":(exclude)"+pat)
	}
	for _, p := range literalExcludes {
		args = append(args, ":(exclude,literal)"+p)
	}
	return args
}

// globMatch replicates Python fnmatch behaviour where * crosses directory
// separators. filepath.Match does not do this, so we compile the glob to a
// regexp. We try the pattern as-is, then again with ** collapsed to *, which
// mirrors the Python fallback.
func globMatch(path, pattern string) bool {
	if matchGlob(path, pattern) {
		return true
	}
	alt := strings.ReplaceAll(pattern, "**", "*")
	if alt != pattern {
		return matchGlob(path, alt)
	}
	return false
}

// matchGlob compiles a glob to a regexp and tests path against it.
func matchGlob(path, pattern string) bool {
	re, err := globToRegexp(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

// globToRegexp converts a glob pattern to a full-match regexp. Both * and **
// match across path separators (Python fnmatch semantics).
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteByte('^')
	i := 0
	for i < len(pattern) {
		ch := pattern[i]
		switch ch {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				sb.WriteString(`.*`)
				i += 2
				// consume optional trailing slash after **
				if i < len(pattern) && pattern[i] == '/' {
					sb.WriteString(`/?`)
					i++
				}
			} else {
				// Python fnmatch: single * also crosses /
				sb.WriteString(`.*`)
				i++
			}
		case '?':
			sb.WriteByte('.')
			i++
		case '.', '+', '^', '$', '{', '}', '(', ')', '[', ']', '|', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(ch)
			i++
		default:
			sb.WriteByte(ch)
			i++
		}
	}
	sb.WriteByte('$')
	return regexp.Compile(sb.String())
}

func untrackedPasses(path string, paths []string, excludes []string) bool {
	for _, pat := range excludes {
		if globMatch(path, pat) {
			return false
		}
	}
	if len(paths) == 0 {
		return true
	}
	for _, pat := range paths {
		if globMatch(path, pat) {
			return true
		}
		// git treats a bare directory pathspec (e.g. "internal") as selecting
		// that directory's whole subtree, and diff scope gets this for free
		// by handing Paths straight to git. This in-process matcher backs
		// repo-scope file listing and untracked-file filtering, neither of
		// which goes through git's pathspec engine, so it must replicate that
		// directory-recursion semantic itself or the same --path value
		// silently selects nothing under survey scope. Only a literal
		// (glob-metacharacter-free) pathspec gets this treatment — a real
		// glob like "internal/*" already means what it says.
		dir := strings.TrimSuffix(pat, "/")
		if dir != "" && !strings.ContainsAny(pat, "*?[") && strings.HasPrefix(path, dir+"/") {
			return true
		}
	}
	return false
}

// synthesizeNewFileDiff renders an untracked file as a new-file diff section
// of at most about maxSection bytes. Content beyond that is never read; cut
// reports that the section ends in sectionOmittedMarker instead.
func synthesizeNewFileDiff(repo, relpath string, maxSection int) (block string, cut bool) {
	full := filepath.Join(repo, relpath)
	// Lstat + IsRegular: an untracked symlink (leak -> /etc/passwd) must
	// not pull its target's contents into the review prompt — os.ReadFile
	// follows symlinks, and confinement of the link path says nothing
	// about the target.
	info, err := os.Lstat(full)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	f, err := os.Open(full)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, int64(maxSection)+1))
	if err != nil {
		return "", false
	}
	cut = len(data) > maxSection
	lines := strings.Split(string(data), "\n")
	// splitLines removes a trailing empty element from a trailing newline
	// to match Python's str.splitlines() behaviour.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	header := fmt.Sprintf("diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n", relpath, relpath, relpath)
	var body strings.Builder
	n := 0
	for _, line := range lines {
		if len(header)+body.Len()+len(line)+2 > maxSection {
			cut = true
			break
		}
		body.WriteByte('+')
		body.WriteString(line)
		body.WriteByte('\n')
		n++
	}
	var sb strings.Builder
	sb.WriteString(header)
	_, _ = fmt.Fprintf(&sb, "@@ -0,0 +1,%d @@\n", n)
	sb.WriteString(body.String())
	if cut {
		_, _ = fmt.Fprintf(&sb, sectionOmittedMarker, maxSection)
	}
	return sb.String(), cut
}

func resolveBase(repo, requested string) (string, error) {
	candidates := BaseCandidates
	if requested != "" {
		candidates = []string{requested}
	}
	for _, ref := range candidates {
		err := git(repo, "rev-parse", "--verify", "--quiet", ref)
		if err == nil {
			return ref, nil
		}
	}
	return "", nil
}

// buildRangeArgs returns the correct diff range arguments. Use three-dot
// (base...HEAD) when a merge base exists so only this branch's changes show;
// fall back to two-ref form for unrelated histories or tree-only refs like
// the empty tree.
func buildRangeArgs(repo, base string) []string {
	err := exec.Command("git", "-C", repo, "merge-base", base, "HEAD").Run()
	if err == nil {
		return []string{base + "...HEAD"}
	}
	return []string{base, "HEAD"}
}

// fileDiffBytes approximates a file's contribution by scanning the
// already-built diff for its blocks — avoids re-running git per file.
func fileDiffBytes(s *Scope, relpath string) int {
	marker := fmt.Sprintf("diff --git a/%s b/%s", relpath, relpath)
	total := 0
	pos := 0
	for {
		start := strings.Index(s.Diff[pos:], marker)
		if start == -1 {
			break
		}
		start += pos
		next := strings.Index(s.Diff[start+1:], "\ndiff --git ")
		var end int
		if next == -1 {
			end = len(s.Diff)
		} else {
			end = start + 1 + next
		}
		total += end - start
		pos = end
	}
	// A collapsed move's header names two different paths, so the marker
	// above never matches it; count it for both of them.
	for _, sec := range splitSections(s.Diff) {
		if from, to, ok := renameOf(sec.text); ok && (from == relpath || to == relpath) {
			total += len(sec.text)
		}
	}
	return total
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
