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

func TestGuardExtraHosts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	handler := Guard(mux, "192.168.1.5", "FD00::5", "mymac.local", "", "fd01:0:0:0::7")

	for host, want := range map[string]int{
		"192.168.1.5:8080":       200,
		"[fd00::5]:8080":         200,
		"[fd00:0::5]:8080":       200,
		"[fd01::7]:8080":         200,
		"[::ffff:127.0.0.1]:80":  200,
		"MyMac.local:8080":       200,
		"localhost:8080":         200,
		"192.168.1.6:8080":       403,
		"evil.example:8080":      403,
		"192.168.1.5.nip.io":     403,
		"":                       403,
		"mymac.local.evil.com:1": 403,
	} {
		t.Run(host, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/test", nil)
			req.Host = host
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != want {
				t.Errorf("Host %q = %d, want %d", host, w.Code, want)
			}
		})
	}

	t.Run("CSRF still applies to an extra host", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", nil)
		req.Host = "192.168.1.5:8080"
		req.Header.Set("Origin", "http://evil.example")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Errorf("got %d, want 403", w.Code)
		}
	})

	t.Run("CSRF same-origin on an extra host", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", nil)
		req.Host = "192.168.1.5:8080"
		req.Header.Set("Origin", "http://192.168.1.5:8080")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("got %d, want 200", w.Code)
		}
	})
}
