package config

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	AuthAPIKey  = "api_key"
	AuthChatGPT = "chatgpt"
	AuthCodex   = "codex"
)

var accountNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// IsOAuth reports whether a profile uses a stored OpenAI subscription session.
func IsOAuth(auth string) bool { return auth == AuthChatGPT || auth == AuthCodex }

// OAuthBaseURL returns the only inference endpoint accepted for an OAuth mode.
// Unknown modes return an empty endpoint, never an implicit fallback.
func OAuthBaseURL(auth string) string {
	switch auth {
	case AuthChatGPT:
		return defaultOpenAIBaseURL
	case AuthCodex:
		return "https://chatgpt.com/backend-api/codex"
	default:
		return ""
	}
}

func oauthCredential(auth, account string) CredentialSource {
	kind := CredentialChatGPT
	if auth == AuthCodex {
		kind = CredentialCodex
	}
	return CredentialSource{Kind: kind, Account: chatGPTAccount(account)}
}

func chatGPTAccount(account string) string {
	if account == "" {
		return "default"
	}
	return account
}

func validateProfileAuth(name string, profile *InferenceProfile) error {
	if profile.Auth != "" && profile.Auth != AuthAPIKey && !IsOAuth(profile.Auth) {
		return fmt.Errorf("inference_profiles.%s.auth must be api_key, chatgpt, or codex", name)
	}
	if !IsOAuth(profile.Auth) {
		if profile.Account != "" {
			return fmt.Errorf("inference_profiles.%s.account requires auth: chatgpt or codex", name)
		}
		return nil
	}
	profile.Account = chatGPTAccount(profile.Account)
	if !accountNamePattern.MatchString(profile.Account) {
		return fmt.Errorf("inference_profiles.%s.account must be 1-64 letters, digits, hyphens, or underscores, starting with a letter or digit", name)
	}
	if profile.APIKey != "" || profile.APIKeyEnv != "" {
		return fmt.Errorf("inference_profiles.%s cannot combine auth: %s with api_key or api_key_env", name, profile.Auth)
	}
	return validateOAuthRoute(ResolvedRole{Auth: profile.Auth, Account: profile.Account, BaseURL: profile.Endpoint(), Protocol: profile.Protocol})
}

func validateOAuthRoute(role ResolvedRole) error {
	if !IsOAuth(role.Auth) || role.Protocol != "responses" || strings.TrimRight(role.BaseURL, "/") != OAuthBaseURL(role.Auth) {
		return fmt.Errorf("auth: %s requires protocol: responses at %s; subscription tokens cannot be sent to other endpoints", role.Auth, OAuthBaseURL(role.Auth))
	}
	if role.APIKey != "" {
		return fmt.Errorf("auth: %s cannot be combined with an API key", role.Auth)
	}
	if !accountNamePattern.MatchString(chatGPTAccount(role.Account)) {
		return fmt.Errorf("invalid subscription account label")
	}
	return nil
}
