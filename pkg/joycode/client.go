package joycode

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/common"
)

const (
	DefaultModel  = "JoyAI-Code-1.5"
	ClientVersion = "2.7.5"
	UserAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) " +
		"JoyCode/2.7.5 Chrome/133.0.0.0 Electron/35.2.0 Safari/537.36"
)

// Upstream endpoints, one per JoyCode API the proxy exposes. The editor plugin
// calls these paths directly with the ptKey header; the proxy relays to the
// same paths instead of rewriting them, so the endpoint table is the upstream
// table. chat, web-search, modelList and userInfo are the v2 routes the plugin
// uses today; responses and messages only exist under their own version.
const (
	EndpointChatCompletions = "/api/saas/openai/v2/chat/completions"
	EndpointResponses       = "/api/saas/openai/v1/responses"
	EndpointMessages        = "/api/saas/anthropic/v1/messages"
	EndpointModelList       = "/api/saas/models/v2/modelList"
	EndpointUserInfo        = "/api/saas/user/v2/userInfo"
	EndpointWebSearch       = "/api/saas/openai/v2/web-search"
	EndpointRerank          = "/api/saas/openai/v2/rerank"
)

// BaseURL is the JoyCode origin requests are sent to by default. It is the
// saas host the editor plugin itself talks to.
var BaseURL = envOr("JOYCODE_BASE_URL", "http://joycode-api-saas.jd.com")

// GatewayURL switches upstream traffic to the public color gateway. The gateway
// routes by functionId and authenticates the caller with an HMAC signature
// instead of exposing /api/saas paths, so it is the route available outside the
// JD intranet. Empty (the default) talks to BaseURL directly.
var GatewayURL = envOr("JOYCODE_GATEWAY_URL", "")

// color gateway signing material (逆向自 JoyCode 2.7.5 / joycoder-editor 3.8.57).
const (
	colorGatewayAppID = "joycode_ide"
	colorGatewayPath  = "/api"
	colorHMACKey      = "0691a3f0b37b4a85aeb63ad0fc7db3ed"
)

// gatewayFunctionID maps an upstream endpoint to the color gateway functionId
// that serves it. Every endpoint the proxy exposes has a functionId except
// rerank, which the gateway does not publish: an unmapped endpoint is reported
// instead of being sent to a guess.
var gatewayFunctionID = map[string]string{
	EndpointChatCompletions: "chat_completions",
	EndpointResponses:       "responses_completions",
	EndpointMessages:        "anthropic_completions",
	EndpointModelList:       "joycode_modelList",
	EndpointUserInfo:        "joycode_userInfo",
	EndpointWebSearch:       "web_search",
}

var Models = []string{
	"JoyAI-Code",
	"Claude-Opus-4.7",
	"MiniMax-M2.7",
	"Kimi-K2.6",
	"Kimi-K2.5",
	"GLM-5.1",
	"GLM-5",
	"GLM-4.7",
	"Doubao-Seed-2.0-pro",
}

type Client struct {
	PtKey       string
	UserID      string
	SessionID   string
	Tenant      string
	LoginType   string
	OrgFullName string
	Native      *NativeContext
	Models      []string
	httpClient  *http.Client
}

// NativeContext optionally overrides account authentication for plugin-native
// adapters such as Anthropic Messages and GPT Responses, where JoyCode expects
// the editor plugin login rather than the account's own credentials.
type NativeContext struct {
	PtKey       string
	LoginType   string
	Tenant      string
	OrgFullName string
}

type gzipReadCloser struct {
	io.Reader
	body io.Closer
	gzip io.Closer
}

func (r *gzipReadCloser) Close() error {
	gzipErr := r.gzip.Close()
	bodyErr := r.body.Close()
	if gzipErr != nil {
		return gzipErr
	}
	return bodyErr
}

// defaultTransport is a shared transport with sane connection-pool defaults
// so that clients created without an explicit transport still reuse TCP
// connections instead of dialing anew for every request.
var defaultTransport = &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 10,
	IdleConnTimeout:     90 * time.Second,
}

// envOr 读取环境变量，为空则返回 fallback
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func NewClient(ptKey, userID string) *Client {
	return &Client{
		PtKey:     ptKey,
		UserID:    userID,
		SessionID: newHexID(),
		httpClient: &http.Client{
			Timeout:   30 * time.Minute,
			Transport: defaultTransport,
		},
	}
}

func (c *Client) SetTimeout(d time.Duration) {
	c.httpClient.Timeout = d
}

func (c *Client) SetTransport(transport http.RoundTripper) {
	c.httpClient.Transport = transport
}

// SetNativeContext pins the editor plugin login used when the caller's account
// has plugin credentials available.
func (c *Client) SetNativeContext(ctx NativeContext) {
	c.Native = &ctx
}

