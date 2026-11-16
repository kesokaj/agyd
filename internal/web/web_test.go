package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"agy-api/internal/api"
)

func TestWebRoutes(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux)

	tests := []struct {
		path string
		want int
		ct   string
		csp  bool
	}{
		{"/", http.StatusFound, "", true},
		{"/docs", http.StatusOK, "text/html; charset=utf-8", true},
		{"/openapi.json", http.StatusOK, "application/json", true},
		{"/static/docs.js", http.StatusOK, "text/javascript; charset=utf-8", true},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != tt.want {
				t.Errorf("GET %s = %d, want %d", tt.path, w.Code, tt.want)
			}
			if tt.want == http.StatusOK {
				if ct := w.Header().Get("Content-Type"); ct != tt.ct {
					t.Errorf("Content-Type = %q, want %q", ct, tt.ct)
				}
				if csp := w.Header().Get("Content-Security-Policy"); tt.csp && csp != "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'" {
					t.Errorf("CSP = %q", csp)
				}
				if nos := w.Header().Get("X-Content-Type-Options"); nos != "nosniff" {
					t.Errorf("nosniff = %q", nos)
				}
			}
		})
	}
}

func TestOpenAPIRoutesMatchAPI(t *testing.T) {
	b, err := staticFS.ReadFile("static/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}

	apiMap := make(map[string]bool)
	for _, r := range api.Routes() {
		apiMap[r.Method+" "+r.Pattern] = true
	}

	openapiMap := make(map[string]bool)
	for path, methods := range doc.Paths {
		for method := range methods {
			if method == "parameters" {
				continue
			}
			// capitalize method
			m := "GET"
			if method == "post" {
				m = "POST"
			}
			if method == "delete" {
				m = "DELETE"
			}
			openapiMap[m+" "+path] = true
		}
	}

	if !reflect.DeepEqual(apiMap, openapiMap) {
		t.Errorf("routes mismatch:\nAPI: %v\nOpenAPI: %v", apiMap, openapiMap)
	}
}
