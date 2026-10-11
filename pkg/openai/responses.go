package openai

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

// responsesEndpoint 是 JoyCode 上游 Responses API 端点。
// 官方 client（joycoder-editor 3.x）中 rawUrl 为 /api/saas/openai/v1/responses，
// gateway 模式按 functionId=responses_completions 路由。
const responsesEndpoint = "/api/saas/openai/v1/responses"

// 官方客户端请求 Responses 端点时的指纹（customFetch / OpenAiNativeHandlerInner）。
// 老指纹（"JoyCode"/2.7.5）会被上游以 "请升级版本" 拒绝，因此单独维护。
const (
	responsesClient        = "JoyCode IDE"
	responsesClientVersion = "3.0.10"
)

// responsesAllowedFields 是官方客户端 buildRequestBody 实际发送的字段白名单。
// 其余字段（Codex 特有的 include、prompt_cache_key 等）会被丢弃，
// 避免上游因未知参数报错。
var responsesAllowedFields = map[string]bool{
	"model":                true,
	"input":                true,
	"stream":               true,
	"store":                true,
	"instructions":         true,
	"reasoning":            true,
	"temperature":          true,
	"max_output_tokens":    true,
	"previous_response_id": true,
	"modelVersion":         true,
	"text":                 true,
	"tools":                true,
	"tool_choice":          true,
	// 请求包装字段（customFetch 注入，prepareBody 的 extra 会覆盖默认值）
	"client":        true,
	"clientVersion": true,
	"tenant":        true,
	"orgFullName":   true,
	"userId":        true,
	"language":      true,
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		slog.Error("decode responses request", "error", err)
		writeError(w, 400, fmt.Sprintf("请求体解析失败: %s。", err.Error()))
		return
	}

	systemDefault := ""
	if s.store != nil {
		systemDefault = s.store.GetSetting("default_model")
	}
	rawModel, _ := body["model"].(string)
	model := ResolveModel(rawModel, store.GetAccountDefaultModel(r), systemDefault)
	store.SetModel(r, model)

	// OpenAI Responses 规范默认非流式；CPA/codex 会显式传 stream:true
	wantStream := false
	if v, ok := body["stream"].(bool); ok {
		wantStream = v
	}

	// 按官方客户端 buildRequestBody 的口径构造上游请求：
	// 仅保留白名单字段，补默认值，并用 Responses 指纹替换 client 信息。
	up := map[string]interface{}{}
	for k, v := range body {
		if !responsesAllowedFields[k] {
			slog.Debug("responses: dropping unsupported field", "field", k)
			continue
		}
		up[k] = v
	}
	up["model"] = model
	up["stream"] = true // 官方客户端始终流式请求；非流式由本网关聚合
	if _, ok := up["store"]; !ok {
		up["store"] = false
	}
	if _, ok := up["reasoning"]; !ok {
		up["reasoning"] = map[string]interface{}{"effort": "medium"}
	}
	if _, ok := up["max_output_tokens"]; !ok {
		up["max_output_tokens"] = 4096
	}
	if _, ok := up["temperature"]; !ok {
		up["temperature"] = 1
	}
	up["client"] = responsesClient
	up["clientVersion"] = responsesClientVersion

	client := s.getClient(r)
	if wantStream {
		s.handleStreamResponses(w, r, client, up, model)
	} else {
		s.handleNonStreamResponses(w, r, client, up, model)
	}
}

// postResponsesStream 连接上游；若新指纹被 "请升级版本" 拒绝，
// 自动降级为旧指纹（JoyCode/ClientVersion）重试一次。
func (s *Server) postResponsesStream(client *joycode.Client, body map[string]interface{}) (*http.Response, error) {
	resp, err := client.PostStream(responsesEndpoint, body)
	if err == nil {
		return resp, nil
	}
	var apiErr *joycode.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == 403 && strings.Contains(apiErr.Body, "升级") {
		slog.Warn("responses: new client fingerprint rejected, retrying legacy fingerprint")
		body["client"] = "JoyCode"
		body["clientVersion"] = joycode.ClientVersion
		return client.PostStream(responsesEndpoint, body)
	}
	return nil, err
}

