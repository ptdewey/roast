package scope

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRevisionPinsSelectedCommit(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "code.go", "base\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	writeFile(t, repo, "code.go", "selected\n")
	writeFile(t, repo, "generated.go", "generated\n")
	writeFile(t, repo, "deleted.gen", "generated\n")
	writeFile(t, repo, ".gitattributes", "generated.go linguist-generated\n*.gen linguist-generated\n")
	writeFile(t, repo, "vendor/hidden.go", "vendored\n")
	writeFile(t, repo, "excluded.go", "excluded\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "selected")
	selected := gitRun(t, repo, "rev-parse", "HEAD")
	tree := gitRun(t, repo, "rev-parse", "HEAD^{tree}")
	writeFile(t, repo, "code.go", "newer\n")
	writeFile(t, repo, ".gitattributes", "* linguist-generated\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "newer")
	writeFile(t, repo, "code.go", "dirty\n")
	writeFile(t, repo, "untracked.go", "untracked\n")
	sc, err := Build(repo, BuildOpts{Revision: selected, Excludes: []string{"excluded.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if sc.HeadOID != selected || sc.BaseOID != base || sc.Base != base || sc.RevisionTreeOID != tree {
		t.Fatalf("unpinned identity: %+v", sc)
	}
	if !slices.Equal(sc.Files, []string{".gitattributes", "code.go"}) {
		t.Fatalf("files: %v", sc.Files)
	}
	if !strings.Contains(sc.Diff, "+selected") || strings.Contains(sc.Diff, "+dirty") || strings.Contains(sc.Diff, "+newer") {
		t.Fatalf("wrong diff: %s", sc.Diff)
	}
	// Deletion attributes must also come from the selected target, not HEAD.
	gitAdd(t, repo, "code.go")
	gitCommit(t, repo, "save dirty test fixture")
	gitRun(t, repo, "checkout", "--detach", selected)
	gitRun(t, repo, "rm", "deleted.gen")
	gitCommit(t, repo, "delete generated")
	sc, err = Build(repo, BuildOpts{Revision: "HEAD"})
	if err != nil || slices.Contains(sc.Files, "deleted.gen") {
		t.Fatalf("deleted generated file leaked: %+v, %v", sc, err)
	}
}

func TestRevisionRootAndFailures(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "root.go", "root\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "root")
	sc, err := Build(repo, BuildOpts{Revision: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if sc.BaseOID != EmptyTree || !strings.Contains(sc.Diff, "+root") {
		t.Fatalf("root: %+v", sc)
	}
	for _, rev := range []string{"missing", "HEAD..HEAD", "--all", "HEAD^{tree}"} {
		if _, err := Build(repo, BuildOpts{Revision: rev}); err == nil {
			t.Errorf("accepted %q", rev)
		}
	}
	for _, opts := range []BuildOpts{{Revision: "HEAD", Base: "HEAD"}, {Revision: "HEAD", StagedOnly: true}, {Revision: "HEAD", UncommittedOnly: true}, {Revision: "HEAD", Scope: "repo"}, {Revision: "HEAD", Scope: "branch"}} {
		if _, err := Build(repo, opts); err == nil {
			t.Errorf("accepted %+v", opts)
		}
	}
	empty := initRepo(t)
	writeFile(t, empty, "untracked.go", "must not review\n")
	if _, err := Build(empty, BuildOpts{Revision: "HEAD"}); err == nil {
		t.Fatal("fell back to unborn worktree")
	}
}

func TestRevisionMergeFirstParent(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "base.go", "base\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "base")
	gitRun(t, repo, "branch", "side")
	writeFile(t, repo, "main.go", "main\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "main")
	first := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "checkout", "side")
	writeFile(t, repo, "side.go", "side\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "side")
	gitRun(t, repo, "checkout", "--detach", first)
	gitRun(t, repo, "merge", "--no-ff", "side", "-m", "merge")
	sc, err := Build(repo, BuildOpts{Revision: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if sc.BaseOID != first || !slices.Equal(sc.Files, []string{"side.go"}) || !strings.Contains(strings.Join(sc.Notes, " "), "first parent") {
		t.Fatalf("merge: %+v", sc)
	}
}

func jjRun(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("jj", append([]string{"--no-pager", "--color=never"}, args...)...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "JJ_CONFIG="+filepath.Join(t.TempDir(), "config.toml"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jj %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestJJRevision(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj unavailable")
	}
	repo := initRepo(t)
	writeFile(t, repo, "base.go", "base\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "base")
	jjRun(t, repo, "git", "init", "--colocate")
	writeFile(t, repo, "selected.go", "selected\n")
	sc, err := Build(repo, BuildOpts{Revision: "@"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sc.Files, []string{"selected.go"}) || !strings.Contains(sc.Diff, "+selected") {
		t.Fatalf("dirty @ not snapshotted: %+v", sc)
	}
	target := sc.HeadOID
	jjRun(t, repo, "new")
	writeFile(t, repo, "newer.go", "newer\n")
	sc, err = Build(repo, BuildOpts{Revision: target})
	if err != nil || !slices.Equal(sc.Files, []string{"selected.go"}) {
		t.Fatalf("selected history: %+v %v", sc, err)
	}
	for _, rev := range []string{"all()", "none()", "missing_bookmark"} {
		if _, err := Build(repo, BuildOpts{Revision: rev}); err == nil {
			t.Errorf("accepted jj %q", rev)
		}
	}
	nested := filepath.Join(repo, "nested")
	if err := os.Mkdir(nested, 0755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, nested, "init")
	gitRun(t, nested, "config", "user.name", "Test")
	gitRun(t, nested, "config", "user.email", "test@example.com")
	writeFile(t, nested, "nested.go", "nested\n")
	gitAdd(t, nested, ".")
	gitCommit(t, nested, "nested")
	sc, err = Build(nested, BuildOpts{Revision: "HEAD"})
	if err != nil || !slices.Equal(sc.Files, []string{"nested.go"}) {
		t.Fatalf("outer jj overrode nested Git: %+v %v", sc, err)
	}
}

func TestJJRevisionRejectsNoncolocatedAndMissingCommand(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj unavailable")
	}
	outer := initRepo(t)
	nested := filepath.Join(outer, "jj-only")
	if err := os.Mkdir(nested, 0755); err != nil {
		t.Fatal(err)
	}
	jjRun(t, nested, "git", "init", "--no-colocate")
	if _, err := RepositoryForRevision(nested); err == nil || !strings.Contains(err.Error(), "colocated") {
		t.Fatalf("noncolocated must not discover outer Git: %v", err)
	}
	if _, err := Build(nested, BuildOpts{Revision: "@"}); err == nil || !strings.Contains(err.Error(), "colocated") {
		t.Fatalf("noncolocated build: %v", err)
	}
	repo := initRepo(t)
	writeFile(t, repo, "base.go", "base\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "base")
	jjRun(t, repo, "git", "init", "--colocate")
	t.Setenv("PATH", t.TempDir())
	if _, err := Build(repo, BuildOpts{Revision: "HEAD"}); err == nil || !strings.Contains(err.Error(), "jj") {
		t.Fatalf("missing jj must not fall back: %v", err)
	}
}

func TestJJRevisionConflictedTarget(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj unavailable")
	}
	repo := initRepo(t)
	writeFile(t, repo, "code.go", "base\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "base")
	jjRun(t, repo, "git", "init", "--colocate")
	writeFile(t, repo, "code.go", "left\n")
	left, err := Build(repo, BuildOpts{Revision: "@"})
	if err != nil {
		t.Fatal(err)
	}
	jjRun(t, repo, "new", "@-")
	writeFile(t, repo, "code.go", "right\n")
	right, err := Build(repo, BuildOpts{Revision: "@"})
	if err != nil {
		t.Fatal(err)
	}
	jjRun(t, repo, "new", left.HeadOID, right.HeadOID)
	if _, err := Build(repo, BuildOpts{Revision: "@"}); err == nil || !strings.Contains(err.Error(), "conflicted") {
		t.Fatalf("conflict must fail closed: %v", err)
	}
}

func TestRevisionCompactionTruncationAndPathspec(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "old.go", strings.Repeat("unchanged line\n", 100))
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "base")
	gitRun(t, repo, "mv", "old.go", "new.go")
	writeFile(t, repo, "large.go", strings.Repeat("new line\n", 1000))
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "target")
	sc, err := Build(repo, BuildOpts{Revision: "HEAD", MaxFileDiffBytes: 200})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sc.Diff, "rename from old.go") || !strings.Contains(sc.Diff, "roast: the rest") {
		t.Fatalf("lost compaction/truncation: %s", sc.Diff)
	}
	sc, err = Build(repo, BuildOpts{Revision: "HEAD", Paths: []string{"new.go"}})
	if err != nil || !slices.Equal(sc.Files, []string{"new.go"}) || strings.Contains(sc.Diff, "rename from") {
		t.Fatalf("pathspec escape: %+v %v", sc, err)
	}
}

func TestRevisionRejectsAmbiguousGitRef(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "code.go", "base\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "base")
	gitRun(t, repo, "branch", "ambiguous")
	writeFile(t, repo, "code.go", "target\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "target")
	gitRun(t, repo, "tag", "ambiguous")
	gitRun(t, repo, "config", "core.warnAmbiguousRefs", "false")
	if _, err := Build(repo, BuildOpts{Revision: "ambiguous"}); err == nil {
		t.Fatal("accepted ambiguous Git branch/tag")
	}
	if _, err := Build(repo, BuildOpts{Revision: "refs/tags/ambiguous"}); err != nil {
		t.Fatalf("explicit tag: %v", err)
	}
}

func TestRevisionPinsPatchAttributes(t *testing.T) {
	for _, targetBinary := range []bool{false, true} {
		t.Run(map[bool]string{false: "historical-text", true: "historical-binary"}[targetBinary], func(t *testing.T) {
			repo := initRepo(t)
			attrs := "*.go diff\n"
			if targetBinary {
				attrs = "*.go -diff\n"
			}
			writeFile(t, repo, ".gitattributes", attrs)
			writeFile(t, repo, "code.go", "base\n")
			gitAdd(t, repo, ".")
			gitCommit(t, repo, "base")
			writeFile(t, repo, "code.go", "selected\n")
			gitAdd(t, repo, ".")
			gitCommit(t, repo, "selected")
			selected := gitRun(t, repo, "rev-parse", "HEAD")
			liveAttrs := "*.go -diff\n"
			if targetBinary {
				liveAttrs = "*.go diff\n"
			}
			writeFile(t, repo, ".gitattributes", liveAttrs)
			gitAdd(t, repo, ".")
			gitCommit(t, repo, "newer attrs")
			sc, err := Build(repo, BuildOpts{Revision: selected})
			if err != nil {
				t.Fatal(err)
			}
			if targetBinary != strings.Contains(sc.Diff, "Binary files") {
				t.Fatalf("wrong target attribute rendering: %s", sc.Diff)
			}
			if !targetBinary && !strings.Contains(sc.Diff, "+selected") {
				t.Fatalf("live attrs hid historical patch: %s", sc.Diff)
			}
		})
	}
}

func TestRevisionSHA256Root(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "--object-format=sha256").CombinedOutput(); err != nil {
		t.Skipf("SHA256 Git unavailable: %s", out)
	}
	gitRun(t, repo, "config", "user.name", "Test")
	gitRun(t, repo, "config", "user.email", "test@example.com")
	writeFile(t, repo, "root.go", "root\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "root")
	sc, err := Build(repo, BuildOpts{Revision: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.BaseOID) != 64 || sc.Base != sc.BaseOID || !strings.Contains(sc.Diff, "+root") {
		t.Fatalf("SHA256 root: %+v", sc)
	}
}

func TestRevisionPreservesCallerIndex(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "code.go", "base\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "base")
	index := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INDEX_FILE", index)
	if _, err := Build(repo, BuildOpts{Revision: "HEAD"}); err == nil || !strings.Contains(err.Error(), "GIT_INDEX_FILE") {
		t.Fatalf("expected actionable rejection: %v", err)
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("caller index mutated")
	}
}

func TestRevisionRejectsGitRoutingOverrides(t *testing.T) {
	for _, name := range []string{"GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, filepath.Join(t.TempDir(), "must-not-create"))
			if _, err := Build(t.TempDir(), BuildOpts{Revision: "HEAD"}); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("routing override not rejected: %v", err)
			}
			if _, err := os.Stat(os.Getenv(name)); !os.IsNotExist(err) {
				t.Fatalf("routing destination touched: %v", err)
			}
		})
	}
}
