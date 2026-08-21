package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
)

func TestResolveModelWithCatalog_NormalizedGPT(t *testing.T) {
	got := ResolveModelWithCatalog("gpt-5.6-sol", "", "", []string{"GPT-5.6 Sol"})
	if got != "GPT-5.6 Sol" {
		t.Fatalf("resolved model = %q", got)
	}
}

func TestNativeResponsesModelID(t *testing.T) {
	cases := map[string]string{
		"GPT-5.6 Sol": "gpt-5.6-sol",
		"gpt-5.6-sol": "gpt-5.6-sol",
		"GPT-5.6  Sol": "gpt-5.6-sol",
		"  GPT-5.6 Sol  ": "gpt-5.6-sol",
	}
	for in, want := range cases {
		if got := nativeResponsesModelID(in); got != want {
			t.Errorf("nativeResponsesModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResponsesRequestToChat_ConvertsTools(t *testing.T) {
	body := responsesRequestToChat(map[string]interface{}{
		"model": "GPT-5.6 Sol",
		"input": []interface{}{
			map[string]interface{}{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"a.go"}`},
			map[string]interface{}{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		},
		"tools": []interface{}{map[string]interface{}{
			"type": "function", "name": "read_file", "description": "Read a file",
			"parameters": map[string]interface{}{"type": "object"},
		}},
		"tool_choice": map[string]interface{}{"type": "function", "name": "read_file"},
	})
	messages, ok := body["messages"].([]interface{})
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v", body["messages"])
	}
	call := messages[0].(map[string]interface{})
	toolCalls := call["tool_calls"].([]interface{})
	if call["role"] != "assistant" || toolCalls[0].(map[string]interface{})["id"] != "call_1" {
		t.Fatalf("function call message = %#v", call)
	}
	output := messages[1].(map[string]interface{})
	if output["role"] != "tool" || output["tool_call_id"] != "call_1" || output["content"] != "ok" {
		t.Fatalf("function output message = %#v", output)
	}
	tools := body["tools"].([]interface{})
	fn := tools[0].(map[string]interface{})["function"].(map[string]interface{})
	if fn["name"] != "read_file" || fn["description"] != "Read a file" {
		t.Fatalf("tools = %#v", tools)
	}
	choice := body["tool_choice"].(map[string]interface{})
	if choice["function"].(map[string]interface{})["name"] != "read_file" {
		t.Fatalf("tool_choice = %#v", choice)
	}
}

func TestNativeChat_UsesPluginShortKeyAndChatEndpoint(t *testing.T) {
	frontend, cleanup := setupShortKeyGPTServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
		// The upstream openai-response route only accepts the canonical model id,
		// not the catalog display label — sending "GPT-5.6 Sol" triggers a 1032
		// "HTTP调用异常" from the gateway. See nativeResponsesModelID.
		if body["tenant"] != "JD" || body["model"] != "gpt-5.6-sol" {
			t.Errorf("native body = %#v", body)
		}
		writeTestJSON(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	})
	defer cleanup()

	resp, err := http.Post(frontend.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, data)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["object"] != "chat.completion" || result["model"] != "GPT-5.6 Sol" {
		t.Fatalf("chat response = %#v", result)
	}
}

func TestResponses_UsesChatUpstream(t *testing.T) {
	frontend, cleanup := setupShortKeyGPTServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
		if body["model"] != "gpt-5.6-sol" {
			t.Errorf("upstream model = %#v, want gpt-5.6-sol", body["model"])
		}
		messages := body["messages"].([]interface{})
		if messages[0].(map[string]interface{})["content"] != "hi" {
			t.Errorf("chat messages = %#v", messages)
		}
		writeTestJSON(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	})
	defer cleanup()

	resp, err := http.Post(frontend.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.6-sol","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["object"] != "response" || result["status"] != "completed" || result["model"] != "GPT-5.6 Sol" {
		t.Fatalf("responses result = %#v", result)
	}
	output := result["output"].([]interface{})
	content := output[0].(map[string]interface{})["content"].([]interface{})
	if content[0].(map[string]interface{})["text"] != "hello" {
		t.Fatalf("responses output = %#v", output)
	}
}

func TestResponsesStream_ConvertsChatEvents(t *testing.T) {
	frontend, cleanup := setupShortKeyGPTServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
		if body["stream"] != true {
			t.Errorf("stream = %#v", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"},\"finish_reason\":null}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	defer cleanup()

	resp, err := http.Post(frontend.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.6-sol","input":"你好","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	stream := string(data)
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", `"delta":"你"`, `"delta":"好"`, "event: response.completed"} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream missing %q:\n%s", want, stream)
		}
	}
}

func setupShortKeyGPTServer(t *testing.T, handler func(http.ResponseWriter, *http.Request, map[string]interface{})) (*httptest.Server, func()) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/saas/openai/v2/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("ptKey"); got != "plugin-short-key" {
			t.Errorf("ptKey = %q", got)
		}
		if got := r.Header.Get("loginType"); got != "ERP" {
			t.Errorf("loginType = %q", got)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		handler(w, r, body)
	}))

	client := joycode.NewClient("account-long-key", "jd_user")
	client.ColorBaseURL = ""
	client.SetNativeContext(joycode.NativeContext{PtKey: "plugin-short-key", LoginType: "ERP", Tenant: "JD"})
	client.SetHTTPClient(&http.Client{Transport: redirectTransport{target: backend.URL}})
	st, storeCleanup, err := newTempStore()
	if err != nil {
		backend.Close()
		t.Fatal(err)
	}
	_ = st.SetSetting("available_models", "GPT-5.6 Sol")
	_ = st.SetSetting("model_adapters", `{"GPT-5.6 Sol":"openai-response"}`)
	server := NewServer(client, st)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	frontend := httptest.NewServer(mux)
	return frontend, func() {
		frontend.Close()
		storeCleanup()
		backend.Close()
	}
}

func writeTestJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, body)
}
