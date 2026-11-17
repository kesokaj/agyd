package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agyd/internal/acp"
)

type fakeACPService struct {
	mu       sync.Mutex
	sessions map[string]acp.SessionInfo
	inFlight map[string]chan struct{}
	seq      int
	failRPC  error
}

func newFakeACPService() *fakeACPService {
	return &fakeACPService{
		sessions: make(map[string]acp.SessionInfo),
		inFlight: make(map[string]chan struct{}),
	}
}

func (f *fakeACPService) Status(_ context.Context) (acp.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRPC != nil {
		return acp.Status{}, f.failRPC
	}
	return acp.Status{
		Ready:              true,
		ProtocolVersion:    1,
		Backend:            "agy",
		AgentInfo:          acp.AgentInfo{Name: "antigravity-cli", Title: "Antigravity CLI", Version: "1.5.9"},
		PermissionPolicy:   acp.PermissionAllowAll,
		ActiveSessionCount: len(f.sessions),
	}, nil
}

func (f *fakeACPService) NewSession(_ context.Context, cwd string) (acp.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRPC != nil {
		return acp.SessionInfo{}, f.failRPC
	}
	if cwd == "" {
		cwd = "/workspace"
	}
	if !strings.HasPrefix(cwd, "/") || strings.Contains(cwd, "..") {
		return acp.SessionInfo{}, acp.ErrInvalidConfig
	}
	f.seq++
	id := "sess_test"
	if f.seq > 1 {
		id = "sess_test_2"
	}
	info := acp.SessionInfo{
		SessionID:  id,
		Cwd:        cwd,
		Backend:    "agy",
		CreateTime: time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC),
	}
	f.sessions[id] = info
	return info, nil
}

func (f *fakeACPService) GetSession(sessionID string) (acp.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	if !ok {
		return acp.SessionInfo{}, acp.ErrSessionNotFound
	}
	return s, nil
}

func (f *fakeACPService) ListSessions() []acp.SessionInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]acp.SessionInfo, 0, len(f.sessions))
	for _, s := range f.sessions {
		out = append(out, s)
	}
	return out
}

func (f *fakeACPService) CloseSession(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[sessionID]; !ok {
		return acp.ErrSessionNotFound
	}
	delete(f.sessions, sessionID)
	return nil
}

