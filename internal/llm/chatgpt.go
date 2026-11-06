package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"charm.land/fantasy"
)

const chatGPTBaseURL = "https://api.openai.com/v1"
const chatGPTNamespace = "roast"

func validateChatGPTConfig(cfg ClientConfig) error {
	if cfg.Protocol != "responses" || cfg.BaseURL != chatGPTBaseURL {
		return fmt.Errorf("ChatGPT authentication requires responses at %s", chatGPTBaseURL)
	}
	if cfg.APIKey != "" {
		return fmt.Errorf("ChatGPT authentication cannot be combined with an API key")
	}
	if cfg.TokenSource == nil {
		return fmt.Errorf("%w: ChatGPT token source is not configured", ErrAuth)
	}
	return nil
}

func validChatGPTDestination(req *http.Request) bool {
	u := req.URL
	return req.Method == http.MethodPost && u != nil && u.Scheme == "https" && u.Host == "api.openai.com" &&
		u.Path == "/v1/responses" && u.RawPath == "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil &&
		u.Opaque == "" && (req.Host == "" || req.Host == "api.openai.com")
}

type chatGPTTransport struct {
	base        http.RoundTripper
	token       func(context.Context) (string, error)
	codex       bool
	credentials func(context.Context) (string, string, error)
}

type chatGPTHTTPError struct {
	StatusCode int
	cause      error
}

func (e *chatGPTHTTPError) Error() string { return e.cause.Error() }
func (e *chatGPTHTTPError) Unwrap() error { return e.cause }

func chatGPTFailure(code string) error {
	switch code {
	case "subscription_sharing_usage_limit_exceeded", "usage_limit_reached", "insufficient_quota":
		return fmt.Errorf("%w: ChatGPT plan usage limit reached; see ChatGPT settings → Usage", ErrUsageLimit)
	case "subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable":
		return fmt.Errorf("%w: ChatGPT plan usage temporarily unavailable", ErrEndpoint)
	case "subscription_sharing_invalid_user", "chatpass_v2_scope_not_authorized", "chatpass_v2_invalid_authorization_context", "invalid_api_key", "invalid_token", "token_expired", "authentication_error", "access_denied":
		return fmt.Errorf("%w: ChatGPT authorization failed; check selected account and permissions", ErrAuth)
	case "subscription_sharing_user_not_eligible", "subscription_sharing_route_not_supported", "subscription_sharing_unsupported_capability":
		return fmt.Errorf("%w: ChatGPT plan policy or capability does not permit this request", ErrEndpoint)
	}
	return nil
}

type chatGPTReplayKey struct{}
type chatGPTCallState struct {
	codex        bool
	replay       map[string]*chatGPTToolMetadata
	completed    map[string]*chatGPTToolMetadata
	pending      []json.RawMessage
	failure      error
	instructions []string
}

// Metadata lives only in the current conversation, not in a global response cache.
// SIWC advertises one namespace; Codex advertises flat tools. The mode-specific
// metadata key keeps their replay state isolated; local tool names stay unqualified.
type chatGPTToolMetadata struct {
	Namespace string
	Reasoning []json.RawMessage
}

func (*chatGPTToolMetadata) Options() {}
func (m *chatGPTToolMetadata) UnmarshalJSON(data []byte) error {
	type plain chatGPTToolMetadata
	return json.Unmarshal(data, (*plain)(m))
}
func (m *chatGPTToolMetadata) MarshalJSON() ([]byte, error) {
	type plain chatGPTToolMetadata
	return json.Marshal((*plain)(m))
}

type chatGPTModel struct {
	fantasy.LanguageModel
	codex bool
}

