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

func (m *Manager) remember(id string, mem memo) {
	m.mu.Lock()
	m.memos[id] = mem
	m.mu.Unlock()
}

func (m *Manager) recall(id string) (memo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, ok := m.memos[id]
	return mem, ok
}

// setCurrent moves the pointer to id and writes the state file. Callers hold
// opMu, so state writes happen in pointer order.
func (m *Manager) setCurrent(id string) {
	m.mu.Lock()
	m.current = id
	mem := m.memos[id]
	m.mu.Unlock()
	m.saveState(config.ConversationState{
		CurrentConversationID: id,
		Cwd:                   mem.cwd,
		Model:                 mem.settings.Model,
		ThinkingLevel:         mem.settings.ThinkingLevel,
		Mode:                  mem.settings.Mode,
	})
}

// saveState never fails a request: a write error is logged (D14).
func (m *Manager) saveState(st config.ConversationState) {
	if err := m.state.Save(st); err != nil {
		m.log.Warn("could not write the state file; the current conversation will not survive a restart", "err", err)
	}
}
