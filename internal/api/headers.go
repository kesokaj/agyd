package api

import (
	"net/http"

	"github.com/kesokaj/agyd/internal/acp"
	"github.com/kesokaj/agyd/internal/conversation"
)

// Visibility headers (D5, D17): every response from this handler names the
// conversation it used, or the current one, with agy's confirmed settings.
// A header is absent when its value is unknown (no current conversation, or
// the current one is not live so agy has not confirmed its settings).
const (
	HeaderConversation  = "Agyd-Conversation"
	HeaderModel         = "Agyd-Model"
	HeaderThinkingLevel = "Agyd-Thinking-Level"
	HeaderMode          = "Agyd-Mode"
)

// currentHeaders sets the visibility headers from the current conversation
// before any handler runs, so errors and unknown paths carry them too. It
// never resumes or creates a conversation; handlers that address one
// overwrite the headers with setConversationHeaders.
func (s *server) currentHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.setCurrentHeaders(w)
		next.ServeHTTP(w, r)
	})
}

func (s *server) setCurrentHeaders(w http.ResponseWriter) {
	id := s.mgr.CurrentID()
	if id == "" {
		setHeaders(w, "", conversation.Settings{})
		return
	}
	var set conversation.Settings
	if s.svc != nil {
		if info, err := s.svc.GetSession(id); err == nil {
			set = conversation.SettingsOf(info)
		}
	}
	setHeaders(w, id, set)
}

// setConversationHeaders names the conversation a request resolved to.
func setConversationHeaders(w http.ResponseWriter, info acp.SessionInfo) {
	setHeaders(w, info.SessionID, conversation.SettingsOf(info))
}

func setHeaders(w http.ResponseWriter, id string, set conversation.Settings) {
	h := w.Header()
	for _, kv := range [...]struct{ key, value string }{
		{HeaderConversation, id},
		{HeaderModel, set.Model},
		{HeaderThinkingLevel, set.ThinkingLevel},
		{HeaderMode, set.Mode},
	} {
		if kv.value == "" {
			h.Del(kv.key)
		} else {
			h.Set(kv.key, kv.value)
		}
	}
}
