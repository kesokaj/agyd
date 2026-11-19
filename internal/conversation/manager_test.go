package conversation

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/kesokaj/agyd/internal/acp"
	"github.com/kesokaj/agyd/internal/config"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	m     *Manager
	agent *fakeAgent
	state *memState
	logs  *syncBuffer
}

func newHarness(t *testing.T, defaults Settings) *harness {
	t.Helper()
	h := &harness{agent: newFakeAgent(), state: &memState{}, logs: &syncBuffer{}}
	h.m = New(Options{
		Agent:     h.agent,
		State:     h.state,
		Log:       slog.New(slog.NewTextHandler(h.logs, nil)),
		Workspace: "/ws",
		Defaults:  defaults,
	})
	return h
}

func TestStartupCreatesAndSavesState(t *testing.T) {
	h := newHarness(t, Settings{Model: "claude-sonnet-5-5"})
	info, fresh, err := h.m.Startup(t.Context())
	if err != nil || !fresh {
		t.Fatalf("Startup = %+v, fresh=%v, err=%v; want a fresh conversation", info, fresh, err)
	}
	if info.Cwd != "/ws" || info.Setting(acp.ConfigModel) != "claude-sonnet-5-5" {
		t.Errorf("startup conversation = %+v; want cwd /ws and the default model applied", info)
	}
	st := h.state.get()
	rec := st.Conversations[info.SessionID]
	if st.CurrentConversationID != info.SessionID || rec.Cwd != "/ws" || rec.Model != "claude-sonnet-5-5" || rec.ThinkingLevel != "medium" {
		t.Errorf("state = %+v", st)
	}
	if st.Defaults != (config.Settings{}) {
		t.Errorf("startup flag/env defaults leaked into persisted state.Defaults: %+v", st.Defaults)
	}
	if h.m.CurrentID() != info.SessionID {
		t.Errorf("CurrentID = %q", h.m.CurrentID())
	}
}

func TestStartupResumesFromStateAndReappliesSettings(t *testing.T) {
	h := newHarness(t, Settings{ThinkingLevel: "low"})
	h.state.st = config.State{
		CurrentConversationID: "h1",
		Defaults:              config.Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "high"},
		Conversations: map[string]config.ConversationRecord{
			"h1":    {Cwd: "/hist/override", ThinkingLevel: "high", Mode: "plan"},
			"unlit": {Cwd: "/unprompted/dir", Mode: "plan"},
		},
	}
	info, fresh, err := h.m.Startup(t.Context())
	if err != nil || fresh || info.SessionID != "h1" {
		t.Fatalf("Startup = %+v, fresh=%v, err=%v; want h1 resumed", info, fresh, err)
	}
	if info.Cwd != "/hist/override" || info.Setting(acp.ConfigModel) != "claude-sonnet-5-5" ||
		info.Setting(acp.ConfigThinking) != "high" || info.Setting(acp.ConfigMode) != "plan" {
		t.Errorf("resumed info = %+v; want /hist/override, saved default model, and remembered high/plan", info)
	}
	// Startup flag/env ThinkingLevel=low overrides saved high for running defaults without clobbering state.json.
	if got := h.m.Defaults(); got != (Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "low"}) {
		t.Errorf("running Defaults = %+v, want saved model + startup thinking override", got)
	}
	st := h.state.get()
	if st.Defaults != (config.Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "high"}) {
		t.Errorf("saved Defaults clobbered = %+v", st.Defaults)
	}
	if st.Conversations["unlit"].Cwd != "/unprompted/dir" {
		t.Errorf("non-current conversation lost from state.json: %+v", st.Conversations)
	}
	list, err := h.m.List(t.Context(), "/unprompted/dir")
	if err != nil || len(list) != 1 || list[0].ConversationID != "unlit" {
		t.Errorf("List(/unprompted/dir) = %+v, %v; want remembered unprompted conversation", list, err)
	}
}

func TestStartupFallsBackToFresh(t *testing.T) {
	for name, setup := range map[string]func(*harness){
		"missing conversation": func(h *harness) {
			h.state.st = config.State{
				CurrentConversationID: "gone",
				Conversations:         map[string]config.ConversationRecord{"gone": {Cwd: "/x"}},
			}
		},
		"corrupt state": func(h *harness) { h.state.loadErr = errors.New("state: corrupt") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Settings{})
			setup(h)
			info, fresh, err := h.m.Startup(t.Context())
			if err != nil || !fresh || !strings.HasPrefix(info.SessionID, "new_") {
				t.Fatalf("Startup = %+v, fresh=%v, err=%v; want a fresh conversation", info, fresh, err)
			}
			if _, stillThere := h.state.get().Conversations["gone"]; stillThere {
				t.Errorf("stale conversation 'gone' kept in state.json: %+v", h.state.get())
			}
			if !strings.Contains(h.logs.String(), "level=WARN") {
				t.Errorf("no warning logged:\n%s", h.logs)
			}
		})
	}
}

