package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kesokaj/agyd/internal/acp"
	"github.com/kesokaj/agyd/internal/config"
	"github.com/kesokaj/agyd/internal/conversation"
)

type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) logger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: level}))
}

func (c *logCapture) raw() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *logCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

func (c *logCapture) byMsg(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.raw()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func (c *logCapture) requireOne(t *testing.T, msg string) map[string]any {
	t.Helper()
	got := c.byMsg(t, msg)
	if len(got) != 1 {
		t.Fatalf("got %d %q log entries, want 1; all logs:\n%s", len(got), msg, c.raw())
	}
	return got[0]
}

type hookACPService struct {
	*fakeACPService
	mu       sync.Mutex
	promptFn func(ctx context.Context, id string, blocks []acp.ContentBlock, onUpdate func([]byte)) (acp.PromptResult, error)
}

func (h *hookACPService) setPromptFn(fn func(ctx context.Context, id string, blocks []acp.ContentBlock, onUpdate func([]byte)) (acp.PromptResult, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.promptFn = fn
}

func (h *hookACPService) Prompt(ctx context.Context, id string, blocks []acp.ContentBlock, onUpdate func([]byte)) (acp.PromptResult, error) {
	h.mu.Lock()
	fn := h.promptFn
	h.mu.Unlock()
	if fn != nil {
		return fn(ctx, id, blocks, onUpdate)
	}
	return h.fakeACPService.Prompt(ctx, id, blocks, onUpdate)
}

type deadlineFlushWriter struct {
	*httptest.ResponseRecorder
	flushed     bool
	deadlineSet bool
}

func (w *deadlineFlushWriter) Flush()                           { w.flushed = true; w.ResponseRecorder.Flush() }
func (w *deadlineFlushWriter) SetWriteDeadline(time.Time) error { w.deadlineSet = true; return nil }

type logTestEnv struct {
	h    http.Handler
	svc  *hookACPService
	logs *logCapture
}

func newLogTestEnv(t *testing.T, level slog.Level, extra ...ExtraRoute) *logTestEnv {
	t.Helper()
	root := realTempDir(t)
	dir, ws, histCwd := filepath.Join(root, "state"), filepath.Join(root, "workspace"), filepath.Join(root, "hist", "one")
	for _, d := range []string{ws, histCwd} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	svc := &hookACPService{fakeACPService: newFakeACPService(histCwd)}
	logs := &logCapture{}
	mgr := conversation.New(conversation.Options{
		Agent: svc, State: config.NewStateStore(dir), Log: discardLog(), Workspace: ws,
	})
	h := NewHandler(logs.logger(level), svc, mgr, Config{DefaultWorkspace: ws, ExtraRoutes: extra})
	return &logTestEnv{h: h, svc: svc, logs: logs}
}

func (e *logTestEnv) do(method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	return e.doCtx(context.Background(), method, target, body, hdr...)
}

func (e *logTestEnv) doCtx(ctx context.Context, method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r).WithContext(ctx)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func quietExtraRoutes() []ExtraRoute {
	okText := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	redir := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/docs", http.StatusFound) })
	static := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/static/app.js":
			_, _ = w.Write([]byte("console.log(1);"))
		case "/static/boom.js":
			w.WriteHeader(http.StatusInternalServerError)
		case "/static/cancel":
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	})
	return []ExtraRoute{
		{Methods: []string{http.MethodGet}, Pattern: "/{$}", Handler: redir},
		{Methods: []string{http.MethodGet}, Pattern: "/docs", Handler: okText},
		{Methods: []string{http.MethodGet}, Pattern: "/openapi.json", Handler: okText},
		{Methods: []string{http.MethodGet}, Pattern: "/static/", Handler: static},
	}
}

func usageJSON(t *testing.T, total any, input, output int) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(struct {
		Input  int `json:"inputTokens,omitempty"`
		Output int `json:"outputTokens,omitempty"`
		Total  any `json:"totalTokens"`
	}{Input: input, Output: output, Total: total})
	if err != nil {
		t.Fatalf("json.Marshal usage: %v", err)
	}
	return b
}

