package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/openai"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

func TestInvalidModelStatsForJSONAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "proxy.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: ")
				}
				fmt.Fprint(w, `{"error":{"code":"6002","message":"model does not exist"}}`)
				if stream {
					fmt.Fprint(w, "\n\ndata: [DONE]\n\n")
				}
			}))
			defer upstream.Close()
			c := joycode.NewClient("test", "test")
			c.ColorBaseURL = upstream.URL
			mux := http.NewServeMux()
			openai.NewServer(c, st).RegisterRoutes(mux)
			handler := requestLogMiddleware(mux, st)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"nonexistent","stream":%t,"messages":[]}`, stream))))
			deadline := time.Now().Add(2 * time.Second)
			for {
				logs, err := st.GetRecentLogs(10)
				if err != nil {
					t.Fatal(err)
				}
				if len(logs) > 0 {
					if len(logs) != 1 || logs[0].Model != "nonexistent" || logs[0].StatusCode != 404 || logs[0].ErrorMessage == "" {
						t.Fatalf("incorrect stats: %+v", logs)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("log not written")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
