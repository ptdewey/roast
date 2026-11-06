package llm

import (
	"charm.land/fantasy"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bluesky-social/roast/internal/testutil"
)

type codexRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func codexTestClient(t *testing.T, handler http.HandlerFunc, source func(context.Context) (string, string, error)) *client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := New(t.Context(), ClientConfig{Codex: true, CodexTokenSource: source, Protocol: "responses", BaseURL: codexBaseURL, Model: "test-model", MaxOutputTokens: 1024, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	cc := c.(*client)
	local, _ := url.Parse(server.URL)
	cc.transport.base.(*codexTransport).base = codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		// This test-only rewrite runs AFTER the production destination and token guards.
		clone := req.Clone(req.Context())
		u := *req.URL
		u.Scheme = local.Scheme
		u.Host = local.Host
		clone.URL = &u
		clone.Host = local.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
	return cc
}

func codexFixture(w http.ResponseWriter, tool, args string, reasoning bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	if reasoning {
		// encrypted_content deliberately arrives only on .done, as in real streams.
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[]}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[],\"encrypted_content\":\"opaque-reasoning\"}}\n\n")
	}
	_ = testutil.WriteInferenceStream(w, "responses", map[string]any{
		"id": "resp_1", "object": "response", "status": "completed", "model": "test-model",
		"output": []any{map[string]any{"id": "fc_" + tool, "type": "function_call", "call_id": "call_" + tool, "name": tool, "arguments": args, "status": "completed"}},
		"usage":  map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14},
	})
}

func TestCodexToolLoopAndStructured(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(fmt.Sprint(structured), func(t *testing.T) {
			var mu sync.Mutex
			var requests []map[string]any
			var tokens []string
			var issued atomic.Int32
			var captured atomic.Int32
			c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				requests = append(requests, body)
				tokens = append(tokens, r.Header.Get("Authorization"))
				n := len(requests)
				mu.Unlock()
				if r.Header.Get("ChatGPT-Account-Id") != fmt.Sprintf("account-%d", n) ||
					r.Header.Get("Originator") != "roast" || r.Header.Get("Version") != CodexClientVersion ||
					r.Header.Get("OpenAI-Beta") != "responses=experimental" ||
					!strings.HasPrefix(r.Header.Get("Session_id"), "roast-") ||
					r.Header.Get("X-Client-Request-Id") != r.Header.Get("Session_id") ||
					body["prompt_cache_key"] != r.Header.Get("Session_id") ||
					r.Header.Get("X-Codex-Routing-Hint") != "model=test-model" {
					t.Error("incorrect Codex headers")
				}
				if r.URL.Path != "/backend-api/codex/responses" || r.Header.Get("OpenAI-Project") != "" || r.Header.Get("OpenAI-Organization") != "" {
					t.Error("unexpected routing headers")
				}
				tool, args := "read", `{"path":"x.go"}`
				if structured {
					tool, args = "review_schema", `{"answer":"done"}`
				} else if n == 2 {
					tool, args = "submit", `{"answer":"done"}`
				}
				codexFixture(w, tool, args, n == 1)
			}, func(context.Context) (string, string, error) {
				n := issued.Add(1)
				return fmt.Sprintf("rotated-%d", n), fmt.Sprintf("account-%d", n), nil
			})
			c.transport.cfg.CaptureResponse = func(raw RawResponse) error {
				if strings.Contains(string(raw.Body), `"namespace"`) {
					t.Error("unexpected namespace")
				}
				captured.Add(1)
				return nil
			}
			var out struct {
				Answer string `json:"answer"`
			}
			var reads int
			var err error
			if structured {
				err = c.CompleteStructured(t.Context(), "system instructions", "user", "review_schema", &out)
			} else {
				err = c.Converse(t.Context(), "system instructions", "user", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
					reads++
					return map[string]any{"content": "package x"}, nil
				})}, &out)
			}
			if err != nil || out.Answer != "done" {
				t.Fatalf("result=%+v error=%v", out, err)
			}
			want := 2
			if structured {
				want = 1
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != want || issued.Load() != int32(want) || captured.Load() != int32(want) || c.Usage().Calls != want || c.Usage().OutputTokens != 4*want {
				t.Fatalf("requests=%d issued=%d capture=%d usage=%+v", len(requests), issued.Load(), captured.Load(), c.Usage())
			}
			for i, body := range requests {
				if tokens[i] != fmt.Sprintf("Bearer rotated-%d", i+1) {
					t.Fatalf("token=%q", tokens[i])
				}
				if body["store"] != false || body["stream"] != true || body["instructions"] != "system instructions" {
					t.Fatalf("normalization=%#v", body)
				}
				for _, key := range []string{"max_output_tokens", "metadata", "temperature", "previous_response_id"} {
					if _, ok := body[key]; ok {
						t.Errorf("unsupported field %s", key)
					}
				}
				tools := body["tools"].([]any)
				for _, raw := range tools {
					tool := raw.(map[string]any)
					strict, exists := tool["strict"]
					if tool["type"] != "function" || !exists || strict != nil {
						t.Fatalf("flat tools=%#v", tools)
					}
				}
				if structured && tools[0].(map[string]any)["name"] != "review_schema" {
					t.Fatal("schema name lost")
				}
				if body["parallel_tool_calls"] != true || body["text"].(map[string]any)["verbosity"] != "low" {
					t.Fatalf("policy=%#v", body)
				}

			}
			if !structured {
				if reads != 1 {
					t.Fatalf("reads=%d", reads)
				}
				input := requests[1]["input"].([]any)
				var reasoning, call, result int
				for i, raw := range input {
					item := raw.(map[string]any)
					switch item["type"] {
					case "reasoning":
						reasoning = i + 1
						if item["encrypted_content"] != "opaque-reasoning" {
							t.Fatal("encrypted reasoning lost")
						}
					case "function_call":
						call = i + 1
						if item["namespace"] != nil || item["call_id"] != "call_read" || item["name"] != "read" {
							t.Fatalf("replay=%#v", item)
						}
					case "function_call_output":
						result = i + 1
						if item["call_id"] != "call_read" || !strings.Contains(item["output"].(string), "package x") {
							t.Fatalf("result=%#v", item)
						}
					}
				}
				if reasoning == 0 || call <= reasoning || result <= call {
					t.Fatalf("replay ordering=%#v", input)
				}
			}
		})
	}
}

