package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	fantasyanthropic "charm.land/fantasy/providers/anthropic"
	fantasyopenai "charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/bluesky-social/roast/internal/buildinfo"
)

const (
	defaultMaxAttempts      = 4
	defaultInputByteCeiling = 3_000_000
	maxCapturedResponseSize = 16 << 20
)

// ClientConfig configures one finder or verifier model.
type ClientConfig struct {
	Role    string
	BaseURL string
	APIKey  string
	// ChatGPT (SIWC) and Codex (direct subscription OAuth) are mutually exclusive.
	// Neither mode uses APIKey or ambient credentials; Codex credentials are fetched
	// coherently as a token/workspace pair immediately before each guarded request.
	ChatGPT          bool
	Codex            bool
	CodexTokenSource func(context.Context) (token string, accountID string, err error)
	TokenSource      func(context.Context) (string, error)
	Model            string
	MaxOutputTokens  int
	MaxAttempts      int
	RequestTimeout   time.Duration
	Protocol         string
	Ledger           *Ledger
	CaptureResponse  func(RawResponse) error
}

// RawResponse is a provider HTTP response body without requests or headers.
type RawResponse struct {
	Role        string
	Protocol    string
	Model       string
	ContentType string
	Body        []byte
}

type client struct {
	cfg       ClientConfig
	model     fantasy.LanguageModel
	transport *captureTransport

	mu    sync.Mutex
	usage Usage
}

// New constructs the Fantasy provider model selected by cfg.Protocol.
func New(ctx context.Context, cfg ClientConfig) (Client, error) {
	if cfg.MaxOutputTokens <= 0 {
		return nil, fmt.Errorf("%s: max output tokens must be positive", cfg.Model)
	}
	if cfg.ChatGPT && cfg.Codex {
		return nil, fmt.Errorf("ChatGPT and Codex authentication are mutually exclusive")
	}
	if !cfg.ChatGPT && cfg.TokenSource != nil || !cfg.Codex && cfg.CodexTokenSource != nil {
		return nil, fmt.Errorf("subscription token source does not match authentication mode")
	}
	if cfg.Codex {
		if err := validateCodexConfig(cfg); err != nil {
			return nil, err
		}
	} else if cfg.ChatGPT {
		if err := validateChatGPTConfig(cfg); err != nil {
			return nil, err
		}
	} else if strings.TrimSpace(cfg.APIKey) == "" {
		// Fantasy omits an empty key, and the SDKs then fall back to ambient
		// credentials: environment variables, profile files, or federation.
		return nil, fmt.Errorf("%s: API key is not configured", cfg.Model)
	}
	sdkKey := cfg.APIKey
	if cfg.ChatGPT || cfg.Codex {
		// A non-secret placeholder prevents SDK fallback to ambient credentials.
		// The guarded transport always replaces it immediately before sending.
		sdkKey = "roast-chatgpt-dynamic-auth"
	}
	envHeaders := sdkEnvironmentHeaders(cfg.Protocol)
	httpClient := providerHTTPClient(cfg, envHeaders)
	var (
		provider fantasy.Provider
		err      error
	)
	switch cfg.Protocol {
	case "", "chat":
		provider, err = openaicompat.New(
			openaicompat.WithAPIKey(cfg.APIKey),
			openaicompat.WithBaseURL(cfg.BaseURL),
			openaicompat.WithHTTPClient(httpClient),
			openaicompat.WithUserAgent(userAgent()),
			openaicompat.WithSDKOptions(deleteHeaders(envHeaders)...),
		)
	case "responses":
		provider, err = fantasyopenai.New(
			fantasyopenai.WithAPIKey(sdkKey),
			fantasyopenai.WithBaseURL(cfg.BaseURL),
			fantasyopenai.WithHTTPClient(httpClient),
			fantasyopenai.WithUserAgent(userAgent()),
			fantasyopenai.WithSDKOptions(deleteHeaders(envHeaders)...),
			fantasyopenai.WithUseResponsesAPI(),
			fantasyopenai.WithResponsesAPIFunc(func(string) bool { return true }),
		)
	case "anthropic":
		// The Anthropic SDK appends /v1/messages itself; Roast profiles use the
		// same base-URL convention as the OpenAI protocols and include /v1.
		baseURL := strings.TrimSuffix(strings.TrimRight(cfg.BaseURL, "/"), "/v1")
		provider, err = fantasyanthropic.New(
			fantasyanthropic.WithAPIKey(cfg.APIKey),
			fantasyanthropic.WithBaseURL(baseURL),
			fantasyanthropic.WithHTTPClient(httpClient),
			fantasyanthropic.WithUserAgent(userAgent()),
		)
	default:
		return nil, fmt.Errorf("unsupported inference protocol %q", cfg.Protocol)
	}
	if err != nil {
		return nil, fmt.Errorf("initialize %s provider for model %q: %w", protocolName(cfg), cfg.Model, err)
	}
	mdl, err := provider.LanguageModel(ctx, cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("initialize %s model %q: %w", protocolName(cfg), cfg.Model, err)
	}
	if cfg.ChatGPT || cfg.Codex {
		mdl = &chatGPTModel{LanguageModel: mdl, codex: cfg.Codex}
	}
	return &client{cfg: cfg, model: mdl, transport: httpClient.Transport.(*captureTransport)}, nil
}

