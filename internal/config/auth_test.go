package config

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const subscriptionConfig = `version: 1
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

func TestChatGPTProfileIgnoresAmbientKeysAndGateways(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "never-use-this-secret")
	t.Setenv("ANTHROPIC_API_KEY", "other-secret")
	t.Setenv("OPENAI_BASE_URL", "https://gateway.example/v1")
	cfg, err := Load(writeConfig(t, subscriptionConfig))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"finder", "verifier"} {
		role, err := cfg.ResolveRole(name)
		if err != nil {
			t.Fatal(err)
		}
		if role.Auth != AuthChatGPT || role.Account != "default" || role.APIKey != "" || role.BaseURL != defaultOpenAIBaseURL || role.Credential.Kind != CredentialChatGPT {
			t.Fatalf("%s resolved incorrectly: %#v", name, role)
		}
		if err := ValidateRole(name, role); err != nil {
			t.Fatal(err)
		}
		if got := role.Credential.String(); !strings.Contains(got, "ChatGPT OAuth") || strings.Contains(got, "secret") {
			t.Fatalf("credential description: %s", got)
		}
	}
	profile := cfg.InferenceProfiles["subscription"]
	if strings.Contains(profile.DescribeEndpoint(), "OPENAI_BASE_URL") || !strings.Contains(profile.DescribeCredential("finder"), "ChatGPT OAuth") {
		t.Fatalf("misleading subscription description: %#v", profile)
	}
}

func TestChatGPTProfileRejectsIncompatibleSettings(t *testing.T) {
	for _, fields := range []string{
		"auth: unknown", "auth: chatgpt\n    api_key: secret", "auth: chatgpt\n    api_key_env: MY_KEY",
		"auth: chatgpt\n    endpoint: https://gateway.example/v1", "auth: chatgpt\n    endpoint: https://api.openai.com/v1#fragment",
		"auth: chatgpt\n    protocol: chat", "auth: chatgpt\n    protocol: anthropic",
		"auth: chatgpt\n    account: ../escape", "auth: chatgpt\n    account: '-unsafe'", "account: default",
	} {
		t.Run(fields, func(t *testing.T) {
			input := strings.Replace(subscriptionConfig, "auth: chatgpt", fields, 1)
			if _, err := Load(writeConfig(t, input)); err == nil {
				t.Fatal("accepted incompatible auth profile")
			}
		})
	}
}

func TestChatGPTProfileRoleEnvironmentCannotRebindCredentials(t *testing.T) {
	for _, override := range []struct{ name, value string }{
		{"ROAST_FINDER_API_KEY", "role-secret"},
		{"ROAST_FINDER_BASE_URL", "https://gateway.example/v1"},
		{"ROAST_FINDER_PROTOCOL", "anthropic"},
	} {
		t.Run(override.name, func(t *testing.T) {
			t.Setenv(override.name, override.value)
			cfg, err := Load(writeConfig(t, subscriptionConfig))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cfg.ResolveRole("finder"); err == nil || strings.Contains(err.Error(), "role-secret") {
				t.Fatalf("override error: %v", err)
			}
		})
	}
}

func TestChatGPTProfileEndpointFlagsFailClosed(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "never-use-this-secret")
	cfg, err := Load(writeConfig(t, subscriptionConfig))
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	gateway, anthropic := "https://gateway.example/v1", "anthropic"
	for _, changed := range []ResolvedRole{
		OverrideEndpoint("finder", role, &gateway, nil),
		OverrideEndpoint("finder", role, nil, &anthropic),
	} {
		if changed.Auth != AuthChatGPT || changed.APIKey != "" || changed.Credential.Kind != CredentialChatGPT {
			t.Fatalf("subscription was rebound: %#v", changed)
		}
		if err := ValidateRole("finder", changed); err == nil {
			t.Fatal("accepted incompatible subscription flag")
		}
	}
}

func TestChatGPTFinderAuthorRoutePreservesAccount(t *testing.T) {
	input := strings.Replace(subscriptionConfig, "model: account-model", "account: personal\n    model: gpt-reviewer", 1)
	input = strings.Replace(input, "  finder:\n    profile: subscription", "  finder:\n    profile: original\n    author_routes:\n      anthropic: subscription", 1)
	input = strings.Replace(input, "  subscription:", "  original:\n    model: claude-reviewer\n    protocol: anthropic\n    api_key_env: CLAUDE_KEY\n  subscription:", 1)
	cfg, err := Load(writeConfig(t, input))
	if err != nil {
		t.Fatal(err)
	}
	finder, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	flip, err := cfg.ResolveFinderFlip(finder, "claude-author")
	if err != nil {
		t.Fatal(err)
	}
	if flip.Role.Auth != AuthChatGPT || flip.Role.Account != "personal" || flip.Role.APIKey != "" {
		t.Fatalf("bad author-route resolution: %#v", flip)
	}
}

func TestExplicitAPIKeyAuthPreservesExistingBehavior(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "existing-key")
	cfg, err := Load(writeConfig(t, strings.Replace(subscriptionConfig, "auth: chatgpt", "auth: api_key", 1)))
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if role.APIKey != "existing-key" || role.Credential.Kind != CredentialProviderEnv {
		t.Fatalf("API-key behavior changed: %#v", role)
	}
}

func TestChatGPTProvisionDoesNotImportLegacyAPIKey(t *testing.T) {
	template := strings.Replace(subscriptionConfig, "version: 1", "version: 1\nprovisioning:\n  import_legacy_credential: true", 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(template)) }))
	defer server.Close()
	legacy := filepath.Join(t.TempDir(), "legacy.yaml")
	if err := os.WriteFile(legacy, []byte("gateway:\n  api_key: never-import-secret\nroles:\n  finder:\n    model: old-model\n    api: responses\n"), 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "config.yaml")
	if err := Provision(t.Context(), server.URL, destination, legacy); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(destination)
	if err != nil {
		t.Fatal(err)
	}
	profile := cfg.InferenceProfiles["subscription"]
	if profile.Auth != AuthChatGPT || profile.APIKey != "" || profile.APIKeyEnv != "" {
		t.Fatalf("legacy key imported into OAuth profile: %#v", profile)
	}
	data, err := os.ReadFile(destination)
	if err != nil || strings.Contains(string(data), "never-import-secret") {
		t.Fatalf("legacy secret was written to OAuth configuration: %v", err)
	}
}
