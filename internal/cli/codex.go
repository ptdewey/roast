package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bluesky-social/roast/internal/buildinfo"
	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/llm"
)

func listSubscriptionModels(ctx context.Context, provider, account string, client *http.Client) ([]chatGPTModel, error) {
	switch provider {
	case config.AuthChatGPT:
		token, err := chatGPTTokenSource(account)(ctx)
		if err != nil {
			return nil, err
		}
		return listChatGPTModels(ctx, client, token)
	case config.AuthCodex:
		token, workspace, err := codexTokenSource(account)(ctx)
		if err != nil {
			return nil, err
		}
		return listCodexModels(ctx, client, token, workspace)
	default:
		return nil, &usageError{msg: "--provider must be chatgpt or codex"}
	}
}

func listCodexModels(ctx context.Context, client *http.Client, token, workspace string) ([]chatGPTModel, error) {
	if strings.TrimSpace(token) == "" || strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("%w: Codex session unavailable; run roast auth login --provider codex", llm.ErrAuth)
	}
	endpoint := config.OAuthBaseURL(config.AuthCodex) + "/models?" + url.Values{"client_version": {llm.CodexClientVersion}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-Id", workspace)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("Originator", "roast")
	req.Header.Set("Version", llm.CodexClientVersion)
	req.Header.Set("User-Agent", "roast/"+buildinfo.Version)
	req.Header.Set("Accept", "application/json")
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := copy.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &endpointError{err: errors.New("codex model catalog request failed")}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: Codex model catalog rejected the session; run roast auth login --provider codex", llm.ErrAuth)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &endpointError{err: fmt.Errorf("codex model catalog returned HTTP %d", resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return nil, &endpointError{err: errors.New("could not read bounded Codex model catalog")}
	}
	var body struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, &endpointError{err: errors.New("invalid Codex model catalog response")}
	}
	catalog := make([]chatGPTModel, 0, len(body.Models))
	for _, model := range body.Models {
		if model.Visibility == "list" && model.Slug != "" {
			catalog = append(catalog, chatGPTModel{model.Slug, model.DisplayName})
		}
	}
	return catalog, nil
}
