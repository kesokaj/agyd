// Package conversation owns the current-conversation pointer, the default
// settings, per-conversation remembered settings and the state file (D10).
// Every pointer change goes through one Manager so concurrent requests see a
// consistent current conversation.
package conversation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"agyd/internal/acp"
	"agyd/internal/config"
)

// Agent is the subset of the ACP client the Manager drives.
type Agent interface {
	NewSession(ctx context.Context, cwd string) (acp.SessionInfo, error)
	GetSession(sessionID string) (acp.SessionInfo, error)
	ListSessions() []acp.SessionInfo
	CloseSession(ctx context.Context, sessionID string) error
	SetConfigOption(ctx context.Context, sessionID, configID, value string) (acp.SessionInfo, error)
	ListAgentSessions(ctx context.Context) ([]acp.AgentSession, error)
	ResumeSession(ctx context.Context, sessionID, cwd string) (acp.SessionInfo, error)
}

// StateStore persists state.json across restarts.
type StateStore interface {
	Load() (config.State, error)
	Save(config.State) error
}

// Options configure a Manager.
type Options struct {
	Agent     Agent
	State     StateStore
	Log       *slog.Logger
	Workspace string   // cwd for conversations created without one
	Defaults  Settings // startup defaults from flags/env (override saved defaults for this run)
}

// memo is what agyd remembers about a conversation to re-apply after resume.
type memo struct {
	cwd      string
	settings Settings
}

// Manager is safe for concurrent use.
type Manager struct {
	agent           Agent
	state           StateStore
	log             *slog.Logger
	workspace       string
	startupDefaults Settings

	opMu   sync.Mutex // serializes pointer and settings changes (create, use, resume, update, delete)
	saveMu sync.Mutex // serializes state.json snapshots + writes (lock order: opMu -> saveMu -> mu)

	mu            sync.Mutex // guards the fields below
	current       string
	defaults      Settings
	savedDefaults Settings
	memos         map[string]memo
	options       []acp.ConfigOption
}

// CreateRequest describes a new conversation. Settings given here are strict:
// a value agy rejects fails the request.
type CreateRequest struct {
	Cwd      string
	Settings Settings
	NoUse    bool // keep the current pointer where it is
}

// New returns a Manager with no current conversation; call Startup.
func New(o Options) *Manager {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Manager{
		agent: o.Agent, state: o.State, log: o.Log, workspace: o.Workspace,
		startupDefaults: o.Defaults, defaults: o.Defaults, memos: map[string]memo{},
	}
}

// CurrentID returns the current conversation id, or "".
func (m *Manager) CurrentID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Startup loads defaults and conversations from state.json, then resumes the
// current conversation or creates a fresh one. fresh reports which. Failures
// to read state or resume are logged and fall back to a fresh conversation.
func (m *Manager) Startup(ctx context.Context) (info acp.SessionInfo, fresh bool, err error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	var st config.State
	if m.state != nil {
		var lerr error
		st, lerr = m.state.Load()
		if lerr != nil {
			m.log.Warn("state file unreadable; starting a fresh conversation", "err", lerr)
		}
	}
	m.loadState(st)
	if id := st.CurrentConversationID; id != "" {
		info, err := m.use(ctx, id)
		if err == nil {
			return info, false, nil
		}
		m.log.Warn("could not resume the last conversation; starting a fresh one", "conversationId", id, "err", err)
	}
	info, err = m.create(ctx, CreateRequest{})
	return info, err == nil, err
}

