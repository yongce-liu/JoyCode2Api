package imageinput

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

const (
	SettingKey              = "auto_compress_images"
	maxIncomingRequestBytes = 100 << 20
)

// SettingsGetter is the minimal settings interface required by the middleware.
type SettingsGetter interface {
	GetSetting(key string) string
}

// Middleware preprocesses image-bearing JSON requests before protocol-specific handlers.
func Middleware(settings SettingsGetter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shouldInspect(r) || !compressionEnabled(settings) {
			next.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxIncomingRequestBytes+1))
		if err != nil {
			writeRequestError(w, r.URL.Path, http.StatusBadRequest, "读取请求体失败: "+err.Error(), "invalid_request")
			return
		}
		_ = r.Body.Close()
		if len(body) > maxIncomingRequestBytes {
			writeRequestError(w, r.URL.Path, http.StatusRequestEntityTooLarge, "请求体超过代理允许的 100 MiB 上限。", "request_too_large")
			return
		}

		processed, stats, err := CompressJSONBody(body, SafeBodyTarget)
		if err != nil {
			slog.Warn("image request preprocessing failed", "path", r.URL.Path, "error", err)
			writeRequestError(w, r.URL.Path, http.StatusRequestEntityTooLarge,
				"请求中的图片自动压缩后仍无法满足 JoyCode 网关 5 MiB 限制。请压缩对话或减少图片数量。详情: "+err.Error(),
				"request_too_large")
			return
		}
		if stats.CompressedImages > 0 {
			slog.Info("compressed request images",
				"path", r.URL.Path,
				"images", stats.ImageCount,
				"compressed", stats.CompressedImages,
				"original_bytes", stats.OriginalBytes,
				"final_bytes", stats.FinalBytes,
			)
		}
		replaceRequestBody(r, processed)
		next.ServeHTTP(w, r)
	})
}

func shouldInspect(r *http.Request) bool {
	if r.Method != http.MethodPost || r.Body == nil || !strings.HasPrefix(r.URL.Path, "/v1/") {
		return false
	}
	encoding := strings.TrimSpace(r.Header.Get("Content-Encoding"))
	return encoding == "" || strings.EqualFold(encoding, "identity")
}

func compressionEnabled(settings SettingsGetter) bool {
	if settings == nil {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(settings.GetSetting(SettingKey)), "false")
}

func replaceRequestBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

func writeRequestError(w http.ResponseWriter, path string, status int, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	var payload interface{}
	if path == "/v1/messages" {
		payload = map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    "invalid_request_error",
				"message": message,
			},
		}
	} else {
		payload = map[string]interface{}{
			"error": map[string]interface{}{
				"type":    "invalid_request_error",
				"message": message,
				"code":    code,
				"param":   nil,
			},
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		_, _ = fmt.Fprint(w, `{"error":{"type":"api_error","message":"failed to encode request error"}}`)
		return
	}
	_, _ = w.Write(data)
}
