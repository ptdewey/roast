package config

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexProfileIgnoresAmbientCredentialsAndEndpoint(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "never-read-api-key")
	t.Setenv("OPENAI_BASE_URL", "https://gateway.example/v1")
	input := strings.Replace(subscriptionConfig, "auth: chatgpt", "auth: codex\n    account: personal", 1)
	cfg, err := Load(writeConfig(t, input))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"finder", "verifier"} {
		role, err := cfg.ResolveRole(name)
		if err != nil {
			t.Fatal(err)
		}
		if role.Auth != AuthCodex || role.Account != "personal" || role.APIKey != "" || role.BaseURL != "https://chatgpt.com/backend-api/codex" || role.Credential.Kind != CredentialCodex {
			t.Fatalf("incorrect Codex selection: %#v", role)
		}
		if err := ValidateRole(name, role); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(role.Credential.String(), "Codex OAuth") {
			t.Fatalf("credential source: %s", role.Credential)
		}
	}
	profile := cfg.InferenceProfiles["subscription"]
	if profile.Endpoint() != OAuthBaseURL(AuthCodex) || strings.Contains(profile.DescribeEndpoint(), "OPENAI_BASE_URL") || !strings.Contains(profile.DescribeCredential("finder"), "Codex OAuth") {
		t.Fatalf("bad profile description: %#v", profile)
	}
}

func TestOAuthModesCannotCrossEndpoints(t *testing.T) {
	for _, auth := range []string{AuthChatGPT, AuthCodex} {
		for _, endpoint := range []string{"https://api.openai.com/v1", "https://chatgpt.com/backend-api/codex", "https://gateway.example/v1"} {
			t.Run(auth+"/"+endpoint, func(t *testing.T) {
				input := strings.Replace(subscriptionConfig, "auth: chatgpt", "auth: "+auth+"\n    endpoint: "+endpoint, 1)
				_, err := Load(writeConfig(t, input))
				if (err == nil) != (endpoint == OAuthBaseURL(auth)) {
					t.Fatalf("accepted=%v, endpoint=%s auth=%s", err == nil, endpoint, auth)
				}
			})
		}
	}
	for _, field := range []string{"api_key: secret", "api_key_env: PROFILE_KEY", "protocol: chat", "protocol: anthropic", "account: ../escape"} {
		input := strings.Replace(subscriptionConfig, "auth: chatgpt", "auth: codex\n    "+field, 1)
		if _, err := Load(writeConfig(t, input)); err == nil {
			t.Fatalf("accepted incompatible Codex setting %s", field)
		}
	}
}

func TestCodexRoleOverridesFailClosed(t *testing.T) {
	for _, override := range []struct{ name, value string }{
		{"ROAST_FINDER_API_KEY", "role-secret"}, {"ROAST_FINDER_BASE_URL", "https://api.openai.com/v1"}, {"ROAST_FINDER_PROTOCOL", "anthropic"},
	} {
		t.Run(override.name, func(t *testing.T) {
			t.Setenv(override.name, override.value)
			cfg, err := Load(writeConfig(t, strings.Replace(subscriptionConfig, "auth: chatgpt", "auth: codex", 1)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cfg.ResolveRole("finder"); err == nil || strings.Contains(err.Error(), "role-secret") {
				t.Fatalf("unsafe override error %v", err)
			}
		})
	}
	cfg, err := Load(writeConfig(t, strings.Replace(subscriptionConfig, "auth: chatgpt", "auth: codex", 1)))
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	wrong, protocol := OAuthBaseURL(AuthChatGPT), "chat"
	for _, changed := range []ResolvedRole{OverrideEndpoint("finder", role, &wrong, nil), OverrideEndpoint("finder", role, nil, &protocol)} {
		if changed.Auth != AuthCodex || changed.APIKey != "" || changed.Credential.Kind != CredentialCodex {
			t.Fatalf("Codex credential rebound: %#v", changed)
		}
		if err := ValidateRole("finder", changed); err == nil {
			t.Fatal("accepted incompatible Codex route flag")
		}
	}
}

func TestCodexProvisionDoesNotImportLegacyAPIKey(t *testing.T) {
	template := strings.Replace(subscriptionConfig, "auth: chatgpt", "auth: codex", 1)
	template = strings.Replace(template, "version: 1", "version: 1\nprovisioning:\n  import_legacy_credential: true", 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(template)) }))
	defer server.Close()
	legacy := filepath.Join(t.TempDir(), "legacy.yaml")
	if err := os.WriteFile(legacy, []byte("gateway:\n  api_key: never-import-secret\nroles:\n  finder:\n    model: old-model\n    api: responses\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Provision(t.Context(), server.URL, path, legacy); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.InferenceProfiles["subscription"]; p.Auth != AuthCodex || p.APIKey != "" {
		t.Fatalf("imported legacy key into Codex profile: %#v", p)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "never-import-secret") {
		t.Fatalf("legacy secret persisted: %v", err)
	}
}

func TestCodexAuthorRoutePreservesIndependentBackend(t *testing.T) {
	cfg := Defaults()
	cfg.InferenceProfiles["finder"] = InferenceProfile{Auth: AuthChatGPT, Model: "gpt-finder", Protocol: "responses", MaxOutputTokens: 1024}
	cfg.InferenceProfiles["independent"] = InferenceProfile{Auth: AuthCodex, Account: "personal", Model: "gpt-independent", Protocol: "responses", MaxOutputTokens: 1024}
	cfg.Roles["finder"] = Role{Profile: "finder", AuthorRoutes: map[string]string{"exact-custom-author": "independent"}}
	finder, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	flip, err := cfg.ResolveFinderFlip(finder, "exact-custom-author")
	if err != nil {
		t.Fatal(err)
	}
	if flip.Role.Auth != AuthCodex || flip.Role.Account != "personal" || flip.Role.BaseURL != OAuthBaseURL(AuthCodex) || flip.Role.APIKey != "" {
		t.Fatalf("bad author-route result: %#v", flip)
	}
}
