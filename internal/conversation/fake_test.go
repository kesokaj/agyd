package conversation

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"agyd/internal/acp"
	"agyd/internal/config"
)

// fakeAgent is an in-memory Agent that behaves like agy 1.5.11: resume resets
// settings to defaults, unknown values are -32602 (ErrInvalidArgument), and
// session/list covers every conversation, live or not.
type fakeAgent struct {
	mu        sync.Mutex
	live      map[string]acp.SessionInfo
	history   map[string]acp.AgentSession
	busy      map[string]bool
	newCount  int
	setCalls  int
	listErr   error
	seq       int
	closeErrs map[string]error
	// afterGet, when set, runs once after the next GetSession returns, outside
	// the lock: a concurrent change landing between a caller's read and its
	// next step.
	afterGet func(id string)
}

var fakeChoices = map[string][]string{
	acp.ConfigModel:    {"gemini-4-carbon", "claude-sonnet-5-5"},
	acp.ConfigThinking: {"low", "medium", "high"},
	acp.ConfigMode:     {"default", "plan"},
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{
		live: map[string]acp.SessionInfo{},
		history: map[string]acp.AgentSession{
			"h1": {SessionID: "h1", Cwd: "/hist/one", Title: "one", UpdateTime: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
			"h2": {SessionID: "h2", Cwd: "/hist/two", Title: "two", UpdateTime: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)},
		},
		busy:      map[string]bool{},
		closeErrs: map[string]error{},
	}
}

func defaultOptions() []acp.ConfigOption {
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

func (f *fakeAgent) NewSession(_ context.Context, cwd string) (acp.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.newCount++
	id := fmt.Sprintf("new_%d", f.seq)
	info := acp.SessionInfo{SessionID: id, Cwd: cwd, CreateTime: time.Date(2026, 11, 18, 0, 0, f.seq, 0, time.UTC), ConfigOptions: defaultOptions()}
	f.live[id] = info
	f.history[id] = acp.AgentSession{SessionID: id, Cwd: cwd, UpdateTime: info.CreateTime}
	return info, nil
}

func (f *fakeAgent) GetSession(id string) (acp.SessionInfo, error) {
	f.mu.Lock()
	info, ok := f.live[id]
	hook := f.afterGet
	f.afterGet = nil
	f.mu.Unlock()
	if hook != nil {
		defer hook(id)
	}
	if !ok {
		return acp.SessionInfo{}, acp.ErrSessionNotFound
	}
	return info, nil
}

// setLive changes a live conversation's setting as another client would.
func (f *fakeAgent) setLive(id, configID, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.live[id]
	opts := slices.Clone(info.ConfigOptions)
	for i := range opts {
		if opts[i].ID == configID {
			opts[i].CurrentValue = value
		}
	}
	info.ConfigOptions = opts
	f.live[id] = info
}

func (f *fakeAgent) liveSetting(id, configID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[id].Setting(configID)
}

func (f *fakeAgent) ListSessions() []acp.SessionInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []acp.SessionInfo
	for _, s := range f.live {
		out = append(out, s)
	}
	return out
}

func (f *fakeAgent) CloseSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.live[id]; !ok {
		return acp.ErrSessionNotFound
	}
	if f.busy[id] {
		return acp.ErrPromptInFlight
	}
	delete(f.live, id)
	return nil
}

func (f *fakeAgent) SetConfigOption(_ context.Context, id, configID, value string) (acp.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	info, ok := f.live[id]
	if !ok {
		return acp.SessionInfo{}, acp.ErrSessionNotFound
	}
	if f.busy[id] {
		return acp.SessionInfo{}, acp.ErrPromptInFlight
	}
	if !slices.Contains(fakeChoices[configID], value) {
		return acp.SessionInfo{}, fmt.Errorf("%w: Unknown model ID: %s", acp.ErrInvalidArgument, value)
	}
	opts := slices.Clone(info.ConfigOptions)
	for i := range opts {
		if opts[i].ID == configID {
			opts[i].CurrentValue = value
		}
	}
	info.ConfigOptions = opts
	f.live[id] = info
	return info, nil
}

func (f *fakeAgent) ListAgentSessions(context.Context) ([]acp.AgentSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []acp.AgentSession
	for _, s := range f.history {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeAgent) ResumeSession(_ context.Context, id, cwd string) (acp.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if info, ok := f.live[id]; ok {
		return info, nil
	}
	if _, ok := f.history[id]; !ok {
		return acp.SessionInfo{}, acp.ErrSessionNotFound
	}
	info := acp.SessionInfo{SessionID: id, Cwd: cwd, CreateTime: time.Now().UTC(), ConfigOptions: defaultOptions()}
	f.live[id] = info
	return info, nil
}

// restart simulates agy exiting: every live session is gone.
func (f *fakeAgent) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live = map[string]acp.SessionInfo{}
}

// memState is an in-memory StateStore.
type memState struct {
	mu      sync.Mutex
	st      config.State
	loadErr error
	saveErr error
	onSave  func(config.State)
	saves   int
}

func (m *memState) Load() (config.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneState(m.st), m.loadErr
}

func (m *memState) Save(st config.State) error {
	if m.onSave != nil {
		m.onSave(st)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	m.st = cloneState(st)
	m.saves++
	return nil
}

func (m *memState) get() config.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneState(m.st)
}

func cloneState(st config.State) config.State {
	if st.Conversations != nil {
		cp := make(map[string]config.ConversationRecord, len(st.Conversations))
		for k, v := range st.Conversations {
			cp[k] = v
		}
		st.Conversations = cp
	}
	return st
}
