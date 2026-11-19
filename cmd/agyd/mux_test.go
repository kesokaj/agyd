package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kesokaj/agyd/internal/conversation"
)

func TestMuxUnknownPathJSON(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	mux := newMux(log, nil, conversation.New(conversation.Options{}), "/tmp")

	req := httptest.NewRequest("GET", "/v1/typo", nil)
	req.Host = "localhost" // Bypass host guard
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("got content-type %q, want application/json", ct)
	}

	var res map[string]any
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("failed to parse JSON body: %v", err)
	}
	if errMap, ok := res["error"].(map[string]any); ok {
		if errMap["status"] != "NOT_FOUND" {
			t.Errorf("got status %v, want NOT_FOUND", errMap["status"])
		}
	} else {
		t.Errorf("missing error envelope in response")
	}
}

func TestMuxIsGuarded(t *testing.T) {
	mux := newMux(slog.New(slog.NewJSONHandler(io.Discard, nil)), nil, conversation.New(conversation.Options{}), "/tmp")
	tests := []struct {
		name, method, path, host, fetchSite string
	}{
		{"foreign host on docs", http.MethodGet, "/docs", "evil.example", ""},
		{"foreign host on docs post", http.MethodPost, "/docs", "evil.example", ""},
		{"foreign host on api", http.MethodGet, "/v1/status", "evil.example:8080", ""},
		{"cross-site post on docs", http.MethodPost, "/docs", "localhost:8080", "cross-site"},
		{"cross-site delete", http.MethodDelete, "/v1/conversations/x", "localhost:8080", "cross-site"},
		{"cross-site cancel", http.MethodPost, "/v1/conversations/x/cancel", "localhost:8080", "cross-site"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Host = tt.host
			if tt.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s Host=%s = %d, want 403", tt.method, tt.path, tt.host, w.Code)
			}
		})
	}
}