func assertHTTPRequestEntry(t *testing.T, entry map[string]any, wantLevel, wantMethod, wantPath string, wantStatus int) {
	t.Helper()
	for _, c := range []struct {
		key  string
		want any
	}{{"level", wantLevel}, {"method", wantMethod}, {"path", wantPath}, {"status", float64(wantStatus)}} {
		if got := entry[c.key]; got != c.want {
			t.Errorf("%s = %v, want %v", c.key, got, c.want)
		}
	}
	if dur, ok := entry["durationMs"].(float64); !ok || dur < 0 {
		t.Errorf("durationMs = %v (present=%v), want >= 0", entry["durationMs"], ok)
	}
}

func TestAccessLogAndPromptLifecycle(t *testing.T) {
	t.Run("v1_requests_info_level_conversation_and_no_query_string", testV1AccessLogs)
	t.Run("quiet_routes_debug_on_2xx_3xx_and_info_on_4xx_5xx", testQuietRouteAccessLogs)
	t.Run("panic_recovered_logged_at_info_with_status_500", testPanicRecoveredAccessLog)
	t.Run("status_writer_defaults_unwrap_and_first_write_header", testStatusWriterBehavior)
	t.Run("client_cancel_before_headers_status_0_canceled_true", testClientCancelBeforeHeaders)
	t.Run("sse_incremental_flush_preserved_through_status_writer", testSSEIncrementalFlush)
	t.Run("prompt_started_and_finished_json_sse_usage_and_errors", testPromptLifecycleLogs)
	t.Run("usage_total_tokens_valid_absent_zero_and_malformed", testUsageTotalTokens)
	t.Run("privacy_sentinels_never_logged_at_debug", testPrivacySentinelsAtDebug)
}

func testV1AccessLogs(t *testing.T) {
	e := newLogTestEnv(t, slog.LevelInfo)

	want(t, e.do(http.MethodGet, "/v1/status?filter=PRIVACY_CANARY_STATUS_123", ""), http.StatusOK, "")
	entry := e.logs.requireOne(t, "http request")
	assertHTTPRequestEntry(t, entry, "INFO", http.MethodGet, "/v1/status", http.StatusOK)
	if _, ok := entry["conversationId"]; ok {
		t.Errorf("unexpected conversationId before any conversation exists: %v", entry["conversationId"])
	}
	if _, ok := entry["canceled"]; ok {
		t.Errorf("unexpected canceled field on completed request: %v", entry["canceled"])
	}
	if strings.Contains(e.logs.raw(), "PRIVACY_CANARY_STATUS_123") {
		t.Fatalf("query string leaked into logs:\n%s", e.logs.raw())
	}

	e.logs.reset()
	createRec := e.do(http.MethodPost, "/v1/conversations", `{}`)
	want(t, createRec, http.StatusCreated, "")
	convID := decode[map[string]any](t, createRec)["conversationId"].(string)
	createEntry := e.logs.requireOne(t, "http request")
	assertHTTPRequestEntry(t, createEntry, "INFO", http.MethodPost, "/v1/conversations", http.StatusCreated)
	if createEntry["conversationId"] != convID {
		t.Errorf("conversationId = %v, want %q", createEntry["conversationId"], convID)
	}

	e.logs.reset()
	want(t, e.do(http.MethodGet, "/v1/status?foo=bar", ""), http.StatusOK, "")
	statusEntry := e.logs.requireOne(t, "http request")
	assertHTTPRequestEntry(t, statusEntry, "INFO", http.MethodGet, "/v1/status", http.StatusOK)
	if statusEntry["conversationId"] != convID || strings.Contains(e.logs.raw(), "foo=bar") {
		t.Errorf("statusEntry=%+v raw=%s", statusEntry, e.logs.raw())
	}
}