// SetModelCatalog records the live plugin catalog on the system client.
func (c *Client) SetModelCatalog(models []string) {
	c.Models = append([]string(nil), models...)
}

// SetContext records the tenant metadata carried by the login credentials.
func (c *Client) SetContext(tenant, loginType, orgFullName string) {
	c.Tenant = tenant
	c.LoginType = loginType
	c.OrgFullName = orgFullName
}

func newHexID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// endpointURL resolves an upstream endpoint to the URL it is sent to: the
// signed color-gateway URL when deployment configured one, otherwise the plain
// saas path.
func (c *Client) endpointURL(endpoint string) (string, error) {
	if GatewayURL == "" {
		return strings.TrimRight(BaseURL, "/") + endpoint, nil
	}
	functionID, ok := gatewayFunctionID[endpoint]
	if !ok {
		return "", fmt.Errorf("endpoint %s has no color gateway functionId", endpoint)
	}
	origin, err := url.Parse(GatewayURL)
	if err != nil || origin.Host == "" {
		return "", fmt.Errorf("invalid JOYCODE_GATEWAY_URL %q", GatewayURL)
	}
	query, sign := colorSign(functionID)
	return origin.Scheme + "://" + origin.Host + strings.TrimRight(origin.Path, "/") +
		colorGatewayPath + "?" + query + "&sign=" + sign, nil
}

// colorSign builds the signed query the color gateway authenticates. The signed
// string is the appid, functionId and millisecond timestamp joined with "&",
// while the query carries them in that same order; the signature is appended
// separately so the signed string never includes it.
func colorSign(functionID string) (query, sign string) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(colorHMACKey))
	mac.Write([]byte(colorGatewayAppID + "&" + functionID + "&" + ts))
	sign = hex.EncodeToString(mac.Sum(nil))
	query = "appid=" + colorGatewayAppID + "&functionId=" + functionID + "&t=" + ts
	return query, sign
}

// headers builds the headers JoyCode authenticates a request with. A pinned
// plugin login wins over the account credentials; the login type is only
// guessed when the credentials do not carry one, because the editor plugin key
// authenticates as ERP while browser accounts authenticate as PIN_JD_CLOUD.
func (c *Client) headers() http.Header {
	ptKey, loginType := c.PtKey, c.LoginType
	if n := c.Native; n != nil {
		if n.PtKey != "" {
			ptKey = n.PtKey
		}
		if n.LoginType != "" {
			loginType = n.LoginType
		}
	}
	if loginType == "" {
		if looksLikePluginPtKey(ptKey) {
			loginType = "ERP"
		} else {
			loginType = "PIN_JD_CLOUD"
		}
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Source-Type", "joycoder-ide")
	h.Set("ptKey", ptKey)
	h.Set("loginType", loginType)
	h.Set("User-Agent", UserAgent)
	h.Set("Accept", "*/*")
	// gzip is never requested: a gzip block is only readable once complete,
	// which would hold back every SSE event.
	h.Set("Accept-Encoding", "identity")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	return h
}

// Protocol identifies which JoyCode API a request belongs to. It selects the
// metadata defaults only: headers, endpoint resolution and the payload itself
// are the same for every protocol.
type Protocol int

const (
	ProtocolOpenAI Protocol = iota
	ProtocolAnthropic
)

// mergeMetadata adds the plugin metadata JoyCode requires to a client payload.
// The payload is decoded into raw fields so nested values (tools, thinking,
// images, ...) are re-serialized as the client sent them; a field the client
// already provided always wins over the defaults.
func (c *Client) mergeMetadata(payload []byte, protocol Protocol) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &fields); err != nil {
			return nil, fmt.Errorf("request body must be a JSON object: %w", err)
		}
	}
	tenant := c.Tenant
	orgFullName := c.OrgFullName
	if actx := c.Native; actx != nil {
		if actx.Tenant != "" {
			tenant = actx.Tenant
		}
		if actx.OrgFullName != "" {
			orgFullName = actx.OrgFullName
		}
	}
	if tenant == "" {
		if protocol == ProtocolAnthropic || looksLikePluginPtKey(c.PtKey) {
			tenant = "JD"
		} else {
			tenant = "JOYCODE"
		}
	}
	defaults := map[string]interface{}{
		"tenant":        tenant,
		"orgFullName":   orgFullName,
		"userId":        c.UserID,
		"client":        "JoyCode",
		"clientVersion": ClientVersion,
		"language":      "UNKNOWN",
	}
	for key, value := range defaults {
		if _, present := fields[key]; present {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[key] = raw
	}
	return json.Marshal(fields)
}

// Forward posts a client payload to a JoyCode endpoint and returns the raw
// upstream response. Nothing is rewritten on the way back: the caller relays
// status, headers and body verbatim, so an upstream error reaches the client
// exactly as the upstream sent it.
func (c *Client) Forward(endpoint string, protocol Protocol, payload []byte) (*http.Response, error) {
	return c.ForwardContext(context.Background(), endpoint, protocol, payload)
}