func (f *fakeACPService) Prompt(ctx context.Context, sessionID string, blocks []acp.ContentBlock, onUpdate func(rawUpdate []byte)) (acp.PromptResult, error) {
	f.mu.Lock()
	if _, ok := f.sessions[sessionID]; !ok {
		f.mu.Unlock()
		return acp.PromptResult{}, acp.ErrSessionNotFound
	}
	if _, busy := f.inFlight[sessionID]; busy {
		f.mu.Unlock()
		return acp.PromptResult{}, acp.ErrPromptInFlight
	}
	ch := make(chan struct{})
	f.inFlight[sessionID] = ch
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.inFlight, sessionID)
		f.mu.Unlock()
	}()

	promptText := ""
	if len(blocks) > 0 {
		promptText = blocks[0].Text
	}
	if promptText == "auth_error" {
		return acp.PromptResult{}, &acp.RPCError{Code: -32000, Message: "authentication required: run agy acp --login"}
	}
	if promptText == "block_until_cancel" {
		select {
		case <-ch:
			return acp.PromptResult{SessionID: sessionID, StopReason: "cancelled"}, nil
		case <-ctx.Done():
			return acp.PromptResult{}, ctx.Err()
		}
	}

	upd := []byte(`{"sessionId":"` + sessionID + `","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello from ACP"}}}`)
	if onUpdate != nil {
		onUpdate(upd)
	}
	return acp.PromptResult{
		SessionID:  sessionID,
		StopReason: "end_turn",
		Text:       "Hello from ACP",
		Updates:    []json.RawMessage{json.RawMessage(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello from ACP"}}`)},
	}, nil
}

func (f *fakeACPService) Cancel(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[sessionID]; !ok {
		return acp.ErrSessionNotFound
	}
	if ch, ok := f.inFlight[sessionID]; ok {
		close(ch)
		delete(f.inFlight, sessionID)
	}
	return nil
}

func (f *fakeACPService) Close() error { return nil }

func newTestMux(svc acp.Service) http.Handler {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewHandler(log, svc, Config{DefaultWorkspace: "/workspace"})
}

func authReq(method, target, body string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestStatusAndHealthEndpoints(t *testing.T) {
	mux := newTestMux(newFakeACPService())

	t.Run("healthz returns 204", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusNoContent {
			t.Errorf("GET /healthz status = %d, want 204", rec.Code)
		}
	})

	t.Run("status returns 200 without auth header", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body)
		}
		var st struct {
			acp.Status
			Workspace string `json:"workspace"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !st.Ready || st.ProtocolVersion != 1 {
			t.Errorf("status = %+v, want Ready=true ProtocolVersion=1", st)
		}
		if st.Workspace != "/workspace" {
			t.Errorf("workspace = %q, want /workspace", st.Workspace)
		}
	})
}

func TestSessionEndpoints(t *testing.T) {
	mux := newTestMux(newFakeACPService())

	// Create session with default workspace.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authReq(http.MethodPost, "/v1/sessions", `{}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want 201; body = %s", rec.Code, rec.Body)
	}
	var created acp.SessionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if created.SessionID != "sess_test" || created.Cwd != "/workspace" {
		t.Fatalf("created = %+v, want sess_test in /workspace", created)
	}

	// Reject relative cwd with 400 INVALID_CWD.
	recBadCwd := httptest.NewRecorder()
	mux.ServeHTTP(recBadCwd, authReq(http.MethodPost, "/v1/sessions", `{"cwd":"relative/path"}`))
	if recBadCwd.Code != http.StatusBadRequest || !strings.Contains(recBadCwd.Body.String(), "INVALID_CWD") {
		t.Errorf("relative cwd = %d %s, want 400 INVALID_CWD", recBadCwd.Code, recBadCwd.Body)
	}

	// Reject .gemini credential directory as cwd with 400 INVALID_CWD.
	recGeminiCwd := httptest.NewRecorder()
	mux.ServeHTTP(recGeminiCwd, authReq(http.MethodPost, "/v1/sessions", `{"cwd":"/home/agy/.gemini"}`))
	if recGeminiCwd.Code != http.StatusBadRequest || !strings.Contains(recGeminiCwd.Body.String(), "INVALID_CWD") {
		t.Errorf(".gemini cwd = %d %s, want 400 INVALID_CWD", recGeminiCwd.Code, recGeminiCwd.Body)
	}

	// Get session.
	recGet := httptest.NewRecorder()
	mux.ServeHTTP(recGet, authReq(http.MethodGet, "/v1/sessions/sess_test", ""))
	if recGet.Code != http.StatusOK {
		t.Errorf("GET /v1/sessions/sess_test = %d, want 200", recGet.Code)
	}

	// List sessions.
	recList := httptest.NewRecorder()
	mux.ServeHTTP(recList, authReq(http.MethodGet, "/v1/sessions", ""))
	if recList.Code != http.StatusOK || !strings.Contains(recList.Body.String(), `"sess_test"`) {
		t.Errorf("GET /v1/sessions = %d %s, want 200 with sess_test", recList.Code, recList.Body)
	}

	// Delete session.
	recDel := httptest.NewRecorder()
	mux.ServeHTTP(recDel, authReq(http.MethodDelete, "/v1/sessions/sess_test", ""))
	if recDel.Code != http.StatusNoContent {
		t.Errorf("DELETE /v1/sessions/sess_test = %d, want 204", recDel.Code)
	}

	// Get deleted session returns 404 SESSION_NOT_FOUND.
	rec404 := httptest.NewRecorder()
	mux.ServeHTTP(rec404, authReq(http.MethodGet, "/v1/sessions/sess_test", ""))
	if rec404.Code != http.StatusNotFound || !strings.Contains(rec404.Body.String(), "SESSION_NOT_FOUND") {
		t.Errorf("GET after delete = %d %s, want 404 SESSION_NOT_FOUND", rec404.Code, rec404.Body)
	}
}

