package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
)

func TestModelsRefreshUsesAccountAndCallableIDs(t *testing.T) {
	h, st := setupTestHandler(t)
	if err := st.AddAccount("user", "test-key", "", true, ""); err != nil {
		t.Fatal(err)
	}
	version := 1
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ptKey") != "test-key" {
			t.Error("wrong account")
		}
		fmt.Fprintf(w, `{"data":[{"label":"Display name","chatApiModel":"dynamic-%d"},{"label":"duplicate","chatApiModel":"dynamic-%d"}]}`, version, version)
	}))
	defer upstream.Close()
	old := joycode.DefaultColorBaseURL
	joycode.DefaultColorBaseURL = upstream.URL
	defer func() { joycode.DefaultColorBaseURL = old }()
	for _, path := range []string{"/api/models", "/api/accounts/user/models"} {
		for version = 1; version <= 2; version++ {
			mux := http.NewServeMux()
			h.RegisterRoutes(mux)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			models := decodeJSON(t, w)["models"].([]interface{})
			if len(models) != 1 {
				t.Fatalf("not deduplicated: %v", models)
			}
			m := models[0].(map[string]interface{})
			if m["id"] != fmt.Sprintf("dynamic-%d", version) || m["name"] != "Display name" {
				t.Fatalf("stale/wrong model: %v", m)
			}
		}
	}
}
