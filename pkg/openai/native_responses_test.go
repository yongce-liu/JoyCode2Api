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

func TestResolveModelWithCatalog_PreservesRequestedID(t *testing.T) {
	got := ResolveModelWithCatalog("gpt-5.6-sol", "", "", []string{"GPT-5.6 Sol"})
	if got != "gpt-5.6-sol" {
		t.Fatalf("resolved model = %q", got)
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

func TestNativeChat_PreservesCatalogModelID(t *testing.T) {
	frontend, cleanup := setupShortKeyGPTServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
		if r.URL.Path != "/api/saas/openai/v2/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if body["model"] != "GPT-5.6 Sol" {
			t.Errorf("model = %#v, want catalog ID", body["model"])
		}
		writeTestJSON(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	})
	defer cleanup()

	resp, err := http.Post(frontend.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"GPT-5.6 Sol","messages":[{"role":"user","content":"hi"}]}`))
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

func TestResponses_UsesNativeUpstream(t *testing.T) {
	frontend, cleanup := setupShortKeyGPTServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
		if r.URL.Path != "/api/saas/openai/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if body["model"] != "GPT-5.6 Sol" {
			t.Errorf("model = %#v, want catalog ID", body["model"])
		}
		writeTestJSON(w, `{"id":"resp-1","object":"response","status":"completed","model":"GPT-5.6 Sol","output":[],"usage":{"input_tokens":2,"output_tokens":1}}`)
	})
	defer cleanup()

	resp, err := http.Post(frontend.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"GPT-5.6 Sol","input":"hi"}`))
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
}

func TestResponses_UsesAccountCredentialsWithoutPluginAdapter(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/saas/openai/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("ptKey"); got != "account-long-key" {
			t.Errorf("ptKey = %q, want account credential", got)
		}
		if got := r.Header.Get("loginType"); got != "N_PIN_PC" {
			t.Errorf("loginType = %q, want N_PIN_PC", got)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "GPT-5.6 Sol" {
			t.Errorf("model = %#v, want catalog ID", body["model"])
		}
		tools, ok := body["tools"].([]interface{})
		if !ok || len(tools) != 1 {
			t.Fatalf("tools = %#v, want one supported function tool", body["tools"])
		}
		tool := tools[0].(map[string]interface{})
		if tool["type"] != "function" || tool["name"] != "lookup" {
			t.Fatalf("unexpected remaining tool: %#v", tool)
		}
		if _, exists := body["tool_choice"]; exists {
			t.Fatalf("unsupported image tool choice was not removed: %#v", body["tool_choice"])
		}
		writeTestJSON(w, `{"id":"resp-account","object":"response","status":"completed","model":"GPT-5.6 Sol","output":[]}`)
	}))
	defer backend.Close()

	client := joycode.NewClient("account-long-key", "jd_user")
	client.ColorBaseURL = ""
	client.SetHTTPClient(&http.Client{Transport: redirectTransport{target: backend.URL}})
	st, storeCleanup, err := newTempStore()
	if err != nil {
		t.Fatal(err)
	}
	defer storeCleanup()
	if err := st.SetSetting("available_models", "GPT-5.6 Sol"); err != nil {
		t.Fatal(err)
	}

	server := NewServer(client, st)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	frontend := httptest.NewServer(mux)
	defer frontend.Close()

	requestBody := `{"model":"GPT-5.6 Sol","input":"hi","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"image_generation","output_format":"png"}],"tool_choice":{"type":"image_generation"}}`
	resp, err := http.Post(frontend.URL+"/v1/responses", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, data)
	}
}

func TestNativeResponsesModel_ExplicitNonResponsesAdapterWins(t *testing.T) {
	st, cleanup, err := newTempStore()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := st.SetSetting("model_adapters", `{"GPT-5.6 Sol":"chat-completions"}`); err != nil {
		t.Fatal(err)
	}
	if IsNativeResponsesModel("GPT-5.6 Sol", st) {
		t.Fatal("explicit non-Responses adapter must not use the native Responses endpoint")
	}
}

func TestResponsesStream_UsesNativeResponsesUpstream(t *testing.T) {
	frontend, cleanup := setupShortKeyGPTServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
		if r.URL.Path != "/api/saas/openai/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if body["model"] != "GPT-5.6 Sol" {
			t.Errorf("model = %#v, want catalog label", body["model"])
		}
		if body["stream"] != true {
			t.Errorf("stream = %#v", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: event: response.created\n\n")
		io.WriteString(w, "data: data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n")
		io.WriteString(w, "data: event: response.output_text.delta\n\n")
		io.WriteString(w, "data: data: {\"type\":\"response.output_text.delta\",\"delta\":\"你好\"}\n\n")
		io.WriteString(w, "data: event: response.completed\n\n")
		io.WriteString(w, "data: data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	defer cleanup()

	resp, err := http.Post(frontend.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"GPT-5.6 Sol","input":"你好","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	stream := string(data)
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", `"delta":"你好"`, "event: response.completed"} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream missing %q:\n%s", want, stream)
		}
	}
	if strings.Contains(stream, "data: event:") || strings.Contains(stream, "data: data:") {
		t.Fatalf("gateway SSE envelope was not removed:\n%s", stream)
	}
}

