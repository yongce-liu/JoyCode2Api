package openai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

const nativeChatEndpoint = "/api/saas/openai/v1/chat/completions"

func modelCatalog(s *store.Store) []string {
	if s == nil {
		return nil
	}
	parts := strings.Split(s.GetSetting("available_models"), ",")
	models := make([]string, 0, len(parts))
	for _, part := range parts {
		if model := strings.TrimSpace(part); model != "" {
			models = append(models, model)
		}
	}
	return models
}

func modelAdapters(s *store.Store) map[string]string {
	if s == nil {
		return nil
	}
	adapters := map[string]string{}
	_ = json.Unmarshal([]byte(s.GetSetting("model_adapters")), &adapters)
	return adapters
}

// IsNativeResponsesModel is retained for compatibility. It identifies GPT
// models that require the plugin short key; JoyCode still serves them through
// Chat Completions even when the client-facing protocol is Responses.
func IsNativeResponsesModel(model string, s *store.Store) bool {
	for id, adapter := range modelAdapters(s) {
		if sameModelID(id, model) && strings.EqualFold(adapter, "openai-response") {
			return true
		}
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt")
}

// nativeResponsesModelID normalizes a catalog display name to the canonical
// upstream model id expected by the openai-response adapter. JoyCode's color
// gateway forwards openai-response models straight to the real OpenAI upstream,
// which rejects the human-facing catalog label ("GPT-5.6 Sol") with a 1032
// "HTTP调用异常" error and only accepts the lowercased, hyphenated id
// ("gpt-5.6-sol"). Chat-completions adapter models (GLM, Claude, ...) keep
// their catalog id, so this transform is applied only on the GPT native path.
func nativeResponsesModelID(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	return strings.Join(strings.Fields(id), "-")
}

func setting(s *store.Store, key string) string {
	if s == nil {
		return ""
	}
	return s.GetSetting(key)
}

// handleShortKeyChat sends an OpenAI Chat Completions request with the plugin
// short-key context. The upstream endpoint remains chat/completions.
func (s *Server) handleShortKeyChat(w http.ResponseWriter, r *http.Request, client *joycode.Client, body map[string]interface{}, model string, stream bool) {
	body["model"] = nativeResponsesModelID(model)
	if stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "streaming not supported")
			return
		}
		resp, err := client.PostNativeStream(nativeChatEndpoint, body)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		relayChatStream(w, flusher, r, resp.Body, model)
		return
	}
	resp, err := client.PostNative(nativeChatEndpoint, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	recordChatUsage(r, resp)
	writeJSON(w, http.StatusOK, TranslateResponse(resp, model))
}

// handleResponses keeps Codex's client-facing Responses API while translating
// its request to JoyCode's Chat Completions upstream.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var request map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid Responses request: "+err.Error())
		return
	}
	requested, _ := request["model"].(string)
	model := ResolveModelWithCatalog(requested, store.GetAccountDefaultModel(r), setting(s.store, "default_model"), modelCatalog(s.store))
	if !IsNativeResponsesModel(model, s.store) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("model %q does not use the GPT short-key adapter", model))
		return
	}
	request["model"] = model
	store.SetModel(r, model)
	chatBody := responsesRequestToChat(request)
	chatBody["model"] = nativeResponsesModelID(model)
	stream, _ := request["stream"].(bool)
	client := s.getClient(r)
	if stream {
		s.streamChatAsResponses(w, r, client, chatBody, model)
		return
	}
	chatResp, err := client.PostNative(nativeChatEndpoint, chatBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	recordChatUsage(r, chatResp)
	writeJSON(w, http.StatusOK, chatResponseToResponses(chatResp, model))
}

