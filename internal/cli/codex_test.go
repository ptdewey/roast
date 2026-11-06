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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/chatgpt"
	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/llm"
)

type fakeCodexSession struct {
	fakeChatGPTSession
	credentialCalls int
}

func (s *fakeCodexSession) CodexCredentials(_ context.Context, account string) (string, string, error) {
	s.credentialCalls++
	s.account = account
	return "codex-access-secret", "codex-workspace", s.failure
}
func (s *fakeCodexSession) Status(ctx context.Context, account string) (chatgpt.Account, error) {
	identity, err := s.fakeChatGPTSession.Status(ctx, account)
	identity.AccountID = "codex-workspace"
	return identity, err
}
func useFakeCodex(t *testing.T, session *fakeCodexSession) {
	t.Helper()
	previous := newCodexManager
	newCodexManager = func() (codexSession, error) { return session, nil }
	t.Cleanup(func() { newCodexManager = previous })
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
}

func TestCodexAuthCommandsUseSelectedProviderOnly(t *testing.T) {
	codex := &fakeCodexSession{}
	siwc := &fakeChatGPTSession{failure: errors.New("wrong credential store selected")}
	useFakeCodex(t, codex)
	useFakeChatGPT(t, siwc)
	for _, args := range [][]string{
		{"auth", "login", "--provider", "codex", "--no-browser", "--account", "personal"},
		{"auth", "status", "--provider", "codex", "--json", "--account", "personal"},
		{"auth", "logout", "--provider", "codex", "--account", "personal"},
	} {
		var stdout, stderr bytes.Buffer
		code, err := ExecuteContext(t.Context(), args, &stdout, &stderr)
		if err != nil || code != ExitClean {
			t.Fatalf("%v: code=%d err=%v", args, code, err)
		}
		if codex.account != "personal" || strings.Contains(stdout.String()+stderr.String(), "secret") {
			t.Fatalf("incorrect account or credential exposure")
		}
		if args[1] == "status" {
			var identity chatgpt.Account
			if err := json.Unmarshal(stdout.Bytes(), &identity); err != nil || identity.AccountID != "codex-workspace" {
				t.Fatalf("Codex status: %s", stdout.String())
			}
		}
	}
	if siwc.loginCalls != 0 || siwc.logoutCalls != 0 || siwc.tokenCalls != 0 || siwc.account != "" {
		t.Fatal("Codex touched SIWC credentials")
	}
	if codex.loginCalls != 1 || codex.logoutCalls != 1 || codex.credentialCalls != 0 {
		t.Fatalf("wrong account operations: %#v", codex)
	}
	var stdout bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"auth", "status"}, &stdout, io.Discard)
	if code != ExitAuth || err == nil || codex.account != "personal" {
		t.Fatalf("default provider changed or selected sibling: code=%d err=%v", code, err)
	}
}

func TestUnknownAuthProviderFailsBeforeCredentials(t *testing.T) {
	codex := &fakeCodexSession{}
	siwc := &fakeChatGPTSession{}
	useFakeCodex(t, codex)
	useFakeChatGPT(t, siwc)
	for _, subcommand := range []string{"login", "status", "logout", "models"} {
		code, err := ExecuteContext(t.Context(), []string{"auth", subcommand, "--provider", "unknown"}, io.Discard, io.Discard)
		if code != ExitUsage || err == nil {
			t.Fatalf("%s: code=%d err=%v", subcommand, code, err)
		}
	}
	if codex.account != "" || siwc.account != "" {
		t.Fatal("unknown provider touched credentials")
	}
}

func TestCodexClientConfigAndPreflightUseCoherentCredentialSource(t *testing.T) {
	codex := &fakeCodexSession{}
	useFakeCodex(t, codex)
	role := config.ResolvedRole{Auth: config.AuthCodex, Account: "personal", Protocol: "responses", BaseURL: config.OAuthBaseURL(config.AuthCodex), Model: "codex-model", MaxOutputTokens: 1024}
	cfg := clientConfig("finder", role, nil, nil, 1, time.Second)
	if !cfg.Codex || cfg.ChatGPT || cfg.APIKey != "" || cfg.TokenSource != nil || cfg.CodexTokenSource == nil || codex.credentialCalls != 0 {
		t.Fatalf("bad Codex configuration: %#v", cfg)
	}
	token, workspace, err := cfg.CodexTokenSource(t.Context())
	if err != nil || token != "codex-access-secret" || workspace != "codex-workspace" || codex.account != "personal" {
		t.Fatalf("invalid credential pair: err=%v", err)
	}
	if err := subscriptionPreflight(t.Context(), role); err != nil || codex.credentialCalls != 2 {
		t.Fatalf("preflight failed: %v", err)
	}
}

func TestCodexModelCatalogContract(t *testing.T) {
	client := subscriptionHTTPClient()
	client.Transport = authTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Scheme != "https" || req.URL.Host != "chatgpt.com" || req.URL.Path != "/backend-api/codex/models" || req.URL.Query().Get("client_version") != llm.CodexClientVersion {
			t.Errorf("wrong catalog request %s", req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer codex-secret" || req.Header.Get("ChatGPT-Account-Id") != "workspace-one" || req.Header.Get("Originator") != "roast" {
			t.Error("wrong Codex account headers")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"model-b","display_name":"Model B","visibility":"list"},{"slug":"hidden","visibility":"hidden"},{"slug":"model-a","display_name":"Model A","visibility":"list"}]}`))}, nil
	})
	models, err := listCodexModels(t.Context(), client, "codex-secret", "workspace-one")
	if err != nil || len(models) != 2 || models[0].Slug != "model-b" || models[1].Slug != "model-a" {
		t.Fatalf("models=%#v err=%v", models, err)
	}
}

func TestCodexCatalogRejectsMissingCredentialsAndRedactsFailures(t *testing.T) {
	var calls int
	client := subscriptionHTTPClient()
	client.Transport = authTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("secret echoed in transport error")
	})
	for _, pair := range [][2]string{{"", "workspace"}, {"token", ""}} {
		if _, err := listCodexModels(t.Context(), client, pair[0], pair[1]); !errors.Is(err, llm.ErrAuth) || calls != 0 {
			t.Fatalf("missing auth did not fail closed: %v", err)
		}
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusOK} {
		client.Transport = authTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("codex-secret echoed by server"))}, nil
		})
		_, err := listCodexModels(t.Context(), client, "codex-secret", "workspace")
		if err == nil || strings.Contains(err.Error(), "codex-secret") {
			t.Fatalf("unsafe catalog error: %v", err)
		}
	}
}

func TestCodexCatalogDoesNotFollowRedirects(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true; _, _ = fmt.Fprint(w, `{"models":[]}`) }))
	defer target.Close()
	client := &http.Client{Transport: authTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "chatgpt.com" {
			return http.DefaultTransport.RoundTrip(req)
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{target.URL}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}
	if _, err := listCodexModels(t.Context(), client, "codex-secret", "workspace"); err == nil || redirected {
		t.Fatalf("followed catalog redirect: %v", err)
	}
}

func TestCodexDoctorRejectsCrossProviderRouteBeforeCredentials(t *testing.T) {
	codex := &fakeCodexSession{}
	useFakeCodex(t, codex)
	role := config.ResolvedRole{Auth: config.AuthCodex, Account: "default", Protocol: "responses", BaseURL: config.OAuthBaseURL(config.AuthChatGPT), Model: "codex-model", MaxOutputTokens: 512}
	result := doctorEndpointCheck(t.Context(), subscriptionHTTPClient(), "finder", role)
	if result.Status != doctorFail || codex.credentialCalls != 0 {
		t.Fatalf("unsafe doctor behavior: %#v", result)
	}
}

func TestCodexReviewFailsBeforeArtifactsWithoutSession(t *testing.T) {
	codex := &fakeCodexSession{fakeChatGPTSession: fakeChatGPTSession{failure: errors.New("Codex requires sign in")}}
	useFakeCodex(t, codex)
	recordDir := filepath.Join(t.TempDir(), "records")
	t.Setenv("ROAST_RECORD_DIR", recordDir)
	path := writeCodexCLIConfig(t)
	code, err := ExecuteContext(t.Context(), []string{"review", "--config", path, "--quick"}, io.Discard, io.Discard)
	if code != ExitAuth || !errors.Is(err, llm.ErrAuth) {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if _, err := os.Stat(recordDir); !os.IsNotExist(err) {
		t.Fatalf("created unauthenticated review artifacts: %v", err)
	}
}

func writeCodexCLIConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `version: 1
inference_profiles:
  finder:
    auth: codex
    model: codex-finder
  verifier:
    auth: codex
    model: codex-verifier
