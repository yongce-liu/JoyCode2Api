package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

// relay builds the handler for one agent endpoint. It forwards the request to
// the JoyCode endpoint that speaks the same protocol and copies the answer
// back, status line included, so upstream failures reach the client unchanged.
func (s *Server) relay(endpoint string, protocol joycode.Protocol, shape errorShape) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requirePOST(w, r, shape) {
			return
		}
		payload, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, shape, "invalid_request_error", "读取请求体失败: "+err.Error())
			return
		}
		_ = r.Body.Close()

		payload, model, err := s.applyDefaultModel(r, payload)
		if err != nil {
			writeError(w, http.StatusBadRequest, shape, "invalid_request_error", err.Error())
			return
		}
		store.SetModel(r, model)

		client := s.getClient(r)
		if client == nil {
			writeError(w, http.StatusBadGateway, shape, "api_error", "no JoyCode credential available")
			return
		}
		resp, err := client.ForwardContext(r.Context(), endpoint, protocol, payload)
		if err != nil {
			reqLog(r).Error("relay upstream request failed", "endpoint", endpoint, "model", model, "error", err)
			writeError(w, http.StatusBadGateway, shape, "api_error", err.Error())
			return
		}
		defer resp.Body.Close()
		relayResponse(w, r, resp, shape, wantsStream(payload))
	}
}

// wantsStream reports whether the client asked for a token-by-token answer. It
// decides which framing the answer is allowed to carry: JoyCode labels every
// body text/event-stream, even the plain JSON it returns when stream is false.
func wantsStream(payload []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return false
	}
	return probe.Stream
}

// applyDefaultModel fills in the configured default when the client sent no
// model at all. An explicitly requested model is never rewritten: JoyCode
// rejects an id it does not serve, and that error names the offending model
// instead of answering from a different one.
func (s *Server) applyDefaultModel(r *http.Request, payload []byte) ([]byte, string, error) {
	fields := map[string]json.RawMessage{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &fields); err != nil {
			return nil, "", fmt.Errorf("请求体必须是一个 JSON 对象: %w", err)
		}
	}
	if raw, ok := fields["model"]; ok {
		var requested string
		if err := json.Unmarshal(raw, &requested); err == nil && strings.TrimSpace(requested) != "" {
			// The payload is relayed byte-for-byte; only JoyCode's own metadata
			// envelope is added on the way out.
			return payload, requested, nil
		}
	}
	model := joycode.ResolveModel("", store.GetAccountDefaultModel(r), s.setting("default_model"))
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, "", err
	}
	fields["model"] = raw
	updated, err := json.Marshal(fields)
	if err != nil {
		return nil, "", err
	}
	return updated, model, nil
}

// relayResponse copies the upstream response to the client as-is. The relayed
// bytes are also observed for token usage, which the dashboard reports.
//
// One header is corrected, and only when the pair does not match: JoyCode
// labels every answer text/event-stream, but a request that did not ask for
// streaming is answered with a single JSON object. A client that switches on
// the content type (Claude Code behind a gateway) reads that JSON as an event
// stream and reports an empty or truncated response, so the label is made to
// match the body. The same mismatch happens the other way round: JoyCode's
// Anthropic endpoint reports failures as an application/json envelope even when
// the client asked for a stream, and a client holding a stream that carries no
// event cannot tell that answer from a cut connection. That envelope is
// therefore framed as the error event the client's protocol defines; the
// upstream message is copied, nothing else is added.
func relayResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, shape errorShape, clientStream bool) {
	body := bufio.NewReaderSize(resp.Body, 64*1024)
	usage := &usageObserver{}
	source := io.TeeReader(body, usage)
	streamLabel := isEventStream(resp.Header)

	// Only a non-streaming client is made to wait: it cannot use a partial
	// answer anyway, so the first byte is inspected before the status line and
	// the content type go out.
	jsonPayload := peekIsJSON(body)
	if jsonPayload && !clientStream {
		resp.Header.Set("Content-Type", "application/json")
	}
	if jsonPayload && clientStream && !streamLabel {
		resp.Header.Set("Content-Type", "text/event-stream")
	}

	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if clientStream && (streamLabel || jsonPayload) {
		relayEventStream(w, r, source, body, shape)
	} else if _, err := io.Copy(w, source); err != nil {
		reqLog(r).Error("relay response copy error", "error", err)
	}
	usage.record(r)
}