func TestStartupLogsRejectedDefault(t *testing.T) {
	h := newHarness(t, Settings{Model: "retired-model", ThinkingLevel: "high"})
	info, _, err := h.m.Startup(t.Context())
	if err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if info.Setting(acp.ConfigModel) != "gemini-4-carbon" || info.Setting(acp.ConfigThinking) != "high" {
		t.Errorf("settings = %q/%q; want agy's model kept and the valid thinking level applied", info.Setting(acp.ConfigModel), info.Setting(acp.ConfigThinking))
	}
	if !strings.Contains(h.logs.String(), "retired-model") {
		t.Errorf("rejected default not logged:\n%s", h.logs)
	}
}

func TestResolveConcurrentEmptyPointerCreatesOne(t *testing.T) {
	h := newHarness(t, Settings{})
	ids := make([]string, 20)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			info, err := h.m.Resolve(t.Context(), "")
			if err != nil {
				t.Errorf("Resolve: %v", err)
			}
			ids[i] = info.SessionID
		})
	}
	wg.Wait()
	if h.agent.newCount != 1 {
		t.Fatalf("created %d conversations, want 1", h.agent.newCount)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("resolved ids differ: %v", ids)
		}
	}
}

func TestResolveResumesCurrentAfterAgentRestart(t *testing.T) {
	h := newHarness(t, Settings{})
	info, _, _ := h.m.Startup(t.Context())
	if _, err := h.m.Update(t.Context(), "", Settings{ThinkingLevel: "high"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	h.agent.restart()
	got, err := h.m.Resolve(t.Context(), "")
	if err != nil || got.SessionID != info.SessionID {
		t.Fatalf("Resolve after restart = %+v, %v; want %s resumed", got, err, info.SessionID)
	}
	if got.Setting(acp.ConfigThinking) != "high" {
		t.Errorf("thinking after resume = %q, want the remembered high", got.Setting(acp.ConfigThinking))
	}
	if h.agent.newCount != 1 {
		t.Errorf("newCount = %d, want 1 (resume, not create)", h.agent.newCount)
	}
}

func TestResolveExplicitIDIsLiveOnly(t *testing.T) {
	h := newHarness(t, Settings{})
	if _, err := h.m.Resolve(t.Context(), "h1"); !errors.Is(err, acp.ErrSessionNotFound) {
		t.Errorf("Resolve(h1 not live) err = %v, want ErrSessionNotFound", err)
	}
}

func TestUseSwitchesAndResumes(t *testing.T) {
	h := newHarness(t, Settings{Mode: "plan"})
	first, _, _ := h.m.Startup(t.Context())
	got, err := h.m.Use(t.Context(), "h2")
	if err != nil || got.SessionID != "h2" || got.Cwd != "/hist/two" {
		t.Fatalf("Use(h2) = %+v, %v", got, err)
	}
	if got.Setting(acp.ConfigMode) != "plan" {
		t.Errorf("resumed without remembered settings: mode = %q, want the default plan", got.Setting(acp.ConfigMode))
	}
	if r, _ := h.m.Resolve(t.Context(), ""); r.SessionID != "h2" {
		t.Errorf("Resolve after use = %q, want h2", r.SessionID)
	}
	if st := h.state.get(); st.CurrentConversationID != "h2" || st.Conversations["h2"].Cwd != "/hist/two" {
		t.Errorf("state = %+v", st)
	}
	if _, err := h.m.Use(t.Context(), "nope"); !errors.Is(err, acp.ErrSessionNotFound) {
		t.Errorf("Use(nope) err = %v, want ErrSessionNotFound", err)
	}
	if _, err := h.m.Use(t.Context(), first.SessionID); err != nil || h.m.CurrentID() != first.SessionID {
		t.Errorf("Use(live) = %v, current %q", err, h.m.CurrentID())
	}
}

func TestCreateSettingsAreStrict(t *testing.T) {
	h := newHarness(t, Settings{})
	first, _, _ := h.m.Startup(t.Context())
	_, err := h.m.Create(t.Context(), CreateRequest{Settings: Settings{Model: "bogus"}})
	if !errors.Is(err, acp.ErrInvalidArgument) {
		t.Fatalf("Create(bogus) err = %v, want ErrInvalidArgument", err)
	}
	if len(h.agent.ListSessions()) != 1 || h.m.CurrentID() != first.SessionID {
		t.Errorf("failed create left %d live sessions, current %q", len(h.agent.ListSessions()), h.m.CurrentID())
	}
	if _, err := h.m.Create(t.Context(), CreateRequest{Settings: Settings{Model: "a b"}}); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("Create(a b) err = %v, want ErrInvalidSetting", err)
	}
	other, err := h.m.Create(t.Context(), CreateRequest{Cwd: "/other", Settings: Settings{ThinkingLevel: "low"}, NoUse: true})
	if err != nil || other.Cwd != "/other" || other.Setting(acp.ConfigThinking) != "low" {
		t.Fatalf("Create NoUse = %+v, %v", other, err)
	}
	if h.m.CurrentID() != first.SessionID {
		t.Errorf("NoUse create moved the pointer to %q", h.m.CurrentID())
	}
	st := h.state.get()
	if st.CurrentConversationID != first.SessionID || st.Conversations[other.SessionID].Cwd != "/other" {
		t.Errorf("NoUse create did not persist conversation folder in state.json: %+v", st)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	h := newHarness(t, Settings{})
	if _, err := h.m.Update(t.Context(), "", Settings{Model: "claude-sonnet-5-5"}); !errors.Is(err, ErrNoCurrent) {
		t.Errorf("Update with no current err = %v, want ErrNoCurrent", err)
	}
	info, _, _ := h.m.Startup(t.Context())
	if _, err := h.m.Update(t.Context(), "", Settings{}); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("empty Update err = %v, want ErrInvalidSetting", err)
	}
	got, err := h.m.Update(t.Context(), "", Settings{ThinkingLevel: "high"})
	if err != nil || got.Setting(acp.ConfigThinking) != "high" || h.state.get().Conversations[info.SessionID].ThinkingLevel != "high" {
		t.Fatalf("Update = %+v, %v; state %+v", got, err, h.state.get())
	}
	h.agent.busy[info.SessionID] = true
	if _, err := h.m.Update(t.Context(), info.SessionID, Settings{Mode: "plan"}); !errors.Is(err, acp.ErrPromptInFlight) {
		t.Errorf("Update mid-turn err = %v, want ErrPromptInFlight", err)
	}
	if err := h.m.Delete(t.Context(), info.SessionID); !errors.Is(err, acp.ErrPromptInFlight) {
		t.Errorf("Delete mid-turn err = %v, want ErrPromptInFlight", err)
	}
	h.agent.busy[info.SessionID] = false
	if err := h.m.Delete(t.Context(), info.SessionID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	st := h.state.get()
	if h.m.CurrentID() != "" || st.CurrentConversationID != "" || len(st.Conversations) != 0 {
		t.Errorf("pointer/record not cleared: %q, state %+v", h.m.CurrentID(), st)
	}
	if _, err := h.m.Current(t.Context()); !errors.Is(err, ErrNoCurrent) {
		t.Errorf("Current after delete err = %v, want ErrNoCurrent", err)
	}
	if next, err := h.m.Resolve(t.Context(), ""); err != nil || next.SessionID == info.SessionID {
		t.Errorf("Resolve after delete = %+v, %v; want a new conversation", next, err)
	}
}

func TestDefaultsAndModels(t *testing.T) {
	h := newHarness(t, Settings{Model: "gemini-4-carbon"})
	if _, err := h.m.Models(); !errors.Is(err, ErrNoModels) {
		t.Errorf("Models before any conversation err = %v, want ErrNoModels", err)
	}
	if _, err := h.m.SetDefaults(Settings{ThinkingLevel: "x=y"}, true); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("SetDefaults(x=y) err = %v, want ErrInvalidSetting", err)
	}
	if _, _, err := h.m.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	savesAfterStartup := h.state.saves
	models, err := h.m.Models()
	if err != nil || len(models) != 3 {
		t.Fatalf("Models = %+v, %v", models, err)
	}
	if _, err := h.m.SetDefaults(Settings{Model: "unknown-family"}, true); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("SetDefaults(unknown model) err = %v, want ErrInvalidSetting", err)
	}
	if _, err := h.m.SetDefaults(Settings{}, true); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("SetDefaults(empty) err = %v, want ErrInvalidSetting", err)
	}
	if h.state.saves != savesAfterStartup {
		t.Fatalf("refused defaults wrote state.json (%d -> %d)", savesAfterStartup, h.state.saves)
	}

	got, err := h.m.SetDefaults(Settings{ThinkingLevel: "high"}, true)
	if err != nil || got != (Settings{Model: "gemini-4-carbon", ThinkingLevel: "high"}) {
		t.Fatalf("SetDefaults = %+v, %v", got, err)
	}
	if st := h.state.get(); st.Defaults != (config.Settings{ThinkingLevel: "high"}) {
		t.Errorf("state.json defaults = %+v; want only persisted ThinkingLevel=high", st.Defaults)
	}

	savesBeforeNoPersist := h.state.saves
	if _, err := h.m.SetDefaults(Settings{Mode: "plan"}, false); err != nil || h.state.saves != savesBeforeNoPersist {
		t.Errorf("persist=false wrote state.json: saves=%d (want %d), err=%v", h.state.saves, savesBeforeNoPersist, err)
	}
	if h.m.Defaults().Mode != "plan" || h.state.get().Defaults.Mode != "" {
		t.Errorf("Defaults = %+v, state.Defaults = %+v", h.m.Defaults(), h.state.get().Defaults)
	}

	h.state.saveErr = errors.New("disk full")
	if _, err := h.m.SetDefaults(Settings{Model: "claude-sonnet-5-5"}, true); err == nil {
		t.Fatal("SetDefaults with saveErr returned nil")
	}
	if h.m.Defaults().Model != "gemini-4-carbon" {
		t.Errorf("failed Save mutated running Defaults: %+v", h.m.Defaults())
	}
}

