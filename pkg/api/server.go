// Package api exposes JoyCode to coding agents without translating protocols.
//
// Every agent-facing endpoint mirrors the JoyCode endpoint that speaks the same
// wire protocol: the client payload is forwarded as-is (plus the plugin metadata
// JoyCode requires) and the upstream status, headers and body are relayed back
// verbatim, errors included. A client that asks for a protocol or model JoyCode
// does not serve receives JoyCode's own error instead of a fallback.
//
// Only the response framing is adjusted, because JoyCode labels every body
// text/event-stream: the content type is matched to the body it describes, and
// an error body that arrives where an event stream was expected is framed as
// the error event the client's protocol defines. See relayResponse.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

// maxRequestBytes caps a client request body; JoyCode rejects anything past
// 5 MiB itself, but the proxy still needs a bound of its own.
const maxRequestBytes = 100 << 20

// ClientResolver returns the joycode.Client that holds the caller's credentials.
type ClientResolver func(r *http.Request) *joycode.Client

// Server implements the agent-facing API.
type Server struct {
	Client   *joycode.Client
	Resolver ClientResolver
	store    *store.Store
}

// NewServer creates a new relay server.
func NewServer(c *joycode.Client, s *store.Store) *Server {
	return &Server{Client: c, store: s}
}

func (s *Server) getClient(r *http.Request) *joycode.Client {
	if s.Resolver != nil {
		return s.Resolver(r)
	}
	return s.Client
}

// RegisterRoutes registers every agent-facing endpoint on the mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/chat/completions", s.relay(joycode.EndpointChatCompletions, joycode.ProtocolOpenAI, shapeOpenAI))
	mux.HandleFunc("/v1/responses", s.relay(joycode.EndpointResponses, joycode.ProtocolOpenAI, shapeOpenAI))
	mux.HandleFunc("/v1/messages", s.relay(joycode.EndpointMessages, joycode.ProtocolAnthropic, shapeAnthropic))
	mux.HandleFunc("/v1/web-search", s.relay(joycode.EndpointWebSearch, joycode.ProtocolOpenAI, shapeOpenAI))
	mux.HandleFunc("/v1/rerank", s.relay(joycode.EndpointRerank, joycode.ProtocolOpenAI, shapeOpenAI))
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/health", s.handleHealth)
}

// errorShape selects the JSON error envelope the client's protocol expects.
type errorShape int

const (
	shapeOpenAI errorShape = iota
	shapeAnthropic
)

func writeCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("writeJSON: marshal failed", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	w.Write(b)
}

// writeError reports a failure the proxy itself detected. Failures coming from
// JoyCode are relayed untouched instead of being re-shaped here.
func writeError(w http.ResponseWriter, code int, shape errorShape, errType, message string) {
	if shape == shapeAnthropic {
		writeJSON(w, code, map[string]interface{}{
			"type":  "error",
			"error": map[string]interface{}{"type": errType, "message": message},
		})
		return
	}
	writeJSON(w, code, map[string]interface{}{
		"error": map[string]interface{}{"type": errType, "message": message},
	})
}

func requirePOST(w http.ResponseWriter, r *http.Request, shape errorShape) bool {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		w.WriteHeader(http.StatusOK)
		return false
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shape, "invalid_request_error", "method not allowed")
		return false
	}
	return true
}

func requireGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		w.WriteHeader(http.StatusOK)
		return false
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shapeOpenAI, "invalid_request_error", "method not allowed")
		return false
	}
	return true
}

func (s *Server) setting(key string) string {
	if s.store == nil {
		return ""
	}
	return s.store.GetSetting(key)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok", "service": "joycode-proxy",
		"endpoints": []string{
			"/v1/chat/completions", "/v1/responses", "/v1/messages",
			"/v1/models", "/v1/web-search", "/v1/rerank",
		},
	})
}

// handleModels lists the models the account may call. The proxy keeps offering
// the OpenAI list shape because that is what the agent clients parse; the ids
// themselves come from JoyCode.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !requireGET(w, r) {
		return
	}
	client := s.getClient(r)
	if client == nil {
		writeError(w, http.StatusBadGateway, shapeOpenAI, "api_error", "no JoyCode credential available")
		return
	}
	models, err := client.ListModels()
	if err != nil {
		slog.Error("list models upstream error", "error", err)
		writeError(w, http.StatusBadGateway, shapeOpenAI, "api_error", err.Error())
		return
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ChatAPIModel)
		if id == "" {
			id = strings.TrimSpace(model.ModelID)
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 && s.store != nil {
		_ = s.store.SetSetting("available_models", strings.Join(ids, ","))
	}
	data := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]interface{}{
			"id": id, "object": "model", "created": 1700000000, "owned_by": "joycode",
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"object": "list", "data": data})
}
