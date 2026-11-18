package api

import (
	"errors"
	"net/http"
	"strings"

	"agyd/internal/acp"
	"agyd/internal/conversation"
)

// settingsRequest is the body of PATCH /v1/current, PATCH
// /v1/conversations/{id} and PUT /v1/defaults. Omitted fields keep their
// value.
type settingsRequest struct {
	Model         string `json:"model"`
	ThinkingLevel string `json:"thinkingLevel"`
	Mode          string `json:"mode"`
}

func (r settingsRequest) settings() conversation.Settings {
	return conversation.Settings{Model: r.Model, ThinkingLevel: r.ThinkingLevel, Mode: r.Mode}
}

type createConversationRequest struct {
	settingsRequest
	Cwd string `json:"cwd"`
	Use *bool  `json:"use"` // default true: the new conversation becomes current
}

type listConversationsResponse struct {
	Items []conversation.Conversation `json:"items"`
}

func (s *server) handleCreateConversation(w http.ResponseWriter, r *http.Request) error {
	var req createConversationRequest
	if err := readJSON(r, &req, true); err != nil {
		return err
	}
	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" {
		cwd = s.cfg.DefaultWorkspace
	}
	if !acp.IsValidCwd(cwd) {
		return &apiError{
			Status: http.StatusBadRequest,
			Code:   "INVALID_ARGUMENT",
			Reason: "INVALID_CWD",
			Field:  "cwd",
			Msg:    "cwd must be a clean absolute path outside .gemini",
		}
	}
	info, err := s.mgr.Create(r.Context(), conversation.CreateRequest{
		Cwd:      cwd,
		Settings: req.settings(),
		NoUse:    req.Use != nil && !*req.Use,
	})
	if err != nil {
		return err
	}
	setConversationHeaders(w, info)
	writeJSON(w, http.StatusCreated, s.mgr.View(info))
	return nil
}

func (s *server) handleListConversations(w http.ResponseWriter, r *http.Request) error {
	items, err := s.mgr.List(r.Context(), r.URL.Query().Get("cwd"))
	if err != nil {
		return err
	}
	if items == nil {
		items = []conversation.Conversation{}
	}
	writeJSON(w, http.StatusOK, listConversationsResponse{Items: items})
	return nil
}

// handleGetConversation serves a live conversation, else its entry in agy's
// history (no settings: agy reports those for live conversations only).
func (s *server) handleGetConversation(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("conversationId")
	info, err := s.mgr.Resolve(r.Context(), id)
	if err == nil {
		setConversationHeaders(w, info)
		writeJSON(w, http.StatusOK, s.mgr.View(info))
		return nil
	}
	if !errors.Is(err, acp.ErrSessionNotFound) {
		return err
	}
	all, err := s.mgr.List(r.Context(), "")
	if err != nil {
		return err
	}
	for _, c := range all {
		if c.ConversationID == id {
			setHeaders(w, id, conversation.Settings{})
			writeJSON(w, http.StatusOK, c)
			return nil
		}
	}
	return acp.ErrSessionNotFound
}

func (s *server) handleUpdateConversation(w http.ResponseWriter, r *http.Request) error {
	return s.update(w, r, r.PathValue("conversationId"))
}

func (s *server) handleUpdateCurrent(w http.ResponseWriter, r *http.Request) error {
	return s.update(w, r, "")
}

// update applies new settings to conversation id ("" = current). A turn in
// flight is 409 PROMPT_IN_FLIGHT (D11); a value agy rejects is 400.
func (s *server) update(w http.ResponseWriter, r *http.Request, id string) error {
	var req settingsRequest
	if err := readJSON(r, &req, false); err != nil {
		return err
	}
	info, err := s.mgr.Update(r.Context(), id, req.settings())
	if err != nil {
		return err
	}
	setConversationHeaders(w, info)
	writeJSON(w, http.StatusOK, s.mgr.View(info))
	return nil
}

func (s *server) handleDeleteConversation(w http.ResponseWriter, r *http.Request) error {
	if err := s.mgr.Delete(r.Context(), r.PathValue("conversationId")); err != nil {
		return err
	}
	s.setCurrentHeaders(w) // the pointer may have been cleared
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *server) handleUseConversation(w http.ResponseWriter, r *http.Request) error {
	info, err := s.mgr.Use(r.Context(), r.PathValue("conversationId"))
	if err != nil {
		return err
	}
	setConversationHeaders(w, info)
	writeJSON(w, http.StatusOK, s.mgr.View(info))
	return nil
}

func (s *server) handleGetCurrent(w http.ResponseWriter, r *http.Request) error {
	info, err := s.mgr.Current(r.Context())
	if err != nil {
		return err
	}
	setConversationHeaders(w, info)
	writeJSON(w, http.StatusOK, s.mgr.View(info))
	return nil
}