func TestRelayNativeResponses_UnwrapsGatewaySSE(t *testing.T) {
	input := strings.Join([]string{
		"data: event: response.created",
		"",
		`data: data: {"type":"response.created","response":{"status":"in_progress"}}`,
		"",
		"data: event: response.completed",
		"",
		`data: data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", nil)

	relayNativeResponses(w, w, req, strings.NewReader(input))
	stream := w.Body.String()

	for _, want := range []string{
		"event: response.created\ndata: {\"type\":\"response.created\"",
		"event: response.completed\ndata: {\"type\":\"response.completed\"",
		"data: [DONE]",
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream missing %q:\n%s", want, stream)
		}
	}
	if strings.Contains(stream, "data: event:") || strings.Contains(stream, "data: data:") {
		t.Fatalf("gateway SSE envelope was not removed:\n%s", stream)
	}
}

func TestResponsesStreamPrematureCloseSurfacesError(t *testing.T) {
	stream := runResponsesRelay(t, strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"))

	assertResponsesStreamError(t, stream, "upstream stream closed before finish_reason")
}

func TestResponsesStreamDoneWithoutFinishReasonSurfacesError(t *testing.T) {
	stream := runResponsesRelay(t, strings.NewReader("data: [DONE]\n\n"))

	assertResponsesStreamError(t, stream, "upstream stream closed before finish_reason")
}

func TestResponsesStreamReadErrorSurfacesError(t *testing.T) {
	body := &streamErrReader{
		data: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"),
		err:  io.ErrUnexpectedEOF,
	}
	stream := runResponsesRelay(t, body)

	assertResponsesStreamError(t, stream, "unexpected EOF")
}

func TestResponsesStreamUpstreamErrorSurfacesError(t *testing.T) {
	stream := runResponsesRelay(t, strings.NewReader("data: {\"error\":{\"message\":\"generation failed\",\"code\":\"server_error\"}}\n\n"))

	assertResponsesStreamError(t, stream, "generation failed")
	if !strings.Contains(stream, `"code":"server_error"`) {
		t.Fatalf("upstream error code missing:\n%s", stream)
	}
}

func runResponsesRelay(t *testing.T, body io.Reader) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	relayChatAsResponses(w, w, req, body, "test-model")
	return w.Body.String()
}

func assertResponsesStreamError(t *testing.T, stream, message string) {
	t.Helper()
	if strings.Contains(stream, "event: response.completed") {
		t.Fatalf("failed stream must not complete:\n%s", stream)
	}
	if !strings.Contains(stream, "event: error") || !strings.Contains(stream, `"type":"error"`) || !strings.Contains(stream, message) {
		t.Fatalf("stream error missing:\n%s", stream)
	}
}

func setupShortKeyGPTServer(t *testing.T, handler func(http.ResponseWriter, *http.Request, map[string]interface{})) (*httptest.Server, func()) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/saas/openai/v2/chat/completions" && r.URL.Path != "/api/saas/openai/v1/responses" {
			t.Errorf("unexpected path = %q", r.URL.Path)
		}
		if r.URL.Path == "/api/saas/openai/v1/responses" {
			if got := r.Header.Get("ptKey"); got != "plugin-short-key" {
				t.Errorf("responses ptKey = %q", got)
			}
			if got := r.Header.Get("loginType"); got != "ERP" {
				t.Errorf("responses loginType = %q", got)
			}
		} else {
			if got := r.Header.Get("ptKey"); got != "account-long-key" {
				t.Errorf("chat ptKey = %q", got)
			}
			if got := r.Header.Get("loginType"); got != "N_PIN_PC" {
				t.Errorf("chat loginType = %q", got)
			}
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

func TestRelayNativeResponses_NormalizesGatewayJSONError(t *testing.T) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", nil)

	relayNativeResponses(w, w, req, strings.NewReader("{\"code\":\"1\",\"echo\":\"content length exceeded 5242880 bytes\"}\n"))
	stream := w.Body.String()

	if !strings.Contains(stream, "event: error") || !strings.Contains(stream, `"code":"request_too_large"`) {
		t.Fatalf("gateway error was not normalized:\n%s", stream)
	}
	if strings.Contains(stream, `{"code":"1","echo"`) {
		t.Fatalf("raw gateway JSON leaked outside SSE framing:\n%s", stream)
	}
	if strings.Count(stream, "event: error") != 1 {
		t.Fatalf("error event count is not one:\n%s", stream)
	}
}

func TestRelayNativeResponses_CleanEOFMissingTerminalEmitsError(t *testing.T) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	input := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n"

	relayNativeResponses(w, w, req, strings.NewReader(input))
	stream := w.Body.String()

	if !strings.Contains(stream, "event: error") || !strings.Contains(stream, "closed before a terminal Responses event") {
		t.Fatalf("missing terminal error:\n%s", stream)
	}
}
