package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

// stubUpstream stands in for the JoyCode gateway: it records the request it
// received and answers with a canned status/body.
type stubUpstream struct {
	server *httptest.Server
	status int
	header http.Header
	body   string

	mu         sync.Mutex
	path       string
	rawBody    []byte
	reqHeaders http.Header
}

func newStubUpstream(t *testing.T) *stubUpstream {
	t.Helper()
	up := &stubUpstream{status: http.StatusOK}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.path = r.URL.Path
		up.rawBody = raw
		up.reqHeaders = r.Header.Clone()
		up.mu.Unlock()
		for key, values := range up.header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(up.status)
		_, _ = io.WriteString(w, up.body)
	}))
	t.Cleanup(up.server.Close)
	return up
}

func (u *stubUpstream) received() (string, []byte, http.Header) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.path, u.rawBody, u.reqHeaders
}

func (u *stubUpstream) reply(status int, contentType, body string) {
	u.status = status
	u.body = body
	if contentType != "" {
		u.header = http.Header{"Content-Type": {contentType}}
	}
}

// newRelay wires a Server to the stub gateway.
func newRelay(t *testing.T, up *stubUpstream) *http.ServeMux {
	t.Helper()
	prev := joycode.BaseURL
	joycode.BaseURL = up.server.URL
	t.Cleanup(func() { joycode.BaseURL = prev })

	client := joycode.NewClient("pt-test-key", "user-1")
	client.SetContext("JD", "ERP", "")
	mux := http.NewServeMux()
	NewServer(client, nil).RegisterRoutes(mux)
	return mux
}

func post(t *testing.T, mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var decoded map[string]interface{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	return decoded
}

// The proxy must not interpret an upstream failure: whatever status and body
// JoyCode answers with reaches the client unchanged, including business errors
// that arrive inside an HTTP 200 body and gateway limit errors that carry only
// an "echo" field.
func TestRelayDoesNotRewriteUpstreamErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"quota envelope in 200", http.StatusOK, `{"code":"1050","msg":"quota exhausted"}`},
		{"gateway echo in 200", http.StatusOK, `{"code":"1","echo":"content length exceeded 5242880 bytes"}`},
		{"bare code in 200", http.StatusOK, `{"code":403,"data":null}`},
		{"provider error status", http.StatusNotFound, `{"type":"error","error":{"type":"not_found_error","message":"model claude-x not found"}}`},
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid credential"}}`},
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				up := newStubUpstream(t)
				up.reply(tc.status, "application/json", tc.body)
				mux := newRelay(t, up)

				rec := post(t, mux, path, `{"model":"m","stream":false}`)
				if rec.Code != tc.status {
					t.Errorf("status = %d, want %d", rec.Code, tc.status)
				}
				if got := rec.Body.String(); got != tc.body {
					t.Errorf("body = %s, want %s", got, tc.body)
				}
			})
		}
	}
}

// A streaming answer is relayed byte-for-byte, so an error event in the middle
// of the stream is not reworded either.
func TestRelayStreamsUpstreamBytesUnchanged(t *testing.T) {
	const stream = "event: response.created\n" +
		"data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"code\":\"1\",\"echo\":\"content length exceeded 5242880 bytes\"}\n\n"
	up := newStubUpstream(t)
	up.reply(http.StatusOK, "text/event-stream", stream)
	mux := newRelay(t, up)

	rec := post(t, mux, "/v1/responses", `{"model":"gpt-x","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != stream {
		t.Errorf("stream altered:\ngot  %q\nwant %q", got, stream)
	}
}

// JoyCode labels every answer text/event-stream, including the single JSON
// document it returns for a request that did not ask for streaming. The label
// must match the body: a client that switches on the content type reads the
// JSON as an event stream with no events and reports a truncated answer.
func TestRelayMatchesContentTypeToPayload(t *testing.T) {
	const answer = `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			up := newStubUpstream(t)
			up.reply(http.StatusOK, "text/event-stream", answer)
			mux := newRelay(t, up)

			rec := post(t, mux, path, `{"model":"m","stream":false}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := rec.Body.String(); got != answer {
				t.Errorf("body = %s, want %s", got, answer)
			}
		})
	}
}

// A stream request that JoyCode answers with its JSON failure envelope would
// otherwise reach the client as an event stream without events. The message is
// copied into the error event the client protocol expects.
func TestRelayFramesUpstreamFailureForStreamClients(t *testing.T) {
	const failure = `{"error":{"code":"1050","message":"quota exhausted"}}`
	cases := []struct {
		path string
		want []string
	}{
		{"/v1/messages", []string{"event: error", `"type":"error"`, "quota exhausted"}},
		{"/v1/chat/completions", []string{"data: ", `"code":"upstream_error"`, "quota exhausted"}},
		{"/v1/responses", []string{"data: ", `"type":"api_error"`, "quota exhausted"}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			up := newStubUpstream(t)
			up.reply(http.StatusOK, "text/event-stream", failure)
			mux := newRelay(t, up)

			rec := post(t, mux, tc.path, `{"model":"m","stream":true}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
				t.Errorf("Content-Type = %q, want text/event-stream", got)
			}
			body := rec.Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q: %s", want, body)
				}
			}
		})
	}
}

