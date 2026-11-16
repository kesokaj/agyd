package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuard(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	handler := Guard(mux)

	allowedHosts := []string{
		"127.0.0.1:8080",
		"localhost",
		"LOCALHOST:1",
		"[::1]:8080",
	}
	for _, h := range allowedHosts {
		t.Run("allowed host "+h, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/test", nil)
			req.Host = h
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Errorf("got %d, want 200", w.Code)
			}
		})
	}

	deniedHosts := []string{
		"localhost.",
		"evil.example",
		"127.0.0.1.nip.io",
	}
	for _, h := range deniedHosts {
		t.Run("denied host "+h, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/test", nil)
			req.Host = h
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != 403 {
				t.Errorf("got %d, want 403", w.Code)
			}
		})
	}

	t.Run("CSRF cross-site", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", nil)
		req.Host = "localhost:8080"
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Errorf("got %d, want 403", w.Code)
		}
	})

	t.Run("CSRF origin mismatch", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", nil)
		req.Host = "localhost:8080"
		req.Header.Set("Origin", "http://localhost:9999")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Errorf("got %d, want 403", w.Code)
		}
	})

	t.Run("CSRF same-origin", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", nil)
		req.Host = "localhost:8080"
		req.Header.Set("Origin", "http://localhost:8080")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("got %d, want 200", w.Code)
		}
	})

	t.Run("CSRF no headers curl", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", nil)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("got %d, want 200", w.Code)
		}
	})
}
