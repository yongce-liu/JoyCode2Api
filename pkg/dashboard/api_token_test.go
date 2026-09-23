package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/auth"
)

func TestCustomTokenEndpoint(t *testing.T) {
	h, s := setupTestHandler(t)
	for _, id := range []string{"a", "b"} {
		if err := s.AddAccount(id, "pt-"+id, "", false, ""); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	for _, tt := range []struct {
		path, body string
		status     int
	}{
		{"/api/accounts/a/token", `{"api_token":"sk-custom"}`, 200},
		{"/api/accounts/b/token", `{"api_token":"sk-custom"}`, 409},
		{"/api/accounts/a/token", `{"api_token":"b"}`, 409},
		{"/api/accounts/a/token", `{"api_token":"has spaces"}`, 400},
		{"/api/accounts/a/token", `{}`, 400},
		{"/api/accounts/a/token", `{"api_token":123}`, 400},
		{"/api/accounts/missing/token", `{"api_token":"valid"}`, 404},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("PUT", tt.path, strings.NewReader(tt.body)))
		if w.Code != tt.status {
			t.Fatalf("%s: status %d want %d: %s", tt.path, w.Code, tt.status, w.Body)
		}
	}
	a, _ := s.GetAccount("a")
	if a.APIToken != "sk-custom" {
		t.Fatal("token not saved or overwritten after failure")
	}
	// Both manual updates and random resets remain dashboard-authenticated.
	if err := s.SetSetting("auth_password_hash", "configured"); err != nil {
		t.Fatal(err)
	}
	protected := auth.JWTMiddleware(s, mux)
	for _, tt := range []struct{ method, path string }{{"PUT", "/api/accounts/a/token"}, {"POST", "/api/accounts/a/renew-token"}} {
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{"api_token":"unauthorized"}`)))
		if w.Code != 401 {
			t.Fatalf("unauthenticated update returned %d", w.Code)
		}
	}
	a, _ = s.GetAccount("a")
	if a.APIToken != "sk-custom" {
		t.Fatal("unauthorized update changed token")
	}
}