func (m *chatGPTModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	state := &chatGPTCallState{codex: m.codex, replay: map[string]*chatGPTToolMetadata{}, completed: map[string]*chatGPTToolMetadata{}}
	for _, msg := range call.Prompt {
		for _, content := range msg.Content {
			if msg.Role == fantasy.MessageRoleSystem {
				if text, ok := fantasy.AsContentType[fantasy.TextPart](content); ok {
					state.instructions = append(state.instructions, text.Text)
				}
			}
			if part, ok := fantasy.AsContentType[fantasy.ToolCallPart](content); ok {
				if meta, ok := part.ProviderOptions[subscriptionMetadataKey(m.codex)].(*chatGPTToolMetadata); ok {
					state.replay[part.ToolCallID] = meta
				}
			}
		}
	}
	stream, err := m.LanguageModel.Stream(context.WithValue(ctx, chatGPTReplayKey{}, state), call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range stream {
			if state.failure != nil {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: state.failure})
				return
			}
			if part.Type == fantasy.StreamPartTypeToolCall {
				if meta := state.completed[part.ID]; meta != nil {
					if part.ProviderMetadata == nil {
						part.ProviderMetadata = fantasy.ProviderMetadata{}
					}
					part.ProviderMetadata[subscriptionMetadataKey(m.codex)] = meta
				}
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

func (t *chatGPTTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Do not acquire or expose a credential for an unapproved destination, including
	// SDK environment overrides and redirects. The http.Client also refuses redirects.
	if !t.codex && !validChatGPTDestination(req) || t.codex && !validCodexDestination(req) {
		return nil, fmt.Errorf("%w: rejected ChatGPT inference destination", ErrEndpoint)
	}
	if t.codex && !validCodexRequestHeaders(req.Header) {
		return nil, fmt.Errorf("%w: invalid Codex request headers", ErrEndpoint)
	}
	req = req.Clone(req.Context())
	body, err := normalizeSubscriptionRequest(req, t.codex)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEndpoint, err)
	}
	if t.codex {
		if err := setCodexHeaders(req, body, ""); err != nil {
			return nil, err
		}
	}
	var token, accountID string
	if t.codex {
		token, accountID, err = t.credentials(req.Context())
	} else {
		token, err = t.token(req.Context())
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	if err != nil || !validCredentialHeader(token) || t.codex && !validCredentialHeader(accountID) {
		// Never include provider errors or token text in diagnostics.
		if t.codex {
			return nil, fmt.Errorf("%w: Codex credentials unavailable; run roast auth login --provider codex", ErrAuth)
		}
		return nil, fmt.Errorf("%w: ChatGPT credentials unavailable; run roast auth login", ErrAuth)
	}
	if t.codex {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Del("OpenAI-Organization")
	req.Header.Del("OpenAI-Project")
	req.Header.Del("api-key")
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	resp, err := t.base.RoundTrip(req)
	if t.codex && err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		return nil, fmt.Errorf("%w: Codex transport failed", ErrEndpoint)
	}
	if err == nil && resp != nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		// Neither SDK diagnostics nor capture artifacts may retain error bodies: a
		// remote endpoint can echo a bearer token in diagnostic text.
		var problem struct {
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&problem)
		_ = resp.Body.Close()
		cause := chatGPTFailure(problem.Error.Code)
		if t.codex {
			cause = codexFailure(problem.Error.Code, problem.Error.Type)
		}
		if cause == nil {
			cause = fmt.Errorf("%w: ChatGPT inference returned HTTP %d", ErrEndpoint, resp.StatusCode)
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				cause = fmt.Errorf("%w: ChatGPT authorization denied", ErrAuth)
			}
		}
		return nil, &chatGPTHTTPError{StatusCode: resp.StatusCode, cause: cause}
	}
	if t.codex && err == nil && resp != nil {
		if err := prepareCodexEventStream(resp); err != nil {
			return nil, err
		}
	}
	if err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		state, _ := req.Context().Value(chatGPTReplayKey{}).(*chatGPTCallState)
		if state != nil {
			resp.Body = &chatGPTStreamBody{ReadCloser: resp.Body, state: state}
		}
	}
	return resp, err
}

func normalizeChatGPTRequest(req *http.Request) ([]byte, error) {
	return normalizeSubscriptionRequest(req, false)
}

