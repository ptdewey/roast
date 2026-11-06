package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/runbundle"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestExitCodeContract(t *testing.T) {
	want := map[string]int{
		"clean": ExitClean, "findings": ExitFindings, "usage": ExitUsage,
		"auth": ExitAuth, "partial": ExitPartial, "cancelled": ExitCancelled,
	}
	for name, code := range want {
		if code != map[string]int{"clean": 0, "findings": 1, "usage": 2, "auth": 3, "partial": 4, "cancelled": 5}[name] {
			t.Fatalf("%s exit code = %d", name, code)
		}
	}
}

func TestErrorCategoryFor(t *testing.T) {
	tests := []struct {
		name string
		code int
		err  error
		want ErrorCategory
	}{
		{"clean", ExitClean, nil, ErrorNone},
		{"findings", ExitFindings, nil, ErrorFindings},
		{"usage", ExitUsage, &usageError{msg: "bad flag"}, ErrorUsage},
		{"auth", ExitAuth, fmt.Errorf("credential: %w", llm.ErrAuth), ErrorAuth},
		{"endpoint", ExitPartial, fmt.Errorf("provider: %w", llm.ErrEndpoint), ErrorEndpoint},
		{"budget", ExitPartial, fmt.Errorf("tokens: %w", llm.ErrBudgetExhausted), ErrorResource},
		{"cancelled", ExitCancelled, context.Canceled, ErrorCancelled},
		{"partial", ExitPartial, errors.New("artifact failure"), ErrorPartial},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ErrorCategoryFor(test.code, test.err); got != test.want {
				t.Fatalf("ErrorCategoryFor(%d, %v) = %q, want %q", test.code, test.err, got, test.want)
			}
		})
	}
}

func TestJSONErrorOutput(t *testing.T) {
	t.Setenv("ROAST_NO_HOOK", "")
	repo := t.TempDir()
	runGit(t, repo, "init")
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var stdout, stderr bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{
		"review", "--error-format", "json", "--format", "invalid",
	}, &stdout, &stderr)
	if code != ExitUsage || err == nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	WriteError(&stderr, code, err)
	var got struct {
		Category ErrorCategory `json:"error_category"`
		Message  string        `json:"message"`
		ExitCode int           `json:"exit_code"`
	}
	if decodeErr := json.Unmarshal(stderr.Bytes(), &got); decodeErr != nil {
		t.Fatalf("decode error output: %v: %s", decodeErr, stderr.String())
	}
	if got.Category != ErrorUsage || got.ExitCode != ExitUsage || !strings.Contains(got.Message, "invalid --format") {
		t.Fatalf("error output = %#v", got)
	}
}

func TestInvalidResourceOverridesAreUsageErrors(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	t.Setenv("ROAST_FINDER_MODEL", "finder-test")
	t.Setenv("ROAST_FINDER_API_KEY", "test-key")
	repo := initChangedRepo(t)
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	tests := []struct {
		name, env, value string
		args             []string
	}{
		{name: "negative diff environment", env: "ROAST_MAX_DIFF_BYTES", value: "-1"},
		{name: "malformed timeout environment", env: "ROAST_REQUEST_TIMEOUT", value: "later"},
		{name: "zero attempts environment", env: "ROAST_MAX_ATTEMPTS", value: "0"},
		{name: "zero attempts flag", args: []string{"--max-attempts=0"}},
		{name: "negative timeout flag", args: []string{"--request-timeout=-1s"}},
		{name: "malformed finder output tokens", env: "ROAST_FINDER_MAX_OUTPUT_TOKENS", value: "lots"},
		{name: "zero verifier output tokens", env: "ROAST_VERIFIER_MAX_OUTPUT_TOKENS", value: "0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.env != "" {
				t.Setenv(test.env, test.value)
			}
			args := append([]string{"review", "--quick", "--uncommitted-only"}, test.args...)
			code, runErr := ExecuteContext(t.Context(), args, io.Discard, io.Discard)
			if code != ExitUsage || runErr == nil || ErrorCategoryFor(code, runErr) != ErrorUsage {
				t.Fatalf("code=%d category=%q err=%v", code, ErrorCategoryFor(code, runErr), runErr)
			}
			if test.env != "" && !strings.Contains(runErr.Error(), test.env) {
				t.Fatalf("error does not name %s: %v", test.env, runErr)
			}
		})
	}
}