func userAgent() string { return "roast/" + buildinfo.Version }

func (c *client) Name() string { return c.cfg.Model }

func (c *client) Usage() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage
}

func (c *client) addUsage(providerUsage fantasy.Usage) Usage {
	input := max(int(providerUsage.InputTokens), 0)
	cacheRead := max(int(providerUsage.CacheReadTokens), 0)
	cacheWrite := max(int(providerUsage.CacheCreationTokens), 0)
	uncached := input
	// OpenAI-shaped protocols report cached input as a subset of input_tokens;
	// Anthropic reports input, cache-read, and cache-creation as disjoint counts.
	if c.cfg.Protocol == "" || c.cfg.Protocol == "chat" || c.cfg.Protocol == "responses" {
		cacheRead = min(cacheRead, input)
		cacheWrite = min(cacheWrite, input-cacheRead)
		uncached = input - cacheRead - cacheWrite
	}
	u := Usage{
		UncachedInputTokens: uncached,
		CachedInputTokens:   cacheRead,
		CacheWriteTokens:    cacheWrite,
		OutputTokens:        max(int(providerUsage.OutputTokens), 0),
		Calls:               1,
	}
	c.mu.Lock()
	c.usage.UncachedInputTokens += u.UncachedInputTokens
	c.usage.CachedInputTokens += u.CachedInputTokens
	c.usage.CacheWriteTokens += u.CacheWriteTokens
	c.usage.OutputTokens += u.OutputTokens
	c.usage.Calls += u.Calls
	c.mu.Unlock()
	return u
}

func maxAttempts(cfg ClientConfig) int {
	if cfg.MaxAttempts > 0 {
		return cfg.MaxAttempts
	}
	return defaultMaxAttempts
}

func protocolName(cfg ClientConfig) string {
	if cfg.Protocol == "" {
		return "chat"
	}
	return cfg.Protocol
}

type captureError struct{ err error }

type sessionContextKey struct{}

// Each model attempt owns its response body, including when a provider stops
// reading at a terminal SSE event rather than EOF. Never share bodies between
// concurrent verifier calls.
type responseBodyKey struct{}
type responseBody struct{ body io.ReadCloser }

func (b *responseBody) close() error {
	if b.body == nil {
		return nil
	}
	return b.body.Close()
}

func (e *captureError) Error() string { return e.err.Error() }
func (e *captureError) Unwrap() error { return e.err }

type captureTransport struct {
	base http.RoundTripper
	cfg  ClientConfig
	// stripHeaders are SDK environment headers removed from every request.
	// Only the Anthropic provider needs this hook: Fantasy exposes no SDK
	// request options for it.
	stripHeaders []string
	mu           sync.Mutex
	failed       error
}

func (t *captureTransport) fail(err error) error {
	wrapped := &captureError{err: err}
	t.mu.Lock()
	t.failed = wrapped
	t.mu.Unlock()
	return wrapped
}

