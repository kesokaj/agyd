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

	"github.com/kesokaj/agyd/internal/acp"
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
		dir := realTempDir(t)
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
		dir := realTempDir(t)
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
			rec := e.do(http.MethodPost, "/v1/conversations", `{"cwd":"`+cwd+`"}`)
			want(t, rec, http.StatusBadRequest, "INVALID_CWD")
			if strings.Contains(rec.Body.String(), store) {
				t.Errorf("error leaked .gemini target path %q: %s", store, rec.Body)
			}
		}
		if after := len(e.svc.ListSessions()); after != before {
			t.Errorf("refused cwd created a conversation: live %d -> %d", before, after)
		}
	})
}

func TestCwdRejectsSymlinks(t *testing.T) {
	e := newTestEnv(t)
	base := realTempDir(t)
	realParent := filepath.Join(base, "real")
	realTarget := filepath.Join(realParent, "sub")
	if err := os.MkdirAll(realTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	leafLink := filepath.Join(base, "leaf-link")
	if err := os.Symlink(realTarget, leafLink); err != nil {
		t.Fatal(err)
	}
	midLink := filepath.Join(base, "mid-link")
	if err := os.Symlink(realParent, midLink); err != nil {
		t.Fatal(err)
	}
	midPath := filepath.Join(midLink, "sub")

	secretFile := filepath.Join(base, "secret-target.txt")
	if err := os.WriteFile(secretFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(base, "file-link")
	if err := os.Symlink(secretFile, fileLink); err != nil {
		t.Fatal(err)
	}

	before := len(e.svc.ListSessions())

	t.Run("symlink at end of cwd is 400 INVALID_CWD naming real path", func(t *testing.T) {
		rec := e.do(http.MethodPost, "/v1/conversations", `{"cwd":"`+leafLink+`"}`)
		want(t, rec, http.StatusBadRequest, "INVALID_CWD")
		if !strings.Contains(rec.Body.String(), realTarget) {
			t.Errorf("leaf symlink error body %s does not name real path %q", rec.Body, realTarget)
		}
	})

	t.Run("symlink in middle of cwd is 400 INVALID_CWD naming real path", func(t *testing.T) {
		rec := e.do(http.MethodPost, "/v1/conversations", `{"cwd":"`+midPath+`"}`)
		want(t, rec, http.StatusBadRequest, "INVALID_CWD")
		if !strings.Contains(rec.Body.String(), realTarget) {
			t.Errorf("middle symlink error body %s does not name real path %q", rec.Body, realTarget)
		}
	})

	t.Run("symlink to regular file is 400 INVALID_CWD without disclosing file target", func(t *testing.T) {
		rec := e.do(http.MethodPost, "/v1/conversations", `{"cwd":"`+fileLink+`"}`)
		want(t, rec, http.StatusBadRequest, "INVALID_CWD")
		if strings.Contains(rec.Body.String(), secretFile) {
			t.Errorf("file symlink error body %s leaked non-directory target %q", rec.Body, secretFile)
		}
	})

	t.Run("use on history conversation with symlink cwd is 400 INVALID_CWD naming real path", func(t *testing.T) {
		e.svc.mu.Lock()
		e.svc.history["hist_link"] = acp.AgentSession{SessionID: "hist_link", Cwd: leafLink, Title: "linked"}
		e.svc.mu.Unlock()
		rec := e.do(http.MethodPost, "/v1/conversations/hist_link/use", "")
		want(t, rec, http.StatusBadRequest, "INVALID_CWD")
		if !strings.Contains(rec.Body.String(), realTarget) {
			t.Errorf("use symlink error body %s does not name real path %q", rec.Body, realTarget)
		}
	})

	t.Run("whitespace-only and uncleaned dotdot through symlink are 400 INVALID_CWD", func(t *testing.T) {
		want(t, e.do(http.MethodPost, "/v1/conversations", `{"cwd":"   "}`), http.StatusBadRequest, "INVALID_CWD")
		want(t, e.do(http.MethodPost, "/v1/conversations", `{"cwd":"`+midLink+`/../real/sub"}`), http.StatusBadRequest, "INVALID_CWD")
	})

	if after := len(e.svc.ListSessions()); after != before {
		t.Fatalf("refused symlink cwd created a conversation: live %d -> %d", before, after)
	}

	t.Run("real path passes", func(t *testing.T) {
		c := createConv(t, e, `{"cwd":"`+realTarget+`"}`)
		if c.Cwd != realTarget {
			t.Errorf("created Cwd = %q, want %q", c.Cwd, realTarget)
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

func TestExtraRoutesMethodTable(t *testing.T) {
	e := newTestEnv(t)
	created := decode[map[string]any](t, e.do(http.MethodPost, "/v1/conversations", `{}`))
	id := created["conversationId"].(string)

	h := NewHandler(discardLog(), e.svc, e.mgr, Config{
		DefaultWorkspace: e.workspace,
		ExtraRoutes: []ExtraRoute{
			{
				Methods: []string{http.MethodGet, http.MethodHead},
				Pattern: "/docs",
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					if r.Method == http.MethodGet {
						_, _ = io.WriteString(w, "ok")
					}
				}),
			},
			{
				Methods: []string{http.MethodGet, http.MethodHead},
				Pattern: "/{$}",
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, "/docs", http.StatusFound)
				}),
			},
		},
	})

	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}

	// Allowed methods reach the extra handler without attaching Agyd-* headers.
	rec := do(http.MethodGet, "/docs")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("GET /docs = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(HeaderConversation); got != "" {
		t.Errorf("GET /docs %s = %q, want empty", HeaderConversation, got)
	}
	rec = do(http.MethodHead, "/docs")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD /docs = %d len=%d, want 200 empty", rec.Code, rec.Body.Len())
	}

	// Wrong methods on extra routes answer 405 JSON with Allow and Agyd-* headers.
	for _, path := range []string{"/", "/docs"} {
		rec = do(http.MethodPost, path)
		want(t, rec, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("POST %s Allow = %q, want %q", path, got, "GET, HEAD")
		}
		if got := rec.Header().Get(HeaderConversation); got != id {
			t.Errorf("POST %s %s = %q, want %q", path, HeaderConversation, got, id)
		}
	}

	// Unknown paths stay 404 without Allow.
	rec = do(http.MethodPost, "/unknown")
	want(t, rec, http.StatusNotFound, "NOT_FOUND")
	if got := rec.Header().Get("Allow"); got != "" {
		t.Errorf("POST /unknown Allow = %q, want empty", got)
	}
}
