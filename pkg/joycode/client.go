package joycode

import (
	"bytes"
	"compress/gzip"
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

	// color gateway 签名（逆向自 JoyCode 2.7.5 / joycoder-editor 3.8.57）
	colorGatewayAppID = "joycode_ide"
	colorGatewayPath  = "/api"
	colorHMACKey      = "0691a3f0b37b4a85aeb63ad0fc7db3ed"
)

var (
	BaseURL             = envOr("JOYCODE_BASE_URL", "https://joycode-api.jd.com")
	SaasBaseURL         = envOr("JOYCODE_SAAS_BASE_URL", "http://joycode-api-saas.jd.com")
	DefaultColorBaseURL = envOr("JOYCODE_COLOR_BASE_URL", "https://api-ai.jd.com")
)

// colorEndpoint 把旧 v1 路径映射到 (functionId, v2 路径)。
// gateway 模式靠 query 的 functionId 路由；direct 模式用 v2 路径。
type colorEndpoint struct {
	functionID string
	v2Path     string
}

var colorEndpoints = map[string]colorEndpoint{
	"/api/saas/openai/v1/chat/completions": {"chat_completions", "/api/saas/openai/v2/chat/completions"},
	"/api/saas/openai/v1/responses":        {"responses_completions", "/api/saas/openai/v1/responses"},
	"/api/saas/models/v1/modelList":        {"joycode_modelList", "/api/saas/models/v2/modelList"},
	"/api/saas/openai/v1/web-search":       {"web_search", "/api/saas/openai/v2/web-search"},
	"/api/saas/user/v1/userInfo":           {"joycode_userInfo", "/api/saas/user/v2/userInfo"},
	"/api/saas/anthropic/v1/messages":      {"anthropic_completions", "/api/saas/anthropic/v1/messages"},
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
	PtKey          string
	AnthropicPtKey string
	UserID         string
	SessionID      string
	ColorBaseURL   string
	MasterBaseURL  string
	Tenant         string
	LoginType      string
	OrgFullName    string
	Native         *NativeContext
	Models         []string
	ModelAdapters  map[string]string
	httpClient     *http.Client
}

// NativeContext optionally overrides account authentication and tenant routing
// for plugin-native adapters such as Anthropic Messages and GPT Responses.
type NativeContext struct {
	PtKey         string
	LoginType     string
	ColorBaseURL  string
	MasterBaseURL string
	Tenant        string
	OrgFullName   string
}

// AnthropicContext is kept as an alias for callers compiled against the
// earlier Claude-only implementation.
type AnthropicContext = NativeContext

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
		PtKey:        ptKey,
		UserID:       userID,
		SessionID:    newHexID(),
		ColorBaseURL: DefaultColorBaseURL,
		httpClient: &http.Client{
			Timeout:   30 * time.Minute,
			Transport: defaultTransport,
		},
	}
}

// SetHTTPClient replaces the internal HTTP client. Intended for testing.
func (c *Client) SetHTTPClient(hc *http.Client) {
	c.httpClient = hc
}

func (c *Client) SetTimeout(d time.Duration) {
	c.httpClient.Timeout = d
}

func (c *Client) SetTransport(transport http.RoundTripper) {
	c.httpClient.Transport = transport
}

func (c *Client) SetAnthropicPtKey(ptKey string) {
	c.AnthropicPtKey = ptKey
}

// SetAnthropicContext pins the full client-login context used for native
// Anthropic (Claude) requests. It supersedes SetAnthropicPtKey: headers,
// body metadata and gateway routing all follow this context.
func (c *Client) SetAnthropicContext(ctx AnthropicContext) {
	c.SetNativeContext(ctx)
}

// SetNativeContext pins the editor plugin login used by native model adapters.
func (c *Client) SetNativeContext(ctx NativeContext) {
	c.Native = &ctx
}

// SetModelCatalog records the live plugin catalog on the system client.
func (c *Client) SetModelCatalog(models []string, adapters map[string]string) {
	c.Models = append([]string(nil), models...)
	c.ModelAdapters = make(map[string]string, len(adapters))
	for model, adapter := range adapters {
		c.ModelAdapters[model] = adapter
	}
}

