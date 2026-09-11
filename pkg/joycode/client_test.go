package joycode

import (
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testServerClient builds a Client whose requests land on an httptest server
// while keeping the original path and query string.
func testServerClient(handler http.Handler) (*Client, func()) {
	srv := httptest.NewServer(handler)
	c := NewClient("test-key", "test-user")
	c.httpClient = srv.Client()
	c.httpClient.Transport = redirectTransport{target: srv.URL, base: http.DefaultTransport}
	return c, srv.Close
}

type redirectTransport struct {
	target string
	base   http.RoundTripper
}

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	newURL := rt.target + req.URL.Path
	if req.URL.RawQuery != "" {
		newURL += "?" + req.URL.RawQuery
	}
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, newURL, req.Body)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header
	return rt.base.RoundTrip(newReq)
}

func TestNewClient_SessionIDUnique(t *testing.T) {
	a := NewClient("k", "u")
	b := NewClient("k", "u")
	if a.SessionID == b.SessionID {
		t.Errorf("two clients should have different session IDs, got same %q", a.SessionID)
	}
}

func TestNewClient_EmptyCredentials(t *testing.T) {
	c := NewClient("", "")
	if c.PtKey != "" || c.UserID != "" {
		t.Errorf("expected empty credentials, got PtKey=%q UserID=%q", c.PtKey, c.UserID)
	}
	if c.SessionID == "" {
		t.Error("SessionID should still be generated with empty credentials")
	}
	if c.httpClient == nil {
		t.Error("httpClient should be initialised")
	}
}

func TestHeaders_ContainsRequiredFields(t *testing.T) {
	c := NewClient("my-key", "u1")
	h := c.headers()

	for _, key := range []string{"Content-Type", "User-Agent", "Accept", "Accept-Encoding", "Accept-Language"} {
		if v := h.Get(key); v == "" {
			t.Errorf("headers missing required field %q", key)
		}
	}
	for _, key := range []string{"ptKey", "loginType", "source-type"} {
		if h.Get(key) == "" {
			t.Errorf("headers missing required field %q", key)
		}
	}
}

func TestHeaders_PtKeySet(t *testing.T) {
	c := NewClient("abc123token", "u1")
	if got := c.headers().Get("ptKey"); got != "abc123token" {
		t.Errorf("ptKey header = %q, want %q", got, "abc123token")
	}
}

func TestHeaders_BrowserKeyDefaultsToPinJdCloud(t *testing.T) {
	c := NewClient("acct-key", "u1")
	h := c.headers()
	if got := h.Get("ptKey"); got != "acct-key" {
		t.Errorf("ptKey = %q, want the account key", got)
	}
	if got := h.Get("loginType"); got != "PIN_JD_CLOUD" {
		t.Errorf("loginType = %q, want PIN_JD_CLOUD fallback", got)
	}
}

func TestHeaders_PluginKeyDefaultsToERP(t *testing.T) {
	// Both the 52-character legacy key and the 74-character key the editor
	// plugin issues today authenticate as ERP.
	for _, length := range []int{52, 74} {
		c := NewClient(strings.Repeat("k", length), "u1")
		if got := c.headers().Get("loginType"); got != "ERP" {
			t.Errorf("ptKey length %d: loginType = %q, want ERP", length, got)
		}
	}
}

func TestHeaders_PinnedLoginWins(t *testing.T) {
	c := NewClient("acct-key", "u1")
	c.SetNativeContext(NativeContext{PtKey: "plugin-key", LoginType: "ERP"})
	h := c.headers()
	if got := h.Get("ptKey"); got != "plugin-key" {
		t.Errorf("ptKey = %q, want the pinned plugin key", got)
	}
	if got := h.Get("loginType"); got != "ERP" {
		t.Errorf("loginType = %q, want ERP", got)
	}
}

