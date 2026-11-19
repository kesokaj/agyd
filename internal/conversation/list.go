package conversation

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/kesokaj/agyd/internal/acp"
)

// Conversation is the API view of one conversation, live or from agy's
// history. Settings are shown for live conversations only (agy reports them
// per live session).
type Conversation struct {
	ConversationID string     `json:"conversationId"`
	Cwd            string     `json:"cwd"`
	Title          string     `json:"title,omitempty"`
	Live           bool       `json:"live"`
	Current        bool       `json:"current"`
	Model          string     `json:"model,omitempty"`
	ThinkingLevel  string     `json:"thinkingLevel,omitempty"`
	Mode           string     `json:"mode,omitempty"`
	CreateTime     *time.Time `json:"createTime,omitempty"`
	UpdateTime     *time.Time `json:"updateTime,omitempty"`
}

// View returns the API view of a live conversation.
func (m *Manager) View(info acp.SessionInfo) Conversation {
	s := SettingsOf(info)
	c := Conversation{
		ConversationID: info.SessionID,
		Cwd:            info.Cwd,
		Title:          info.Title,
		Live:           true,
		Current:        info.SessionID == m.CurrentID(),
		Model:          s.Model,
		ThinkingLevel:  s.ThinkingLevel,
		Mode:           s.Mode,
	}
	if !info.CreateTime.IsZero() {
		t := info.CreateTime
		c.CreateTime = &t
	}
	return c
}

// List merges live conversations with agy's session/list, keeps those whose
// cwd equals cwd (when given), and orders them newest first. If session/list
// fails, the live conversations are returned and the failure logged.
func (m *Manager) List(ctx context.Context, cwd string) ([]Conversation, error) {
	byID := map[string]*Conversation{}
	for _, info := range m.agent.ListSessions() {
		c := m.View(info)
		byID[c.ConversationID] = &c
	}
	hist, err := m.agent.ListAgentSessions(ctx)
	if err != nil {
		m.log.Warn("agy session/list failed; listing live conversations only", "err", err)
	} else {
		mergeHistory(byID, hist, m.memosSnapshot(), m.CurrentID())
	}
	out := make([]Conversation, 0, len(byID))
	for _, c := range byID {
		if cwd == "" || c.Cwd == cwd {
			out = append(out, *c)
		}
	}
	slices.SortFunc(out, func(a, b Conversation) int {
		return cmp.Or(-latest(a).Compare(latest(b)), cmp.Compare(a.ConversationID, b.ConversationID))
	})
	return out, nil
}

func mergeHistory(byID map[string]*Conversation, hist []acp.AgentSession, memos map[string]memo, cur string) {
	for _, h := range hist {
		c, ok := byID[h.SessionID]
		if !ok {
			folder := h.Cwd
			if mem, known := memos[h.SessionID]; known && mem.cwd != "" {
				folder = mem.cwd
			}
			c = &Conversation{ConversationID: h.SessionID, Cwd: folder, Current: h.SessionID == cur}
			byID[h.SessionID] = c
		}
		if c.Title == "" {
			c.Title = h.Title
		}
		if !h.UpdateTime.IsZero() {
			t := h.UpdateTime
			c.UpdateTime = &t
		}
	}
	for id, mem := range memos {
		if _, ok := byID[id]; !ok && mem.cwd != "" {
			byID[id] = &Conversation{ConversationID: id, Cwd: mem.cwd, Current: id == cur}
		}
	}
}

// latest is the newer of a conversation's create and update times.
func latest(c Conversation) time.Time {
	var t time.Time
	if c.CreateTime != nil {
		t = *c.CreateTime
	}
	if c.UpdateTime != nil && c.UpdateTime.After(t) {
		t = *c.UpdateTime
	}
	return t
}