func normalizeSubscriptionRequest(req *http.Request, codex bool) ([]byte, error) {
	if req.Body == nil {
		return nil, fmt.Errorf("missing ChatGPT request body")
	}
	defer req.Body.Close()
	var body map[string]json.RawMessage
	limited := &io.LimitedReader{R: req.Body, N: maxCapturedResponseSize + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(&body); err != nil || body == nil {
		return nil, fmt.Errorf("invalid ChatGPT request body")
	}
	if codex {
		var extra any
		if decoder.Decode(&extra) != io.EOF || limited.N <= 0 {
			return nil, fmt.Errorf("invalid Codex request body")
		}
		for name := range body {
			switch name {
			case "model", "input", "instructions", "tools", "tool_choice", "text", "reasoning":
			default:
				delete(body, name)
			}
		}
	}
	for _, name := range []string{"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata", "moderation", "multi_agent", "prompt", "prompt_cache_retention", "safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user", "previous_response_id"} {
		delete(body, name)
	}
	// MaxOutputTokens is still a local ledger reservation. Subscription requests
	// cannot set a server-side output cap, so actual usage can exceed that estimate.
	body["store"] = json.RawMessage(`false`)
	body["stream"] = json.RawMessage(`true`)
	body["include"] = json.RawMessage(`["reasoning.encrypted_content"]`)
	var input []map[string]json.RawMessage
	if err := json.Unmarshal(body["input"], &input); err != nil || input == nil {
		return nil, fmt.Errorf("ChatGPT input must be an array")
	}
	state, _ := req.Context().Value(chatGPTReplayKey{}).(*chatGPTCallState)
	replay := make([]map[string]json.RawMessage, 0, len(input))
	if state != nil && len(state.instructions) > 0 {
		body["instructions"], _ = json.Marshal(strings.Join(state.instructions, "\n"))
	}
	for _, item := range input {
		if state != nil && len(state.instructions) > 0 && (string(item["role"]) == `"system"` || string(item["role"]) == `"developer"`) {
			continue
		}
		if string(item["role"]) == `"system"` {
			item["role"] = json.RawMessage(`"developer"`)
		}
		if string(item["type"]) == `"function_call"` {
			if codex && len(item["namespace"]) > 0 && string(item["namespace"]) != `""` {
				return nil, fmt.Errorf("unexpected Codex tool namespace")
			}
			namespace := chatGPTNamespace
			if codex {
				namespace = ""
			}
			var id string
			_ = json.Unmarshal(item["call_id"], &id)
			if state != nil {
				if meta := state.replay[id]; meta != nil {
					namespace = meta.Namespace
					for _, reasoning := range meta.Reasoning {
						var entry map[string]json.RawMessage
						if err := json.Unmarshal(reasoning, &entry); err != nil {
							return nil, fmt.Errorf("invalid reasoning replay")
						}
						replay = append(replay, entry)
					}
				}
			}
			if codex && namespace != "" || !codex && namespace != chatGPTNamespace {
				return nil, fmt.Errorf("unsupported ChatGPT tool namespace")
			}
			if codex {
				delete(item, "namespace")
			} else {
				item["namespace"], _ = json.Marshal(namespace)
			}
		}
		replay = append(replay, item)
	}
	body["input"], _ = json.Marshal(replay)
	var tools []map[string]json.RawMessage
	if raw := body["tools"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("invalid ChatGPT tools")
		}
	}
	if len(tools) > 0 {
		for _, tool := range tools {
			if string(tool["type"]) != `"function"` {
				return nil, fmt.Errorf("unsupported ChatGPT tool type")
			}
		}
		if codex {
			for _, tool := range tools {
				tool["strict"] = json.RawMessage(`null`)
			}
			body["tools"], _ = json.Marshal(tools)
		} else {
			// Shape follows openai-go responses.NamespaceToolParam. Function names and
			// call IDs remain unchanged; namespace is a separate function-call field.
			body["tools"], _ = json.Marshal([]any{map[string]any{"type": "namespace", "name": chatGPTNamespace, "description": "Roast local review tools", "tools": tools}})
		}
	}
	if codex {
		body["parallel_tool_calls"] = json.RawMessage(`true`)
		if len(body["tool_choice"]) == 0 {
			body["tool_choice"] = json.RawMessage(`"auto"`)
		}
		var text map[string]json.RawMessage
		_ = json.Unmarshal(body["text"], &text)
		if text == nil {
			text = map[string]json.RawMessage{}
		}
		text["verbosity"] = json.RawMessage(`"low"`)
		body["text"], _ = json.Marshal(text)
		if len(body["reasoning"]) == 0 {
			body["reasoning"] = json.RawMessage(`{"effort":"medium","summary":"auto"}`)
		}
		session, _ := req.Context().Value(sessionContextKey{}).(string)
		if session == "" {
			session = "roast-" + rand.Text()
		}
		body["prompt_cache_key"], _ = json.Marshal(session)
	}
	return json.Marshal(body)
}

