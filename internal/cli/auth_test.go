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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/chatgpt"
	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/testutil"
)

type fakeChatGPTSession struct {
	account     string
	tokenCalls  int
	loginCalls  int
	logoutCalls int
	failure     error
}

func (s *fakeChatGPTSession) Login(_ context.Context, account string, visit func(string) error) (chatgpt.Account, error) {
	s.loginCalls++
	s.account = account
	if err := visit("https://auth.openai.com/api/accounts/authorize?state=test-state"); err != nil {
		return chatgpt.Account{}, err
	}
	return s.Status(context.Background(), account)
}
func (s *fakeChatGPTSession) Status(_ context.Context, account string) (chatgpt.Account, error) {
	s.account = account
	return chatgpt.Account{Name: account, Email: "reviewer@example.com", ExpiresAt: time.Unix(2000000000, 0)}, s.failure
}
func (s *fakeChatGPTSession) Token(_ context.Context, account string) (string, error) {
	s.tokenCalls++
	s.account = account
	return "oauth-access-secret", s.failure
}
func (s *fakeChatGPTSession) Logout(_ context.Context, account string) error {
	s.logoutCalls++
	s.account = account
	return s.failure
}

func useFakeChatGPT(t *testing.T, session *fakeChatGPTSession) {
	t.Helper()
	previous := newChatGPTManager
	newChatGPTManager = func() (chatGPTSession, error) { return session, nil }
	t.Cleanup(func() { newChatGPTManager = previous })
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
}

func TestChatGPTAuthCommands(t *testing.T) {
	session := &fakeChatGPTSession{}
	useFakeChatGPT(t, session)
	for _, args := range [][]string{
		{"auth", "login", "--no-browser", "--account", "personal"},
		{"auth", "status", "--json", "--account", "personal"},
		{"auth", "logout", "--account", "personal"},
	} {
		var stdout, stderr bytes.Buffer
		code, err := ExecuteContext(t.Context(), args, &stdout, &stderr)
		if code != ExitClean || err != nil {
			t.Fatalf("%v: code=%d err=%v", args, code, err)
		}
		if session.account != "personal" || strings.Contains(stdout.String()+stderr.String(), "secret") {
			t.Fatalf("bad account/credential exposure: %s %s", stdout.String(), stderr.String())
		}
		if args[1] == "status" {
			var identity chatgpt.Account
			if err := json.Unmarshal(stdout.Bytes(), &identity); err != nil || identity.Email != "reviewer@example.com" {
				t.Fatalf("status: %s", stdout.String())
			}
		}
	}
	if session.loginCalls != 1 || session.logoutCalls != 1 || session.tokenCalls != 0 {
		t.Fatalf("unexpected session operations: %#v", session)
	}
}

func TestChatGPTUsageLimitIsResourceError(t *testing.T) {
	if ErrorCategoryFor(ExitPartial, llm.ErrUsageLimit) != ErrorResource || exitCode(llm.ErrUsageLimit) != ExitPartial {
		t.Fatal("subscription quota should be a resource error, not authentication or usage")
	}
}

