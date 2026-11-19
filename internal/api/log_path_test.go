package api

import (
	"log/slog"
	"net/http"
	"testing"
)

func TestAccessLogCapturesPathBeforeHandlerMutation(t *testing.T) {
	docsRoute := ExtraRoute{
		Methods: []string{http.MethodGet, http.MethodHead},
		Pattern: "/docs",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path = "/docs.html"
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("docs"))
		}),
	}

	t.Run("GET /docs is quiet at INFO and POST /docs 405 logs original path at INFO", func(t *testing.T) {
		env := newLogTestEnv(t, slog.LevelInfo, docsRoute)

		rec := env.do(http.MethodGet, "/docs", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /docs = %d, want 200", rec.Code)
		}
		if got := env.logs.byMsg(t, "http request"); len(got) != 0 {
			t.Fatalf("GET /docs at INFO emitted %d logs, want 0: %v", len(got), got)
		}

		rec = env.do(http.MethodPost, "/docs", "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST /docs = %d, want 405", rec.Code)
		}
		entry := env.logs.requireOne(t, "http request")
		if entry["level"] != "INFO" || entry["method"] != http.MethodPost || entry["path"] != "/docs" || entry["status"] != float64(http.StatusMethodNotAllowed) {
			t.Errorf("POST /docs log = %v, want INFO POST /docs 405", entry)
		}
	})

	t.Run("GET /docs logs original /docs path at DEBUG", func(t *testing.T) {
		env := newLogTestEnv(t, slog.LevelDebug, docsRoute)

		rec := env.do(http.MethodGet, "/docs", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /docs = %d, want 200", rec.Code)
		}
		entry := env.logs.requireOne(t, "http request")
		if entry["level"] != "DEBUG" || entry["method"] != http.MethodGet || entry["path"] != "/docs" || entry["status"] != float64(http.StatusOK) {
			t.Errorf("GET /docs DEBUG log = %v, want DEBUG GET /docs 200 (not /docs.html)", entry)
		}
	})
}
