package api

import (
	"context"
	"log/slog"
	"net/http"
)

type contextKey string

const requestIDKey contextKey = "request_id"

// WithRequestID injects a request ID into the request context so every log
// line of one request can be correlated.
func WithRequestID(r *http.Request, id uint64) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
}

func reqID(r *http.Request) uint64 {
	if v, ok := r.Context().Value(requestIDKey).(uint64); ok {
		return v
	}
	return 0
}

// reqLog returns a slog.Logger pre-loaded with the request ID.
func reqLog(r *http.Request) *slog.Logger {
	return slog.With("request_id", reqID(r))
}
