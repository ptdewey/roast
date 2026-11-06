package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCodexCredentialModes(t *testing.T) {
	source := func(context.Context) (string, string, error) { return "secret", "workspace", nil }
	token := func(context.Context) (string, error) { return "secret", nil }
	for _, cfg := range []ClientConfig{
		{Codex: true, ChatGPT: true, CodexTokenSource: source, TokenSource: token},
		{Codex: true, CodexTokenSource: source, TokenSource: token},
		{ChatGPT: true, CodexTokenSource: source, TokenSource: token},
		{APIKey: "key", CodexTokenSource: source},
		{APIKey: "key", TokenSource: token},
	} {
		cfg.Protocol = "responses"
		cfg.BaseURL = codexBaseURL
		cfg.Model = "test"
		cfg.MaxOutputTokens = 1
		if _, err := New(t.Context(), cfg); err == nil {
			t.Fatalf("accepted wrong mode: %+v", cfg)
		}
	}
	for _, pair := range [][2]string{{"", "workspace"}, {"secret", ""}, {"secret\r\nInjected: x", "workspace"}, {"secret", "workspace\nother"}, {"secret space", "workspace"}} {
		c := codexTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("sent invalid credentials") }, func(context.Context) (string, string, error) { return pair[0], pair[1], nil })
		var out struct{ Answer string }
		if err := c.CompleteStructured(t.Context(), "sys", "user", "submit", &out); !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("credentials: %v", err)
		}
	}
}

func TestCodexGuardsBeforeCredentials(t *testing.T) {
	var issued atomic.Int32
	tr := &codexTransport{chatGPTTransport{codex: true, credentials: func(context.Context) (string, string, error) { issued.Add(1); return "secret", "workspace", nil }, base: codexRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Error("sent malformed request"); return nil, nil })}}
	for _, target := range []string{
		"https://api.openai.com/v1/responses", "https://gateway.example/responses",
		"http://chatgpt.com/backend-api/codex/responses", "https://chatgpt.com:443/backend-api/codex/responses",
		codexBaseURL + "/responses?x=y", codexBaseURL + "/responses#x", "https://user@chatgpt.com/backend-api/codex/responses",
		codexBaseURL + "/%72esponses", codexBaseURL + "/responses/", "https://chatgpt.com/backend-api/responses",
	} {
		req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"model":"test","input":[]}`))
		if _, err := tr.RoundTrip(req); !errors.Is(err, ErrEndpoint) {
			t.Fatalf("destination %s: %v", target, err)
		}
	}
	for _, body := range []string{`null`, `{"model":"test","input":[]} {}`, `{"model":"test","input":[{"type":"function_call","namespace":"roast"}]}`, `{"model":"test","input":[],"tools":[{"type":"namespace"}]}`, `{"model":"test\r\nx","input":[]}`, `{"model":"test;tier=priority","input":[]}`} {
		req, _ := http.NewRequest(http.MethodPost, codexBaseURL+"/responses", strings.NewReader(body))
		if _, err := tr.RoundTrip(req); !errors.Is(err, ErrEndpoint) {
			t.Fatalf("body %s: %v", body, err)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, codexBaseURL+"/responses", strings.NewReader(`{"model":"test","input":[]}`))
	req.Header.Set("X-Invalid", "bad\r\nvalue")
	if _, err := tr.RoundTrip(req); !errors.Is(err, ErrEndpoint) {
		t.Fatalf("headers: %v", err)
	}
	if issued.Load() != 0 {
		t.Fatalf("issued %d credentials before validation", issued.Load())
	}
}

func TestCodexLegacyErrorTypes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, typ := range []string{"usage_limit_reached", "usage_not_included", "authentication_error", "unknown-SECRET"} {
			t.Run(fmt.Sprintf("%t/%s", stream, typ), func(t *testing.T) {
				c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":%q,\"message\":\"SECRET\"}}}\n\n", typ)
					} else {
						w.WriteHeader(429)
						fmt.Fprintf(w, `{"error":{"type":%q,"message":"SECRET"}}`, typ)
					}
				}, func(context.Context) (string, string, error) { return "SECRET", "workspace", nil })
				var out struct{ Answer string }
				err := c.CompleteStructured(t.Context(), "sys", "user", "submit", &out)
				want := ErrUsageLimit
				if typ == "authentication_error" {
					want = ErrAuth
				}
				if typ == "unknown-SECRET" {
					want = ErrEndpoint
				}
				if !errors.Is(err, want) || strings.Contains(err.Error(), "SECRET") {
					t.Fatalf("error: %v", err)
				}
			})
		}
	}
}

func TestCodexAmbientCredentialsIgnored(t *testing.T) {
	for name, value := range map[string]string{
		"OPENAI_API_KEY": "SECRET-ambient", "OPENAI_BASE_URL": "https://gateway.example/v1",
		"OPENAI_ORG_ID": "ambient-org", "OPENAI_PROJECT_ID": "ambient-project",
	} {
		t.Setenv(name, value)
	}
	c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer chosen" || r.Header.Get("ChatGPT-Account-Id") != "chosen-workspace" || r.Header.Get("OpenAI-Organization") != "" || r.Header.Get("OpenAI-Project") != "" {
			t.Error("ambient credential leakage")
		}
		codexFixture(w, "submit", `{"Answer":"done"}`, false)
	}, func(context.Context) (string, string, error) { return "chosen", "chosen-workspace", nil })
	var out struct{ Answer string }
	if err := c.CompleteStructured(t.Context(), "sys", "user", "submit", &out); err != nil || out.Answer != "done" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestCodexRejectsNonStreamSuccess(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream-invalid", ""} {
		c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			fmt.Fprint(w, `{"error":{"message":"SECRET"}}`)
		}, func(context.Context) (string, string, error) { return "SECRET", "workspace", nil })
		c.transport.cfg.CaptureResponse = func(RawResponse) error { t.Error("captured malformed success"); return nil }
		var out struct{ Answer string }
		if err := c.CompleteStructured(t.Context(), "sys", "user", "submit", &out); !errors.Is(err, ErrEndpoint) || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("error=%v", err)
		}
	}
}

func TestCodexFullHistoryReplay(t *testing.T) {
	var calls atomic.Int32
	c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		n := calls.Add(1)
		if n == 3 {
			raw, _ := json.Marshal(body["input"])
			if strings.Count(string(raw), `"encrypted_content":"opaque-reasoning"`) != 1 || strings.Count(string(raw), `"type":"function_call_output"`) != 2 {
				t.Errorf("lost full history: %s", raw)
			}
		}
		tool, args := "read", `{"path":"x.go"}`
		if n == 3 {
			tool, args = "submit", `{"answer":"done"}`
		}
		recorder := httptest.NewRecorder()
		codexFixture(recorder, tool, args, n == 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.ReplaceAll(recorder.Body.String(), "call_read", fmt.Sprintf("call_read_%d", n)))
	}, func(context.Context) (string, string, error) { return "secret", "workspace", nil })
	var out struct{ Answer string }
	if err := c.Converse(t.Context(), "system instructions", "user", ConverseOpts{MaxTurns: 3, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) { return map[string]any{"content": "package x"}, nil })}, &out); err != nil || out.Answer != "done" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}
