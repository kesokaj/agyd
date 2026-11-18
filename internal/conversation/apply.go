package conversation

import (
	"context"
	"fmt"

	"agyd/internal/acp"
	"agyd/internal/config"
)

// applyStrict sets every non-empty value of s that differs from agy's current
// one. The first failure is returned with the info agy last confirmed.
func (m *Manager) applyStrict(ctx context.Context, info acp.SessionInfo, s Settings) (acp.SessionInfo, error) {
	for _, p := range s.pairs() {
		if p.value == "" || info.Setting(p.id) == p.value {
			continue
		}
		next, err := m.agent.SetConfigOption(ctx, info.SessionID, p.id, p.value)
		if err != nil {
			return info, fmt.Errorf("setting %s to %q: %w", p.id, p.value, err)
		}
		info = next
	}
	m.noteOptions(info.ConfigOptions)
	return info, nil
}

// applyLenient is applyStrict for defaults and remembered settings: a
// rejected value is logged and agy's own value kept (D13).
func (m *Manager) applyLenient(ctx context.Context, info acp.SessionInfo, s Settings) acp.SessionInfo {
	for _, p := range s.pairs() {
		if p.value == "" || info.Setting(p.id) == p.value {
			continue
		}
		next, err := m.agent.SetConfigOption(ctx, info.SessionID, p.id, p.value)
		if err != nil {
			m.log.Warn("agy rejected a default setting; keeping agy's value",
				"conversationId", info.SessionID, "setting", p.id, "value", p.value, "kept", info.Setting(p.id), "err", err)
			continue
		}
		info = next
	}
	m.noteOptions(info.ConfigOptions)
	return info
}

func (m *Manager) loadState(st config.State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.savedDefaults = Settings(st.Defaults)
	m.defaults = m.savedDefaults.overlay(m.startupDefaults)
	for id, rec := range st.Conversations {
		m.memos[id] = memo{
			cwd:      rec.Cwd,
			settings: Settings{Model: rec.Model, ThinkingLevel: rec.ThinkingLevel, Mode: rec.Mode},
		}
	}
}

func (m *Manager) remember(id string, mem memo) {
	m.mu.Lock()
	m.memos[id] = mem
	m.mu.Unlock()
}

func (m *Manager) forget(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.memos[id]
	delete(m.memos, id)
	return ok
}

func (m *Manager) recall(id string) (memo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, ok := m.memos[id]
	return mem, ok
}

func (m *Manager) memosSnapshot() map[string]memo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]memo, len(m.memos))
	for k, v := range m.memos {
		out[k] = v
	}
	return out
}

// setCurrent moves the pointer to id and writes state.json. Callers hold opMu,
// so pointer updates happen in order.
func (m *Manager) setCurrent(id string) {
	m.mu.Lock()
	m.current = id
	m.mu.Unlock()
	m.persistState()
}

// persistState writes a full state.json snapshot under saveMu. Conversation
// lifecycle operations never fail on a state write error; they log a warning.
func (m *Manager) persistState() {
	if m.state == nil {
		return
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.Lock()
	st := m.snapshotStateLocked()
	m.mu.Unlock()
	if err := m.state.Save(st); err != nil {
		m.log.Warn("could not write the state file; conversation state will not survive a restart", "err", err)
	}
}

func (m *Manager) snapshotStateLocked() config.State {
	var convs map[string]config.ConversationRecord
	if len(m.memos) > 0 {
		convs = make(map[string]config.ConversationRecord, len(m.memos))
		for id, mem := range m.memos {
			convs[id] = config.ConversationRecord{
				Cwd:           mem.cwd,
				Model:         mem.settings.Model,
				ThinkingLevel: mem.settings.ThinkingLevel,
				Mode:          mem.settings.Mode,
			}
		}
	}
	return config.State{
		CurrentConversationID: m.current,
		Defaults:              config.Settings(m.savedDefaults),
		Conversations:         convs,
	}
}