func TestListMergesLiveAndHistory(t *testing.T) {
	h := newHarness(t, Settings{})
	cur, _, _ := h.m.Startup(t.Context())
	list, err := h.m.List(t.Context(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 || list[0].ConversationID != cur.SessionID || !list[0].Live || !list[0].Current {
		t.Fatalf("list = %+v; want the live current one first", list)
	}
	if list[1].ConversationID != "h2" || list[1].Live || list[1].Current || list[2].ConversationID != "h1" {
		t.Errorf("history order = %+v; want h2 then h1", list[1:])
	}
	if list[0].Model != "gemini-4-carbon" || list[1].Model != "" {
		t.Errorf("settings shown for %+v", list[:2])
	}
	only, _ := h.m.List(t.Context(), "/hist/one")
	if len(only) != 1 || only[0].ConversationID != "h1" {
		t.Errorf("cwd filter = %+v", only)
	}
	h.agent.listErr = errors.New("agy list failed")
	if live, err := h.m.List(t.Context(), ""); err != nil || len(live) != 1 {
		t.Errorf("List with a failing session/list = %+v, %v; want live conversations only", live, err)
	}
}

func TestConcurrentUseAndPrompt(t *testing.T) {
	h := newHarness(t, Settings{})
	if _, _, err := h.m.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() {
			if i%2 == 0 {
				_, _ = h.m.Use(t.Context(), "h1")
			} else if _, err := h.m.Resolve(t.Context(), ""); err != nil {
				t.Errorf("Resolve: %v", err)
			}
		})
	}
	wg.Wait()
	if h.m.CurrentID() != "h1" {
		t.Errorf("CurrentID = %q, want h1", h.m.CurrentID())
	}
}