func TestReviewExitErrorPrefersArtifactFailureOverFindingGate(t *testing.T) {
	want := errors.New("persist artifacts")
	got := reviewExitError(want, 0, 0, false)
	if !errors.Is(got, want) {
		t.Fatalf("reviewExitError() = %v, want artifact error %v", got, want)
	}
}

func TestHelpNeedsNoConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"--help"}, &stdout, &stderr)
	if err != nil || code != ExitClean {
		t.Fatalf("code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "finder proposes potential defects") {
		t.Fatalf("help output missing description: %s", stdout.String())
	}
}

func TestPublicCommandSurface(t *testing.T) {
	root := newRootCommand(io.Discard, io.Discard)
	commands := make(map[string]bool)
	for _, command := range root.Commands() {
		commands[command.Name()] = true
	}
	for _, name := range []string{"auth", "config", "doctor", "gate", "review", "runs", "skill"} {
		if !commands[name] {
			t.Errorf("missing public command %q", name)
		}
	}
	for _, name := range []string{"eval", "install", "internal-update-helper", "login", "replay"} {
		if commands[name] {
			t.Errorf("unexpected command %q", name)
		}
	}
	runs, _, err := root.Find([]string{"runs"})
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range runs.Commands() {
		if command.Name() == "label" {
			t.Error("eval adjudication command runs label must not ship in the main binary")
		}
	}
}

func TestCommandUsageErrorsExitWithUsageCode(t *testing.T) {
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	for _, args := range [][]string{
		{"gate", "bogus"}, {"config", "bogus"}, {"skill", "bogus"},
		{"runs", "show"}, {"runs", "show", "one", "two"},
		{"runs", "checkout"}, {"runs", "checkout", "one", "two", "three"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			code, err := ExecuteContext(t.Context(), args, &stdout, io.Discard)
			if code != ExitUsage || ErrorCategoryFor(code, err) != ErrorUsage {
				t.Fatalf("code=%d err=%v", code, err)
			}
		})
	}
	for _, group := range []string{"gate", "config", "skill"} {
		var stdout bytes.Buffer
		code, err := ExecuteContext(t.Context(), []string{group}, &stdout, io.Discard)
		if code != ExitClean || err != nil || !strings.Contains(stdout.String(), "Available Commands") {
			t.Fatalf("%s without a subcommand: code=%d err=%v output=%s", group, code, err, stdout.String())
		}
	}
}

func TestReviewHelpShowsEffectiveDefaults(t *testing.T) {
	var stdout bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"review", "--help"}, &stdout, io.Discard)
	if err != nil || code != ExitClean {
		t.Fatalf("code=%d err=%v", code, err)
	}
	help := stdout.String()
	for _, want := range []string{
		"interactive finder work deadline (default 10m, 25m with --scope repo; 0 disables)",
		"shared finder/verifier token ceiling (default 3000000; 0 disables)",
		"maximum interactive finder turns (40 with --scope repo) (default 16)",
		"timeout for each inference request (default 10m; 0 disables)",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help missing %q", want)
		}
	}
}

func TestBareCommandOutsideGitShowsHelp(t *testing.T) {
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var stdout, stderr bytes.Buffer
	code, runErr := ExecuteContext(t.Context(), nil, &stdout, &stderr)
	if code != ExitClean || runErr != nil {
		t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, runErr, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "not in a Git repository") || !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("unexpected help: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func TestExplicitReviewOutsideGitStillFails(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ROAST_FINDER_MODEL", "finder-test")
	t.Setenv("ROAST_VERIFIER_MODEL", "verifier-test")
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, runErr := ExecuteContext(t.Context(), []string{"review", "--config", configPath}, &stdout, &stderr)
	if code != ExitUsage || runErr == nil || !strings.Contains(runErr.Error(), "not inside a git repository") {
		t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, runErr, stdout.String(), stderr.String())
	}
}

