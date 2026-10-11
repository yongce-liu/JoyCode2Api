package openai

import (
	"strings"
	"testing"
)

func TestNormalizeResponsesDataLine_UnwrapsGatewayEnvelope(t *testing.T) {
	in := `{"code":0,"msg":"success","data":{"type":"response.created","response":{"id":"resp_1"}}}`
	got := normalizeResponsesDataLine(in)
	if !strings.Contains(got, `"response.created"`) {
		t.Fatalf("expected unwrapped event, got %s", got)
	}
	if strings.Contains(got, `"code"`) {
		t.Fatalf("envelope should be stripped, got %s", got)
	}
}

func TestNormalizeResponsesDataLine_KeepsPlainEvents(t *testing.T) {
	in := `{"type":"response.output_text.delta","delta":"hi"}`
	if got := normalizeResponsesDataLine(in); got != in {
		t.Fatalf("plain event must passthrough, got %s", got)
	}
}

func TestNormalizeResponsesDataLine_PassthroughNonJSON(t *testing.T) {
	for _, in := range []string{"[DONE]", "", "not-json", `{"code":0,"msg":"no data"}`} {
		if got := normalizeResponsesDataLine(in); got != strings.TrimSpace(in) {
			t.Fatalf("input %q should passthrough, got %q", in, got)
		}
	}
}

func TestResponsesAllowedFields(t *testing.T) {
	allowed := []string{"model", "input", "stream", "store", "instructions", "reasoning",
		"temperature", "max_output_tokens", "previous_response_id", "modelVersion", "text",
		"tools", "tool_choice", "client", "clientVersion", "tenant", "orgFullName", "userId", "language"}
	for _, f := range allowed {
		if !responsesAllowedFields[f] {
			t.Fatalf("field %s should be allowed", f)
		}
	}
	blocked := []string{"include", "prompt_cache_key", "metadata", "parallel_tool_calls", "truncation", "service_tier"}
	for _, f := range blocked {
		if responsesAllowedFields[f] {
			t.Fatalf("field %s should be dropped", f)
		}
	}
}

func TestServeResponsesStreamNormalizesUpstreamFraming(t *testing.T) {
	upstream := "data: event: response.created\n\n" +
		"data: data: {\"type\":\"response.created\",\"response\":{\"id\":\"1\"}}\n\n" +
		"data: data: {\"type\":\"response.completed\",\"response\":{\"id\":\"1\",\"status\":\"completed\"}}\n\n"
	var out strings.Builder
	nop := nopFlusher{}
	s := &Server{}
	var types []string
	s.serveResponsesStream(&out, nop, strings.NewReader(upstream), func(ev map[string]interface{}) {
		if typ, ok := ev["type"].(string); ok {
			types = append(types, typ)
		}
	})
	got := out.String()
	if strings.Contains(got, "data: data:") || strings.Contains(got, "data: event:") {
		t.Fatalf("double-wrapped framing leaked through: %q", got)
	}
	if !strings.Contains(got, "event: response.completed\ndata: {\"type\":\"response.completed\"") {
		t.Fatalf("expected standard event/data framing, got: %q", got)
	}
	if len(types) != 2 || types[1] != "response.completed" {
		t.Fatalf("onEvent types = %v", types)
	}
}

type nopFlusher struct{}

func (nopFlusher) Flush() {}