func TestStartupNilStateAndSettingsOfSanitizes(t *testing.T) {
	m := New(Options{Agent: newFakeAgent(), Workspace: "/work"})
	info, fresh, err := m.Startup(t.Context())
	if err != nil || !fresh || info.SessionID == "" {
		t.Fatalf("Startup with nil State = %+v, fresh=%v, err=%v", info, fresh, err)
	}

	got := SettingsOf(acp.SessionInfo{ConfigOptions: []acp.ConfigOption{
		{ID: acp.ConfigModel, CurrentValue: "bad/model:1"},
		{ID: acp.ConfigThinking, CurrentValue: "high"},
		{ID: acp.ConfigMode, CurrentValue: "plan mode"},
	}})
	if got != (Settings{ThinkingLevel: "high"}) {
		t.Errorf("SettingsOf sanitized = %+v, want {ThinkingLevel:high}", got)
	}
}

func newJSONHarness(t *testing.T, defaults Settings) *harness {
	t.Helper()
	h := &harness{agent: newFakeAgent(), state: &memState{}, logs: &syncBuffer{}}
	h.m = New(Options{
		Agent:     h.agent,
		State:     h.state,
		Log:       slog.New(slog.NewJSONHandler(h.logs, nil)),
		Workspace: "/ws",
		Defaults:  defaults,
	})
	return h
}