func testQuietRouteAccessLogs(t *testing.T) {
	quietOK := []struct {
		path   string
		status int
	}{
		{"/healthz", http.StatusOK},
		{"/", http.StatusFound},
		{"/docs", http.StatusOK},
		{"/openapi.json", http.StatusOK},
		{"/static/app.js", http.StatusOK},
	}
	quietErr := []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodPost, "/docs", http.StatusMethodNotAllowed},
		{http.MethodGet, "/static/missing.js", http.StatusNotFound},
		{http.MethodGet, "/static/boom.js", http.StatusInternalServerError},
	}

	infoEnv := newLogTestEnv(t, slog.LevelInfo, quietExtraRoutes()...)
	for _, tc := range quietOK {
		want(t, infoEnv.do(http.MethodGet, tc.path, ""), tc.status, "")
	}
	if got := infoEnv.logs.byMsg(t, "http request"); len(got) != 0 {
		t.Fatalf("quiet 2xx/3xx routes emitted %d INFO logs, want 0:\n%s", len(got), infoEnv.logs.raw())
	}
	for _, tc := range quietErr {
		infoEnv.logs.reset()
		want(t, infoEnv.do(tc.method, tc.path, ""), tc.status, "")
		assertHTTPRequestEntry(t, infoEnv.logs.requireOne(t, "http request"), "INFO", tc.method, tc.path, tc.status)
	}

	debugEnv := newLogTestEnv(t, slog.LevelDebug, quietExtraRoutes()...)
	for _, tc := range quietOK {
		debugEnv.logs.reset()
		want(t, debugEnv.do(http.MethodGet, tc.path, ""), tc.status, "")
		assertHTTPRequestEntry(t, debugEnv.logs.requireOne(t, "http request"), "DEBUG", http.MethodGet, tc.path, tc.status)
	}
	debugEnv.logs.reset()
	want(t, debugEnv.do(http.MethodGet, "/static/boom.js", ""), http.StatusInternalServerError, "")
	assertHTTPRequestEntry(t, debugEnv.logs.requireOne(t, "http request"), "INFO", http.MethodGet, "/static/boom.js", http.StatusInternalServerError)
}

func testPanicRecoveredAccessLog(t *testing.T) {
	e := newLogTestEnv(t, slog.LevelInfo, ExtraRoute{
		Methods: []string{http.MethodGet},
		Pattern: "/v1/panic-test",
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("kaboom") }),
	})
	want(t, e.do(http.MethodGet, "/v1/panic-test", ""), http.StatusInternalServerError, "INTERNAL")
	e.logs.requireOne(t, "panic in http handler")
	assertHTTPRequestEntry(t, e.logs.requireOne(t, "http request"), "INFO", http.MethodGet, "/v1/panic-test", http.StatusInternalServerError)
}

func testStatusWriterBehavior(t *testing.T) {
	dw := &deadlineFlushWriter{ResponseRecorder: httptest.NewRecorder()}
	sw := &statusWriter{ResponseWriter: dw}
	rc := http.NewResponseController(sw)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("SetWriteDeadline through statusWriter: %v", err)
	}
	if err := rc.Flush(); err != nil {
		t.Fatalf("Flush through statusWriter: %v", err)
	}
	if !dw.flushed || !dw.deadlineSet {
		t.Fatalf("Unwrap did not reach underlying writer: flushed=%v deadlineSet=%v", dw.flushed, dw.deadlineSet)
	}

	e := newLogTestEnv(t, slog.LevelInfo,
		ExtraRoute{Methods: []string{http.MethodGet}, Pattern: "/v1/empty-ok", Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})},
		ExtraRoute{
			Methods: []string{http.MethodGet},
			Pattern: "/v1/double-header",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				w.WriteHeader(http.StatusInternalServerError)
			}),
		},
	)
	e.do(http.MethodGet, "/v1/empty-ok", "")
	assertHTTPRequestEntry(t, e.logs.requireOne(t, "http request"), "INFO", http.MethodGet, "/v1/empty-ok", http.StatusOK)

	e.logs.reset()
	e.do(http.MethodGet, "/v1/double-header", "")
	assertHTTPRequestEntry(t, e.logs.requireOne(t, "http request"), "INFO", http.MethodGet, "/v1/double-header", http.StatusAccepted)
}