func TestMuxWebRoutesMethodNotAllowed(t *testing.T) {
	mux := newMux(slog.New(slog.NewJSONHandler(io.Discard, nil)), nil, conversation.New(conversation.Options{}), "/tmp")

	t.Run("wrong method on web routes returns 405 JSON with Allow GET, HEAD", func(t *testing.T) {
		for _, tc := range []struct {
			method string
			path   string
		}{
			{http.MethodPost, "/docs"},
			{http.MethodPut, "/docs"},
			{http.MethodDelete, "/docs"},
			{http.MethodPatch, "/docs"},
			{http.MethodOptions, "/docs"},
			{http.MethodPost, "/"},
			{http.MethodDelete, "/"},
			{http.MethodPost, "/openapi.json"},
			{http.MethodPut, "/openapi.json"},
			{http.MethodPost, "/static/docs.js"},
			{http.MethodDelete, "/static/docs.js"},
			{http.MethodPost, "/static/"},
			{http.MethodPost, "/static"},
		} {
			t.Run(tc.method+" "+tc.path, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, tc.path, nil)
				req.Host = "localhost:8080"
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)

				if w.Code != http.StatusMethodNotAllowed {
					t.Fatalf("%s %s = %d, want 405; body=%s", tc.method, tc.path, w.Code, w.Body.String())
				}
				if got := w.Header().Get("Allow"); got != "GET, HEAD" {
					t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
				}
				if ct := w.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", ct)
				}
				var res struct {
					Error struct {
						Code    int    `json:"code"`
						Status  string `json:"status"`
						Message string `json:"message"`
						Details []struct {
							Reason string `json:"reason"`
							Domain string `json:"domain"`
						} `json:"details"`
					} `json:"error"`
				}
				if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
					t.Fatalf("decode JSON error envelope: %v", err)
				}
				if res.Error.Code != http.StatusMethodNotAllowed || res.Error.Status != "UNIMPLEMENTED" {
					t.Errorf("error envelope = %+v, want code 405 UNIMPLEMENTED", res.Error)
				}
				if len(res.Error.Details) == 0 || res.Error.Details[0].Reason != "METHOD_NOT_ALLOWED" || res.Error.Details[0].Domain != "agyd.local" {
					t.Errorf("error details = %+v, want METHOD_NOT_ALLOWED in agyd.local", res.Error.Details)
				}
			})
		}
	})

	t.Run("GET and HEAD succeed on web routes", func(t *testing.T) {
		for _, tc := range []struct {
			path string
			want int
		}{
			{"/", http.StatusFound},
			{"/docs", http.StatusOK},
			{"/openapi.json", http.StatusOK},
			{"/static/docs.js", http.StatusOK},
		} {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				req := httptest.NewRequest(method, tc.path, nil)
				req.Host = "localhost:8080"
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)

				if w.Code != tc.want {
					t.Errorf("%s %s = %d, want %d", method, tc.path, w.Code, tc.want)
				}
				if w.Header().Get("Content-Security-Policy") == "" {
					t.Errorf("%s %s missing CSP header", method, tc.path)
				}
				if w.Header().Get("X-Content-Type-Options") != "nosniff" {
					t.Errorf("%s %s nosniff = %q", method, tc.path, w.Header().Get("X-Content-Type-Options"))
				}
				if method == http.MethodHead && w.Body.Len() != 0 {
					t.Errorf("HEAD %s wrote %d body bytes, want 0", tc.path, w.Body.Len())
				}
			}
		}
	})

	t.Run("unknown paths near web routes stay 404 without Allow", func(t *testing.T) {
		for _, tc := range []struct {
			method string
			path   string
		}{
			{http.MethodPost, "/docs/extra"},
			{http.MethodGet, "/docs/extra"},
			{http.MethodPost, "/nope"},
			{http.MethodGet, "/openapi.json/extra"},
		} {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Host = "localhost:8080"
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", tc.method, tc.path, w.Code)
			}
			if got := w.Header().Get("Allow"); got != "" {
				t.Errorf("%s %s Allow = %q, want empty", tc.method, tc.path, got)
			}
		}
	})

	t.Run("API route 405 Allow headers remain unchanged", func(t *testing.T) {
		for _, tc := range []struct {
			method string
			path   string
			allow  string
		}{
			{http.MethodDelete, "/v1/models", "GET"},
			{http.MethodPut, "/v1/current", "GET, PATCH"},
			{http.MethodPut, "/v1/conversations/abc", "GET, PATCH, DELETE"},
			{http.MethodPost, "/healthz", "GET"},
		} {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Host = "localhost:8080"
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", tc.method, tc.path, w.Code)
			}
			if got := w.Header().Get("Allow"); got != tc.allow {
				t.Errorf("%s %s Allow = %q, want %q", tc.method, tc.path, got, tc.allow)
			}
		}
	})
}

func TestMuxDocsRouteLoggedAtDebug(t *testing.T) {
	var buf bytes.Buffer
	var lv slog.LevelVar
	mux := newMux(newLogger(&buf, &lv), nil, conversation.New(conversation.Options{}), "/tmp")

	doGet := func(path string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, w.Code)
		}
	}

	parseLogs := func(raw string) []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("unmarshal log line %q: %v", line, err)
			}
			out = append(out, rec)
		}
		return out
	}

	quietPaths := []string{"/docs", "/openapi.json", "/healthz"}

	t.Run("INFO emits zero access logs for quiet web and health routes", func(t *testing.T) {
		lv.Set(slog.LevelInfo)
		buf.Reset()
		for _, path := range quietPaths {
			doGet(path)
		}
		if recs := parseLogs(buf.String()); len(recs) != 0 {
			t.Fatalf("INFO emitted %d logs for %v, want 0: %v", len(recs), quietPaths, recs)
		}
	})

	t.Run("DEBUG logs /docs, /openapi.json, and /healthz with original path", func(t *testing.T) {
		lv.Set(slog.LevelDebug)
		for _, path := range quietPaths {
			buf.Reset()
			doGet(path)
			recs := parseLogs(buf.String())
			if len(recs) != 1 {
				t.Fatalf("GET %s emitted %d DEBUG logs, want 1: %v", path, len(recs), recs)
			}
			rec := recs[0]
			if rec["severity"] != "DEBUG" || rec["message"] != "http request" || rec["method"] != http.MethodGet || rec["path"] != path || rec["status"] != float64(http.StatusOK) {
				t.Errorf("GET %s log = %v, want DEBUG http request GET %s 200", path, rec, path)
			}
		}
	})
}
