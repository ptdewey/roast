package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/chatgpt"
	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/textsafe"
)

// chatGPTSession keeps the command layer independent of the credential format.
// Its public account view cannot expose access, refresh, or ID tokens.
type chatGPTSession interface {
	Login(context.Context, string, func(string) error) (chatgpt.Account, error)
	Status(context.Context, string) (chatgpt.Account, error)
	Token(context.Context, string) (string, error)
	Logout(context.Context, string) error
}

var newChatGPTManager = func() (chatGPTSession, error) { return chatgpt.Default() }

type codexSession interface {
	chatGPTSession
	CodexCredentials(context.Context, string) (string, string, error)
}

var newCodexManager = func() (codexSession, error) { return chatgpt.DefaultCodex() }

func subscriptionManager(provider string) (chatGPTSession, error) {
	switch provider {
	case config.AuthChatGPT:
		return newChatGPTManager()
	case config.AuthCodex:
		return newCodexManager()
	default:
		return nil, &usageError{msg: "--provider must be chatgpt or codex"}
	}
}

func subscriptionAuthError(err error) error {
	var usage *usageError
	if errors.As(err, &usage) {
		return err
	}
	return chatGPTAuthError(err)
}

func subscriptionLabel(provider string) string {
	if provider == config.AuthCodex {
		return "Codex"
	}
	return "ChatGPT"
}

func codexTokenSource(account string) func(context.Context) (string, string, error) {
	factory := newCodexManager
	return func(ctx context.Context) (string, string, error) {
		manager, err := factory()
		if err != nil {
			return "", "", chatGPTAuthError(err)
		}
		token, workspace, err := manager.CodexCredentials(ctx, account)
		return token, workspace, chatGPTAuthError(err)
	}
}

func subscriptionPreflight(ctx context.Context, role config.ResolvedRole) error {
	if role.Auth == config.AuthCodex {
		_, _, err := codexTokenSource(role.Account)(ctx)
		return err
	}
	if role.Auth == config.AuthChatGPT {
		_, err := chatGPTTokenSource(role.Account)(ctx)
		return err
	}
	return nil
}

func chatGPTAuthError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %v", llm.ErrAuth, err)
}

func chatGPTTokenSource(account string) func(context.Context) (string, error) {
	// Capture the factory, not credentials: each request rechecks expiry and can
	// refresh the account under the credential store's cross-process lock.
	factory := newChatGPTManager
	return func(ctx context.Context) (string, error) {
		manager, err := factory()
		if err != nil {
			return "", chatGPTAuthError(err)
		}
		token, err := manager.Token(ctx, account)
		return token, chatGPTAuthError(err)
	}
}

func newAuthCommand(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{Use: "auth", Short: "Manage OpenAI subscription authentication", Args: noArgs, RunE: showHelp}
	var account, provider string
	root.PersistentFlags().StringVar(&account, "account", "default", "local account label within the selected provider")
	root.PersistentFlags().StringVar(&provider, "provider", config.AuthChatGPT, "OAuth provider: chatgpt (app plan sharing) or codex (older Codex flow)")

	var noBrowser bool
	var timeout time.Duration
	login := &cobra.Command{
		Use: "login", Short: "Authorize the selected OpenAI subscription provider in your browser", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 {
				return &usageError{msg: "--timeout must be positive"}
			}
			manager, err := subscriptionManager(provider)
			if err != nil {
				return subscriptionAuthError(err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			identity, err := manager.Login(ctx, account, func(url string) error {
				if _, err := fmt.Fprintf(stderr, "Continue with %s in your browser:\n%s\n", subscriptionLabel(provider), url); err != nil {
					return err
				}
				if !noBrowser {
					if err := openAuthBrowser(ctx, url); err != nil {
						_, _ = fmt.Fprintln(stderr, "Could not open a browser; open the URL above manually.")
					}
				}
				return nil
			})
			if err != nil {
				return chatGPTAuthError(err)
			}
			_, err = fmt.Fprintf(stdout, "Signed in to %s as %s (account %s).\n", subscriptionLabel(provider), textsafe.Line(identity.Email), textsafe.Line(identity.Name))
			return err
		},
	}
	login.Flags().BoolVar(&noBrowser, "no-browser", false, "print the sign-in URL without opening a browser")
	login.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "maximum time to complete browser sign-in")
	root.AddCommand(login)

	var statusJSON bool
	status := &cobra.Command{
		Use: "status", Short: "Show the selected account without exposing credentials", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := subscriptionManager(provider)
			if err != nil {
				return subscriptionAuthError(err)
			}
			identity, err := manager.Status(cmd.Context(), account)
			if err != nil {
				return chatGPTAuthError(err)
			}
			if statusJSON {
				return json.NewEncoder(stdout).Encode(identity)
			}
			_, err = fmt.Fprintf(stdout, "%s account %s: %s (access token expires %s; refreshes automatically).\n", subscriptionLabel(provider), textsafe.Line(identity.Name), textsafe.Line(identity.Email), identity.ExpiresAt.UTC().Format(time.RFC3339))
			return err
		},
	}
	status.Flags().BoolVar(&statusJSON, "json", false, "emit non-secret account information as JSON")
	root.AddCommand(status)
	root.AddCommand(&cobra.Command{
		Use: "logout", Short: "Sign out of the selected subscription session", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := subscriptionManager(provider)
			if err != nil {
				return subscriptionAuthError(err)
			}
			if err := manager.Logout(cmd.Context(), account); err != nil {
				return chatGPTAuthError(err)
			}
			_, err = fmt.Fprintf(stdout, "Signed out of %s account %s.\n", subscriptionLabel(provider), textsafe.Line(account))
			return err
		},
	})
	var modelsJSON bool
	models := &cobra.Command{
		Use: "models", Short: "List models available to the selected subscription account", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			catalog, err := listSubscriptionModels(cmd.Context(), provider, account, subscriptionHTTPClient())
			if err != nil {
				return err
			}
			if modelsJSON {
				return json.NewEncoder(stdout).Encode(catalog)
			}
			for _, model := range catalog {
				if _, err := fmt.Fprintf(stdout, "%s\t%s\n", textsafe.Line(model.Slug), textsafe.Line(model.DisplayName)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	models.Flags().BoolVar(&modelsJSON, "json", false, "emit available model slugs and display names as JSON")
	root.AddCommand(models)
	return root
}

func openAuthBrowser(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{url}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		name, args = "xdg-open", []string{url}
	}
	return exec.CommandContext(ctx, name, args...).Run()
}

func subscriptionHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type chatGPTModel struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

func listChatGPTModels(ctx context.Context, client *http.Client, token string) ([]chatGPTModel, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%w: ChatGPT access token is unavailable", llm.ErrAuth)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// Keep this helper safe even if a caller supplies a different client.
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := copy.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &endpointError{err: errors.New("ChatGPT model catalog request failed")}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: ChatGPT model catalog rejected the session; run roast auth login", llm.ErrAuth)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &endpointError{err: fmt.Errorf("ChatGPT model catalog returned HTTP %d", resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return nil, &endpointError{err: errors.New("could not read bounded ChatGPT model catalog")}
	}
	var body struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, &endpointError{err: errors.New("invalid ChatGPT model catalog response")}
	}
	catalog := make([]chatGPTModel, 0, len(body.Models))
	for _, model := range body.Models {
		if model.Visibility == "list" && model.Slug != "" {
			catalog = append(catalog, chatGPTModel{model.Slug, model.DisplayName})
		}
	}
	return catalog, nil
}