func TestCodexConfigurationAndDestinationGuards(t *testing.T) {
	var issued atomic.Int32
	source := func(context.Context) (string, string, error) { issued.Add(1); return "secret", "account", nil }
	valid := ClientConfig{Codex: true, CodexTokenSource: source, Protocol: "responses", BaseURL: codexBaseURL, MaxOutputTokens: 1, Model: "test"}
	for _, change := range []func(*ClientConfig){func(c *ClientConfig) { c.APIKey = "conflict" }, func(c *ClientConfig) { c.CodexTokenSource = nil }, func(c *ClientConfig) { c.Protocol = "chat" }, func(c *ClientConfig) { c.BaseURL = "https://gateway.example/v1" }, func(c *ClientConfig) { c.BaseURL += "/" }} {
		cfg := valid
		change(&cfg)
		if _, err := New(t.Context(), cfg); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	transport := &codexTransport{chatGPTTransport{codex: true, credentials: source, base: codexRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("invalid route sent"); return nil, nil })}}
	for _, target := range []string{"http://api.openai.com/v1/responses", "https://evil.example/v1/responses", "https://api.openai.com:443/v1/responses", "https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1/responses?x=y", "https://api.openai.com/v1/responses#x", "https://user@api.openai.com/v1/responses", "https://api.openai.com/v1/%72esponses"} {
		req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"input":[]}`))
		if _, err := transport.RoundTrip(req); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, codexBaseURL+"/responses", strings.NewReader(`{"input":[]}`))
	req.Host = "evil.example"
	if _, err := transport.RoundTrip(req); err == nil {
		t.Fatal("accepted Host override")
	}
	req, _ = http.NewRequest(http.MethodGet, codexBaseURL+"/responses", strings.NewReader(`{"input":[]}`))
	if _, err := transport.RoundTrip(req); err == nil {
		t.Fatal("accepted GET")
	}
	req, _ = http.NewRequest(http.MethodPost, codexBaseURL+"/responses", strings.NewReader(`{"input":"string"}`))
	if _, err := transport.RoundTrip(req); err == nil {
		t.Fatal("accepted non-array input")
	}
	if issued.Load() != 0 {
		t.Fatalf("guard acquired %d tokens", issued.Load())
	}
}

func TestCodexRedirectAndTokenFailure(t *testing.T) {
	var leaked atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	defer sink.Close()
	c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}, func(context.Context) (string, string, error) { return "secret", "account", nil })
	var out struct{ Answer string }
	if err := c.CompleteStructured(t.Context(), "sys", "user", "answer", &out); !errors.Is(err, ErrEndpoint) {
		t.Fatalf("redirect error=%v", err)
	}
	if leaked.Load() != 0 {
		t.Fatal("followed redirect")
	}
	c = codexTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("sent request without token") }, func(context.Context) (string, string, error) { return "", "", errors.New("SECRET refresh error") })
	err := c.CompleteStructured(t.Context(), "sys", "user", "answer", &out)
	if !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("token failure=%v", err)
	}
}

func TestCodexStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{{"usage_limit_reached", ErrUsageLimit}, {"usage_not_included", ErrUsageLimit}, {"subscription_sharing_usage_unavailable", ErrEndpoint}, {"authentication_error", ErrAuth}, {"subscription_sharing_user_not_eligible", ErrEndpoint}, {"invalid_token", ErrAuth}, {"server_error", ErrEndpoint}} {
		t.Run(tc.code, func(t *testing.T) {
			c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\",\"error\":{\"code\":%q,\"message\":\"failure\"}}}\n\n", tc.code)
			}, func(context.Context) (string, string, error) { return "token", "account", nil })
			var out struct{ Answer string }
			err := c.CompleteStructured(t.Context(), "sys", "user", "answer", &out)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
			if tc.want != ErrAuth && errors.Is(err, ErrAuth) {
				t.Fatal("non-auth failure mapped to auth")
			}
		})
	}
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"error":{"message":"failure"}}`, status) }, func(context.Context) (string, string, error) { return "token", "account", nil })
			var out struct{ Answer string }
			err := c.CompleteStructured(t.Context(), "sys", "user", "answer", &out)
			want := ErrAuth
			if status == 429 {
				want = ErrEndpoint
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCodexNormalizationDropsUnsupportedFields(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, codexBaseURL+"/responses", strings.NewReader(`{"input":[{"type":"message","role":"system","content":"sys"}],"background":true,"conversation":"x","max_output_tokens":1,"max_tool_calls":1,"metadata":{},"moderation":{},"multi_agent":true,"prompt":{},"prompt_cache_retention":"24h","safety_identifier":"x","temperature":1,"top_logprobs":1,"top_p":1,"truncation":"auto","user":"x","previous_response_id":"x"}`))
	body, err := normalizeSubscriptionRequest(req, true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if got["store"] != false || got["input"].([]any)[0].(map[string]any)["role"] != "developer" {
		t.Fatalf("normalized=%s", body)
	}
}

func TestCodexUnknownNamespaceNeverDispatches(t *testing.T) {
	c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		codexFixture(recorder, "submit", `{"answer":"done"}`, false)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.ReplaceAll(recorder.Body.String(), `"type":"function_call"`, `"namespace":"other","type":"function_call"`))
	}, func(context.Context) (string, string, error) { return "token", "account", nil })
	var out struct{ Answer string }
	if err := c.CompleteStructured(t.Context(), "sys", "user", "submit", &out); err == nil || out.Answer != "" {
		t.Fatalf("accepted foreign namespace: err=%v out=%+v", err, out)
	}
}

func TestCodexHTTPFailureCodesAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   error
	}{
		{429, "usage_not_included", ErrUsageLimit},
		{503, "subscription_sharing_usage_unavailable", ErrEndpoint},
		{403, "subscription_sharing_user_not_eligible", ErrAuth},
		{401, "subscription_sharing_invalid_user", ErrAuth},
		{403, "chatpass_v2_scope_not_authorized", ErrAuth},
		{500, "SECRET-token-unknown-code", ErrEndpoint},
	} {
		t.Run(tc.code, func(t *testing.T) {
			c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":{"code":%q,"message":%q}}`, tc.code, r.Header.Get("Authorization"))
			}, func(context.Context) (string, string, error) { return "SECRET-bearer", "account", nil })
			c.transport.cfg.CaptureResponse = func(RawResponse) error { t.Error("captured HTTP error body"); return nil }
			var out struct{ Answer string }
			err := c.CompleteStructured(t.Context(), "sys", "user", "submit", &out)
			if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe or incorrect error=%v", err)
			}
			if tc.want != ErrAuth && errors.Is(err, ErrAuth) {
				t.Fatalf("non-auth code treated as auth: %v", err)
			}
		})
	}
}

func TestCodexTokenSourceCancellation(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			c := codexTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("request after token source cancellation") }, func(context.Context) (string, string, error) { return "", "", cause })
			var out struct{ Answer string }
			err := c.CompleteStructured(t.Context(), "sys", "user", "submit", &out)
			if errors.Is(err, ErrAuth) || !errors.Is(err, cause) {
				t.Fatalf("cancellation=%v", err)
			}
		})
	}
}

func TestCodexReaderFailureCannotDispatchCompletedTool(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprint(oversized), func(t *testing.T) {
			c := testClient(nil)
			fake := c.model.(*fakeLanguageModel)
			c.model = &chatGPTModel{LanguageModel: fake, codex: true}
			fake.stream = func(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				return func(yield func(fantasy.StreamPart) bool) {
					state := ctx.Value(chatGPTReplayKey{}).(*chatGPTCallState)
					packet := `data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"submit","namespace":"other"}}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"
					body := &chatGPTStreamBody{ReadCloser: io.NopCloser(strings.NewReader(packet)), state: state}
					if oversized {
						body.total = maxCapturedResponseSize - 1
					}
					_, _ = io.ReadAll(body)
					// Deliberately ignore the reader error, as a parser can do when it has
					// already buffered a terminal event. The wrapper must still fail closed.
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "submit", ToolCallName: "submit", ToolCallInput: `{"answer":"must not dispatch"}`}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				}, nil
			}
			var out struct{ Answer string }
			err := c.CompleteStructured(t.Context(), "sys", "user", "submit", &out)
			if !errors.Is(err, ErrEndpoint) || out.Answer != "" {
				t.Fatalf("reader failure dispatched tool: error=%v out=%+v", err, out)
			}
		})
	}
}