func responsesRequestToChat(request map[string]interface{}) map[string]interface{} {
	body := map[string]interface{}{
		"model":  request["model"],
		"stream": request["stream"],
	}
	messages := make([]interface{}, 0)
	if instructions, ok := request["instructions"].(string); ok && instructions != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": instructions})
	}
	switch input := request["input"].(type) {
	case string:
		messages = append(messages, map[string]interface{}{"role": "user", "content": input})
	case []interface{}:
		for _, raw := range input {
			item, _ := raw.(map[string]interface{})
			switch item["type"] {
			case "function_call":
				messages = append(messages, map[string]interface{}{
					"role": "assistant",
					"tool_calls": []interface{}{map[string]interface{}{
						"id": item["call_id"], "type": "function",
						"function": map[string]interface{}{"name": item["name"], "arguments": item["arguments"]},
					}},
				})
			case "function_call_output":
				messages = append(messages, map[string]interface{}{
					"role": "tool", "tool_call_id": item["call_id"], "content": item["output"],
				})
			default:
				role, _ := item["role"].(string)
				if role == "" {
					role = "user"
				}
				messages = append(messages, map[string]interface{}{"role": role, "content": responsesContentToChat(item["content"])})
			}
		}
	}
	body["messages"] = messages
	if value, ok := request["max_output_tokens"]; ok {
		body["max_tokens"] = value
	}
	for _, key := range []string{"temperature", "top_p", "parallel_tool_calls"} {
		if value, ok := request[key]; ok {
			body[key] = value
		}
	}
	if value, ok := request["tool_choice"]; ok {
		body["tool_choice"] = responsesToolChoiceToChat(value)
	}
	if tools, ok := request["tools"].([]interface{}); ok {
		converted := make([]interface{}, 0, len(tools))
		for _, raw := range tools {
			tool, _ := raw.(map[string]interface{})
			if tool["type"] != "function" {
				continue
			}
			fn := map[string]interface{}{"name": tool["name"], "parameters": tool["parameters"]}
			if value, ok := tool["description"]; ok {
				fn["description"] = value
			}
			if value, ok := tool["strict"]; ok {
				fn["strict"] = value
			}
			converted = append(converted, map[string]interface{}{"type": "function", "function": fn})
		}
		body["tools"] = converted
	}
	return body
}

func responsesToolChoiceToChat(value interface{}) interface{} {
	choice, ok := value.(map[string]interface{})
	if !ok || choice["type"] != "function" {
		return value
	}
	return map[string]interface{}{
		"type":     "function",
		"function": map[string]interface{}{"name": choice["name"]},
	}
}

func responsesContentToChat(value interface{}) interface{} {
	parts, ok := value.([]interface{})
	if !ok {
		return value
	}
	converted := make([]interface{}, 0, len(parts))
	for _, raw := range parts {
		part, _ := raw.(map[string]interface{})
		switch part["type"] {
		case "input_text", "output_text":
			converted = append(converted, map[string]interface{}{"type": "text", "text": part["text"]})
		case "input_image":
			converted = append(converted, map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": part["image_url"]}})
		default:
			converted = append(converted, part)
		}
	}
	return converted
}

func chatResponseToResponses(chat map[string]interface{}, model string) map[string]interface{} {
	responseID := "resp_" + newShortID()
	output := make([]interface{}, 0)
	choices, _ := chat["choices"].([]interface{})
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]interface{})
		message, _ := choice["message"].(map[string]interface{})
		if text, ok := message["content"].(string); ok && text != "" {
			output = append(output, responseMessageItem("msg_"+newShortID(), text, "completed"))
		}
		if calls, ok := message["tool_calls"].([]interface{}); ok {
			for _, raw := range calls {
				call, _ := raw.(map[string]interface{})
				fn, _ := call["function"].(map[string]interface{})
				output = append(output, map[string]interface{}{
					"type": "function_call", "id": "fc_" + newShortID(), "status": "completed",
					"call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"],
				})
			}
		}
	}
	usage := chatUsageToResponses(chat["usage"])
	return map[string]interface{}{
		"id": responseID, "object": "response", "created_at": time.Now().Unix(), "status": "completed",
		"model": model, "output": output, "usage": usage, "error": nil, "incomplete_details": nil,
	}
}

func responseMessageItem(id, text, status string) map[string]interface{} {
	return map[string]interface{}{
		"type": "message", "id": id, "status": status, "role": "assistant",
		"content": []interface{}{map[string]interface{}{"type": "output_text", "text": text, "annotations": []interface{}{}}},
	}
}

func chatUsageToResponses(value interface{}) map[string]interface{} {
	usage, _ := value.(map[string]interface{})
	input := numberAsInt(usage["prompt_tokens"])
	output := numberAsInt(usage["completion_tokens"])
	return map[string]interface{}{
		"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
		"input_tokens_details":  map[string]interface{}{"cached_tokens": 0},
		"output_tokens_details": map[string]interface{}{"reasoning_tokens": 0},
	}
}