func testClientCancelBeforeHeaders(t *testing.T) {
	e := newLogTestEnv(t, slog.LevelDebug, quietExtraRoutes()...)
	createRec := e.do(http.MethodPost, "/v1/conversations", `{}`)
	want(t, createRec, http.StatusCreated, "")
	convID := decode[map[string]any](t, createRec)["conversationId"].(string)
	e.logs.reset()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.doCtx(ctx, http.MethodPost, "/v1/conversations/"+convID+"/prompt", `{"prompt":"block_until_cancel"}`)
	}()
	e.svc.waitInFlight(t, convID)
	cancel()
	<-done

	entry := e.logs.requireOne(t, "http request")
	assertHTTPRequestEntry(t, entry, "INFO", http.MethodPost, "/v1/conversations/"+convID+"/prompt", 0)
	if entry["canceled"] != true || entry["conversationId"] != convID {
		t.Errorf("canceled=%v conversationId=%v, want true / %q", entry["canceled"], entry["conversationId"], convID)
	}
	e.logs.requireOne(t, "prompt started")
	if got := e.logs.byMsg(t, "prompt finished"); len(got) != 0 {
		t.Errorf("unexpected prompt finished log on canceled prompt: %v", got)
	}

	e.logs.reset()
	qCtx, qCancel := context.WithCancel(t.Context())
	qCancel()
	e.doCtx(qCtx, http.MethodGet, "/static/cancel", "")
	qEntry := e.logs.requireOne(t, "http request")
	assertHTTPRequestEntry(t, qEntry, "INFO", http.MethodGet, "/static/cancel", 0)
	if qEntry["canceled"] != true {
		t.Errorf("quiet canceled = %v, want true", qEntry["canceled"])
	}
}