func TestEndpointURL_ResolvesAgainstUpstreamOrigin(t *testing.T) {
	prev := BaseURL
	BaseURL = "http://joycode-api-saas.jd.com/"
	defer func() { BaseURL = prev }()

	c := NewClient("k", "u")
	for endpoint, want := range map[string]string{
		EndpointChatCompletions: "http://joycode-api-saas.jd.com/api/saas/openai/v2/chat/completions",
		EndpointMessages:        "http://joycode-api-saas.jd.com/api/saas/anthropic/v1/messages",
		EndpointModelList:       "http://joycode-api-saas.jd.com/api/saas/models/v2/modelList",
	} {
		got, err := c.endpointURL(endpoint)
		if err != nil {
			t.Fatalf("endpointURL(%q): %v", endpoint, err)
		}
		if got != want {
			t.Errorf("endpointURL(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

// A deployment outside the JD intranet sets JOYCODE_GATEWAY_URL and every call
// is signed and routed by functionId instead of using the saas paths.
func TestEndpointURL_GatewaySignsAndRoutesByFunctionID(t *testing.T) {
	prevURL, prevGateway := BaseURL, GatewayURL
	BaseURL = "http://joycode-api-saas.jd.com"
	GatewayURL = "https://api-ai.jd.com/"
	defer func() { BaseURL, GatewayURL = prevURL, prevGateway }()

	c := NewClient("k", "u")
	for endpoint, wantFunction := range map[string]string{
		EndpointChatCompletions: "chat_completions",
		EndpointResponses:       "responses_completions",
		EndpointMessages:        "anthropic_completions",
		EndpointModelList:       "joycode_modelList",
		EndpointUserInfo:        "joycode_userInfo",
		EndpointWebSearch:       "web_search",
	} {
		got, err := c.endpointURL(endpoint)
		if err != nil {
			t.Fatalf("endpointURL(%q): %v", endpoint, err)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse %q: %v", got, err)
		}
		if u.Host != "api-ai.jd.com" || u.Path != "/api" {
			t.Errorf("%s: host/path = %q/%q, want api-ai.jd.com//api", endpoint, u.Host, u.Path)
		}
		q := u.Query()
		if q.Get("appid") != colorGatewayAppID {
			t.Errorf("%s: appid = %q, want %q", endpoint, q.Get("appid"), colorGatewayAppID)
		}
		if q.Get("functionId") != wantFunction {
			t.Errorf("%s: functionId = %q, want %q", endpoint, q.Get("functionId"), wantFunction)
		}
		if q.Get("t") == "" {
			t.Errorf("%s: missing timestamp", endpoint)
		}
		signStr := colorGatewayAppID + "&" + wantFunction + "&" + q.Get("t")
		mac := hmac.New(sha256.New, []byte(colorHMACKey))
		mac.Write([]byte(signStr))
		if want := hex.EncodeToString(mac.Sum(nil)); q.Get("sign") != want {
			t.Errorf("%s: sign = %q, want %q", endpoint, q.Get("sign"), want)
		}
	}
}

// rerank has no gateway functionId: the gateway must say so instead of being
// handed a request it cannot route.
func TestEndpointURL_GatewayRejectsUnmappedEndpoint(t *testing.T) {
	prevGateway := GatewayURL
	GatewayURL = "https://api-ai.jd.com"
	defer func() { GatewayURL = prevGateway }()

	if _, err := NewClient("k", "u").endpointURL(EndpointRerank); err == nil {
		t.Error("rerank has no color gateway functionId, want an error")
	}
}

// Pinned plugin credentials change the authentication a request carries, but
// never where it is sent: there is one JoyCode origin.
func TestNativeContext_DrivesHeadersOnly(t *testing.T) {
	c := NewClient("acct-key", "jd_abc")
	c.SetNativeContext(NativeContext{
		PtKey:       "plugin-key",
		LoginType:   "ERP",
		Tenant:      "JD",
		OrgFullName: "集团-实验室",
	})

	body, err := c.mergeMetadata([]byte(`{}`), ProtocolAnthropic)
	if err != nil {
		t.Fatalf("mergeMetadata: %v", err)
	}
	if !strings.Contains(string(body), `"tenant":"JD"`) || !strings.Contains(string(body), `"orgFullName":"集团-实验室"`) {
		t.Errorf("pinned tenant metadata missing: %s", body)
	}
	if got, err := c.endpointURL(EndpointMessages); err != nil || got != BaseURL+EndpointMessages {
		t.Errorf("url = %q (err %v), want the default upstream origin", got, err)
	}
}

func TestMergeMetadata_DefaultsAndClientPrecedence(t *testing.T) {
	c := NewClient("pt", "user-42")
	c.Tenant = "JD"
	c.OrgFullName = "org"

	merged, err := c.mergeMetadata([]byte(`{"model":"m","tenant":"CUSTOM"}`), ProtocolOpenAI)
	if err != nil {
		t.Fatalf("mergeMetadata: %v", err)
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(merged, &fields); err != nil {
		t.Fatalf("merged body not JSON: %v", err)
	}
	if fields["userId"] != "user-42" || fields["client"] != "JoyCode" || fields["clientVersion"] != ClientVersion {
		t.Errorf("metadata missing: %v", fields)
	}
	if fields["tenant"] != "CUSTOM" {
		t.Errorf("a client-supplied field must win, got tenant=%v", fields["tenant"])
	}
	if fields["model"] != "m" {
		t.Errorf("payload fields must survive, got model=%v", fields["model"])
	}
}

func TestMergeMetadata_TenantDefaultsPerProtocol(t *testing.T) {
	openai := NewClient("pt", "u")
	body, err := openai.mergeMetadata([]byte(`{}`), ProtocolOpenAI)
	if err != nil {
		t.Fatalf("mergeMetadata: %v", err)
	}
	if !strings.Contains(string(body), `"tenant":"JOYCODE"`) {
		t.Errorf("openai tenant default missing: %s", body)
	}
	anthropic := NewClient("pt", "u")
	body, err = anthropic.mergeMetadata([]byte(`{}`), ProtocolAnthropic)
	if err != nil {
		t.Fatalf("mergeMetadata: %v", err)
	}
	if !strings.Contains(string(body), `"tenant":"JD"`) {
		t.Errorf("anthropic tenant default missing: %s", body)
	}
}

func TestMergeMetadata_RejectsNonObjectBody(t *testing.T) {
	c := NewClient("pt", "u")
	if _, err := c.mergeMetadata([]byte(`[1,2]`), ProtocolOpenAI); err == nil {
		t.Error("array body should be rejected")
	}
}

func TestForward_PostsPayloadWithProtocolHeaders(t *testing.T) {
	var gotPath, gotPtKey, gotEncoding string
	client, closeFn := testServerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotPtKey = r.Header.Get("ptKey")
		gotEncoding = r.Header.Get("Accept-Encoding")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer closeFn()

	resp, err := client.Forward("/api/saas/anthropic/v1/messages", ProtocolAnthropic, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()
	if gotPath != "/api/saas/anthropic/v1/messages" {
		t.Errorf("path = %q", gotPath)
	}
	if gotPtKey != "test-key" {
		t.Errorf("ptKey = %q, want test-key", gotPtKey)
	}
	if gotEncoding != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity", gotEncoding)
	}
}

func TestForward_ReturnsNon200ResponseToCaller(t *testing.T) {
	client, closeFn := testServerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"6002","msg":"model not registered"}`)
	}))
	defer closeFn()

	resp, err := client.Forward("/api/saas/openai/v1/chat/completions", ProtocolOpenAI, []byte(`{}`))
	if err != nil {
		t.Fatalf("Forward must not turn an upstream status into a transport error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), "6002") {
		t.Errorf("upstream body lost: %s", data)
	}
}

// A relayed request must not outlive its client: when the caller gives up, the
// upstream call is cancelled and its connection is released instead of being
// held until the upstream answers.
func TestForwardContext_CancelsUpstreamWhenCallerAborts(t *testing.T) {
	handlerSawCancel := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consume the body first: an upstream that never reads it would only
		// notice the closed connection when it next touches the stream.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			close(handlerSawCancel)
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()

	prev := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = prev }()

	client := NewClient("test-key", "test-user")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		resp, err := client.ForwardContext(ctx, "/api/saas/openai/v1/chat/completions", ProtocolOpenAI, []byte(`{"model":"m"}`))
		if resp != nil {
			resp.Body.Close()
		}
		errCh <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("ForwardContext returned nil after the caller context was cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ForwardContext did not return after the caller context was cancelled")
	}
	select {
	case <-handlerSawCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request was not cancelled with the caller context")
	}
}

func TestForward_UnwrapsGzip(t *testing.T) {
	client, closeFn := testServerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(`{"ok":true}`))
		_ = gz.Close()
	}))
	defer closeFn()

	resp, err := client.Forward("/api/saas/openai/v1/chat/completions", ProtocolOpenAI, []byte(`{}`))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if string(data) != `{"ok":true}` {
		t.Errorf("body = %s, want decompressed JSON", data)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding should be cleared, got %q", resp.Header.Get("Content-Encoding"))
	}
}

func TestListModels(t *testing.T) {
	client, closeFn := testServerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":[{"chatApiModel":"GLM-5.1"}]}`)
	}))
	defer closeFn()

	models, err := client.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ChatAPIModel != "GLM-5.1" {
		t.Errorf("models = %+v", models)
	}
}