// Resolve returns the conversation a request addresses: id when given (live
// conversations only), else the current one. An empty pointer creates a
// conversation; a current conversation that is no longer live (agy
// restarted) is resumed, or replaced by a fresh one if agy lost it.
func (m *Manager) Resolve(ctx context.Context, id string) (acp.SessionInfo, error) {
	if id != "" {
		return m.agent.GetSession(id)
	}
	if info, err := m.agent.GetSession(m.CurrentID()); err == nil {
		return info, nil
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	cur := m.CurrentID()
	if cur == "" {
		return m.create(ctx, CreateRequest{})
	}
	info, err := m.use(ctx, cur)
	if errors.Is(err, acp.ErrSessionNotFound) {
		m.log.Warn("current conversation is gone; starting a fresh one", "conversationId", cur)
		return m.create(ctx, CreateRequest{})
	}
	return info, err
}

// Current returns the current conversation, resuming it if it is not live.
// With no current conversation it returns ErrNoCurrent.
func (m *Manager) Current(ctx context.Context) (acp.SessionInfo, error) {
	cur := m.CurrentID()
	if cur == "" {
		return acp.SessionInfo{}, ErrNoCurrent
	}
	if info, err := m.agent.GetSession(cur); err == nil {
		return info, nil
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.CurrentID() == "" {
		return acp.SessionInfo{}, ErrNoCurrent
	}
	return m.use(ctx, m.CurrentID())
}

// Create starts a conversation with the defaults overlaid by req.Settings and
// makes it current unless req.NoUse.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (acp.SessionInfo, error) {
	if err := req.Settings.validate(); err != nil {
		return acp.SessionInfo{}, err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.create(ctx, req)
}

func (m *Manager) create(ctx context.Context, req CreateRequest) (acp.SessionInfo, error) {
	cwd := req.Cwd
	if cwd == "" {
		cwd = m.workspace
	}
	info, err := m.agent.NewSession(ctx, cwd)
	if err != nil {
		return acp.SessionInfo{}, err
	}
	info = m.applyLenient(ctx, info, m.Defaults())
	if info, err = m.applyStrict(ctx, info, req.Settings); err != nil {
		if cerr := m.agent.CloseSession(context.WithoutCancel(ctx), info.SessionID); cerr != nil {
			m.log.Warn("closing the conversation a failed create left behind", "conversationId", info.SessionID, "err", cerr)
		}
		return acp.SessionInfo{}, err
	}
	m.remember(info.SessionID, memo{cwd: info.Cwd, settings: SettingsOf(info)})
	if !req.NoUse {
		m.setCurrent(info.SessionID)
	} else {
		m.persistState()
	}
	return info, nil
}

// Use makes id current, resuming it (and re-applying its remembered settings,
// else the defaults) when it is not live.
func (m *Manager) Use(ctx context.Context, id string) (acp.SessionInfo, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.use(ctx, id)
}

func (m *Manager) use(ctx context.Context, id string) (acp.SessionInfo, error) {
	if info, err := m.agent.GetSession(id); err == nil {
		m.noteOptions(info.ConfigOptions)
		m.remember(id, memo{cwd: info.Cwd, settings: SettingsOf(info)})
		m.setCurrent(id)
		return info, nil
	}
	mem, known := m.recall(id)
	if mem.cwd == "" {
		cwd, err := m.historyCwd(ctx, id)
		if err != nil {
			return acp.SessionInfo{}, err
		}
		mem.cwd = cwd
	}
	info, err := m.agent.ResumeSession(ctx, id, mem.cwd)
	if err != nil {
		if errors.Is(err, acp.ErrSessionNotFound) && m.forget(id) {
			m.persistState()
		}
		return acp.SessionInfo{}, err
	}
	want := m.Defaults()
	if known {
		want = want.overlay(mem.settings)
	}
	info = m.applyLenient(ctx, info, want)
	m.remember(id, memo{cwd: info.Cwd, settings: SettingsOf(info)})
	m.setCurrent(id)
	return info, nil
}

// historyCwd finds id's working directory in agy's session/list.
func (m *Manager) historyCwd(ctx context.Context, id string) (string, error) {
	list, err := m.agent.ListAgentSessions(ctx)
	if err != nil {
		return "", err
	}
	for _, s := range list {
		if s.SessionID == id {
			return s.Cwd, nil
		}
	}
	return "", fmt.Errorf("%w: %s", acp.ErrSessionNotFound, id)
}

// Update applies s to conversation id ("" = current). Values agy rejects
// fail the request; a conversation in a turn is ErrPromptInFlight.
func (m *Manager) Update(ctx context.Context, id string, s Settings) (acp.SessionInfo, error) {
	if s.IsZero() {
		return acp.SessionInfo{}, fmt.Errorf("%w: give at least one of model, thinkingLevel, mode", ErrInvalidSetting)
	}
	if err := s.validate(); err != nil {
		return acp.SessionInfo{}, err
	}
	var info acp.SessionInfo
	var err error
	if id == "" {
		info, err = m.Current(ctx)
	} else {
		info, err = m.agent.GetSession(id)
	}
	if err != nil {
		return acp.SessionInfo{}, err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	// Re-read under opMu: a change that landed since the first read must not
	// be skipped as "already set".
	if info, err = m.agent.GetSession(info.SessionID); err != nil {
		return acp.SessionInfo{}, err
	}
	info, err = m.applyStrict(ctx, info, s)
	if err != nil {
		return acp.SessionInfo{}, err
	}
	m.remember(info.SessionID, memo{cwd: info.Cwd, settings: SettingsOf(info)})
	m.persistState()
	return info, nil
}

// Delete closes a live conversation. Deleting the current one clears the
// pointer; the next prompt creates a new conversation. The current one is
// cleared even when agy no longer has it live (after an agy restart), so the
// pointer never dangles.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.agent.CloseSession(ctx, id); err != nil {
		if !errors.Is(err, acp.ErrSessionNotFound) || id == "" || m.CurrentID() != id {
			return err
		}
	}
	m.mu.Lock()
	_, hadMemo := m.memos[id]
	delete(m.memos, id)
	cleared := m.current == id
	if cleared {
		m.current = ""
	}
	m.mu.Unlock()
	if cleared || hadMemo {
		m.persistState()
	}
	return nil
}