func testSSEIncrementalFlush(t *testing.T) {
	e := newLogTestEnv(t, slog.LevelInfo)
	releasePrompt := make(chan struct{})
	var promptReturned atomic.Bool

	e.svc.setPromptFn(func(ctx context.Context, id string, _ []acp.ContentBlock, onUpdate func([]byte)) (acp.PromptResult, error) {
		if onUpdate != nil {
			onUpdate([]byte(`{"sessionId":"` + id + `","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"streamed-early"}}}`))
		}
		select {
		case <-releasePrompt:
		case <-ctx.Done():
			return acp.PromptResult{}, ctx.Err()
		}
		promptReturned.Store(true)
		return acp.PromptResult{SessionID: id, StopReason: "end_turn", Text: "streamed-early", Usage: usageJSON(t, 15, 0, 0)}, nil
	})

	ts := httptest.NewServer(e.h)
	defer ts.Close()

	reqCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, ts.URL+"/v1/prompt", strings.NewReader(`{"prompt":"hello"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v1/prompt SSE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	var lastEvent string
	var sawUpdate, sawDone bool
	for sc.Scan() {
		line := sc.Text()
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			lastEvent = ev
			sawDone = sawDone || ev == "prompt_done"
			continue
		}
		if !sawUpdate && lastEvent == "session_update" && strings.Contains(line, "streamed-early") {
			if promptReturned.Load() {
				t.Fatalf("session_update arrived only after Prompt returned; incremental flush broken")
			}
			sawUpdate = true
			close(releasePrompt)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading SSE stream: %v", err)
	}
	if !sawUpdate || !sawDone {
		t.Fatalf("sawUpdate=%v sawDone=%v, want both true", sawUpdate, sawDone)
	}
	resp.Body.Close()
	ts.Close()

	fin := e.logs.requireOne(t, "prompt finished")
	if fin["stream"] != "sse" || fin["updates"] != float64(1) || fin["totalTokens"] != float64(15) {
		t.Errorf("prompt finished = %+v", fin)
	}
	assertHTTPRequestEntry(t, e.logs.requireOne(t, "http request"), "INFO", http.MethodPost, "/v1/prompt", http.StatusOK)
}

func testPromptLifecycleLogs(t *testing.T) {
	e := newLogTestEnv(t, slog.LevelInfo)
	createRec := e.do(http.MethodPost, "/v1/conversations", `{}`)
	want(t, createRec, http.StatusCreated, "")
	convID := decode[map[string]any](t, createRec)["conversationId"].(string)

	e.logs.reset()
	e.svc.setPromptFn(func(_ context.Context, id string, _ []acp.ContentBlock, _ func([]byte)) (acp.PromptResult, error) {
		return acp.PromptResult{
			SessionID: id, StopReason: "end_turn", Text: "done",
			Updates: []json.RawMessage{
				json.RawMessage(`{"sessionUpdate":"agent_thought_chunk"}`),
				json.RawMessage(`{"sessionUpdate":"tool_call"}`),
				json.RawMessage(`{"sessionUpdate":"agent_message_chunk"}`),
			},
			Usage: usageJSON(t, 100, 80, 20),
		}, nil
	})
	want(t, e.do(http.MethodPost, "/v1/conversations/"+convID+"/prompt", `{"prompt":"hi"}`), http.StatusOK, "")

	started := e.logs.requireOne(t, "prompt started")
	if started["level"] != "INFO" || started["conversationId"] != convID ||
		started["model"] != "gemini-4-carbon" || started["thinkingLevel"] != "medium" ||
		started["mode"] != "default" || started["stream"] != "json" {
		t.Errorf("prompt started = %+v", started)
	}
	finished := e.logs.requireOne(t, "prompt finished")
	if finished["level"] != "INFO" || finished["conversationId"] != convID ||
		finished["stream"] != "json" || finished["stopReason"] != "end_turn" ||
		finished["updates"] != float64(3) || finished["totalTokens"] != float64(100) {
		t.Errorf("prompt finished = %+v", finished)
	}
	if dur, ok := finished["durationMs"].(float64); !ok || dur < 0 {
		t.Errorf("prompt finished durationMs = %v", finished["durationMs"])
	}

	e.logs.reset()
	e.svc.setPromptFn(func(_ context.Context, id string, _ []acp.ContentBlock, onUpdate func([]byte)) (acp.PromptResult, error) {
		onUpdate([]byte(`{"sessionId":"` + id + `","update":{"sessionUpdate":"agent_thought_chunk"}}`))
		onUpdate([]byte(`{"sessionId":"` + id + `","update":{"sessionUpdate":"agent_message_chunk"}}`))
		return acp.PromptResult{SessionID: id, StopReason: "end_turn", Text: "ok", Usage: usageJSON(t, 77, 0, 0)}, nil
	})
	want(t, e.do(http.MethodPost, "/v1/conversations/"+convID+"/prompt", `{"prompt":"hi"}`, "Accept", "text/event-stream"), http.StatusOK, "")
	if started = e.logs.requireOne(t, "prompt started"); started["stream"] != "sse" {
		t.Errorf("SSE prompt started stream = %v, want sse", started["stream"])
	}
	if finished = e.logs.requireOne(t, "prompt finished"); finished["stream"] != "sse" || finished["updates"] != float64(2) || finished["totalTokens"] != float64(77) {
		t.Errorf("SSE prompt finished = %+v", finished)
	}

	// Buffered and mid-stream SSE errors must not emit prompt finished.
	e.logs.reset()
	e.svc.setPromptFn(nil)
	want(t, e.do(http.MethodPost, "/v1/conversations/"+convID+"/prompt", `{"prompt":"auth_error"}`), http.StatusUnauthorized, "ACP_AUTH_REQUIRED")
	e.logs.requireOne(t, "prompt started")
	if got := e.logs.byMsg(t, "prompt finished"); len(got) != 0 {
		t.Errorf("unexpected prompt finished on buffered error: %v", got)
	}
	assertHTTPRequestEntry(t, e.logs.requireOne(t, "http request"), "INFO", http.MethodPost, "/v1/conversations/"+convID+"/prompt", http.StatusUnauthorized)

	e.logs.reset()
	e.svc.setPromptFn(func(_ context.Context, id string, _ []acp.ContentBlock, onUpdate func([]byte)) (acp.PromptResult, error) {
		onUpdate([]byte(`{"sessionId":"` + id + `","update":{"sessionUpdate":"agent_thought_chunk"}}`))
		return acp.PromptResult{}, errors.New("mid-stream boom")
	})
	want(t, e.do(http.MethodPost, "/v1/conversations/"+convID+"/prompt", `{"prompt":"hi"}`, "Accept", "text/event-stream"), http.StatusOK, "")
	e.logs.requireOne(t, "prompt started")
	if got := e.logs.byMsg(t, "prompt finished"); len(got) != 0 {
		t.Errorf("unexpected prompt finished on mid-stream SSE error: %v", got)
	}
	assertHTTPRequestEntry(t, e.logs.requireOne(t, "http request"), "INFO", http.MethodPost, "/v1/conversations/"+convID+"/prompt", http.StatusOK)
}

func testUsageTotalTokens(t *testing.T) {
	cases := []struct {
		name    string
		raw     json.RawMessage
		wantVal int
		wantOK  bool
	}{
		{"valid", usageJSON(t, 42, 0, 0), 42, true},
		{"absent", nil, 0, false},
		{"empty", json.RawMessage{}, 0, false},
		{"missing_field", json.RawMessage(`{"input":12,"output":7}`), 0, false},
		{"zero", usageJSON(t, 0, 0, 0), 0, false},
		{"negative", usageJSON(t, -5, 0, 0), 0, false},
		{"wrong_type", usageJSON(t, "42", 0, 0), 0, false},
		{"malformed_json", json.RawMessage(`{malformed`), 0, false},
		{"null", json.RawMessage(`null`), 0, false},
	}
	logs := &logCapture{}
	s := &server{log: logs.logger(slog.LevelInfo)}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := usageTotalTokens(tc.raw)
			if got != tc.wantVal || ok != tc.wantOK {
				t.Errorf("usageTotalTokens(%s) = (%d, %v), want (%d, %v)", string(tc.raw), got, ok, tc.wantVal, tc.wantOK)
			}
			logs.reset()
			s.logPromptFinished(t.Context(), "conv_1", "json", "end_turn", time.Now(), 1, tc.raw)
			fin := logs.requireOne(t, "prompt finished")
			val, present := fin["totalTokens"]
			if present != tc.wantOK || (tc.wantOK && val != float64(tc.wantVal)) {
				t.Errorf("prompt finished totalTokens=%v (present=%v), want %d (present=%v)", val, present, tc.wantVal, tc.wantOK)
			}
		})
	}
}

func testPrivacySentinelsAtDebug(t *testing.T) {
	e := newLogTestEnv(t, slog.LevelDebug)
	const (
		promptCanary    = "PRIVACY_CANARY_PROMPT_99a1"
		replyCanary     = "PRIVACY_CANARY_REPLY_88b2"
		toolArgCanary   = "PRIVACY_CANARY_TOOL_77c3"
		rawUpdateCanary = "PRIVACY_CANARY_UPDATE_66d4"
		queryCanary     = "PRIVACY_CANARY_QUERY_55e5"
		headerCanary    = "PRIVACY_CANARY_HEADER_44f6"
	)
	rawUpdate := `{"sessionUpdate":"tool_call","toolCallId":"tc_1","rawInput":{"cmd":"` + toolArgCanary + `"},"extra":"` + rawUpdateCanary + `"}`
	e.svc.setPromptFn(func(_ context.Context, id string, _ []acp.ContentBlock, onUpdate func([]byte)) (acp.PromptResult, error) {
		if onUpdate != nil {
			onUpdate([]byte(`{"sessionId":"` + id + `","update":` + rawUpdate + `}`))
		}
		return acp.PromptResult{
			SessionID: id, StopReason: "end_turn", Text: replyCanary,
			Updates: []json.RawMessage{json.RawMessage(rawUpdate)},
			Usage:   usageJSON(t, 9, 0, 0),
		}, nil
	})

	body := `{"prompt":"` + promptCanary + `"}`
	want(t, e.do(http.MethodPost, "/v1/prompt?filter="+queryCanary, body, "X-Custom-Header", headerCanary), http.StatusOK, "")
	want(t, e.do(http.MethodPost, "/v1/prompt?cursor="+queryCanary, body, "Accept", "text/event-stream", "X-Custom-Header", headerCanary), http.StatusOK, "")

	if len(e.logs.byMsg(t, "prompt started")) != 2 || len(e.logs.byMsg(t, "prompt finished")) != 2 || len(e.logs.byMsg(t, "http request")) != 2 {
		t.Fatalf("expected 2 started/finished/http logs; got:\n%s", e.logs.raw())
	}
	all := e.logs.raw()
	for _, canary := range []string{promptCanary, replyCanary, toolArgCanary, rawUpdateCanary, queryCanary, headerCanary} {
		if strings.Contains(all, canary) {
			t.Errorf("privacy canary %q leaked into DEBUG logs:\n%s", canary, all)
		}
	}
}
