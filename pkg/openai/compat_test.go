package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

func TestChatModelAndReasoningTools(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]interface{}
				json.NewDecoder(r.Body).Decode(&body)
				if body["model"] != "gpt-5.6-sol" || body["reasoning_effort"] != "high" || body["parallel_tool_calls"] != false || body["max_completion_tokens"] != float64(512) {
					t.Errorf("lost fields: %v", body)
				}
				if len(body["tools"].([]interface{})) != 1 {
					t.Error("lost tools")
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"ping\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"call_1","function":{"name":"ping","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
				}
			}))
			defer upstream.Close()
			c := joycode.NewClient("test", "test")
			c.ColorBaseURL = upstream.URL
			s := NewServer(c, nil)
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"messages":[{"role":"user","content":"test"}],"reasoning_effort":"high","max_completion_tokens":512,"parallel_tool_calls":false,"tools":[{"type":"function","function":{"name":"ping","parameters":{"type":"object"}}}]}`, stream)))
			r = store.InitModel(store.InitTokenUsage(r))
			w := httptest.NewRecorder()
			s.handleChat(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "call_1") || store.GetModel(r) != "gpt-5.6-sol" {
				t.Fatalf("unexpected response: %d %s", w.Code, w.Body)
			}
			in, out := store.GetTokenUsage(r)
			if in != 3 || out != 4 {
				t.Fatalf("usage %d %d", in, out)
			}
		})
	}
}

func TestChatErrorAndCompatibilityRetry(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, fallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/fallback=%t", stream, fallback), func(t *testing.T) {
				st, err := store.Open(filepath.Join(t.TempDir(), "proxy.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				st.SetSetting("reasoning_tool_fallback", fmt.Sprint(fallback))
				calls := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					var b map[string]interface{}
					json.NewDecoder(r.Body).Decode(&b)
					if b["reasoning_effort"] != nil {
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, ": heartbeat\n\ndata: ")
						} else {
							w.Header().Set("Content-Type", "application/json")
						}
						fmt.Fprint(w, `{"error":{"code":400,"cause":"Function tools with reasoning_effort are not supported for gpt-5.6-sol in /v1/chat/completions"}}`)
						if stream {
							fmt.Fprint(w, "\n\ndata: [DONE]\n\n")
						}
						return
					}
					if len(b["tools"].([]interface{})) != 1 {
						t.Error("retry lost tools")
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
					} else {
						fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
					}
				}))
				defer upstream.Close()
				c := joycode.NewClient("test", "test")
				c.ColorBaseURL = upstream.URL
				s := NewServer(c, st)
				r := store.InitTokenUsage(httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"reasoning_effort":"high","tools":[{"type":"function"}],"messages":[]}`, stream))))
				w := httptest.NewRecorder()
				s.handleChat(w, r)
				wantCalls := 1
				if fallback {
					wantCalls = 2
				}
				if calls != wantCalls {
					t.Fatalf("calls=%d", calls)
				}
				if !fallback {
					if stream {
						if store.GetRequestError(r).Status != 400 {
							t.Fatalf("stream failure counted as success: %s", w.Body)
						}
					} else if w.Code != 400 {
						t.Fatalf("status=%d", w.Code)
					}
				}
				if fallback && (w.Code != 200 || store.GetRequestError(r).Status != 0) {
					t.Fatalf("retry failed: %s", w.Body)
				}
			})
		}
	}
}

func TestInvalidModelIsNotReplaced(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), "nonexistent") {
			t.Fatal("model replaced")
		}
		fmt.Fprint(w, `{"error":{"code":"6002","message":"模型不存在！"}}`)
	}))
	defer upstream.Close()
	c := joycode.NewClient("test", "test")
	c.ColorBaseURL = upstream.URL
	r := store.InitModel(httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"nonexistent","messages":[]}`)))
	w := httptest.NewRecorder()
	NewServer(c, nil).handleChat(w, r)
	if w.Code != 404 || store.GetModel(r) != "nonexistent" {
		t.Fatalf("status/model=%d/%s", w.Code, store.GetModel(r))
	}
}

func TestResolveModelDefaultsAndUnlisted(t *testing.T) {
	for _, tt := range []struct{ model, account, system, want string }{
		{"gpt-5.6-sol", "GLM-5.1", "GLM-5", "gpt-5.6-sol"}, {"", "GLM-5.1", "GLM-5", "GLM-5.1"}, {"", "", "GLM-5", "GLM-5"},
	} {
		if got := ResolveModel(tt.model, tt.account, tt.system); got != tt.want {
			t.Fatalf("got %s want %s", got, tt.want)
		}
	}
}

func TestModelsEndpointRefresh(t *testing.T) {
	version := 1
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data":[{"label":"Display name","chatApiModel":"callable-%d"}]}`, version)
	}))
	defer upstream.Close()
	c := joycode.NewClient("test", "test")
	c.ColorBaseURL = upstream.URL
	mux := http.NewServeMux()
	NewServer(c, nil).RegisterRoutes(mux)
	for version = 1; version <= 2; version++ {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"id":"callable-%d"`, version)) {
			t.Fatalf("stale/wrong model: %s", w.Body)
		}
	}
}

func TestFallbackDoesNotRetryUnrelatedFailures(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetSetting("reasoning_tool_fallback", "true")
	for _, status := range []int{400, 401, 429, 500} {
		body := map[string]interface{}{"tools": []interface{}{map[string]interface{}{}}, "reasoning_effort": "high"}
		if NewServer(nil, st).retryWithoutEffort(body, &joycode.APIError{StatusCode: status, Body: "quota exceeded"}) {
			t.Fatalf("retried status %d", status)
		}
	}
}

func TestStreamWithMislabeledJSONError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `{"error":{"code":"6002","message":"模型不存在！"}}`)
	}))
	defer upstream.Close()
	c := joycode.NewClient("test", "test")
	c.ColorBaseURL = upstream.URL
	r := store.InitTokenUsage(httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"nonexistent","stream":true,"messages":[]}`)))
	w := httptest.NewRecorder()
	NewServer(c, nil).handleChat(w, r)
	if store.GetRequestError(r).Status != 404 || !strings.Contains(w.Body.String(), "data: {") || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("incorrect error translation: %+v %s", store.GetRequestError(r), w.Body)
	}
}
