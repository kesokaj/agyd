package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agyd/internal/acp"
	"agyd/internal/config"
	"agyd/internal/conversation"
)

// fakeACPService is an in-memory acp.Service that behaves like agy 1.5.11
// as the real Client wraps it: a value agy does not offer is
// ErrInvalidArgument (-32602), a set-config or close during a turn is
// ErrPromptInFlight, resume resets settings, and session/list covers every
// conversation, live or not.
type fakeACPService struct {
	mu       sync.Mutex
	sessions map[string]acp.SessionInfo
	history  map[string]acp.AgentSession
	inFlight map[string]chan struct{}
	prompted []string // session ids, in prompt order
	seq      int
	failRPC  error
}

var fakeChoices = map[string][]string{
	acp.ConfigModel:    {"gemini-4-carbon", "claude-sonnet-5-5"},
	acp.ConfigThinking: {"low", "medium", "high"},
	acp.ConfigMode:     {"default", "accept-edits", "plan"},
}

func newFakeACPService() *fakeACPService {
	return &fakeACPService{
		sessions: map[string]acp.SessionInfo{},
		history: map[string]acp.AgentSession{
			"hist_1": {SessionID: "hist_1", Cwd: "/hist/one", Title: "old one", UpdateTime: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		},
		inFlight: map[string]chan struct{}{},
	}
}

func fakeOptions() []acp.ConfigOption {
	var out []acp.ConfigOption
	for _, id := range []string{acp.ConfigModel, acp.ConfigThinking, acp.ConfigMode} {
		o := acp.ConfigOption{ID: id, Type: "select", CurrentValue: fakeChoices[id][0]}
		if id == acp.ConfigThinking {
			o.CurrentValue = "medium"
		}
		for _, v := range fakeChoices[id] {
			o.Options = append(o.Options, acp.ConfigValue{Value: v, Name: v})
		}
		out = append(out, o)
	}
	return out
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
		AgentInfo:          acp.AgentInfo{Name: "antigravity-cli", Title: "Antigravity CLI", Version: "1.5.11"},
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
	if !acp.IsValidCwd(cwd) {
		return acp.SessionInfo{}, acp.ErrInvalidConfig
	}
	f.seq++
	id := fmt.Sprintf("conv_%d", f.seq)
	info := acp.SessionInfo{
		SessionID:     id,
		Cwd:           cwd,
		Backend:       "agy",
		CreateTime:    time.Date(2026, 11, 18, 10, 0, f.seq, 0, time.UTC),
		ConfigOptions: fakeOptions(),
	}
	f.sessions[id] = info
	f.history[id] = acp.AgentSession{SessionID: id, Cwd: cwd, UpdateTime: info.CreateTime}
	return info, nil
}

func (f *fakeACPService) GetSession(id string) (acp.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
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

func (f *fakeACPService) CloseSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[id]; !ok {
		return acp.ErrSessionNotFound
	}
	if _, busy := f.inFlight[id]; busy {
		return acp.ErrPromptInFlight
	}
	delete(f.sessions, id)
	return nil
}

func (f *fakeACPService) Prompt(ctx context.Context, id string, blocks []acp.ContentBlock, onUpdate func(rawUpdate []byte)) (acp.PromptResult, error) {
	f.mu.Lock()
	if _, ok := f.sessions[id]; !ok {
		f.mu.Unlock()
		return acp.PromptResult{}, acp.ErrSessionNotFound
	}
	if _, busy := f.inFlight[id]; busy {
		f.mu.Unlock()
		return acp.PromptResult{}, acp.ErrPromptInFlight
	}
	ch := make(chan struct{})
	f.inFlight[id] = ch
	f.prompted = append(f.prompted, id)
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.inFlight, id)
		f.mu.Unlock()
	}()

	text := ""
	if len(blocks) > 0 {
		text = blocks[0].Text
	}
	switch text {
	case "auth_error":
		return acp.PromptResult{}, &acp.RPCError{Code: -32000, Message: "authentication required: run agy acp --login"}
	case "block_until_cancel":
		select {
		case <-ch:
			return acp.PromptResult{SessionID: id, StopReason: "cancelled"}, nil
		case <-ctx.Done():
			return acp.PromptResult{}, ctx.Err()
		}
	}
	upd := `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello from ACP"}}`
	if onUpdate != nil {
		onUpdate([]byte(`{"sessionId":"` + id + `","update":` + upd + `}`))
	}
	return acp.PromptResult{SessionID: id, StopReason: "end_turn", Text: "Hello from ACP", Updates: []json.RawMessage{json.RawMessage(upd)}}, nil
}