func TestChatGPTAuthMissingSessionExitCode(t *testing.T) {
	session := &fakeChatGPTSession{failure: errors.New("ChatGPT account unavailable; run roast auth login")}
	useFakeChatGPT(t, session)
	var stdout, stderr bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"auth", "status"}, &stdout, &stderr)
	if code != ExitAuth || !errors.Is(err, llm.ErrAuth) || ErrorCategoryFor(code, err) != ErrorAuth {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestChatGPTClientConfigurationUsesDynamicTokenSource(t *testing.T) {
	session := &fakeChatGPTSession{}
	useFakeChatGPT(t, session)
	role := config.ResolvedRole{Auth: config.AuthChatGPT, Account: "personal", BaseURL: "https://api.openai.com/v1", Protocol: "responses", Model: "account-model", MaxOutputTokens: 1024}
	cfg := clientConfig("finder", role, nil, nil, 1, time.Second)
	if !cfg.ChatGPT || cfg.TokenSource == nil || cfg.APIKey != "" || session.tokenCalls != 0 {
		t.Fatalf("bad subscription client config: %#v", cfg)
	}
	for range 2 {
		token, err := cfg.TokenSource(t.Context())
		if err != nil || token != "oauth-access-secret" {
			t.Fatalf("token source error: %v", err)
		}
	}
	if session.tokenCalls != 2 || session.account != "personal" {
		t.Fatalf("token source wasn't called per request: %#v", session)
	}
	ordinary := clientConfig("finder", config.ResolvedRole{APIKey: "ordinary-key"}, nil, nil, 1, time.Second)
	if ordinary.ChatGPT || ordinary.TokenSource != nil || ordinary.APIKey != "ordinary-key" {
		t.Fatal("API-key config changed")
	}
}

type authTestTransport func(*http.Request) (*http.Response, error)

func (f authTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestChatGPTModelCatalog(t *testing.T) {
	client := subscriptionHTTPClient()
	client.Transport = authTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://api.openai.com/v1/models" || req.Header.Get("Authorization") != "Bearer oauth-secret" {
			t.Errorf("wrong catalog destination or auth")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"model-b","display_name":"Model B","visibility":"list"},{"slug":"hidden","visibility":"hidden"},{"slug":"model-a","display_name":"Model A","visibility":"list"}]}`)), Header: make(http.Header)}, nil
	})
	models, err := listChatGPTModels(t.Context(), client, "oauth-secret")
	if err != nil || len(models) != 2 || models[0].Slug != "model-b" || models[1].Slug != "model-a" {
		t.Fatalf("catalog=%#v err=%v", models, err)
	}
}

func TestChatGPTModelCatalogErrorsAreRedacted(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := subscriptionHTTPClient()
			client.Transport = authTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("oauth-secret echoed by server")), Header: make(http.Header)}, nil
			})
			_, err := listChatGPTModels(t.Context(), client, "oauth-secret")
			if err == nil || strings.Contains(err.Error(), "oauth-secret") {
				t.Fatalf("unsafe error: %v", err)
			}
			if (status == 401 || status == 403) && !errors.Is(err, llm.ErrAuth) {
				t.Fatalf("not auth error: %v", err)
			}
		})
	}
}

func TestChatGPTModelCatalogDoesNotFollowRedirects(t *testing.T) {
	var redirected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()
	client := subscriptionHTTPClient()
	client.Transport = authTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.openai.com" {
			return http.DefaultTransport.RoundTrip(req)
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{server.URL}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})
	if _, err := listChatGPTModels(t.Context(), client, "oauth-secret"); err == nil || redirected {
		t.Fatalf("followed subscription redirect: %v", err)
	}
}

func TestChatGPTDoctorRejectsGatewayBeforeReadingCredentials(t *testing.T) {
	session := &fakeChatGPTSession{}
	useFakeChatGPT(t, session)
	role := config.ResolvedRole{Auth: config.AuthChatGPT, Account: "default", BaseURL: "https://gateway.example/v1", Protocol: "responses", Model: "account-model", MaxOutputTokens: 512}
	result := doctorEndpointCheck(t.Context(), subscriptionHTTPClient(), "finder", role)
	if result.Status != doctorFail || session.tokenCalls != 0 {
		t.Fatalf("doctor did not fail closed: %#v calls=%d", result, session.tokenCalls)
	}
}

func TestChatGPTReviewMissingSessionFailsBeforeArtifacts(t *testing.T) {
	session := &fakeChatGPTSession{failure: errors.New("ChatGPT account unavailable; run roast auth login")}
	useFakeChatGPT(t, session)
	recordDir := filepath.Join(t.TempDir(), "records")
	t.Setenv("ROAST_RECORD_DIR", recordDir)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	body := `version: 1
inference_profiles:
  subscription:
    auth: chatgpt
    model: account-model
roles:
  finder:
    profile: subscription
  verifier:
    profile: subscription
`
	if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"review", "--config", configPath, "--quick"}, &stdout, &stderr)
	if code != ExitAuth || !errors.Is(err, llm.ErrAuth) {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if _, err := os.Stat(recordDir); !os.IsNotExist(err) {
		t.Fatalf("unauthenticated review created records: %v", err)
	}
}

func TestChatGPTFinderVerifierPipelineWithArtifacts(t *testing.T) {
	testSubscriptionFinderVerifierPipeline(t, config.AuthChatGPT)
}

func testSubscriptionFinderVerifierPipeline(t *testing.T, provider string) {
	session := &fakeChatGPTSession{}
	useFakeChatGPT(t, session)
	codex := &fakeCodexSession{}
	useFakeCodex(t, codex)
	expectedToken, expectedHost, expectedPath := "oauth-access-secret", "api.openai.com", "/v1/responses"
	if provider == config.AuthCodex {
		expectedToken, expectedHost, expectedPath = "codex-access-secret", "chatgpt.com", "/backend-api/codex/responses"
	}
	t.Setenv("OPENAI_API_KEY", "never-use-api-key")
	t.Setenv("OPENAI_BASE_URL", "https://never-contact-gateway.example/v1")
	recordDir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", recordDir)
	repo := initChangedRepo(t)
	t.Chdir(repo)
	var mu sync.Mutex
	var models []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expectedToken {
			t.Error("missing OAuth token")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if r.URL.Path != expectedPath || body["store"] != false || body["stream"] != true || body["max_output_tokens"] != nil || body["instructions"] == nil {
			t.Errorf("subscription request contract failed: %#v", body)
		}
		tools, _ := body["tools"].([]any)
		if provider == config.AuthCodex {
			if len(tools) == 0 {
				t.Error("Codex tools missing")
			}
			for _, tool := range tools {
				if tool.(map[string]any)["type"] != "function" {
					t.Error("Codex tools were not flat functions")
				}
			}
			if r.Header.Get("ChatGPT-Account-Id") != "codex-workspace" {
				t.Error("missing coherent Codex workspace header")
			}
		} else if len(tools) != 1 || tools[0].(map[string]any)["type"] != "namespace" {
			t.Error("missing tool namespace")
		}
		model, _ := body["model"].(string)
		mu.Lock()
		models = append(models, model)
		mu.Unlock()
		tool, args := "findings", `{"assessment":"FINDINGS_PRESENT","findings":[{"reasoning":"panic is reachable","severity":"HIGH","confidence":0.95,"cwe":"CWE-248","description":"The function always panics.","file":"bad.go","lines":[3],"code_quote":"panic(\"oops\")","fixed_code":"return","frontend_category":""}]}`
		if model == "subscription-verifier" {
			tool, args = "submit_verdict", `{"reasoning":"the panic is unconditional","status":"confirmed","confidence":0.99,"grounding":"bad.go line 3 calls panic unconditionally","recalibrated_severity":""}`
		}
		call := map[string]any{"id": "fc-" + tool, "type": "function_call", "call_id": "call-" + tool, "name": tool, "arguments": args, "status": "completed"}
		if provider != config.AuthCodex {
			call["namespace"] = "roast"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_ = testutil.WriteInferenceStream(w, "responses", map[string]any{
			"id": "resp-" + model, "object": "response", "created_at": 1, "status": "completed", "model": model,
			"output": []any{call},
			"usage":  map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
		})
	}))
	defer server.Close()
	local, _ := url.Parse(server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = authTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "https" || req.URL.Host != expectedHost || req.URL.Path != expectedPath {
			return nil, fmt.Errorf("unexpected network request in subscription CLI test")
		}
		clone := req.Clone(req.Context())
		u := *req.URL
		u.Scheme = local.Scheme
		u.Host = local.Host
		clone.URL = &u
		clone.Host = local.Host
		return original.RoundTrip(clone)
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	body := `version: 1
inference_profiles:
  finder:
    auth: chatgpt
    model: subscription-finder
  verifier:
    auth: chatgpt
    model: subscription-verifier
roles:
  finder:
    profile: finder
  verifier:
    profile: verifier
`
	body = strings.ReplaceAll(body, "auth: chatgpt", "auth: "+provider)
	if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"review", "--config", configPath, "--format", "json", "--max-severity", "HIGH", "--max-verified=1"}, &stdout, &stderr)
	if err != nil || code != ExitFindings || !strings.Contains(stdout.String(), `"status":"confirmed"`) {
		t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, err, stdout.String(), stderr.String())
	}
	mu.Lock()
	called := strings.Join(models, ",")
	mu.Unlock()
	if called != "subscription-finder,subscription-verifier" {
		t.Fatalf("models called: %s", called)
	}
	if provider == config.AuthCodex {
		if codex.credentialCalls < 4 || session.tokenCalls != 0 {
			t.Fatalf("wrong credential sources: codex=%d siwc=%d", codex.credentialCalls, session.tokenCalls)
		}
	} else if session.tokenCalls < 4 || codex.credentialCalls != 0 {
		t.Fatalf("wrong credential sources: codex=%d siwc=%d", codex.credentialCalls, session.tokenCalls)
	}
	var artifactFiles int
	if err := filepath.WalkDir(recordDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		artifactFiles++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("oauth-access-secret")) || bytes.Contains(data, []byte("codex-access-secret")) || bytes.Contains(data, []byte("never-use-api-key")) {
			t.Errorf("credential leaked into %s", filepath.Base(path))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if artifactFiles == 0 {
		t.Fatal("review did not record artifacts")
	}
}

func TestChatGPTReviewEndpointFlagCannotAcquireOAuthToken(t *testing.T) {
	session := &fakeChatGPTSession{}
	useFakeChatGPT(t, session)
	t.Setenv("OPENAI_API_KEY", "never-use-api-key")
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `version: 1
inference_profiles:
  subscription:
    auth: chatgpt
    model: account-model
roles:
  finder:
    profile: subscription
  verifier:
    profile: subscription
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{
		{"--finder-base-url", "https://gateway.example/v1"},
		{"--finder-protocol", "anthropic"},
	} {
		args := append([]string{"review", "--config", path, "--quick"}, flags...)
		code, err := ExecuteContext(t.Context(), args, io.Discard, io.Discard)
		if code != ExitUsage || err == nil || session.tokenCalls != 0 {
			t.Fatalf("unsafe CLI override: code=%d err=%v credential calls=%d", code, err, session.tokenCalls)
		}
	}
}
