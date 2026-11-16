package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMuxUnknownPathJSON(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	mux := newMux(log, nil, "/tmp")

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
	mux := newMux(slog.New(slog.NewJSONHandler(io.Discard, nil)), nil, "/tmp")
	tests := []struct {
		name, method, path, host, fetchSite string
	}{
		{"foreign host on docs", http.MethodGet, "/docs", "evil.example", ""},
		{"foreign host on api", http.MethodGet, "/v1/status", "evil.example:8080", ""},
		{"cross-site delete", http.MethodDelete, "/v1/sessions/x", "localhost:8080", "cross-site"},
		{"cross-site cancel", http.MethodPost, "/v1/sessions/x/cancel", "localhost:8080", "cross-site"},
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