func parseJSONLogs(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func assertLogRecord(t *testing.T, rec, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("log[%q] %s = %#v, want %#v", want["msg"], k, rec[k], v)
		}
	}
	if len(rec) != len(want)+1 { // +1 for slog's "time"
		t.Errorf("log[%q] has unexpected extra fields: %v", want["msg"], rec)
	}
}

func TestConversationLifecycleLogs(t *testing.T) {
	h := newJSONHarness(t, Settings{})
	ctx := t.Context()

	c1, err := h.m.Create(ctx, CreateRequest{Cwd: "/proj/a", Settings: Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "high"}})
	if err != nil {
		t.Fatalf("Create c1: %v", err)
	}
	c2, err := h.m.Create(ctx, CreateRequest{Cwd: "/proj/b", Settings: Settings{Mode: "plan"}, NoUse: true})
	if err != nil {
		t.Fatalf("Create c2: %v", err)
	}
	c3, err := h.m.Clear(ctx, "")
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := h.m.Use(ctx, c2.SessionID); err != nil {
		t.Fatalf("Use live: %v", err)
	}
	if _, err := h.m.Use(ctx, "h2"); err != nil {
		t.Fatalf("Use resume: %v", err)
	}
	if _, err := h.m.Update(ctx, "", Settings{ThinkingLevel: "low", Mode: "plan"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := h.m.Delete(ctx, c2.SessionID); err != nil {
		t.Fatalf("Delete non-current: %v", err)
	}
	if err := h.m.Delete(ctx, "h2"); err != nil {
		t.Fatalf("Delete current: %v", err)
	}

	recs := parseJSONLogs(t, h.logs.String())
	want := []map[string]any{
		{"level": "INFO", "msg": "conversation created", "conversationId": c1.SessionID, "cwd": "/proj/a", "model": "claude-sonnet-5-5", "thinkingLevel": "high", "mode": "default", "current": true},
		{"level": "INFO", "msg": "conversation created", "conversationId": c2.SessionID, "cwd": "/proj/b", "model": "gemini-4-carbon", "thinkingLevel": "medium", "mode": "plan", "current": false},
		{"level": "INFO", "msg": "conversation cleared", "fromConversationId": c1.SessionID, "conversationId": c3.SessionID, "cwd": "/proj/a", "model": "claude-sonnet-5-5", "thinkingLevel": "high", "mode": "default"},
		{"level": "INFO", "msg": "conversation switched", "conversationId": c2.SessionID, "cwd": "/proj/b", "resumed": false, "model": "gemini-4-carbon", "thinkingLevel": "medium", "mode": "plan"},
		{"level": "INFO", "msg": "conversation switched", "conversationId": "h2", "cwd": "/hist/two", "resumed": true, "model": "gemini-4-carbon", "thinkingLevel": "medium", "mode": "default"},
		{"level": "INFO", "msg": "conversation updated", "conversationId": "h2", "model": "gemini-4-carbon", "thinkingLevel": "low", "mode": "plan"},
		{"level": "INFO", "msg": "conversation deleted", "conversationId": c2.SessionID, "wasCurrent": false},
		{"level": "INFO", "msg": "conversation deleted", "conversationId": "h2", "wasCurrent": true},
	}
	if len(recs) != len(want) {
		t.Fatalf("got %d log records, want %d:\n%s", len(recs), len(want), h.logs.String())
	}
	for i := range want {
		assertLogRecord(t, recs[i], want[i])
	}
}

func TestDefaultsUpdatedLog(t *testing.T) {
	h := newJSONHarness(t, Settings{Model: "gemini-4-carbon"})
	if _, err := h.m.SetDefaults(Settings{Mode: "plan"}, false); err != nil {
		t.Fatalf("SetDefaults(persist=false): %v", err)
	}
	if recs := parseJSONLogs(t, h.logs.String()); len(recs) != 0 {
		t.Fatalf("persist=false emitted logs: %v", recs)
	}

	if _, err := h.m.SetDefaults(Settings{ThinkingLevel: "high"}, true); err != nil {
		t.Fatalf("SetDefaults(persist=true): %v", err)
	}
	h.state.saveErr = errors.New("disk full")
	if _, err := h.m.SetDefaults(Settings{Model: "claude-sonnet-5-5"}, true); err == nil {
		t.Fatal("SetDefaults with saveErr returned nil")
	}

	recs := parseJSONLogs(t, h.logs.String())
	if len(recs) != 1 {
		t.Fatalf("got %d logs, want 1:\n%s", len(recs), h.logs.String())
	}
	assertLogRecord(t, recs[0], map[string]any{
		"level": "INFO", "msg": "defaults updated",
		"model": "gemini-4-carbon", "thinkingLevel": "high", "mode": "plan",
	})
}