func TestPromptSSEAndJSON(t *testing.T) {
	svc := newFakeACPService()
	mux := newTestMux(svc)

	// Create session first.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authReq(http.MethodPost, "/v1/sessions", `{"cwd":"/workspace"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d", rec.Code)
	}

	t.Run("buffered JSON response", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, authReq(http.MethodPost, "/v1/sessions/sess_test/prompt", `{"prompt":"hello"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST prompt JSON = %d, want 200; body = %s", rec.Code, rec.Body)
		}
		var res acp.PromptResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if res.Text != "Hello from ACP" || res.StopReason != "end_turn" {
			t.Errorf("res = %+v, want Hello from ACP / end_turn", res)
		}
	})

	t.Run("SSE streaming response", func(t *testing.T) {
		req := authReq(http.MethodPost, "/v1/sessions/sess_test/prompt", `{"prompt":"hello"}`)
		req.Header.Set("Accept", "text/event-stream")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST prompt SSE = %d, want 200; body = %s", rec.Code, rec.Body)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "event: session_update\n") || !strings.Contains(body, "event: prompt_done\n") {
			t.Errorf("SSE body missing expected events:\n%s", body)
		}
	})

	t.Run("empty prompt returns 400 FIELD_REQUIRED with BadRequest", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, authReq(http.MethodPost, "/v1/sessions/sess_test/prompt", `{"prompt":""}`))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "FIELD_REQUIRED") {
			t.Errorf("empty prompt = %d %s, want 400 FIELD_REQUIRED", rec.Code, rec.Body)
		}
	})

	t.Run("auth required error maps to 401 ACP_AUTH_REQUIRED", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, authReq(http.MethodPost, "/v1/sessions/sess_test/prompt", `{"prompt":"auth_error"}`))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "ACP_AUTH_REQUIRED") {
			t.Errorf("auth_error = %d %s, want 401 ACP_AUTH_REQUIRED", rec.Code, rec.Body)
		}
	})
}

func TestCancelEndpoint(t *testing.T) {
	svc := newFakeACPService()
	mux := newTestMux(svc)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authReq(http.MethodPost, "/v1/sessions", `{"cwd":"/workspace"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d", rec.Code)
	}

	// Start blocking prompt in background to test 409 PROMPT_IN_FLIGHT and /cancel.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRecorder()
		mux.ServeHTTP(r, authReq(http.MethodPost, "/v1/sessions/sess_test/prompt", `{"prompt":"block_until_cancel"}`))
		done <- r
	}()

	// Wait until session is in-flight.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		_, active := svc.inFlight["sess_test"]
		svc.mu.Unlock()
		if active {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Concurrent prompt must return 409 PROMPT_IN_FLIGHT.
	rec409 := httptest.NewRecorder()
	mux.ServeHTTP(rec409, authReq(http.MethodPost, "/v1/sessions/sess_test/prompt", `{"prompt":"second"}`))
	if rec409.Code != http.StatusConflict || !strings.Contains(rec409.Body.String(), "PROMPT_IN_FLIGHT") {
		t.Errorf("concurrent prompt = %d %s, want 409 PROMPT_IN_FLIGHT", rec409.Code, rec409.Body)
	}

	// Cancel active prompt.
	recCancel := httptest.NewRecorder()
	mux.ServeHTTP(recCancel, authReq(http.MethodPost, "/v1/sessions/sess_test/cancel", ""))
	if recCancel.Code != http.StatusOK || !strings.Contains(recCancel.Body.String(), "CANCEL_REQUESTED") {
		t.Errorf("POST /cancel = %d %s, want 200 CANCEL_REQUESTED", recCancel.Code, recCancel.Body)
	}

	firstRec := <-done
	if firstRec.Code != http.StatusOK {
		t.Errorf("cancelled prompt status = %d, want 200", firstRec.Code)
	}

	// Cancel unknown session returns 404.
	rec404 := httptest.NewRecorder()
	mux.ServeHTTP(rec404, authReq(http.MethodPost, "/v1/sessions/unknown/cancel", ""))
	if rec404.Code != http.StatusNotFound {
		t.Errorf("cancel unknown = %d, want 404", rec404.Code)
	}
}