// ForwardContext is Forward bound to a caller context. A relayed request must
// carry the client's context: when the client gives up, the upstream call has
// to be cancelled too, otherwise an abandoned request keeps its upstream
// connection (and the connection slot it holds) for as long as the upstream
// takes to answer.
func (c *Client) ForwardContext(ctx context.Context, endpoint string, protocol Protocol, payload []byte) (*http.Response, error) {
	body, err := c.mergeMetadata(payload, protocol)
	if err != nil {
		return nil, err
	}
	target, err := c.endpointURL(endpoint)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = c.headers()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if err := decodeStreamBody(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// postJSON posts a small JSON payload and decodes the upstream JSON response.
// It backs the account helpers (model list, user info, search, rerank) that
// need a value rather than a byte-for-byte relay.
func (c *Client) postJSON(endpoint string, payload map[string]interface{}) (map[string]interface{}, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.Forward(endpoint, ProtocolOpenAI, raw)
	if err != nil {
		slog.Error("upstream request failed", "endpoint", endpoint, "error", err)
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		slog.Error("decode upstream response", "endpoint", endpoint, "status", resp.StatusCode, "error", err)
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		slog.Error("upstream non-200", "endpoint", endpoint, "status", resp.StatusCode, "body", common.Truncate(string(data), 500))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		slog.Error("unmarshal upstream response", "endpoint", endpoint, "error", err)
		return nil, fmt.Errorf("invalid JSON response (parse error: %s): %s", err.Error(), common.Truncate(string(data), 500))
	}
	return result, nil
}

// looksLikePluginPtKey recognizes the key the editor plugin stores, which
// authenticates as ERP. Plugin keys are around 50-80 characters, while
// browser/IDE pt_keys are substantially longer; the range stays well below the
// browser length so an account key is never mistaken for a plugin one.
func looksLikePluginPtKey(key string) bool {
	return len(key) >= 48 && len(key) <= 96
}

func decodeStreamBody(resp *http.Response) error {
	if resp.Header.Get("Content-Encoding") != "gzip" {
		return nil
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	resp.Body = &gzipReadCloser{Reader: gz, body: resp.Body, gzip: gz}
	resp.Header.Del("Content-Encoding")
	return nil
}

func (c *Client) ListModels() ([]ModelInfo, error) {
	resp, err := c.postJSON(EndpointModelList, map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	data, ok := resp["data"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected models response format: missing data array")
	}
	models := make([]ModelInfo, 0, len(data))
	for _, item := range data {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var m ModelInfo
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		models = append(models, m)
	}
	return models, nil
}

func (c *Client) WebSearch(query string) ([]interface{}, error) {
	body := map[string]interface{}{
		"messages": []map[string]string{{"role": "user", "content": query}},
		"stream":   false, "model": "search_pro_jina", "language": "UNKNOWN",
	}
	resp, err := c.postJSON(EndpointWebSearch, body)
	if err != nil {
		return nil, err
	}
	results, _ := resp["search_result"].([]interface{})
	return results, nil
}

func (c *Client) Rerank(query string, documents []string, topN int) (map[string]interface{}, error) {
	return c.postJSON(EndpointRerank, map[string]interface{}{
		"model": "Qwen3-Reranker-8B", "query": query,
		"documents": documents, "top_n": topN,
	})
}

func (c *Client) UserInfo() (map[string]interface{}, error) {
	return c.postJSON(EndpointUserInfo, map[string]interface{}{})
}

func (c *Client) Validate() error {
	resp, err := c.UserInfo()
	if err != nil {
		return fmt.Errorf("credential validation failed: %w", err)
	}
	code, ok := resp["code"].(float64)
	if !ok || code != 0 {
		msg, _ := resp["msg"].(string)
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("credential validation failed (code=%.0f): %s", code, msg)
	}
	return nil
}

// UserInfoWithRefresh calls the UserInfo API and returns the refreshed ptKey
// from the response data, if present. Returns (refreshedPtKey, nil) on success.
func (c *Client) UserInfoWithRefresh() (string, error) {
	resp, err := c.UserInfo()
	if err != nil {
		return "", fmt.Errorf("user info request failed: %w", err)
	}
	code, ok := resp["code"].(float64)
	if !ok || code != 0 {
		msg, _ := resp["msg"].(string)
		if msg == "" {
			msg = "unknown error"
		}
		return "", fmt.Errorf("user info failed (code=%.0f): %s", code, msg)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return "", nil
	}
	if ptKey, ok := data["ptKey"].(string); ok && ptKey != "" {
		return ptKey, nil
	}
	return "", nil
}