func (f *fakeACPService) Cancel(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[id]; !ok {
		return acp.ErrSessionNotFound
	}
	if ch, ok := f.inFlight[id]; ok {
		close(ch)
		delete(f.inFlight, id)
	}
	return nil
}

func (f *fakeACPService) Close() error { return nil }

func (f *fakeACPService) SetConfigOption(_ context.Context, id, configID, value string) (acp.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.sessions[id]
	if !ok {
		return acp.SessionInfo{}, acp.ErrSessionNotFound
	}
	if _, busy := f.inFlight[id]; busy {
		return acp.SessionInfo{}, acp.ErrPromptInFlight
	}
	if !slices.Contains(fakeChoices[configID], value) {
		return acp.SessionInfo{}, fmt.Errorf("%w: %w", acp.ErrInvalidArgument, &acp.RPCError{Code: -32602, Message: "Unknown model ID: " + value})
	}
	opts := slices.Clone(info.ConfigOptions)
	for i := range opts {
		if opts[i].ID == configID {
			opts[i].CurrentValue = value
		}
	}
	info.ConfigOptions = opts
	f.sessions[id] = info
	return info, nil
}

func (f *fakeACPService) ListAgentSessions(context.Context) ([]acp.AgentSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]acp.AgentSession, 0, len(f.history))
	for _, s := range f.history {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeACPService) ResumeSession(_ context.Context, id, cwd string) (acp.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if info, ok := f.sessions[id]; ok {
		return info, nil
	}
	if _, ok := f.history[id]; !ok {
		return acp.SessionInfo{}, &acp.RPCError{Code: -32603, Message: "Failed to read session"}
	}
	info := acp.SessionInfo{SessionID: id, Cwd: cwd, Backend: "agy", CreateTime: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), ConfigOptions: fakeOptions()}
	f.sessions[id] = info
	return info, nil
}

// waitInFlight blocks until a turn runs on id.
func (f *fakeACPService) waitInFlight(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		_, active := f.inFlight[id]
		f.mu.Unlock()
		if active {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no turn in flight on %s", id)
}

// testEnv is a handler over a real conversation.Manager on the fake agent.
type testEnv struct {
	h         http.Handler
	svc       *fakeACPService
	mgr       *conversation.Manager
	stateFile string // path PUT /v1/defaults writes
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	svc := newFakeACPService()
	log := discardLog()
	mgr := conversation.New(conversation.Options{
		Agent:     svc,
		State:     config.NewStateStore(dir),
		Log:       log,
		Workspace: "/workspace",
	})
	return &testEnv{h: NewHandler(log, svc, mgr, Config{DefaultWorkspace: "/workspace"}), svc: svc, mgr: mgr, stateFile: filepath.Join(dir, "state.json")}
}

// do serves one request; body "" sends none.
func (e *testEnv) do(method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
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

// want fails unless rec has status code and, when reason is set, that
// ErrorInfo reason in its body.
func want(t *testing.T, rec *httptest.ResponseRecorder, code int, reason string) {
	t.Helper()
	if rec.Code != code || (reason != "" && !strings.Contains(rec.Body.String(), `"reason":"`+reason+`"`)) {
		t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body, code, reason)
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

func discardLog() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }
