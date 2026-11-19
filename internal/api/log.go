package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// statusWriter wraps http.ResponseWriter to capture the first HTTP status code
// while keeping http.ResponseController (Flush, SetWriteDeadline) working via Unwrap.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func isQuietRoute(path string) bool {
	switch path {
	case "/healthz", "/", "/docs", "/openapi.json":
		return true
	}
	return strings.HasPrefix(path, "/static/")
}

func (s *server) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Capture request method and path before downstream handlers (e.g. /docs
		// rewriting r.URL.Path to /docs.html) can mutate the request in place.
		method := r.Method
		path := r.URL.Path
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)

		canceled := !sw.wroteHeader && r.Context().Err() != nil
		status := sw.status
		if !sw.wroteHeader && !canceled {
			status = http.StatusOK
		}
		level := slog.LevelInfo
		if isQuietRoute(path) && !canceled && status < http.StatusBadRequest {
			level = slog.LevelDebug
		}

		attrs := make([]any, 0, 12)
		attrs = append(attrs,
			"method", method,
			"path", path,
			"status", status,
			"durationMs", time.Since(start).Milliseconds(),
		)
		if convID := sw.Header().Get(HeaderConversation); convID != "" {
			attrs = append(attrs, "conversationId", convID)
		}
		if canceled {
			attrs = append(attrs, "canceled", true)
		}
		s.log.Log(r.Context(), level, "http request", attrs...)
	})
}
