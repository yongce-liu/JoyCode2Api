package imageinput

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testSettings map[string]string

func (s testSettings) GetSetting(key string) string {
	return s[key]
}

func TestMiddlewareDisabledPassesOriginalBody(t *testing.T) {
	body := []byte(`{"input":"unchanged"}`)
	var received []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	handler := Middleware(testSettings{SettingKey: "false"}, next)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !bytes.Equal(received, body) {
		t.Fatalf("body changed while compression was disabled: %s", received)
	}
}

func TestMiddlewareEnabledCompressesBeforeProtocolHandler(t *testing.T) {
	imageURL := testPNGDataURL(t, 640, 480, 3)
	body, err := json.Marshal(map[string]interface{}{
		"model": "test",
		"messages": []interface{}{map[string]interface{}{
			"role": "user",
			"content": []interface{}{map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]interface{}{"url": imageURL},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var received []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		if r.ContentLength != int64(len(received)) {
			t.Errorf("content length = %d, body = %d", r.ContentLength, len(received))
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := Middleware(testSettings{}, next)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(received) >= len(body) || !strings.Contains(string(received), "data:image/jpeg;base64,") {
		t.Fatalf("image was not compressed before handler: original=%d received=%d", len(body), len(received))
	}
}

func TestMiddlewareUsesAnthropicErrorShape(t *testing.T) {
	imageURL := testPNGDataURL(t, 640, 480, 4)
	_, encoded := splitImageDataURL(imageURL)
	body, err := json.Marshal(map[string]interface{}{
		"messages": []interface{}{map[string]interface{}{
			"role": "user",
			"content": []interface{}{map[string]interface{}{
				"type": "image",
				"source": map[string]interface{}{
					"type": "base64", "media_type": "image/png", "data": encoded,
				},
			}},
		}},
		"padding": strings.Repeat("x", SafeBodyTarget),
	})
	if err != nil {
		t.Fatal(err)
	}

	nextCalled := false
	handler := Middleware(testSettings{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalled = true
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if nextCalled {
		t.Fatal("oversized request reached protocol handler")
	}
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), `"type":"error"`) || !strings.Contains(w.Body.String(), `"invalid_request_error"`) {
		t.Fatalf("unexpected response: status=%d body=%s", w.Code, w.Body.String())
	}
}