// SetColorContext sets the color-gateway routing context from login credentials.
// Empty colorBaseURL keeps the default gateway origin.
func (c *Client) SetColorContext(colorBaseURL, masterBaseURL, tenant, loginType, orgFullName string) {
	if colorBaseURL != "" {
		c.ColorBaseURL = colorBaseURL
	}
	c.MasterBaseURL = masterBaseURL
	c.Tenant = tenant
	c.LoginType = loginType
	c.OrgFullName = orgFullName
}

func newHexID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// colorSign 构造 color gateway 的 query 串与 HMAC 签名。
// 规范串 = 参数按 key 排序后的 value 拼接（appid < functionId < t）。
func colorSign(functionID string) (query, sign string) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	signStr := colorGatewayAppID + "&" + functionID + "&" + ts
	mac := hmac.New(sha256.New, []byte(colorHMACKey))
	mac.Write([]byte(signStr))
	sign = hex.EncodeToString(mac.Sum(nil))
	query = "appid=" + colorGatewayAppID + "&functionId=" + functionID + "&t=" + ts
	return query, sign
}

// requestURL 根据登录态把端点解析为最终请求 URL。
func (c *Client) requestURL(endpoint string) string {
	return buildRequestURL(endpoint, c.ColorBaseURL, c.MasterBaseURL)
}

// nativeRequestURL resolves an endpoint against the pinned plugin context when
// present, otherwise it falls back to the client's own routing context.
func (c *Client) nativeRequestURL(endpoint string) string {
	if actx := c.Native; actx != nil && (actx.ColorBaseURL != "" || actx.MasterBaseURL != "") {
		color := actx.ColorBaseURL
		if color == "" {
			color = c.ColorBaseURL
		}
		master := actx.MasterBaseURL
		if master == "" {
			master = c.MasterBaseURL
		}
		return buildRequestURL(endpoint, color, master)
	}
	return c.requestURL(endpoint)
}

// buildRequestURL 根据登录态把端点解析为最终请求 URL。
// 有 colorBaseURL → gateway 模式（带签名，functionId 路由）；否则 direct v2（无签名）。
func buildRequestURL(endpoint, colorBaseURL, masterBaseURL string) string {
	ep, ok := colorEndpoints[endpoint]
	if !ok {
		// 未在 color 端点表中的旧端点（如已下线的 rerank），保持 direct 行为
		return BaseURL + endpoint
	}
	if colorBaseURL != "" {
		if u, err := url.Parse(colorBaseURL); err == nil && u.Host != "" {
			basePath := strings.TrimRight(u.Path, "/")
			query, sign := colorSign(ep.functionID)
			return u.Scheme + "://" + u.Host + basePath + colorGatewayPath + "?" + query + "&sign=" + sign
		}
	}
	base := masterBaseURL
	if base == "" {
		base = BaseURL
	}
	return strings.TrimRight(base, "/") + ep.v2Path
}

func (c *Client) headers() http.Header {
	loginType := c.LoginType
	if loginType == "" {
		loginType = "N_PIN_PC"
	}
	return http.Header{
		"Content-Type":    {"application/json; charset=UTF-8"},
		"source-type":     {"joycoder-ide"},
		"ptKey":           {c.PtKey},
		"loginType":       {loginType},
		"User-Agent":      {UserAgent},
		"Accept":          {"*/*"},
		"Accept-Encoding": {"gzip, deflate"},
		"Accept-Language": {"zh-CN,zh;q=0.9,en;q=0.8"},
	}
}

func (c *Client) anthropicHeaders() http.Header {
	ptKey := c.PtKey
	if c.AnthropicPtKey != "" {
		ptKey = c.AnthropicPtKey
	}
	loginType := c.LoginType
	if actx := c.Native; actx != nil {
		if actx.PtKey != "" {
			ptKey = actx.PtKey
		}
		if actx.LoginType != "" {
			loginType = actx.LoginType
		}
	}
	if loginType == "" {
		if looksLikePluginPtKey(ptKey) {
			loginType = "ERP"
		} else {
			loginType = "PIN_JD_CLOUD"
		}
	}
	return http.Header{
		"Content-Type":    {"application/json; charset=utf-8"},
		"source-type":     {"joycoder-ide"},
		"ptKey":           {ptKey},
		"loginType":       {loginType},
		"User-Agent":      {UserAgent},
		"Accept":          {"*/*"},
		"Accept-Encoding": {"gzip, deflate"},
		"Accept-Language": {"zh-CN,zh;q=0.9,en;q=0.8"},
	}
}

