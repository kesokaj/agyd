package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"agyd/internal/acp"
	"agyd/internal/conversation"
)

type promptRequest struct {
	Prompt string `json:"prompt"`
}

// promptResponse is a finished turn plus the conversation and settings it
// ran with (D5).
type promptResponse struct {
	ConversationID string            `json:"conversationId"`
	Model          string            `json:"model,omitempty"`
	ThinkingLevel  string            `json:"thinkingLevel,omitempty"`
	Mode           string            `json:"mode,omitempty"`
	StopReason     string            `json:"stopReason"`
	Text           string            `json:"text"`
	Updates        []json.RawMessage `json:"updates,omitempty"`
	Usage          json.RawMessage   `json:"usage,omitempty"`
}

func newPromptResponse(info acp.SessionInfo, res acp.PromptResult) promptResponse {
	set := conversation.SettingsOf(info)
	return promptResponse{
		ConversationID: info.SessionID,
		Model:          set.Model,
		ThinkingLevel:  set.ThinkingLevel,
		Mode:           set.Mode,
		StopReason:     res.StopReason,
		Text:           res.Text,
		Updates:        res.Updates,
		Usage:          res.Usage,
	}
}

// handlePromptCurrent prompts the current conversation, creating one when
// there is none (D20).
func (s *server) handlePromptCurrent(w http.ResponseWriter, r *http.Request) error {
	return s.prompt(w, r, "")
}

// handlePromptConversation prompts a live conversation by id.
func (s *server) handlePromptConversation(w http.ResponseWriter, r *http.Request) error {
	return s.prompt(w, r, r.PathValue("conversationId"))
}

// prompt reads the body first, so a bad request never creates a
// conversation, then resolves the target once (D10): a pointer switch while
// the turn runs does not move the turn.
func (s *server) prompt(w http.ResponseWriter, r *http.Request, id string) error {
	var req promptRequest
	if err := readJSON(r, &req, false); err != nil {
		return err
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return &apiError{
			Status: http.StatusBadRequest,
			Code:   "INVALID_ARGUMENT",
			Reason: "FIELD_REQUIRED",
			Field:  "prompt",
			Msg:    "prompt is required",
		}
	}
	info, err := s.mgr.Resolve(r.Context(), id)
	if err != nil {
		return err
	}
	setConversationHeaders(w, info)
	blocks := []acp.ContentBlock{{Type: "text", Text: req.Prompt}}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		return s.streamPromptSSE(w, r, info, blocks)
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // buffered prompt turns can exceed the default 30s WriteTimeout
	res, err := s.svc.Prompt(r.Context(), info.SessionID, blocks, nil)
	_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, newPromptResponse(info, res))
	return nil
}

func (s *server) streamPromptSSE(w http.ResponseWriter, r *http.Request, info acp.SessionInfo, blocks []acp.ContentBlock) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // SSE streams can outlive the default HTTP WriteTimeout

	var writeMu sync.Mutex
	started := false
	startStream := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		_ = rc.Flush()
	}
	sendEvent := func(event string, data []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
		startStream()
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		_ = rc.Flush()
		_ = rc.SetWriteDeadline(time.Time{})
	}

	res, err := s.svc.Prompt(r.Context(), info.SessionID, blocks, func(rawUpdate []byte) {
		sendEvent("session_update", rawUpdate)
	})
	if err != nil {
		writeMu.Lock()
		alreadyStarted := started
		writeMu.Unlock()
		if !alreadyStarted {
			return err
		}
		ae := s.classifyError(r, err)
		b, _ := json.Marshal(errorEnvelope(ae))
		sendEvent("prompt_error", b)
		return nil
	}

	done := newPromptResponse(info, res)
	done.Updates = nil // already streamed as session_update events
	doneBytes, _ := json.Marshal(done)
	sendEvent("prompt_done", doneBytes)
	return nil
}

// handleCancelCurrent cancels the current conversation's turn. It never
// creates or resumes: with no current conversation it is 404.
func (s *server) handleCancelCurrent(w http.ResponseWriter, r *http.Request) error {
	id := s.mgr.CurrentID()
	if id == "" {
		return conversation.ErrNoCurrent
	}
	return s.cancel(w, r, id)
}

func (s *server) handleCancelConversation(w http.ResponseWriter, r *http.Request) error {
	return s.cancel(w, r, r.PathValue("conversationId"))
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request, id string) error {
	if err := s.svc.Cancel(r.Context(), id); err != nil {
		return err
	}
	if info, err := s.svc.GetSession(id); err == nil {
		setConversationHeaders(w, info)
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"conversationId": id,
		"state":          "CANCEL_REQUESTED",
	})
	return nil
}
