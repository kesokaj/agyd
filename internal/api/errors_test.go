package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTextLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

// TestErrorShapes pins the error contract found missing by the live surface
// run: every refusal is an AIP-193 JSON body with the right status.
func TestErrorShapes(t *testing.T) {
	e := newTestEnv(t)

	t.Run("wrong method on a known path is 405 with Allow", func(t *testing.T) {
		rec := e.do(http.MethodDelete, "/v1/models", "")
		want(t, rec, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		if got := rec.Header().Get("Allow"); got != "GET" {
			t.Errorf("Allow = %q, want GET", got)
		}
		rec = e.do(http.MethodPut, "/v1/conversations/abc", "")
		want(t, rec, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		if got := rec.Header().Get("Allow"); got != "GET, PATCH, DELETE" {
			t.Errorf("Allow = %q, want GET, PATCH, DELETE", got)
		}
	})

	t.Run("unknown path stays 404", func(t *testing.T) {
		want(t, e.do(http.MethodGet, "/v1/nope", ""), http.StatusNotFound, "NOT_FOUND")
		want(t, e.do(http.MethodPost, "/v1/conversations/a/b/c", "{}"), http.StatusNotFound, "NOT_FOUND")
	})

	t.Run("trailing data after the JSON value is 400", func(t *testing.T) {
		want(t, e.do(http.MethodPut, "/v1/defaults", `{"thinkingLevel":"high"} {"x":1}`), http.StatusBadRequest, "MALFORMED_BODY")
		want(t, e.do(http.MethodPut, "/v1/defaults", `{"thinkingLevel":"high"}garbage`), http.StatusBadRequest, "MALFORMED_BODY")
		if got := e.mgr.Defaults().ThinkingLevel; got == "high" {
			t.Errorf("a refused body changed the defaults: thinkingLevel=%q", got)
		}
		// Trailing whitespace is still one value.
		want(t, e.do(http.MethodPost, "/v1/prompt", "{\"prompt\":\"hi\"}\n\n"), http.StatusOK, "")
	})

	t.Run("body over 1 MiB is 413", func(t *testing.T) {
		big := `{"model":"` + strings.Repeat("x", maxBodyBytes) + `"}`
		want(t, e.do(http.MethodPut, "/v1/defaults", big), http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
	})

	t.Run("cwd must be an existing directory", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		before := len(e.svc.ListSessions())
		want(t, e.do(http.MethodPost, "/v1/conversations", `{"cwd":"`+filepath.Join(dir, "missing")+`"}`), http.StatusBadRequest, "INVALID_CWD")
		want(t, e.do(http.MethodPost, "/v1/conversations", `{"cwd":"`+file+`"}`), http.StatusBadRequest, "INVALID_CWD")
		if after := len(e.svc.ListSessions()); after != before {
			t.Errorf("refused cwd created a conversation: live %d -> %d", before, after)
		}
		want(t, e.do(http.MethodPost, "/v1/conversations", `{"cwd":"`+dir+`"}`), http.StatusCreated, "")
	})

	t.Run("cwd that resolves into .gemini is refused", func(t *testing.T) {
		dir := t.TempDir()
		store := filepath.Join(dir, ".gemini")
		upper := filepath.Join(dir, "x", ".GEMINI")
		for _, d := range []string{store, upper} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		link := filepath.Join(dir, "proj")
		if err := os.Symlink(store, link); err != nil {
			t.Fatal(err)
		}
		before := len(e.svc.ListSessions())
		for _, cwd := range []string{link, filepath.Join(link, "sub"), upper} {
			if cwd == filepath.Join(link, "sub") {
				if err := os.Mkdir(filepath.Join(store, "sub"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			want(t, e.do(http.MethodPost, "/v1/conversations", `{"cwd":"`+cwd+`"}`), http.StatusBadRequest, "INVALID_CWD")
		}
		if after := len(e.svc.ListSessions()); after != before {
			t.Errorf("refused cwd created a conversation: live %d -> %d", before, after)
		}
	})
}

func TestCrossOriginRefusalIsJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /test", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Host = "localhost:8080"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	Guard(mux).ServeHTTP(rec, req)
	want(t, rec, http.StatusForbidden, "CROSS_ORIGIN_REQUEST")
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestClientClosedRequestIsNotAServerError: a handler that fails because the
// client disconnected writes no error body and logs no ERROR.
func TestClientClosedRequestIsNotAServerError(t *testing.T) {
	var logs strings.Builder
	s := &server{log: newTextLogger(&logs)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/prompt", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.adapt(func(http.ResponseWriter, *http.Request) error { return context.Canceled }).ServeHTTP(rec, req)
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %q to a gone client", rec.Body)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("client disconnect logged as ERROR: %s", logs.String())
	}

	// A cancellation the client did not cause is still a server error.
	rec = httptest.NewRecorder()
	s.adapt(func(http.ResponseWriter, *http.Request) error { return context.Canceled }).
		ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/prompt", nil))
	want(t, rec, http.StatusInternalServerError, "INTERNAL")
}