// relayEventStream forwards an event stream chunk by chunk and flushes every
// write so each event reaches the client as soon as upstream produced it.
func relayEventStream(w http.ResponseWriter, r *http.Request, source io.Reader, body *bufio.Reader, shape errorShape) {
	if peekIsJSON(body) {
		relayStreamError(w, source, shape)
		return
	}
	flusher, _ := w.(http.Flusher)
	stream := make([]byte, 32*1024)
	for wrote := false; ; {
		n, err := source.Read(stream)
		if n > 0 {
			wrote = true
			if _, writeErr := w.Write(stream[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				reqLog(r).Error("relay stream read error", "error", err)
			} else if !wrote {
				// The client is holding a stream that will never carry an
				// event; without this it looks like a truncated answer.
				relayStreamError(w, bytes.NewReader(nil), shape)
			}
			return
		}
	}
}

// relayStreamError turns an upstream body that is not an event stream into the
// error event the client protocol expects. Only the framing is added; the
// upstream message is copied.
func relayStreamError(w http.ResponseWriter, body io.Reader, shape errorShape) {
	payload, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		payload = nil
	}
	message := upstreamErrorMessage(payload)

	var frame []byte
	if shape == shapeAnthropic {
		event, _ := json.Marshal(map[string]interface{}{
			"type":  "error",
			"error": map[string]interface{}{"type": "api_error", "message": message},
		})
		frame = []byte("event: error\ndata: " + string(event) + "\n\n")
	} else {
		event, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{"type": "api_error", "code": "upstream_error", "message": message},
		})
		frame = []byte("data: " + string(event) + "\n\n")
	}
	_, _ = w.Write(frame)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// upstreamErrorMessage reads the message out of a JoyCode error envelope,
// falling back to the raw body so nothing is hidden from the client.
func upstreamErrorMessage(body []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		if message := strings.TrimSpace(envelope.Error.Message); message != "" {
			return message
		}
	}
	if message := strings.TrimSpace(string(body)); message != "" {
		return message
	}
	return "上游没有返回任何内容"
}

// peekIsJSON reports whether the buffered body is a JSON document. It waits for
// the first byte, which is why only callers that must decide on the framing
// before anything is written use it.
func peekIsJSON(body *bufio.Reader) bool {
	head, _ := body.Peek(1)
	return len(head) > 0 && (head[0] == '{' || head[0] == '[')
}

// relayedHeaders are the upstream headers that mean something to the client.
// Everything else stays internal: JoyCode marks its answers with gateway
// bookkeeping headers (session tokens, edge node ids) that must not be handed
// downstream or written into proxy logs.
var relayedHeaders = []string{"Content-Type", "Cache-Control", "Retry-After"}

// copyUpstreamHeaders mirrors the allowlisted upstream headers to the client.
func copyUpstreamHeaders(dst, src http.Header) {
	for _, key := range relayedHeaders {
		for _, value := range src.Values(key) {
			dst.Add(key, value)
		}
	}
	dst.Set("Access-Control-Allow-Origin", "*")
}

func isEventStream(header http.Header) bool {
	return strings.HasPrefix(strings.ToLower(header.Get("Content-Type")), "text/event-stream")
}

// usageObserver picks the token counts out of a relayed body. Both the OpenAI
// (prompt_tokens/completion_tokens) and the Anthropic/Responses
// (input_tokens/output_tokens) spellings are recognised, so the dashboard keeps
// its numbers without parsing the response into an intermediate model.
type usageObserver struct {
	buf    []byte
	input  int
	output int
}

// maxObservedLine bounds the buffer when an upstream never emits a newline.
const maxObservedLine = 1 << 20

type tokenCounts struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

type usageEnvelope struct {
	Usage    *tokenCounts `json:"usage"`
	Response *struct {
		Usage *tokenCounts `json:"usage"`
	} `json:"response"`
	Message *struct {
		Usage *tokenCounts `json:"usage"`
	} `json:"message"`
}

func (u *usageObserver) Write(p []byte) (int, error) {
	u.buf = append(u.buf, p...)
	for {
		index := bytes.IndexByte(u.buf, '\n')
		if index < 0 {
			break
		}
		u.observeLine(u.buf[:index])
		u.buf = append(u.buf[:0], u.buf[index+1:]...)
	}
	if len(u.buf) > maxObservedLine {
		u.buf = u.buf[:0]
	}
	return len(p), nil
}

func (u *usageObserver) observeLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data: ")) {
		trimmed = bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data: ")))
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return
	}
	var envelope usageEnvelope
	if json.Unmarshal(trimmed, &envelope) != nil {
		return
	}
	u.apply(envelope.Usage)
	if envelope.Response != nil {
		u.apply(envelope.Response.Usage)
	}
	if envelope.Message != nil {
		u.apply(envelope.Message.Usage)
	}
}

func (u *usageObserver) apply(counts *tokenCounts) {
	if counts == nil {
		return
	}
	if counts.InputTokens > 0 {
		u.input = counts.InputTokens
	} else if counts.PromptTokens > 0 {
		u.input = counts.PromptTokens
	}
	if counts.OutputTokens > 0 {
		u.output = counts.OutputTokens
	} else if counts.CompletionTokens > 0 {
		u.output = counts.CompletionTokens
	}
}

// record stores the observed usage, including a trailing line that arrived
// without a newline (a plain JSON body, for instance).
func (u *usageObserver) record(r *http.Request) {
	if len(u.buf) > 0 {
		u.observeLine(u.buf)
		u.buf = nil
	}
	if u.input > 0 || u.output > 0 {
		store.SetTokenUsage(r, u.input, u.output)
	}
}