// Observe only completed output items, without changing or eagerly draining SSE.
// Pending data is bounded; metadata is owned by this single model call and copied
// into its tool-call history, so concurrent conversations cannot mix context.
type chatGPTStreamBody struct {
	io.ReadCloser
	state *chatGPTCallState
	line  []byte
	total int
}

func (b *chatGPTStreamBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.state.codex && err != nil && err != io.EOF {
		switch {
		case errors.Is(err, context.Canceled):
			err = context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			err = context.DeadlineExceeded
		default:
			err = fmt.Errorf("%w: Codex stream read failed", ErrEndpoint)
		}
		b.state.failure = err
		return 0, err
	}
	b.total += n
	if b.total > maxCapturedResponseSize {
		b.state.failure = fmt.Errorf("%w: oversized ChatGPT stream", ErrEndpoint)
		return 0, b.state.failure
	}
	for _, c := range p[:n] {
		if c == '\n' {
			if eventErr := b.event(b.line); eventErr != nil {
				b.state.failure = eventErr
				return 0, eventErr
			}
			b.line = b.line[:0]
		} else {
			if len(b.line) >= maxCapturedResponseSize {
				b.state.failure = fmt.Errorf("%w: oversized ChatGPT stream event", ErrEndpoint)
				return 0, b.state.failure
			}
			b.line = append(b.line, c)
		}
	}
	return n, err
}
func (b *chatGPTStreamBody) event(line []byte) error {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil
	}
	var event struct {
		Type  string          `json:"type"`
		Item  json.RawMessage `json:"item"`
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
		Code     string `json:"code"`
		Response struct {
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil {
		return nil
	}
	if event.Type == "response.failed" || event.Type == "error" {
		code := event.Response.Error.Code
		if code == "" {
			code = event.Error.Code
		}
		if code == "" {
			code = event.Code
		}
		b.state.failure = chatGPTFailure(code)
		if b.state.codex {
			b.state.failure = codexFailure(code, event.Error.Type, event.Response.Error.Type)
		}
		if b.state.failure == nil {
			b.state.failure = fmt.Errorf("%w: ChatGPT inference stream failed", ErrEndpoint)
		}
	}
	if event.Type != "response.output_item.done" {
		return nil
	}
	var item struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Namespace string `json:"namespace"`
	}
	if json.Unmarshal(event.Item, &item) != nil {
		return nil
	}
	switch item.Type {
	case "reasoning":
		b.state.pending = append(b.state.pending, append(json.RawMessage(nil), event.Item...))
	case "function_call":
		if b.state.codex && item.Namespace != "" || !b.state.codex && item.Namespace != chatGPTNamespace {
			return fmt.Errorf("%w: unexpected ChatGPT tool namespace", ErrEndpoint)
		}
		b.state.completed[item.CallID] = &chatGPTToolMetadata{Namespace: item.Namespace, Reasoning: b.state.pending}
		b.state.pending = nil
	}
	return nil
}