// resolveFinder resolves the finder role from a config document whose last
// section is inference_profiles.
func resolveFinder(t *testing.T, document string) config.ResolvedRole {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(document+"  verifier:\n    model: v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	return role
}

func TestProtocolFlagMovesAnUnmodifiedDefaultEndpoint(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("GATEWAY_KEY", "gateway-key")
	role := resolveFinder(t, "version: 1\ninference_profiles:\n  finder:\n    model: m\n")
	if role.APIKey != "openai-key" {
		t.Fatalf("API key = %q", role.APIKey)
	}
	changed := func(name string) bool { return name == "finder-protocol" }
	got := applyRoleFlags(changed, "finder", role, roleFlags{protocol: "anthropic"})
	if got.BaseURL != config.DefaultBaseURL("anthropic") {
		t.Fatalf("base URL = %q", got.BaseURL)
	}
	if got.APIKey != "anthropic-key" || got.Credential.Env != "ANTHROPIC_API_KEY" {
		t.Fatalf("API key = %q from %v", got.APIKey, got.Credential)
	}

	role = resolveFinder(t, "version: 1\ninference_profiles:\n  finder:\n    model: m\n    endpoint: https://gateway.example/v1\n    api_key_env: GATEWAY_KEY\n")
	got = applyRoleFlags(changed, "finder", role, roleFlags{protocol: "anthropic"})
	if got.BaseURL != role.BaseURL || got.APIKey != "gateway-key" {
		t.Fatalf("custom role changed to %#v", got)
	}
}

func TestProtocolFlagClearsWrongProviderCredential(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("OPENAI_PROFILE_KEY", "profile-key")
	for _, document := range []string{
		"version: 1\ninference_profiles:\n  finder:\n    model: m\n",
		"version: 1\ninference_profiles:\n  finder:\n    model: m\n    api_key_env: OPENAI_PROFILE_KEY\n",
	} {
		role := resolveFinder(t, document)
		changed := func(name string) bool { return name == "finder-protocol" }
		got := applyRoleFlags(changed, "finder", role, roleFlags{protocol: "anthropic"})
		if got.APIKey != "" {
			t.Fatalf("stale credential retained: %q from %v", got.APIKey, got.Credential)
		}
	}
}

func TestProtocolFlagMatchesEnvironmentForUnsetEndpoint(t *testing.T) {
	const gateway = "https://gateway.example/v1"
	t.Setenv("MY_OPENAI", "openai-profile-key")
	t.Setenv("ANTHROPIC_BASE_URL", gateway)
	t.Setenv("ANTHROPIC_API_KEY", "gateway-key")
	const document = "version: 1\ninference_profiles:\n  finder:\n    model: m\n    protocol: responses\n    api_key_env: MY_OPENAI\n"

	t.Setenv("ROAST_FINDER_PROTOCOL", "anthropic")
	fromEnv := resolveFinder(t, document)
	t.Setenv("ROAST_FINDER_PROTOCOL", "")
	changed := func(name string) bool { return name == "finder-protocol" }
	fromFlag := applyRoleFlags(changed, "finder", resolveFinder(t, document), roleFlags{protocol: "anthropic"})

	for path, role := range map[string]config.ResolvedRole{"ROAST_FINDER_PROTOCOL": fromEnv, "--finder-protocol": fromFlag} {
		if role.BaseURL != gateway || role.Protocol != "anthropic" || role.APIKey != "gateway-key" || role.Credential.Env != "ANTHROPIC_API_KEY" {
			t.Errorf("%s: endpoint=%q protocol=%q key=%q source=%v", path, role.BaseURL, role.Protocol, role.APIKey, role.Credential)
		}
	}
}

func TestBaseURLFlagDoesNotCarryAmbientProviderKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("PROFILE_KEY", "profile-key")
	t.Setenv("ROAST_FINDER_API_KEY", "")
	changed := func(name string) bool { return name == "finder-base-url" }
	flags := roleFlags{baseURL: "https://elsewhere.example/v1"}

	ambient := applyRoleFlags(changed, "finder", resolveFinder(t, "version: 1\ninference_profiles:\n  finder:\n    model: m\n"), flags)
	if ambient.APIKey != "" || ambient.Credential.Withheld != "OPENAI_API_KEY" {
		t.Fatalf("ambient key followed --finder-base-url: key=%q source=%v", ambient.APIKey, ambient.Credential)
	}
	if err := config.ValidateRole("finder", ambient); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY is not sent") {
		t.Fatalf("missing-credential error does not explain the withheld key: %v", err)
	}

	profile := applyRoleFlags(changed, "finder", resolveFinder(t, "version: 1\ninference_profiles:\n  finder:\n    model: m\n    api_key_env: PROFILE_KEY\n"), flags)
	if profile.APIKey != "profile-key" || profile.BaseURL != flags.baseURL {
		t.Fatalf("explicit profile credential did not follow the explicit flag: %#v", profile)
	}

	t.Setenv("ROAST_FINDER_API_KEY", "role-key")
	role := applyRoleFlags(changed, "finder", resolveFinder(t, "version: 1\ninference_profiles:\n  finder:\n    model: m\n"), flags)
	if role.APIKey != "role-key" || role.Credential.Kind != config.CredentialRoleEnv {
		t.Fatalf("role-scoped key not used: %#v", role)
	}

	t.Setenv("ROAST_FINDER_API_KEY", "")
	back := applyRoleFlags(changed, "finder", resolveFinder(t, "version: 1\ninference_profiles:\n  finder:\n    model: m\n    endpoint: https://custom.example/v1\n"), roleFlags{baseURL: config.DefaultBaseURL("responses")})
	if back.APIKey != "openai-key" {
		t.Fatalf("ambient key not used at the provider default endpoint: %#v", back)
	}
}