// normalizeResponsesDataLine 解开上游网关在 SSE 事件外的包装：
// {"code":0,"msg":"...","data":{...}} → 内层事件 JSON（官方客户端同款逻辑）。
// IO 事件（含 type 字段）保持不变；解析失败时原样返回。
func normalizeResponsesDataLine(payload string) string {
	t := strings.TrimSpace(payload)
	if t == "" || t == "[DONE]" {
		return t
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(t), &obj); err != nil {
		return t
	}
	if _, hasType := obj["type"]; hasType {
		return t
	}
	inner, ok := obj["data"].(map[string]interface{})
	if !ok {
		return t
	}
	if _, innerType := inner["type"]; !innerType {
		return t
	}
	b, err := json.Marshal(inner)
	if err != nil {
		return t
	}
	return string(b)
}

// startResponseHeartbeat 以 5s 间隔发送 SSE 注释行，直到 stop 关闭并等待 goroutine 退出。
func startResponseHeartbeat(w http.ResponseWriter, flusher http.Flusher) (stop chan<- struct{}, done <-chan struct{}) {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		defer close(doneCh)
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}()
	return stopCh, doneCh
}

func (s *Server) handleStreamResponses(w http.ResponseWriter, r *http.Request, client *joycode.Client, body map[string]interface{}, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		slog.Error("streaming not supported by response writer")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(200)

	stopHeartbeat, heartbeatDone := startResponseHeartbeat(w, flusher)

	streamStart := time.Now()
	resp, err := s.postResponsesStream(client, body)
	if err != nil {
		close(stopHeartbeat)
		<-heartbeatDone
		slog.Error("responses stream upstream error", "model", model, "error", err)
		msg := err.Error()
		if isTimeoutError(msg) {
			msg = "上游服务响应超时，请稍后重试。原始错误: " + msg
		}
		store.SetRequestError(r, upstreamStatus(err), msg)
		// 以标准 Responses SSE error 事件告知下游
		data, _ := json.Marshal(map[string]interface{}{
			"type":  "error",
			"error": map[string]interface{}{"message": msg, "type": "api_error"},
		})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
		flusher.Flush()
		return
	}
	defer resp.Body.Close()
	close(stopHeartbeat)
	<-heartbeatDone
	slog.Info("responses stream: connected to upstream", "model", model, "ttfb_ms", time.Since(streamStart).Milliseconds())

	var inTk, outTk int
	sawCompleted := false
	s.serveResponsesStream(w, flusher, resp.Body, func(event map[string]interface{}) {
		typ, _ := event["type"].(string)
		switch typ {
		case "response.completed":
			sawCompleted = true
			inTk, outTk = responsesUsage(event)
		case "response.failed", "error":
			s.recordResponsesEventError(r, event)
		}
	})
	if inTk > 0 || outTk > 0 {
		store.SetTokenUsage(r, inTk, outTk)
	}
	if !sawCompleted {
		slog.Warn("responses stream ended without response.completed", "model", model)
		if store.GetRequestError(r).Status == 0 {
			store.SetRequestError(r, 502, "upstream stream ended before response.completed")
		}
	}
}