func TestListModels_MissingDataArray(t *testing.T) {
	client, closeFn := testServerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer closeFn()

	if _, err := client.ListModels(); err == nil {
		t.Error("missing data array should be an error")
	}
}

func TestValidate(t *testing.T) {
	client, closeFn := testServerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"ptKey":"fresh"}}`)
	}))
	defer closeFn()

	if err := client.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	key, err := client.UserInfoWithRefresh()
	if err != nil {
		t.Fatalf("UserInfoWithRefresh: %v", err)
	}
	if key != "fresh" {
		t.Errorf("refreshed key = %q, want fresh", key)
	}
}

func TestValidate_InvalidToken(t *testing.T) {
	client, closeFn := testServerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":1050,"msg":"quota exhausted"}`)
	}))
	defer closeFn()

	err := client.Validate()
	if err == nil || !strings.Contains(err.Error(), "quota exhausted") {
		t.Errorf("Validate error = %v, want the upstream reason", err)
	}
}

func TestWebSearchAndRerank(t *testing.T) {
	client, closeFn := testServerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/saas/openai/v2/web-search":
			_, _ = io.WriteString(w, `{"code":0,"search_result":[{"title":"t"}]}`)
		default:
			_, _ = io.WriteString(w, `{"code":0,"results":[{"score":1}]}`)
		}
	}))
	defer closeFn()

	results, err := client.WebSearch("query")
	if err != nil {
		t.Fatalf("WebSearch: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("search results = %v", results)
	}
	reranked, err := client.Rerank("query", []string{"doc"}, 1)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if _, ok := reranked["results"]; !ok {
		t.Errorf("rerank response = %v", reranked)
	}
}
