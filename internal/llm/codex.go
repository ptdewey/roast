package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// CodexClientVersion is the public client protocol version, not a binary dependency.
const CodexClientVersion = "0.162.1"
const codexBaseURL = "https://chatgpt.com/backend-api/codex"

type codexTransport struct{ chatGPTTransport }

func subscriptionMetadataKey(codex bool) string {
	if codex {
		return "roast.codex"
	}
	return "roast.chatgpt"
}

func validateCodexConfig(cfg ClientConfig) error {
	if cfg.Protocol != "responses" || cfg.BaseURL != codexBaseURL {
		return fmt.Errorf("codex authentication requires responses at %s", codexBaseURL)
	}
	if cfg.APIKey != "" || cfg.TokenSource != nil || cfg.ChatGPT {
		return fmt.Errorf("codex authentication cannot be combined with another credential mode")
	}
	if cfg.CodexTokenSource == nil {
		return fmt.Errorf("%w: Codex credentials are not configured", ErrAuth)
	}
	return nil
}

func codexFailure(codes ...string) error {
	for _, code := range codes {
		switch code {
		case "usage_limit_reached", "usage_not_included", "insufficient_quota", "quota_exceeded":
			return fmt.Errorf("%w: Codex subscription usage unavailable; check ChatGPT settings", ErrUsageLimit)
		case "invalid_api_key", "invalid_token", "token_expired", "authentication_error", "access_denied":
			return fmt.Errorf("%w: Codex authorization failed; run roast auth login --provider codex", ErrAuth)
		}
	}
	return nil
}

func validCodexDestination(req *http.Request) bool {
	u := req.URL
	return req.Method == http.MethodPost && u != nil && u.Scheme == "https" && u.Host == "chatgpt.com" &&
		u.Path == "/backend-api/codex/responses" && u.RawPath == "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil &&
		u.Opaque == "" && (req.Host == "" || req.Host == "chatgpt.com")
}

// Credentials must be nonempty single visible ASCII header values, without whitespace.
func validCredentialHeader(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c <= 32 || c >= 127 {
			return false
		}
	}
	return true
}

func validCodexRequestHeaders(headers http.Header) bool {
	for name, values := range headers {
		if name == "" {
			return false
		}
		for _, c := range name {
			if c <= 32 || c >= 127 || strings.ContainsRune("()<>@,;:\"/[]?={}\\", c) {
				return false
			}
		}
		for _, value := range values {
			for _, c := range value {
				if c < 32 && c != '\t' || c == 127 {
					return false
				}
			}
		}
	}
	return true
}

func setCodexHeaders(req *http.Request, body []byte, accountID string) error {
	var payload struct {
		Model   string `json:"model"`
		Session string `json:"prompt_cache_key"`
	}
	if json.Unmarshal(body, &payload) != nil || !validCredentialHeader(payload.Model) || !validCredentialHeader(payload.Session) || strings.ContainsAny(payload.Model, ";=") {
		return fmt.Errorf("%w: invalid Codex routing metadata", ErrEndpoint)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("ChatGPT-Account-Id", accountID)
	req.Header.Set("Originator", "roast")
	req.Header.Set("Version", CodexClientVersion)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("X-Codex-Routing-Hint", "model="+payload.Model)
	req.Header.Set("Session_id", payload.Session)
	req.Header.Set("X-Client-Request-Id", payload.Session)
	return nil
}

// Codex sometimes returns valid SSE with no Content-Type. Normalize that case
// before the SDK and capture transport see it, without draining the stream.
// Explicit non-SSE content types are never overridden.
func prepareCodexEventStream(resp *http.Response) error {
	fail := func() error {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return fmt.Errorf("%w: Codex inference did not return an event stream", ErrEndpoint)
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "text/event-stream" {
			return fail()
		}
	} else {
		if resp.Body == nil {
			return fail()
		}
		const prefixLimit = 4096
		original := resp.Body
		reader := bufio.NewReaderSize(original, prefixLimit)
		prefix := make([]byte, 0, prefixLimit)
		for {
			line, err := reader.ReadSlice('\n')
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
				_ = original.Close()
				if errors.Is(err, context.Canceled) {
					return context.Canceled
				}
				if errors.Is(err, context.DeadlineExceeded) {
					return context.DeadlineExceeded
				}
				return fmt.Errorf("%w: could not inspect Codex event stream", ErrEndpoint)
			}
			if len(prefix)+len(line) > prefixLimit {
				return fail()
			}
			prefix = append(prefix, line...)
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 || trimmed[0] == ':' {
				if err != nil || len(prefix) >= prefixLimit {
					return fail()
				}
				continue
			}
			// Inspect framing, not response content. The existing decoder and
			// managed model still require a successful terminal completion event.
			valid := false
			if bytes.HasPrefix(trimmed, []byte("event:")) {
				event := bytes.TrimSpace(trimmed[len("event:"):])
				valid = bytes.HasPrefix(event, []byte("response.")) || bytes.Equal(event, []byte("error"))
			} else if bytes.HasPrefix(trimmed, []byte("data:")) {
				data := bytes.TrimSpace(trimmed[len("data:"):])
				valid = bytes.HasPrefix(data, []byte("{"))
			}
			if !valid {
				return fail()
			}
			resp.Body = &codexPrefixedBody{Reader: io.MultiReader(bytes.NewReader(prefix), reader), Closer: original}
			break
		}
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	resp.Header.Set("Content-Type", "text/event-stream")
	return nil
}

type codexPrefixedBody struct {
	io.Reader
	io.Closer
}
