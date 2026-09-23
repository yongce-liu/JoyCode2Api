package store

import (
	"context"
	"net/http"
)

type ctxKey struct{}
type requestErrorKey struct{}
type RequestError struct {
	Status  int
	Message string
}

func SetRequestError(r *http.Request, status int, message string) {
	if e, ok := r.Context().Value(requestErrorKey{}).(*RequestError); ok {
		e.Status = status
		e.Message = message
	}
}
func GetRequestError(r *http.Request) RequestError {
	if e, ok := r.Context().Value(requestErrorKey{}).(*RequestError); ok {
		return *e
	}
	return RequestError{}
}

type modelCtxKey struct{}

type accountModelCtxKey struct{}

// InitTokenUsage initializes token usage tracking in the request context.
// Returns a pointer that handlers can update with SetTokenUsage.
func InitTokenUsage(r *http.Request) *http.Request {
	usage := &[2]int{0, 0}
	ctx := context.WithValue(r.Context(), requestErrorKey{}, &RequestError{})
	return r.WithContext(context.WithValue(ctx, ctxKey{}, usage))
}

// SetTokenUsage updates the token usage for the request.
func SetTokenUsage(r *http.Request, inputTokens, outputTokens int) {
	if usage, ok := r.Context().Value(ctxKey{}).(*[2]int); ok {
		usage[0] = inputTokens
		usage[1] = outputTokens
	}
}

// GetTokenUsage retrieves the token usage from the request context.
func GetTokenUsage(r *http.Request) (int, int) {
	if usage, ok := r.Context().Value(ctxKey{}).(*[2]int); ok {
		return usage[0], usage[1]
	}
	return 0, 0
}

// SetModel stores the resolved (translated) model name in the request context.
func SetModel(r *http.Request, model string) {
	if m, ok := r.Context().Value(modelCtxKey{}).(*string); ok {
		*m = model
	}
}

// GetModel retrieves the resolved model name from the request context.
func GetModel(r *http.Request) string {
	if m, ok := r.Context().Value(modelCtxKey{}).(*string); ok {
		return *m
	}
	return ""
}

// InitModel initializes model tracking in the request context.
func InitModel(r *http.Request) *http.Request {
	m := new(string)
	return r.WithContext(context.WithValue(r.Context(), modelCtxKey{}, m))
}

// SetAccountDefaultModel stores the account's default model in the request context.
func SetAccountDefaultModel(r *http.Request, defaultModel string) {
	if m, ok := r.Context().Value(accountModelCtxKey{}).(*string); ok {
		*m = defaultModel
	}
}

// GetAccountDefaultModel retrieves the account's default model from the request context.
func GetAccountDefaultModel(r *http.Request) string {
	if m, ok := r.Context().Value(accountModelCtxKey{}).(*string); ok {
		return *m
	}
	return ""
}

// InitAccountModel initializes account default model tracking in the request context.
func InitAccountModel(r *http.Request) *http.Request {
	m := new(string)
	return r.WithContext(context.WithValue(r.Context(), accountModelCtxKey{}, m))
}