roles:
  finder:
    profile: finder
  verifier:
    profile: verifier
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexFinderVerifierPipelineWithArtifacts(t *testing.T) {
	testSubscriptionFinderVerifierPipeline(t, config.AuthCodex)
}

func TestCodexReviewRouteFlagsCannotAcquireCredentials(t *testing.T) {
	codex := &fakeCodexSession{}
	useFakeCodex(t, codex)
	t.Setenv("OPENAI_API_KEY", "never-use-api-key")
	path := writeCodexCLIConfig(t)
	for _, flags := range [][]string{{"--finder-base-url", "https://api.openai.com/v1"}, {"--finder-protocol", "anthropic"}} {
		args := append([]string{"review", "--config", path, "--quick"}, flags...)
		code, err := ExecuteContext(t.Context(), args, io.Discard, io.Discard)
		if code != ExitUsage || err == nil || codex.credentialCalls != 0 {
			t.Fatalf("unsafe route flag: code=%d err=%v credential calls=%d", code, err, codex.credentialCalls)
		}
	}
}

func TestCodexAuthModelsUsesSelectedAccountStore(t *testing.T) {
	codex := &fakeCodexSession{}
	siwc := &fakeChatGPTSession{failure: errors.New("wrong store")}
	useFakeCodex(t, codex)
	useFakeChatGPT(t, siwc)
	original := http.DefaultTransport
	http.DefaultTransport = authTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "chatgpt.com" || req.URL.Path != "/backend-api/codex/models" || req.Header.Get("Authorization") != "Bearer codex-access-secret" || req.Header.Get("ChatGPT-Account-Id") != "codex-workspace" {
			t.Error("wrong catalog auth or route")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"codex-model","display_name":"Codex Model","visibility":"list"}]}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	var stdout, stderr bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"auth", "models", "--provider", "codex", "--account", "personal", "--json"}, &stdout, &stderr)
	if code != ExitClean || err != nil || codex.account != "personal" || codex.credentialCalls != 1 || siwc.tokenCalls != 0 {
		t.Fatalf("code=%d err=%v account=%s", code, err, codex.account)
	}
	if !strings.Contains(stdout.String(), "codex-model") || strings.Contains(stdout.String()+stderr.String(), "secret") {
		t.Fatalf("bad models output: %s %s", stdout.String(), stderr.String())
	}
}

func TestCodexDoctorUsesAccountSpecificCatalog(t *testing.T) {
	codex := &fakeCodexSession{}
	useFakeCodex(t, codex)
	client := subscriptionHTTPClient()
	client.Transport = authTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "chatgpt.com" || req.Header.Get("ChatGPT-Account-Id") != "codex-workspace" {
			t.Error("wrong doctor catalog route")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":[]}`))}, nil
	})
	role := config.ResolvedRole{Auth: config.AuthCodex, Account: "personal", Protocol: "responses", BaseURL: config.OAuthBaseURL(config.AuthCodex), Model: "codex-model", MaxOutputTokens: 1024}
	result := doctorEndpointCheck(t.Context(), client, "finder", role)
	if result.Status != doctorPass || codex.credentialCalls != 1 || codex.account != "personal" || strings.Contains(result.Detail, "secret") {
		t.Fatalf("doctor failed: %#v", result)
	}
}

func TestCodexMissingSessionExplainsProviderFlag(t *testing.T) {
	manager := chatgpt.NewCodex(filepath.Join(t.TempDir(), "codex"))
	previous := newCodexManager
	newCodexManager = func() (codexSession, error) { return manager, nil }
	t.Cleanup(func() { newCodexManager = previous })
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	code, err := ExecuteContext(t.Context(), []string{"auth", "status", "--provider", "codex"}, io.Discard, io.Discard)
	if code != ExitAuth || err == nil || !strings.Contains(err.Error(), "roast auth login --provider codex") {
		t.Fatalf("missing legacy login guidance: code=%d err=%v", code, err)
	}
}
