package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/testutil"
)

func revisionEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	t.Setenv("ROAST_SNAPSHOT", "1")
	t.Setenv("ROAST_INTERACTIVE_MIN_DIFF_LINES", "0")
	t.Setenv("ROAST_INTERACTIVE_MIN_FILES", "0")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("JJ_CONFIG", t.TempDir())
}

func revisionCommand(t *testing.T, repo, program string, args ...string) string {
	t.Helper()
	cmd := exec.Command(program, args...)
	cmd.Dir = repo
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", program, args, err, data)
	}
	return strings.TrimSpace(string(data))
}

func revisionWrite(t *testing.T, repo, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func revisionConfig(t *testing.T, endpoint string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := fmt.Sprintf(`version: 1
inference_profiles:
  finder-test:
    endpoint: %s/v1
    api_key: local-test-only
    model: finder-test
    protocol: chat
    max_output_tokens: 2048
  verifier-test:
    endpoint: %s/v1
    api_key: local-test-only
    model: verifier-test
    protocol: chat
    max_output_tokens: 2048
roles:
  finder:
    profile: finder-test
  verifier:
    profile: verifier-test
`, endpoint, endpoint)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Exercise the production CLI, provider tool loops, record writer and snapshot
// replay, not just scope.Build. Live HEAD and dirty files must never leak in.
func TestRevisionPinnedReviewAndCheckout(t *testing.T) {
	for _, tc := range []struct {
		name     string
		root, jj bool
		sha256   bool
	}{
		{name: "git older revision"},
		{name: "git root revision", root: true},
		{name: "git SHA256 root revision", root: true, sha256: true},
		{name: "jj described change with empty current change", jj: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.jj {
				if _, err := exec.LookPath("jj"); err != nil {
					t.Skip("jj is not installed")
				}
			}
			revisionEnvironment(t)
			repo := t.TempDir()
			git := func(args ...string) string { return revisionCommand(t, repo, "git", args...) }
			if tc.sha256 {
				git("init", "--object-format=sha256")
			} else {
				git("init")
			}
			git("config", "user.email", "test@example.com")
			git("config", "user.name", "Test")
			if !tc.root {
				revisionWrite(t, repo, "bad.go", "package example\n\nfunc bad() {}\n")
				git("add", ".")
				git("commit", "-m", "base")
			}
			const targetBody = "package example\n\nfunc bad() { panic(\"REVISION_TARGET\") }\n"
			revisionWrite(t, repo, "bad.go", targetBody)
			revisionWrite(t, repo, "target-only.txt", "target tree companion\n")
			git("add", ".")
			git("commit", "-m", "requested described revision")
			target := git("rev-parse", "HEAD")
			targetTree := git("rev-parse", "HEAD^{tree}")
			base := git("hash-object", "-t", "tree", "--stdin")
			if !tc.root {
				base = git("rev-parse", "HEAD^")
			}
			revisionWrite(t, repo, "bad.go", "package example\n\nfunc bad() { panic(\"NEWER_HEAD_ONLY\") }\n")
			revisionWrite(t, repo, "newer-only.txt", "NEWER_HEAD_ONLY\n")
			git("add", ".")
			git("commit", "-m", "newer HEAD")
			liveHead := git("rev-parse", "HEAD")
			revision := target
			if tc.jj {
				revisionCommand(t, repo, "jj", "git", "init", "--colocate")
				revision = revisionCommand(t, repo, "jj", "log", "--no-graph", "-r", target, "-T", "change_id")
				if revision == "" || revision == target {
					t.Fatalf("expected a jj change ID, got %q", revision)
				}
				if got := revisionCommand(t, repo, "jj", "log", "--no-graph", "-r", "@", "-T", "description"); got != "" {
					t.Fatalf("current change is not unnamed: %q", got)
				}
				if got := revisionCommand(t, repo, "jj", "diff", "--summary"); got != "" {
					t.Fatalf("current change is not empty: %q", got)
				}
			} else {
				revisionWrite(t, repo, "bad.go", "package example\n\nfunc bad() { panic(\"DIRTY_WORKTREE_ONLY\") }\n")
				revisionWrite(t, repo, "untracked-only.txt", "UNTRACKED_ONLY\n")
			}
			beforeStatus := git("status", "--porcelain")
			beforeIndex := git("write-tree")
			var mu sync.Mutex
			prompts := map[string][]string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				var request struct {
					Model    string          `json:"model"`
					Messages json.RawMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				mu.Lock()
				prompts[request.Model] = append(prompts[request.Model], string(request.Messages))
				call := len(prompts[request.Model])
				mu.Unlock()
				name, arguments := "read_file", `{"path":"bad.go","offset":1,"limit":20}`
				if call > 1 {
					switch request.Model {
					case "finder-test":
						name, arguments = "submit_findings", `{"assessment":"FINDINGS_PRESENT","findings":[{"reasoning":"panic is unconditional","severity":"HIGH","confidence":0.99,"cwe":"CWE-248","description":"Unconditional panic","file":"bad.go","lines":[3],"code_quote":"panic(\"REVISION_TARGET\")","fixed_code":"return","frontend_category":""}]}`
					case "verifier-test":
						name, arguments = "submit_verdict", `{"reasoning":"unconditional panic in requested revision","status":"confirmed","confidence":0.99,"grounding":"bad.go line 3 panics","recalibrated_severity":""}`
					default:
						http.Error(w, "unexpected model", 400)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = testutil.WriteJSONStream(w, "chat", fmt.Sprintf(`{"id":"test","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-%d","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, request.Model, call, name, arguments))
			}))
			defer server.Close()
			configPath := revisionConfig(t, server.URL)
			t.Chdir(repo)
			var stdout, stderr bytes.Buffer
			code, err := ExecuteContext(t.Context(), []string{"review", "--config", configPath, "--revision", revision, "--format", "json", "--interactive", "--max-severity", "HIGH", "--deadline=0", "--request-timeout=0", "--max-attempts=1"}, &stdout, &stderr)
			if err != nil || code != ExitFindings {
				t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, err, &stdout, &stderr)
			}
			mu.Lock()
			captured := prompts
			mu.Unlock()
			for _, model := range []string{"finder-test", "verifier-test"} {
				turns := captured[model]
				if len(turns) != 2 {
					t.Fatalf("%s turns=%d, want read_file then result", model, len(turns))
				}
				for i, prompt := range turns {
					if !strings.Contains(prompt, "REVISION_TARGET") {
						t.Errorf("%s turn %d missing target: %s", model, i, prompt)
					}
					for _, forbidden := range []string{"NEWER_HEAD_ONLY", "DIRTY_WORKTREE_ONLY", "UNTRACKED_ONLY", "newer-only.txt", "untracked-only.txt"} {
						if strings.Contains(prompt, forbidden) {
							t.Errorf("%s turn %d leaked %q: %s", model, i, forbidden, prompt)
						}
					}
				}
				var messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				}
				if err := json.Unmarshal([]byte(turns[1]), &messages); err != nil {
					t.Fatal(err)
				}
				foundTool := false
				for _, message := range messages {
					if message.Role == "tool" && strings.Contains(string(message.Content), "REVISION_TARGET") {
						foundTool = true
					}
				}
				if !foundTool {
					t.Errorf("%s read_file result missing requested target: %s", model, turns[1])
				}
			}
			first := captured["finder-test"][0]
			if !strings.Contains(first, "+func bad()") || !strings.Contains(first, "    3  func bad()") {
				t.Errorf("initial prompt missing target diff or numbered context: %s", first)
			}
			var result struct {
				RunID string `json:"run_id"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.RunID == "" {
				t.Fatalf("run ID: %v: %s", err, &stdout)
			}
			meta, _, err := record.ReadMeta(result.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if meta.Scope.Revision != revision || meta.Scope.HeadOID != target || meta.Scope.BaseOID != base {
				t.Errorf("scope metadata=%+v, want revision=%s head=%s base=%s", meta.Scope, revision, target, base)
			}
			if !meta.Snapshot.Captured || meta.Snapshot.IndexTree != targetTree || meta.Snapshot.WorktreeTree != targetTree {
				t.Fatalf("snapshot=%+v, want exact tree %s", meta.Snapshot, targetTree)
			}
			destination := filepath.Join(t.TempDir(), "restored")
			stdout.Reset()
			stderr.Reset()
			code, err = ExecuteContext(t.Context(), []string{"runs", "checkout", result.RunID, destination}, &stdout, &stderr)
			if err != nil || code != ExitClean {
				t.Fatalf("checkout code=%d err=%v stderr=%s", code, err, &stderr)
			}
			t.Cleanup(func() {
				cmd := exec.Command("git", "-C", repo, "worktree", "remove", "--force", destination)
				_ = cmd.Run()
			})
			data, err := os.ReadFile(filepath.Join(destination, "bad.go"))
			if err != nil || string(data) != targetBody {
				t.Errorf("restored bad.go=%q err=%v", data, err)
			}
			if got := revisionCommand(t, destination, "git", "rev-parse", "HEAD^{tree}"); got != targetTree {
				t.Errorf("restored tree=%s, want %s", got, targetTree)
			}
			if got := git("rev-parse", "HEAD"); got != liveHead {
				t.Errorf("review moved live HEAD: %s", got)
			}
			if got := git("write-tree"); got != beforeIndex {
				t.Errorf("review changed live index: %s", got)
			}
			if got := git("status", "--porcelain"); got != beforeStatus {
				t.Errorf("review changed live status: %q, want %q", got, beforeStatus)
			}
		})
	}
}

func TestRevisionRejectsInvalidSelectionBeforeInference(t *testing.T) {
	revisionEnvironment(t)
	repo := initChangedRepo(t)
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "second")
	// A branch and tag with the same name must not silently choose one.
	runGit(t, repo, "branch", "ambiguous", "HEAD^")
	runGit(t, repo, "tag", "ambiguous", "HEAD")
	t.Chdir(repo)
	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		http.Error(w, "inference must not run", 500)
	}))
	defer server.Close()
	configPath := revisionConfig(t, server.URL)
	for _, args := range [][]string{
		{"--revision="}, {"--revision", " "}, {"--revision"},
		{"--revision", "does-not-exist"}, {"--revision", "ambiguous"},
		{"--revision", "HEAD", "--base", "HEAD^"},
		{"--revision", "HEAD", "--staged"},
		{"--revision", "HEAD", "--uncommitted-only"},
		{"--revision", "HEAD", "--scope", "repo"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			mu.Lock()
			before := calls
			mu.Unlock()
			command := append([]string{"review", "--config", configPath, "--max-attempts=1"}, args...)
			code, err := ExecuteContext(t.Context(), command, io.Discard, io.Discard)
			if err == nil || code == ExitClean || code == ExitFindings {
				t.Errorf("code=%d err=%v, want rejected selection", code, err)
			}
			mu.Lock()
			after := calls
			mu.Unlock()
			if after != before {
				t.Errorf("invalid selection made %d inference requests", after-before)
			}
		})
	}
	if _, err := exec.LookPath("jj"); err == nil {
		revisionCommand(t, repo, "jj", "git", "init", "--colocate")
		for _, revision := range []string{"does-not-exist", "all()"} {
			t.Run("jj "+revision, func(t *testing.T) {
				code, err := ExecuteContext(t.Context(), []string{"review", "--config", configPath, "--revision", revision, "--max-attempts=1"}, io.Discard, io.Discard)
				if err == nil || code != ExitUsage {
					t.Errorf("code=%d err=%v, want usage rejection", code, err)
				}
			})
		}
		t.Run("jj non-colocated repository", func(t *testing.T) {
			noncolocated := filepath.Join(t.TempDir(), "repo")
			revisionCommand(t, repo, "jj", "git", "init", "--no-colocate", noncolocated)
			t.Chdir(noncolocated)
			code, err := ExecuteContext(t.Context(), []string{"review", "--config", configPath, "--revision", "@", "--max-attempts=1"}, io.Discard, io.Discard)
			if err == nil || code != ExitUsage || !strings.Contains(err.Error(), "colocat") {
				t.Errorf("code=%d err=%v, want clear unsupported repository error", code, err)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("invalid selections made %d inference requests", calls)
	}
}