func (c *Client) prepareBody(extra map[string]interface{}) map[string]interface{} {
	tenant := c.Tenant
	if tenant == "" {
		tenant = "JOYCODE"
	}
	body := map[string]interface{}{
		"tenant":        tenant,
		"orgFullName":   c.OrgFullName,
		"userId":        c.UserID,
		"client":        "JoyCode",
		"clientVersion": ClientVersion,
		"language":      "UNKNOWN",
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (c *Client) prepareAnthropicBody(extra map[string]interface{}) map[string]interface{} {
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
		tenant = "JD"
	}
	body := map[string]interface{}{
		"tenant":        tenant,
		"orgFullName":   orgFullName,
		"userId":        c.UserID,
		"client":        "JoyCode",
		"clientVersion": ClientVersion,
		"language":      "UNKNOWN",
		"stream":        true,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (c *Client) doPost(endpoint string, body map[string]interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("marshal request body", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req, err := http.NewRequest("POST", c.requestURL(endpoint), bytes.NewReader(data))
	if err != nil {
		slog.Error("create request", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req.Header = c.headers()
	return c.httpClient.Do(req)
}

// doPostStream is like doPost but disables Accept-Encoding: gzip so the
// upstream returns raw (uncompressed) SSE. gzip.Reader buffers an entire
// gzip block before yielding any bytes, which breaks chunk-by-chunk
// streaming — the client sees all data arrive at once after a long delay.
func (c *Client) doPostStream(endpoint string, body map[string]interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("marshal stream request body", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req, err := http.NewRequest("POST", c.requestURL(endpoint), bytes.NewReader(data))
	if err != nil {
		slog.Error("create stream request", "endpoint", endpoint, "error", err)
		return nil, err
	}
	h := c.headers()
	h.Set("Accept-Encoding", "identity") // no gzip for streaming
	req.Header = h
	return c.httpClient.Do(req)
}

func (c *Client) doAnthropicPost(endpoint string, body map[string]interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("marshal anthropic request body", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req, err := http.NewRequest("POST", c.nativeRequestURL(endpoint), bytes.NewReader(data))
	if err != nil {
		slog.Error("create anthropic request", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req.Header = c.anthropicHeaders()
	return c.httpClient.Do(req)
}

// doAnthropicPostStream is like doAnthropicPost but disables gzip for the
// same reason as doPostStream — see its comment for details.
func (c *Client) doAnthropicPostStream(endpoint string, body map[string]interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("marshal anthropic stream request body", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req, err := http.NewRequest("POST", c.nativeRequestURL(endpoint), bytes.NewReader(data))
	if err != nil {
		slog.Error("create anthropic stream request", "endpoint", endpoint, "error", err)
		return nil, err
	}
	h := c.anthropicHeaders()
	h.Set("Accept-Encoding", "identity")
	req.Header = h
	return c.httpClient.Do(req)
}

// prepareNativeBody adds the plugin metadata required by native model adapters
// while preserving the caller's stream choice.
func (c *Client) prepareNativeBody(extra map[string]interface{}) map[string]interface{} {
	tenant := c.Tenant
	orgFullName := c.OrgFullName
	if ctx := c.Native; ctx != nil {
		if ctx.Tenant != "" {
			tenant = ctx.Tenant
		}
		if ctx.OrgFullName != "" {
			orgFullName = ctx.OrgFullName
		}
	}
	if tenant == "" && looksLikePluginPtKey(c.PtKey) {
		tenant = "JD"
	}
	body := map[string]interface{}{
		"tenant":        tenant,
		"orgFullName":   orgFullName,
		"userId":        c.UserID,
		"client":        "JoyCode",
		"clientVersion": ClientVersion,
		"language":      "UNKNOWN",
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (c *Client) nativeHeaders() http.Header {
	h := c.headers()
	if ctx := c.Native; ctx != nil {
		if ctx.PtKey != "" {
			h["ptKey"] = []string{ctx.PtKey}
		}
		if ctx.LoginType != "" {
			h["loginType"] = []string{ctx.LoginType}
		}
	}
	if c.Native == nil && looksLikePluginPtKey(c.PtKey) {
		h["loginType"] = []string{"ERP"}
	}
	return h
}

// looksLikePluginPtKey recognizes the short-lived key stored by the editor
// plugin. Current plugin keys are 52 characters, while browser/IDE pt_keys are
// substantially longer. A narrow range avoids changing legacy test/manual keys.
func looksLikePluginPtKey(key string) bool {
	return len(key) >= 48 && len(key) <= 64
}

func (c *Client) doNativePost(endpoint string, body map[string]interface{}, stream bool) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", c.nativeRequestURL(endpoint), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	h := c.nativeHeaders()
	if stream {
		h.Set("Accept-Encoding", "identity")
	}
	req.Header = h
	return c.httpClient.Do(req)
}

func decodeBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	var r io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	return io.ReadAll(r)
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

func (c *Client) Post(endpoint string, body map[string]interface{}) (map[string]interface{}, error) {
	resp, err := c.doPost(endpoint, c.prepareBody(body))
	if err != nil {
		slog.Error("upstream request failed", "endpoint", endpoint, "error", err)
		return nil, err
	}
	data, err := decodeBody(resp)
	if err != nil {
		slog.Error("decode upstream response", "endpoint", endpoint, "status", resp.StatusCode, "error", err)
		return nil, err
	}
	if resp.StatusCode != 200 {
		slog.Error("upstream non-200", "endpoint", endpoint, "status", resp.StatusCode, "body", truncate(string(data), 500))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		slog.Error("unmarshal upstream response", "endpoint", endpoint, "error", err)
		return nil, fmt.Errorf("invalid JSON response (parse error: %s): %s", err.Error(), truncate(string(data), 500))
	}
	return result, nil
}

func (c *Client) PostStream(endpoint string, body map[string]interface{}) (*http.Response, error) {
	resp, err := c.doPostStream(endpoint, c.prepareBody(body))
	if err != nil {
		slog.Error("upstream stream connect", "endpoint", endpoint, "error", err)
		return nil, err
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		slog.Error("upstream stream non-200", "endpoint", endpoint, "status", resp.StatusCode, "body", truncate(string(data), 500))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	if err := decodeStreamBody(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func (c *Client) PostAnthropicStream(endpoint string, body map[string]interface{}) (*http.Response, error) {
	resp, err := c.doAnthropicPostStream(endpoint, c.prepareAnthropicBody(body))
	if err != nil {
		slog.Error("upstream anthropic stream connect", "endpoint", endpoint, "error", err)
		return nil, err
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		slog.Error("upstream anthropic stream non-200", "endpoint", endpoint, "status", resp.StatusCode, "body", truncate(string(data), 500))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	if err := decodeStreamBody(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// PostNative calls a native JSON endpoint using the account credentials, with
// an optional plugin context override when one is available.
func (c *Client) PostNative(endpoint string, body map[string]interface{}) (map[string]interface{}, error) {
	resp, err := c.doNativePost(endpoint, c.prepareNativeBody(body), false)
	if err != nil {
		return nil, err
	}
	data, err := decodeBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("invalid JSON response (parse error: %s): %s", err.Error(), truncate(string(data), 500))
	}
	return result, nil
}

// PostNativeStream calls a native SSE endpoint without gzip buffering, using
// the account credentials unless a plugin context override is available.
func (c *Client) PostNativeStream(endpoint string, body map[string]interface{}) (*http.Response, error) {
	resp, err := c.doNativePost(endpoint, c.prepareNativeBody(body), true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	if err := decodeStreamBody(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func (c *Client) ListModels() ([]ModelInfo, error) {
	resp, err := c.Post("/api/saas/models/v1/modelList", map[string]interface{}{})
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
	resp, err := c.Post("/api/saas/openai/v1/web-search", body)
	if err != nil {
		return nil, err
	}
	results, _ := resp["search_result"].([]interface{})
	return results, nil
}

func (c *Client) Rerank(query string, documents []string, topN int) (map[string]interface{}, error) {
	return c.Post("/api/saas/openai/v1/rerank", map[string]interface{}{
		"model": "Qwen3-Reranker-8B", "query": query,
		"documents": documents, "top_n": topN,
	})
}

func (c *Client) UserInfo() (map[string]interface{}, error) {
	return c.Post("/api/saas/user/v1/userInfo", map[string]interface{}{})
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

func truncate(s string, maxLen int) string {
	return common.Truncate(s, maxLen)
}
