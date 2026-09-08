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

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/common"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Error("decode chat request", "error", err)
		writeError(w, 400, fmt.Sprintf("请求体解析失败: %s。请检查请求是否完整，或尝试开启新对话减少上下文长度。", err.Error()))
		return
	}
	systemDefault := ""
	if s.store != nil {
		systemDefault = s.store.GetSetting("default_model")
	}
	model := ResolveModelWithCatalog(req.Model, store.GetAccountDefaultModel(r), systemDefault, modelCatalog(s.store))
	store.SetModel(r, model)
	req.Model = model
	jcBody := TranslateRequest(&req)
	client := s.getClient(r)
	if req.Stream {
		s.handleStreamChat(w, r, client, jcBody, model)
	} else {
		s.handleNonStreamChat(w, r, client, jcBody, model)
	}
}

func (s *Server) handleNonStreamChat(w http.ResponseWriter, r *http.Request, client *joycode.Client, jcBody map[string]interface{}, model string) {
	resp, err := client.Post("/api/saas/openai/v1/chat/completions", jcBody)
	if err != nil {
		slog.Error("chat non-stream upstream error", "model", model, "error", err)
		msg := err.Error()
		code := 500
		if isTimeoutError(msg) {
			code = 504
			msg = "上游服务响应超时，请稍后重试。原始错误: " + msg
		}
		writeError(w, code, msg)
		return
	}
	if usage, ok := resp["usage"].(map[string]interface{}); ok {
		inTk, _ := usage["prompt_tokens"].(float64)
		outTk, _ := usage["completion_tokens"].(float64)
		store.SetTokenUsage(r, int(inTk), int(outTk))
	}
	writeJSON(w, 200, TranslateResponse(resp, model))
}

func (s *Server) handleStreamChat(w http.ResponseWriter, r *http.Request, client *joycode.Client, jcBody map[string]interface{}, model string) {
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

	// Start a heartbeat goroutine. The upstream JoyCode API buffers the entire
	// response before sending anything (TTFB can be 10–30s for reasoning models).
	// Without keepalive, downstream clients (Claude Code, OpenAI clients) may
	// time out or show "no response" during this gap. SSE comment lines (": ...")
	// are part of the spec and ignored by all compliant clients.
	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		defer close(heartbeatDone)
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-ticker.C:
				if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}()

	streamStart := time.Now()
	resp, err := client.PostStream("/api/saas/openai/v1/chat/completions", jcBody)
	if err != nil {
		close(stopHeartbeat)
		<-heartbeatDone
		slog.Error("chat stream upstream error", "model", model, "error", err)
		msg := err.Error()
		if isTimeoutError(msg) {
			msg = "上游服务响应超时，请稍后重试。原始错误: " + msg
		}
		writeChatStreamError(w, flusher, msg, "upstream_connection_error")
		return
	}
	defer resp.Body.Close()
	close(stopHeartbeat)
	<-heartbeatDone
	slog.Info("stream: connected to upstream", "model", model, "ttfb_ms", time.Since(streamStart).Milliseconds())

	relayChatStream(w, flusher, r, resp.Body, model)
}

type chatStreamChunk struct {
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string      `json:"message"`
		Code    interface{} `json:"code"`
	} `json:"error"`
}

func relayChatStream(w io.Writer, flusher http.Flusher, r *http.Request, body io.Reader, model string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var inTk, outTk, chunkCount int
	var finishReason string
	var streamErr *chatStreamChunk
	sawDone := false

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if !sawDone {
				fmt.Fprintln(w)
				flusher.Flush()
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			fmt.Fprintln(w, line)
			flusher.Flush()
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			continue
		}

		var chunk chatStreamChunk
		if json.Unmarshal([]byte(payload), &chunk) == nil {
			chunkCount++
			if chunk.Usage != nil {
				inTk = chunk.Usage.PromptTokens
				outTk = chunk.Usage.CompletionTokens
			}
			if chunk.Error != nil {
				streamErr = &chunk
				break
			}
			for _, choice := range chunk.Choices {
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					finishReason = *choice.FinishReason
				}
			}
		}

		fmt.Fprintln(w, line)
		flusher.Flush()
	}

	if inTk > 0 || outTk > 0 {
		store.SetTokenUsage(r, inTk, outTk)
	}
	if streamErr != nil {
		message := streamErr.Error.Message
		if message == "" {
			message = "upstream returned a stream error"
		}
		code := fmt.Sprint(streamErr.Error.Code)
		if code == "" || code == "<nil>" {
			code = "upstream_stream_error"
		}
		slog.Error("chat stream upstream error", "model", model, "code", code, "chunks", chunkCount)
		writeChatStreamError(w, flusher, message, code)
		return
	}
	if err := scanner.Err(); err != nil && finishReason == "" {
		slog.Error("chat stream read error before finish_reason", "model", model, "error", err, "chunks", chunkCount, "saw_done", sawDone)
		writeChatStreamError(w, flusher, "upstream stream interrupted before finish_reason: "+err.Error(), "upstream_stream_error")
		return
	}
	if finishReason == "" {
		slog.Warn("chat stream closed before finish_reason", "model", model, "chunks", chunkCount, "saw_done", sawDone)
		writeChatStreamError(w, flusher, "upstream stream closed before finish_reason", "upstream_stream_error")
		return
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("chat stream read error after finish_reason", "model", model, "finish_reason", finishReason, "error", err)
	}
	slog.Info("chat stream completed", "model", model, "finish_reason", finishReason, "chunks", chunkCount, "saw_done", sawDone)
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeChatStreamError(w io.Writer, flusher http.Flusher, message, code string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "api_error",
			"param":   nil,
			"code":    code,
		},
	})
	fmt.Fprintf(w, "data: %s\n\n", payload)
	flusher.Flush()
}

func isTimeoutError(msg string) bool {
	return common.IsTimeoutError(errors.New(msg))
}