func (t *captureTransport) failure() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failed
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	failed := t.failed
	t.mu.Unlock()
	if failed != nil {
		return nil, failed
	}
	sessionID, hasSession := req.Context().Value(sessionContextKey{}).(string)
	if hasSession || len(t.stripHeaders) > 0 {
		req = req.Clone(req.Context())
		for _, name := range t.stripHeaders {
			req.Header.Del(name)
		}
		if hasSession {
			req.Header.Set("X-Agent-Session-Id", sessionID)
		}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if owner, ok := req.Context().Value(responseBodyKey{}).(*responseBody); ok {
		defer func() { owner.body = resp.Body }()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || t.cfg.CaptureResponse == nil {
		return resp, nil
	}
	if resp.ContentLength > maxCapturedResponseSize {
		_ = resp.Body.Close()
		return nil, t.fail(fmt.Errorf("capture %s provider response: body exceeds %d-byte limit", t.cfg.Role, maxCapturedResponseSize))
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if contentType == "text/event-stream" {
		resp.Body = &streamCaptureBody{body: resp.Body, transport: t}
		return resp, nil
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxCapturedResponseSize+1))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if len(body) > maxCapturedResponseSize {
		return nil, t.fail(fmt.Errorf("capture %s provider response: body exceeds %d-byte limit", t.cfg.Role, maxCapturedResponseSize))
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if !json.Valid(body) {
		return nil, t.fail(fmt.Errorf("capture %s provider response: %s returned invalid raw JSON", t.cfg.Role, t.cfg.Model))
	}
	captureErr := t.cfg.CaptureResponse(RawResponse{
		Role: t.cfg.Role, Protocol: protocolName(t.cfg), Model: t.cfg.Model,
		ContentType: "application/json", Body: body,
	})
	if captureErr == nil {
		return resp, nil
	}
	return nil, t.fail(fmt.Errorf("capture %s provider response: %w", t.cfg.Role, captureErr))
}

// Tee raw SSE bytes as Fantasy consumes them. Do not parse provider events,
// eagerly drain the stream, or reconstruct a non-streaming provider response.
type streamCaptureBody struct {
	body      io.ReadCloser
	transport *captureTransport
	data      bytes.Buffer
	err       error
	closeOnce sync.Once
	closeErr  error
}

func (b *streamCaptureBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.body.Read(p)
	if n > maxCapturedResponseSize-b.data.Len() {
		b.err = b.transport.fail(fmt.Errorf("capture %s provider response: body exceeds %d-byte limit", b.transport.cfg.Role, maxCapturedResponseSize))
		return 0, b.err
	}
	_, _ = b.data.Write(p[:n])
	return n, err
}

func (b *streamCaptureBody) Close() error {
	b.closeOnce.Do(func() {
		b.closeErr = b.body.Close()
		if b.err != nil {
			b.closeErr = b.err
			return
		}
		cfg := b.transport.cfg
		if err := cfg.CaptureResponse(RawResponse{
			Role: cfg.Role, Protocol: protocolName(cfg), Model: cfg.Model,
			ContentType: "text/event-stream", Body: append([]byte(nil), b.data.Bytes()...),
		}); err != nil {
			b.closeErr = b.transport.fail(fmt.Errorf("capture %s provider response: %w", cfg.Role, err))
		}
	})
	return b.closeErr
}

func providerHTTPClient(cfg ClientConfig, envHeaders []string) *http.Client {
	var base http.RoundTripper = http.DefaultTransport
	if cfg.ChatGPT {
		base = &chatGPTTransport{base: base, token: cfg.TokenSource}
	}
	if cfg.Codex {
		base = &codexTransport{chatGPTTransport{base: base, codex: true, credentials: cfg.CodexTokenSource}}
	}
	transport := &captureTransport{base: base, cfg: cfg}
	client := &http.Client{Transport: transport}
	if cfg.Protocol == "anthropic" || cfg.ChatGPT || cfg.Codex {
		transport.stripHeaders = envHeaders
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return client
}

func normalizeProviderError(ctx context.Context, modelName string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %s: %w", ErrEndpoint, modelName, err)
	}
	if errors.Is(err, ErrAuth) || errors.Is(err, ErrEndpoint) || errors.Is(err, ErrBudgetExhausted) || errors.Is(err, ErrUsageLimit) {
		return err
	}
	if status, ok := errorStatus(err); ok && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		return fmt.Errorf("%w: %v; check the configured API key", ErrAuth, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrEndpoint, modelName, err)
}

// errorStatus extracts public status fields from provider errors without
// coupling the rest of Roast to a provider SDK's error type.
func errorStatus(err error) (int, bool) {
	for err != nil {
		v := reflect.ValueOf(err)
		if v.Kind() == reflect.Pointer && !v.IsNil() {
			v = v.Elem()
		}
		if v.IsValid() && v.Kind() == reflect.Struct {
			for _, name := range []string{"StatusCode", "HTTPStatusCode"} {
				field := v.FieldByName(name)
				if field.IsValid() && field.Kind() == reflect.Int && field.CanInt() && field.Int() > 0 {
					return int(field.Int()), true
				}
			}
		}
		err = errors.Unwrap(err)
	}
	return 0, false
}