// serveResponsesStream 逐行复印上游 SSE 到下游，并对 data 行做包装解包。
// onEvent 回调收到解包后的事件 JSON（若能解析）。
func (s *Server) serveResponsesStream(w io.Writer, flusher http.Flusher, body interface{ Read([]byte) (int, error) }, onEvent func(map[string]interface{})) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			w.Write([]byte("\n"))
			flusher.Flush()
			continue
		}
		out := line
		eventType := ""
		if strings.HasPrefix(line, "data:") {
			// 上游把整段 SSE（含 event:/data: 行）再包了一层 data: 前缀，
			// 例如 "data: event: response.created" 和 "data: data: {...}"。
			// 与官方客户端 normalizeSseDataPayload 同款逻辑：剥掉所有
			// 重复的 "data:" 前缀；剥完是 event: 行的，整行丢弃。
			payload := line
			for strings.HasPrefix(payload, "data:") {
				payload = strings.TrimSpace(strings.TrimPrefix(payload, "data:"))
			}
			if strings.HasPrefix(payload, "event:") {
				continue
			}
			norm := normalizeResponsesDataLine(payload)
			out = "data: " + norm
			var event map[string]interface{}
			if json.Unmarshal([]byte(norm), &event) == nil {
				onEvent(event)
				eventType, _ = event["type"].(string)
			}
		}
		if eventType != "" {
			// 还原标准 SSE 的 event 行（OpenAI Responses 官方格式）
			w.Write([]byte("event: " + eventType + "\n"))
		}
		w.Write([]byte(out))
		w.Write([]byte("\n"))
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		slog.Error("responses stream read error", "error", err)
	}
}

// responsesUsage 从 response.completed 事件中提取 token 用量。
func responsesUsage(event map[string]interface{}) (inTk, outTk int) {
	respObj, _ := event["response"].(map[string]interface{})
	usage, _ := respObj["usage"].(map[string]interface{})
	in, _ := usage["input_tokens"].(float64)
	out, _ := usage["output_tokens"].(float64)
	return int(in), int(out)
}

// recordResponsesEventError 记录 response.failed / error 事件中的错误信息。
func (s *Server) recordResponsesEventError(r *http.Request, event map[string]interface{}) {
	msg := ""
	if respObj, ok := event["response"].(map[string]interface{}); ok {
		if e, ok := respObj["error"].(map[string]interface{}); ok {
			msg, _ = e["message"].(string)
		}
	}
	if msg == "" {
		if e, ok := event["error"].(map[string]interface{}); ok {
			msg, _ = e["message"].(string)
		}
	}
	if msg == "" {
		msg = "upstream response failed"
	}
	slog.Error("responses upstream event error", "message", msg)
	store.SetRequestError(r, 502, msg)
}

// handleNonStreamResponses 聚合上游流式事件，最终返回单个 Response 对象 JSON。
func (s *Server) handleNonStreamResponses(w http.ResponseWriter, r *http.Request, client *joycode.Client, body map[string]interface{}, model string) {
	resp, err := s.postResponsesStream(client, body)
	if err != nil {
		slog.Error("responses non-stream upstream error", "model", model, "error", err)
		msg := err.Error()
		if isTimeoutError(msg) {
			msg = "上游服务响应超时，请稍后重试。原始错误: " + msg
		}
		writeError(w, upstreamStatus(err), msg)
		return
	}
	defer resp.Body.Close()

	var completed map[string]interface{}
	var inTk, outTk int
	sink := &discardFlusher{}
	s.serveResponsesStream(sink, sink, resp.Body, func(event map[string]interface{}) {
		typ, _ := event["type"].(string)
		switch typ {
		case "response.completed":
			if respObj, ok := event["response"].(map[string]interface{}); ok {
				completed = respObj
			}
			inTk, outTk = responsesUsage(event)
		case "response.failed", "error":
			s.recordResponsesEventError(r, event)
		}
	})
	if inTk > 0 || outTk > 0 {
		store.SetTokenUsage(r, inTk, outTk)
	}
	if completed == nil {
		msg := "上游流式响应未包含 response.completed 事件"
		if re := store.GetRequestError(r); re.Status != 0 {
			msg = re.Message
		}
		writeError(w, 502, msg)
		return
	}
	writeJSON(w, 200, completed)
}

// discardFlusher 丢弃写入的 sink，供非流式聚合复用流式扫描逻辑。
type discardFlusher struct{}

func (d *discardFlusher) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardFlusher) Flush()                      {}
