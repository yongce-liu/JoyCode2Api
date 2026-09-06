package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type streamErrReader struct {
	data []byte
	err  error
}

func (r *streamErrReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func runChatRelay(t *testing.T, body io.Reader) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	relayChatStream(w, w, req, body, "test-model")
	return w.Body.String()
}

func TestChatStreamPrematureCloseSurfacesError(t *testing.T) {
	out := runChatRelay(t, strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"))

	if !strings.Contains(out, "partial") {
		t.Fatalf("partial content missing:\n%s", out)
	}
	assertChatStreamError(t, out)
}

func TestChatStreamReadErrorSurfacesEscapedError(t *testing.T) {
	body := &streamErrReader{
		data: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"),
		err:  io.ErrUnexpectedEOF,
	}
	out := runChatRelay(t, body)

	assertChatStreamError(t, out)
	if !strings.Contains(out, "unexpected EOF") {
		t.Fatalf("read error missing:\n%s", out)
	}
}

func TestChatStreamDoneWithoutFinishReasonSurfacesError(t *testing.T) {
	out := runChatRelay(t, strings.NewReader("data: [DONE]\n\n"))
	assertChatStreamError(t, out)
}

func TestChatStreamNormalCompletionForwardsOneDone(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	out := runChatRelay(t, strings.NewReader(body))

	assertChatStreamComplete(t, out)
}

func TestChatStreamSynthesizesDoneAfterSemanticCompletion(t *testing.T) {
	for _, reason := range []string{"stop", "tool_calls", "length", "future_reason"} {
		t.Run(reason, func(t *testing.T) {
			body := `data: {"choices":[{"delta":{},"finish_reason":"` + reason + `"}]}` + "\n\n"
			out := runChatRelay(t, strings.NewReader(body))
			assertChatStreamComplete(t, out)
		})
	}
}

func TestChatStreamUsageOnlyChunkDoesNotComplete(t *testing.T) {
	body := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	out := runChatRelay(t, strings.NewReader(body))
	assertChatStreamError(t, out)
}

func TestChatStreamUpstreamErrorIsNormalized(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
	}{
		{"string code", "data: {\"error\":{\"message\":\"bad \\\"quote\\\" and \\\\ slash\",\"code\":\"upstream_bad\"}}\n\n", "upstream_bad"},
		{"numeric code", "data: {\"error\":{\"message\":\"gateway failure\",\"code\":1032}}\n\n", "1032"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runChatRelay(t, strings.NewReader(tc.body))
			assertChatStreamError(t, out)

			payload := errorPayload(t, out)
			errObject := payload["error"].(map[string]interface{})
			if errObject["code"] != tc.code {
				t.Fatalf("unexpected error payload: %#v", errObject)
			}
		})
	}
}

func TestChatStreamRoutesUseTruncationRelay(t *testing.T) {
	t.Run("regular", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		}))
		defer backend.Close()

		client := newMockClient(backend)
		server := NewServer(client, nil)
		mux := http.NewServeMux()
		server.RegisterRoutes(mux)
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"GLM-5.1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		mux.ServeHTTP(w, req)
		assertChatStreamError(t, w.Body.String())
	})

	t.Run("short-key", func(t *testing.T) {
		frontend, cleanup := setupShortKeyGPTServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		})
		defer cleanup()

		resp, err := http.Post(frontend.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		assertChatStreamError(t, string(data))
	})
}

func TestWriteChatStreamErrorEscapesMessage(t *testing.T) {
	w := httptest.NewRecorder()
	message := "bad \\\"quote\\\" \\\\ slash\nnext"
	writeChatStreamError(w, w, message, "upstream_stream_error")
	payload := errorPayload(t, w.Body.String())
	errObject := payload["error"].(map[string]interface{})
	if errObject["message"] != message {
		t.Fatalf("message = %#v, want %#v", errObject["message"], message)
	}
}

func assertChatStreamComplete(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, `"error"`) {
		t.Fatalf("completed stream contains error:\n%s", out)
	}
	if got := strings.Count(out, "data: [DONE]"); got != 1 {
		t.Fatalf("DONE count = %d, want 1:\n%s", got, out)
	}
}

func assertChatStreamError(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "data: [DONE]") {
		t.Fatalf("failed stream must not contain DONE:\n%s", out)
	}
	payload := errorPayload(t, out)
	errObject, ok := payload["error"].(map[string]interface{})
	if !ok || errObject["type"] != "api_error" || errObject["message"] == "" {
		t.Fatalf("invalid error object: %#v", payload)
	}
}

func errorPayload(t *testing.T, out string) map[string]interface{} {
	t.Helper()
	var result map[string]interface{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var payload map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload) == nil && payload["error"] != nil {
			result = payload
		}
	}
	if result == nil {
		t.Fatalf("stream error payload missing:\n%s", out)
	}
	return result
}