// JoyCode's Anthropic endpoint reports a failure as an application/json
// envelope even when the client asked for a stream. Relaying that document
// leaves the client with a stream that never carries an event, which it reports
// as a truncated answer; the envelope is framed as the error event the client
// protocol defines instead of being copied as a document it cannot read.
func TestRelayFramesJSONFailureForStreamClients(t *testing.T) {
	const failure = `{"error":{"code":"1050","message":"quota exhausted"}}`
	cases := []struct {
		path string
		want string
	}{
		{"/v1/messages", `event: error`},
		{"/v1/chat/completions", `data: `},
		{"/v1/responses", `data: `},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			up := newStubUpstream(t)
			up.reply(http.StatusOK, "application/json", failure)
			mux := newRelay(t, up)

			rec := post(t, mux, tc.path, `{"model":"m","stream":true}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
				t.Errorf("Content-Type = %q, want text/event-stream", got)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("body missing %q: %s", tc.want, body)
			}
			if !strings.Contains(body, "quota exhausted") {
				t.Errorf("upstream message missing: %s", body)
			}
		})
	}
}

// An empty upstream stream is reported as well: a client that receives a 200
// and then nothing cannot tell the difference from a cut connection.
func TestRelayReportsEmptyStream(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		up := newStubUpstream(t)
		up.reply(http.StatusOK, "text/event-stream", "")
		mux := newRelay(t, up)

		rec := post(t, mux, path, `{"model":"m","stream":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "data: ") {
			t.Errorf("%s: no error event relayed: %q", path, body)
		}
	}
}

// The client payload is forwarded as it was written: nested structures such as
// Anthropic image blocks survive, and only JoyCode's metadata is added.
func TestRelayForwardsPayloadVerbatimAndAddsMetadata(t *testing.T) {
	up := newStubUpstream(t)
	up.reply(http.StatusOK, "application/json", `{"id":"msg_1"}`)
	mux := newRelay(t, up)

	payload := `{"model":"Claude-Opus-4.7","stream":true,"max_tokens":64,"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}`
	rec := post(t, mux, "/v1/messages", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	path, rawBody, headers := up.received()
	if want := "/api/saas/anthropic/v1/messages"; path != want {
		t.Errorf("upstream path = %q, want %q", path, want)
	}
	if got := headers.Get("ptKey"); got != "pt-test-key" {
		t.Errorf("upstream ptKey = %q, want pt-test-key", got)
	}
	sent := decodeJSON(t, rawBody)

	if sent["model"] != "Claude-Opus-4.7" || sent["stream"] != true {
		t.Errorf("model/stream not preserved: %v / %v", sent["model"], sent["stream"])
	}
	thinking, _ := sent["thinking"].(map[string]interface{})
	if thinking["type"] != "enabled" || thinking["budget_tokens"].(float64) != 1024 {
		t.Errorf("thinking config not preserved: %v", sent["thinking"])
	}
	msgs, _ := sent["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("messages not preserved: %v", sent["messages"])
	}
	parts, _ := msgs[0].(map[string]interface{})["content"].([]interface{})
	if len(parts) != 2 {
		t.Fatalf("content blocks not preserved: %v", parts)
	}
	image, _ := parts[1].(map[string]interface{})["source"].(map[string]interface{})
	if image["data"] != "iVBORw0KGgo=" {
		t.Errorf("image block altered: %v", parts[1])
	}

	for key, want := range map[string]string{
		"userId": "user-1", "client": "JoyCode", "clientVersion": joycode.ClientVersion,
		"language": "UNKNOWN", "tenant": "JD",
	} {
		if sent[key] != want {
			t.Errorf("metadata %s = %v, want %q", key, sent[key], want)
		}
	}
}

// Gateway bookkeeping headers (session tokens, edge node ids) stay upstream;
// only the headers the client acts on are relayed.
func TestRelayDropsInternalUpstreamHeaders(t *testing.T) {
	up := newStubUpstream(t)
	up.reply(http.StatusTooManyRequests, "application/json", `{"code":"1050"}`)
	up.header.Set("Retry-After", "30")
	up.header.Set("X-Rp-Sdtoken", "set;1800;secret")
	up.header.Set("Server", "jfe")
	mux := newRelay(t, up)

	rec := post(t, mux, "/v1/chat/completions", `{"model":"m"}`)
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	for _, header := range []string{"X-Rp-Sdtoken", "Server"} {
		if got := rec.Header().Get(header); got != "" {
			t.Errorf("%s leaked downstream: %q", header, got)
		}
	}
}

// A model the client asked for is never rewritten: JoyCode rejects ids it does
// not serve, and that error names the offending model.
func TestRelayKeepsRequestedModel(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		up := newStubUpstream(t)
		up.reply(http.StatusOK, "application/json", `{}`)
		mux := newRelay(t, up)

		post(t, mux, path, `{"model":"claude-sonnet-4-20250514"}`)
		_, rawBody, _ := up.received()
		if got := decodeJSON(t, rawBody)["model"]; got != "claude-sonnet-4-20250514" {
			t.Errorf("%s: model = %v, want claude-sonnet-4-20250514", path, got)
		}
	}
}

// An empty model falls back to the account default; an explicit one never does.
func TestRelayFillsDefaultModelWhenMissing(t *testing.T) {
	up := newStubUpstream(t)
	up.reply(http.StatusOK, "application/json", `{}`)
	mux := newRelay(t, up)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req = store.InitModel(req)
	req = store.InitAccountModel(req)
	store.SetAccountDefaultModel(req, "Claude-Opus-4.7")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	_, rawBody, _ := up.received()
	if got := decodeJSON(t, rawBody)["model"]; got != "Claude-Opus-4.7" {
		t.Errorf("model = %v, want the account default", got)
	}
	if got := store.GetModel(req); got != "Claude-Opus-4.7" {
		t.Errorf("model in request context = %q, want the account default", got)
	}
}

// Token usage is observed while the body is relayed so the dashboard keeps its
// numbers, for both the OpenAI and the Anthropic spellings.
func TestRelayRecordsTokenUsage(t *testing.T) {
	up := newStubUpstream(t)
	up.reply(http.StatusOK, "application/json", `{"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
	mux := newRelay(t, up)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req = store.InitTokenUsage(req)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	input, output := store.GetTokenUsage(req)
	if input != 11 || output != 7 {
		t.Errorf("usage = (%d, %d), want (11, 7)", input, output)
	}

	up.reply(http.StatusOK, "text/event-stream",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n")
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
	req = store.InitTokenUsage(req)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	input, output = store.GetTokenUsage(req)
	if input != 3 || output != 42 {
		t.Errorf("usage = (%d, %d), want (3, 42)", input, output)
	}
}

func TestRelayRejectsNonJSONBody(t *testing.T) {
	up := newStubUpstream(t)
	up.reply(http.StatusOK, "application/json", `{}`)
	mux := newRelay(t, up)

	rec := post(t, mux, "/v1/messages", `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := decodeJSON(t, rec.Body.Bytes())
	if body["type"] != "error" {
		t.Errorf("Anthropic error envelope expected, got %v", body)
	}
	if _, _, rawBody := up.received(); rawBody != nil {
		t.Errorf("invalid body must not reach upstream: %s", rawBody)
	}
}

func TestRelayRejectsWrongMethod(t *testing.T) {
	up := newStubUpstream(t)
	up.reply(http.StatusOK, "application/json", `{}`)
	mux := newRelay(t, up)

	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", path, rec.Code)
		}
	}
}

