package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodexHeaderlessEventStreamsCompleteAndCapture(t *testing.T) {
	for _, mode := range []string{"event-lines", "data-lines", "keepalive", "mixed-case-content-type"} {
		t.Run(mode, func(t *testing.T) {
			fixture := httptest.NewRecorder()
			codexFixture(fixture, "submit", `{"answer":"done"}`, true)
			body := fixture.Body.String()
			if mode == "data-lines" {
				var lines []string
				for _, line := range strings.Split(body, "\n") {
					if !strings.HasPrefix(line, "event:") {
						lines = append(lines, line)
					}
				}
				body = strings.Join(lines, "\n")
			}
			if mode == "keepalive" {
				body = "\n: heartbeat\n\n" + body
			}
			c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept") != "text/event-stream" {
					t.Error("Codex did not request an event stream")
				}
				w.Header()["Content-Type"] = nil // suppress net/http's automatic sniffing
				if mode == "mixed-case-content-type" {
					w.Header().Set("Content-Type", "Text/Event-Stream; charset=utf-8")
				}
				_, _ = io.WriteString(w, body)
			}, func(context.Context) (string, string, error) { return "secret", "workspace", nil })
			var captures int
			c.transport.cfg.CaptureResponse = func(raw RawResponse) error {
				captures++
				if raw.ContentType != "text/event-stream" || string(raw.Body) != body {
					t.Error("headerless stream was not captured intact as SSE")
				}
				return nil
			}
			var out struct {
				Answer string `json:"answer"`
			}
			if err := c.CompleteStructured(t.Context(), "Use submit", "Return done", "submit", &out); err != nil || out.Answer != "done" || captures != 1 {
				t.Fatalf("out=%+v captures=%d err=%v", out, captures, err)
			}
		})
	}
}

func TestCodexHeaderlessNonStreamsStillFailClosed(t *testing.T) {
	for _, body := range []string{`{"error":{"message":"SECRET"}}`, "<html>SECRET</html>", "plain SECRET", "event: not-an-openai-response\n", strings.Repeat(": keepalive\n", 500), "", "\n\n"} {
		t.Run(fmt.Sprintf("%d/%c", len(body), firstByte(body)), func(t *testing.T) {
			c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header()["Content-Type"] = nil
				_, _ = io.WriteString(w, body)
			}, func(context.Context) (string, string, error) { return "SECRET", "workspace", nil })
			c.transport.cfg.CaptureResponse = func(RawResponse) error { t.Error("captured invalid stream"); return nil }
			var out struct{ Answer string }
			if err := c.CompleteStructured(t.Context(), "sys", "user", "submit", &out); !errors.Is(err, ErrEndpoint) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe error %v", err)
			}
		})
	}
}

func firstByte(s string) byte {
	if s == "" {
		return '-'
	}
	return s[0]
}

type codexSniffBody struct {
	*bytes.Reader
	read   int
	closed bool
}

func (b *codexSniffBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *codexSniffBody) Close() error { b.closed = true; return nil }

func TestCodexStreamSniffIsBoundedAndPreservesClose(t *testing.T) {
	body := &codexSniffBody{Reader: bytes.NewReader([]byte(strings.Repeat(":", 100_000)))}
	response := &http.Response{Header: make(http.Header), Body: body}
	if err := prepareCodexEventStream(response); !errors.Is(err, ErrEndpoint) || !body.closed || body.read > 4096 {
		t.Fatalf("unbounded sniff or body leak: read=%d closed=%t err=%v", body.read, body.closed, err)
	}
	wire := "event: response.created\ndata: {}\n\n" + strings.Repeat("tail", 3000)
	body = &codexSniffBody{Reader: bytes.NewReader([]byte(wire))}
	response = &http.Response{Header: make(http.Header), Body: body}
	if err := prepareCodexEventStream(response); err != nil || body.read > 4096 {
		t.Fatalf("read=%d err=%v", body.read, err)
	}
	actual, err := io.ReadAll(response.Body)
	if err != nil || string(actual) != wire {
		t.Fatal("sniff lost or duplicated stream bytes")
	}
	_ = response.Body.Close()
	if !body.closed {
		t.Fatal("sniff lost the original body closer")
	}
}

func TestCodexHeaderlessToolHistoryReplay(t *testing.T) {
	var calls int
	c := codexTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		request, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		tool, args := "read", `{"path":"x.go"}`
		if calls == 2 {
			if !bytes.Contains(request, []byte(`"encrypted_content":"opaque-reasoning"`)) || !bytes.Contains(request, []byte(`"type":"function_call_output"`)) {
				t.Error("headerless SSE lost reasoning/tool replay")
			}
			tool, args = "submit", `{"answer":"done"}`
		}
		fixture := httptest.NewRecorder()
		codexFixture(fixture, tool, args, calls == 1)
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, fixture.Body.String())
	}, func(context.Context) (string, string, error) { return "secret", "workspace", nil })
	var out struct {
		Answer string `json:"answer"`
	}
	var reads int
	err := c.Converse(t.Context(), "Use the review tools", "Inspect x.go and submit", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
		reads++
		return map[string]any{"content": "package x"}, nil
	})}, &out)
	if err != nil || calls != 2 || reads != 1 || out.Answer != "done" {
		t.Fatalf("calls=%d reads=%d out=%+v err=%v", calls, reads, out, err)
	}
}