func TestRunsSyncUploadsExistingBundleWithoutEnablingAutomation(t *testing.T) {
	recordDir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", recordDir)
	t.Setenv("SYNC_UPLOAD_ACCESS", "access")
	t.Setenv("SYNC_UPLOAD_SECRET", "secret")
	runID := "20260920T120000Z-1234abcd"
	runDir := filepath.Join(recordDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, record.MetaFile), []byte(`{"run_id":"`+runID+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var uploaded bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/runs-bucket/manual/sha256/") {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		uploaded = true
		w.Header().Set("ETag", `"uploaded"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := fmt.Sprintf(`upload:
  endpoint: %s
  bucket: runs-bucket
  prefix: manual
  region: us-east-1
  access_key_env: SYNC_UPLOAD_ACCESS
  secret_key_env: SYNC_UPLOAD_SECRET
  path_style: true
`, server.URL)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"runs", "sync", "--config", configPath, "--include-existing"}, &stdout, io.Discard)
	if err != nil || code != ExitClean || !uploaded || !strings.Contains(stdout.String(), "uploaded") {
		t.Fatalf("code=%d err=%v uploaded=%v output=%s", code, err, uploaded, stdout.String())
	}
}

func TestFinderVerifierPipelineWithMockEndpoint(t *testing.T) {
	recordDir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", recordDir)
	repo := initChangedRepo(t)
	var mu sync.Mutex
	var models []string
	var mutateOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var request struct {
			Model    string          `json:"model"`
			Messages json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		models = append(models, request.Model)
		mu.Unlock()

		var name, arguments string
		switch request.Model {
		case "finder-test":
			var mutationErr error
			mutateOnce.Do(func() {
				mutationErr = os.WriteFile(filepath.Join(repo, "bad.go"), []byte("package bad\n\nfunc Bad() {}\n"), 0o644)
			})
			if mutationErr != nil {
				http.Error(w, mutationErr.Error(), http.StatusInternalServerError)
				return
			}
			name = "findings"
			arguments = `{"assessment":"FINDINGS_PRESENT","findings":[{"reasoning":"panic is reachable","severity":"HIGH","confidence":0.95,"cwe":"CWE-248","description":"The function always panics.","file":"bad.go","lines":[3],"code_quote":"panic(\"oops\")","fixed_code":"return","frontend_category":""}]}`
		case "verifier-test":
			if !strings.Contains(string(request.Messages), "panic") {
				http.Error(w, "verifier prompt drifted off the captured worktree", http.StatusBadRequest)
				return
			}
			name = "submit_verdict"
			arguments = `{"reasoning":"the panic is unconditional","status":"confirmed","confidence":0.99,"grounding":"bad.go line 3 calls panic unconditionally","recalibrated_severity":""}`
		default:
			http.Error(w, "unexpected model", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = testutil.WriteJSONStream(w, "chat", fmt.Sprintf(`{"id":"test","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, request.Model, name, arguments))
	}))
	defer server.Close()
	var uploadMu sync.Mutex
	var uploadPaths []string
	uploadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/runs-bucket/test/sha256/") {
			http.Error(w, "unexpected upload request", http.StatusBadRequest)
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		uploadMu.Lock()
		uploadPaths = append(uploadPaths, r.URL.Path)
		uploadMu.Unlock()
		w.Header().Set("ETag", `"uploaded"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer uploadServer.Close()
	t.Setenv("TEST_UPLOAD_ACCESS", "access")
	t.Setenv("TEST_UPLOAD_SECRET", "secret")

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := fmt.Sprintf(`version: 1
inference_profiles:
  finder-test:
    endpoint: %s/v1
    api_key: test-key
    model: finder-test
    protocol: chat
    max_output_tokens: 2048
  verifier-test:
    endpoint: %s/v1
    api_key: test-key
    model: verifier-test
    protocol: chat
    max_output_tokens: 2048
roles:
  finder:
    profile: finder-test
  verifier:
    profile: verifier-test
upload:
  enabled: true
  endpoint: %s
  bucket: runs-bucket
  prefix: test
  region: us-east-1
  access_key_env: TEST_UPLOAD_ACCESS
  secret_key_env: TEST_UPLOAD_SECRET
  path_style: true
`, server.URL, server.URL, uploadServer.URL)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var stdout, stderr bytes.Buffer
	code, runErr := ExecuteContext(t.Context(), []string{
		"review", "--config", configPath, "--format", "json", "--max-severity", "HIGH",
		"--deadline=0", "--request-timeout=0", "--max-tokens-total=0", "--max-verified=0", "--max-attempts=1",
	}, &stdout, &stderr)
	if runErr != nil || code != ExitFindings {
		t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, runErr, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status":"confirmed"`) || !strings.Contains(stdout.String(), `"file":"bad.go"`) {
		t.Fatalf("unexpected JSON report: %s", stdout.String())
	}
	var original struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &original); err != nil || original.RunID == "" {
		t.Fatalf("decode run id: %v: %s", err, stdout.String())
	}
	runDir, err := record.RunDir(original.RunID)
	if err != nil {
		t.Fatalf("resolve run directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "result.json.gz")); err != nil {
		t.Fatalf("production result artifact: %v", err)
	}
	manifest, err := runbundle.Verify(filepath.Join(runDir, runbundle.BundleFile))
	if err != nil {
		t.Fatalf("verify production run bundle: %v", err)
	}
	if manifest.Format != runbundle.Format || manifest.Version != runbundle.Version {
		t.Fatalf("production bundle identity = %q v%d", manifest.Format, manifest.Version)
	}
	responseData, err := os.ReadFile(filepath.Join(runDir, record.ProviderResponsesFile))
	if err != nil {
		t.Fatalf("read provider response artifact: %v", err)
	}
	if got := strings.Count(strings.TrimSpace(string(responseData)), "\n") + 1; got != 2 {
		t.Fatalf("provider response count = %d, want finder and verifier", got)
	}
	foundResponses := false
	for _, entry := range manifest.Files {
		if entry.Path == record.ProviderResponsesFile {
			foundResponses = true
			break
		}
	}
	if !foundResponses {
		t.Fatalf("bundle manifest does not contain %s", record.ProviderResponsesFile)
	}
	meta, _, err := record.ReadMeta(original.RunID)
	if err != nil {
		t.Fatalf("read run metadata: %v", err)
	}
	if got := meta.Budgets["deadline_duration"]; got != "0s" {
		t.Fatalf("deadline duration = %v, want 0s", got)
	}
	if got := meta.Budgets["max_total_tokens"]; got != float64(0) {
		t.Fatalf("max total tokens = %v, want 0", got)
	}
	if got := meta.Budgets["verifier_cap"]; got != float64(0) {
		t.Fatalf("verifier cap = %v, want 0", got)
	}
	if got := meta.Budgets["request_timeout"]; got != "0s" {
		t.Fatalf("request timeout = %v, want 0s", got)
	}
	if got := meta.Budgets["max_attempts"]; got != float64(1) {
		t.Fatalf("max attempts = %v, want 1", got)
	}
	if got := meta.Roles["finder"]; got.Profile != "finder-test" || got.Protocol != "chat" || got.API != "" {
		t.Fatalf("recorded finder role = %#v", got)
	}
	var statusOut bytes.Buffer
	statusCode, statusErr := ExecuteContext(t.Context(), []string{"runs", "status"}, &statusOut, io.Discard)
	if statusErr != nil || statusCode != ExitClean || !strings.Contains(statusOut.String(), "uploaded") {
		t.Fatalf("upload status code=%d err=%v output=%s", statusCode, statusErr, statusOut.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(models, ",") != "finder-test,verifier-test" {
		t.Fatalf("models called = %v", models)
	}
	uploadMu.Lock()
	defer uploadMu.Unlock()
	if len(uploadPaths) != 1 {
		t.Fatalf("automatic upload paths = %v, want one object per completed review", uploadPaths)
	}
}

func TestNativeAnthropicFinderWithChatVerifier(t *testing.T) {
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages":
			var request struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if request.Model != "claude-finder" || r.Header.Get("X-Api-Key") != "anthropic-key" {
				http.Error(w, "unexpected Anthropic request", http.StatusBadRequest)
				return
			}
			_ = testutil.WriteInferenceStream(w, "anthropic", map[string]any{
				"id": "msg", "type": "message", "role": "assistant", "model": request.Model,
				"content": []any{map[string]any{
					"type": "tool_use", "id": "toolu-findings", "name": "findings",
					"input": map[string]any{
						"assessment": "FINDINGS_PRESENT",
						"findings": []any{map[string]any{
							"reasoning": "panic is reachable", "severity": "HIGH", "confidence": 0.95,
							"cwe": "CWE-248", "description": "The function always panics.", "file": "bad.go",
							"lines": []any{3}, "code_quote": `panic("oops")`, "fixed_code": "return", "frontend_category": "",
						}},
					},
				}},
				"stop_reason": "tool_use", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
			})
		case "/v1/chat/completions":
			_ = testutil.WriteJSONStream(w, "chat", `{"id":"test","object":"chat.completion","created":1,"model":"verifier-test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"submit_verdict","arguments":"{\"reasoning\":\"the panic is unconditional\",\"status\":\"confirmed\",\"confidence\":0.99,\"grounding\":\"bad.go line 3 calls panic unconditionally\",\"recalibrated_severity\":\"\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repo := initChangedRepo(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := fmt.Sprintf(`version: 1
inference_profiles:
  primary:
    endpoint: %s/v1
    api_key: openai-key
    model: gpt-primary
    protocol: chat
    max_output_tokens: 2048
  finder-test:
    endpoint: %s/v1
    api_key: anthropic-key
    model: claude-finder
    protocol: anthropic
    max_output_tokens: 2048
  verifier-test:
    endpoint: %s/v1
    api_key: openai-key
    model: verifier-test
    protocol: chat
    max_output_tokens: 2048
roles:
  finder:
    profile: primary
    author_routes:
      exact-author-model: finder-test
  verifier:
    profile: verifier-test
`, server.URL, server.URL, server.URL)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var stdout, stderr bytes.Buffer
	code, runErr := ExecuteContext(t.Context(), []string{
		"review", "--config", configPath, "--format", "json", "--max-severity", "HIGH",
		"--author-model", "exact-author-model", "--deadline=0", "--max-tokens-total=0", "--max-verified=1",
	}, &stdout, &stderr)
	if runErr != nil || code != ExitFindings {
		t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, runErr, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status":"confirmed"`) {
		t.Fatalf("unexpected report: %s", stdout.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(paths, ",") != "/v1/messages,/v1/chat/completions" {
		t.Fatalf("request paths = %v", paths)
	}
}

func TestRepositorySurveyUsesManifestAndInteractiveTools(t *testing.T) {
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	var mu sync.Mutex
	requests := 0
	var prompts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		raw, _ := json.Marshal(request["messages"])
		mu.Lock()
		requests++
		call := requests
		prompts = append(prompts, string(raw))
		mu.Unlock()

		name, arguments := "read_file", `{"path":"bad.go","offset":1,"limit":20}`
		if call > 1 {
			name = "submit_findings"
			arguments = `{"assessment":"NO_FINDINGS","findings":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = testutil.WriteJSONStream(w, "chat", fmt.Sprintf(`{"id":"test","object":"chat.completion","created":1,"model":"finder-test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-%d","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, call, name, arguments))
	}))
	defer server.Close()

	repo := initChangedRepo(t)
	runGit(t, repo, "add", "bad.go")
	runGit(t, repo, "commit", "-m", "change")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := fmt.Sprintf(`version: 1
inference_profiles:
  finder-test:
    endpoint: %s/v1
    api_key: test-key
    model: finder-test
    protocol: chat
    max_output_tokens: 2048
  verifier-test:
    endpoint: %s/v1
    api_key: test-key
    model: verifier-test
    protocol: chat
    max_output_tokens: 2048
roles:
  finder:
    profile: finder-test
  verifier:
    profile: verifier-test
`, server.URL, server.URL)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var stdout, stderr bytes.Buffer
	code, runErr := ExecuteContext(t.Context(), []string{
		"review", "--config", configPath, "--scope", "repo", "--format", "json",
	}, &stdout, &stderr)
	if runErr != nil || code != ExitClean {
		t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, runErr, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"assessment":"NO_FINDINGS_IN_SAMPLED_AREAS"`) ||
		!strings.Contains(stdout.String(), `"review_kind":"survey_triage"`) {
		t.Fatalf("unexpected survey output: %s", stdout.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 2 {
		t.Fatalf("finder requests = %d, want 2", requests)
	}
	joined := strings.Join(prompts, "\n")
	if !strings.Contains(joined, "Repository under review") || !strings.Contains(joined, "bad.go") {
		t.Fatalf("survey prompt did not contain manifest: %s", joined)
	}
}

func initChangedRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	path := filepath.Join(repo, "bad.go")
	if err := os.WriteFile(path, []byte("package example\n\nfunc bad() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "bad.go")
	runGit(t, repo, "commit", "-m", "initial")
	if err := os.WriteFile(path, []byte("package example\n\nfunc bad() { panic(\"oops\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