func TestModelsEndpointUsesUpstreamCatalog(t *testing.T) {
	up := newStubUpstream(t)
	up.reply(http.StatusOK, "application/json",
		`{"code":0,"data":[{"chatApiModel":"GLM-5.1"},{"modelId":"Kimi-K2.6"},{"chatApiModel":""}]}`)
	mux := newRelay(t, up)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if path, _, _ := up.received(); path != "/api/saas/models/v2/modelList" {
		t.Errorf("upstream path = %q", path)
	}
	body := decodeJSON(t, rec.Body.Bytes())
	data, _ := body["data"].([]interface{})
	var ids []string
	for _, item := range data {
		entry, _ := item.(map[string]interface{})
		ids = append(ids, entry["id"].(string))
	}
	if len(ids) != 2 || ids[0] != "GLM-5.1" || ids[1] != "Kimi-K2.6" {
		t.Errorf("model ids = %v, want [GLM-5.1 Kimi-K2.6]", ids)
	}
}

func TestModelsEndpointRelaysUpstreamFailure(t *testing.T) {
	up := newStubUpstream(t)
	up.reply(http.StatusUnauthorized, "application/json", `{"code":401,"msg":"credential expired"}`)
	mux := newRelay(t, up)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "credential expired") {
		t.Errorf("upstream reason missing from body: %s", rec.Body.String())
	}
}