func (s *Server) streamChatAsResponses(w http.ResponseWriter, r *http.Request, client *joycode.Client, body map[string]interface{}, model string) {
	resp, err := client.PostNativeStream(nativeChatEndpoint, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	responseID := "resp_" + newShortID()
	messageID := "msg_" + newShortID()
	sequence := 0
	writeEvent := func(event map[string]interface{}) {
		event["sequence_number"] = sequence
		sequence++
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	baseResponse := map[string]interface{}{
		"id": responseID, "object": "response", "created_at": time.Now().Unix(), "status": "in_progress",
		"model": model, "output": []interface{}{}, "error": nil, "incomplete_details": nil,
	}
	writeEvent(map[string]interface{}{"type": "response.created", "response": baseResponse})
	message := responseMessageItem(messageID, "", "in_progress")
	message["content"] = []interface{}{}
	writeEvent(map[string]interface{}{"type": "response.output_item.added", "output_index": 0, "item": message})
	part := map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}}
	writeEvent(map[string]interface{}{"type": "response.content_part.added", "item_id": messageID, "output_index": 0, "content_index": 0, "part": part})

	var text strings.Builder
	type streamedCall struct {
		ID, CallID, Name string
		Arguments        strings.Builder
		OutputIndex      int
	}
	calls := map[int]*streamedCall{}
	usage := chatUsageToResponses(nil)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]interface{}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if rawUsage, ok := chunk["usage"]; ok {
			usage = chatUsageToResponses(rawUsage)
			store.SetTokenUsage(r, numberAsInt(usage["input_tokens"]), numberAsInt(usage["output_tokens"]))
		}
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]interface{})
		delta, _ := choice["delta"].(map[string]interface{})
		if value, ok := delta["content"].(string); ok && value != "" {
			text.WriteString(value)
			writeEvent(map[string]interface{}{
				"type": "response.output_text.delta", "item_id": messageID, "output_index": 0, "content_index": 0,
				"delta": value, "logprobs": []interface{}{},
			})
		}
		if rawCalls, ok := delta["tool_calls"].([]interface{}); ok {
			for _, raw := range rawCalls {
				callDelta, _ := raw.(map[string]interface{})
				index := numberAsInt(callDelta["index"])
				call := calls[index]
				fn, _ := callDelta["function"].(map[string]interface{})
				if call == nil {
					call = &streamedCall{ID: "fc_" + newShortID(), OutputIndex: len(calls) + 1}
					call.CallID, _ = callDelta["id"].(string)
					call.Name, _ = fn["name"].(string)
					calls[index] = call
					writeEvent(map[string]interface{}{
						"type": "response.output_item.added", "output_index": call.OutputIndex,
						"item": map[string]interface{}{"type": "function_call", "id": call.ID, "status": "in_progress", "call_id": call.CallID, "name": call.Name, "arguments": ""},
					})
				}
				if id, ok := callDelta["id"].(string); ok && id != "" {
					call.CallID = id
				}
				if name, ok := fn["name"].(string); ok && name != "" {
					call.Name = name
				}
				if args, ok := fn["arguments"].(string); ok && args != "" {
					call.Arguments.WriteString(args)
					writeEvent(map[string]interface{}{
						"type": "response.function_call_arguments.delta", "item_id": call.ID, "output_index": call.OutputIndex, "delta": args,
					})
				}
			}
		}
	}

	writeEvent(map[string]interface{}{"type": "response.output_text.done", "item_id": messageID, "output_index": 0, "content_index": 0, "text": text.String(), "logprobs": []interface{}{}})
	doneMessage := responseMessageItem(messageID, text.String(), "completed")
	writeEvent(map[string]interface{}{"type": "response.content_part.done", "item_id": messageID, "output_index": 0, "content_index": 0, "part": doneMessage["content"].([]interface{})[0]})
	writeEvent(map[string]interface{}{"type": "response.output_item.done", "output_index": 0, "item": doneMessage})
	output := []interface{}{doneMessage}
	callIndexes := make([]int, 0, len(calls))
	for index := range calls {
		callIndexes = append(callIndexes, index)
	}
	sort.Ints(callIndexes)
	for _, index := range callIndexes {
		call := calls[index]
		if call == nil {
			continue
		}
		writeEvent(map[string]interface{}{"type": "response.function_call_arguments.done", "item_id": call.ID, "output_index": call.OutputIndex, "arguments": call.Arguments.String()})
		item := map[string]interface{}{"type": "function_call", "id": call.ID, "status": "completed", "call_id": call.CallID, "name": call.Name, "arguments": call.Arguments.String()}
		writeEvent(map[string]interface{}{"type": "response.output_item.done", "output_index": call.OutputIndex, "item": item})
		output = append(output, item)
	}
	completed := map[string]interface{}{
		"id": responseID, "object": "response", "created_at": baseResponse["created_at"], "status": "completed",
		"model": model, "output": output, "usage": usage, "error": nil, "incomplete_details": nil,
	}
	writeEvent(map[string]interface{}{"type": "response.completed", "response": completed})
}

func recordChatUsageLine(r *http.Request, line string) {
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var chunk map[string]interface{}
	if json.Unmarshal([]byte(payload), &chunk) == nil {
		recordChatUsage(r, chunk)
	}
}

func recordChatUsage(r *http.Request, value map[string]interface{}) {
	usage, _ := value["usage"].(map[string]interface{})
	if usage != nil {
		store.SetTokenUsage(r, numberAsInt(usage["prompt_tokens"]), numberAsInt(usage["completion_tokens"]))
	}
}

func numberAsInt(value interface{}) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	default:
		return 0
	}
}
