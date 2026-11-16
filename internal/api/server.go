// Package api implements the HTTP + Server-Sent Events (SSE) gateway for agy-api.
package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"agy-api/internal/acp"
)

// Config holds HTTP server configuration.
type Config struct {
	DefaultWorkspace string
}

type server struct {
	log *slog.Logger
	svc acp.Service
	cfg Config
}

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// NewHandler constructs the HTTP mux for agy-api.
func NewHandler(log *slog.Logger, svc acp.Service, cfg Config) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	if cfg.DefaultWorkspace == "" {
		cfg.DefaultWorkspace = "/workspace"
	}
	s := &server{log: log, svc: svc, cfg: cfg}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /v1/status", s.adapt(s.handleStatus))
	mux.Handle("POST /v1/sessions", s.adapt(s.handleCreateSession))
	mux.Handle("GET /v1/sessions", s.adapt(s.handleListSessions))
	mux.Handle("GET /v1/sessions/{sessionId}", s.adapt(s.handleGetSession))
	mux.Handle("POST /v1/sessions/{sessionId}/prompt", s.adapt(s.handlePrompt))
	mux.Handle("POST /v1/sessions/{sessionId}/cancel", s.adapt(s.handleCancel))
	mux.Handle("DELETE /v1/sessions/{sessionId}", s.adapt(s.handleDeleteSession))
	return s.recoverMiddleware(mux)
}

func (s *server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.ErrorContext(r.Context(), "panic in http handler",
					"panic", rec,
					"path", r.URL.Path,
					"stack_trace", string(debug.Stack()),
				)
				writeError(w, &apiError{
					Status: http.StatusInternalServerError,
					Code:   "INTERNAL",
					Reason: "INTERNAL",
					Msg:    "internal error",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *server) adapt(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			ae := s.classifyError(r, err)
			writeError(w, ae)
		}
	})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) error {
	st, err := s.svc.Status(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

type createSessionRequest struct {
	Cwd string `json:"cwd"`
}

func (s *server) handleCreateSession(w http.ResponseWriter, r *http.Request) error {
	var req createSessionRequest
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
	info, err := s.svc.NewSession(r.Context(), cwd)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, info)
	return nil
}

type listSessionsResponse struct {
	Items []acp.SessionInfo `json:"items"`
}

func (s *server) handleListSessions(w http.ResponseWriter, _ *http.Request) error {
	items := s.svc.ListSessions()
	if items == nil {
		items = []acp.SessionInfo{}
	}
	writeJSON(w, http.StatusOK, listSessionsResponse{Items: items})
	return nil
}

func (s *server) handleGetSession(w http.ResponseWriter, r *http.Request) error {
	info, err := s.svc.GetSession(r.PathValue("sessionId"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, info)
	return nil
}

func (s *server) handleDeleteSession(w http.ResponseWriter, r *http.Request) error {
	if err := s.svc.CloseSession(r.Context(), r.PathValue("sessionId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type promptRequest struct {
	Prompt string `json:"prompt"`
}

func (s *server) handlePrompt(w http.ResponseWriter, r *http.Request) error {
	sessionID := r.PathValue("sessionId")
	if _, err := s.svc.GetSession(sessionID); err != nil {
		return err
	}
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
	blocks := []acp.ContentBlock{{Type: "text", Text: req.Prompt}}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		return s.streamPromptSSE(w, r, sessionID, blocks)
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // buffered prompt turns can exceed the default 30s WriteTimeout
	res, err := s.svc.Prompt(r.Context(), sessionID, blocks, nil)
	_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (s *server) streamPromptSSE(w http.ResponseWriter, r *http.Request, sessionID string, blocks []acp.ContentBlock) error {
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

	res, err := s.svc.Prompt(r.Context(), sessionID, blocks, func(rawUpdate []byte) {
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

	doneBytes, _ := json.Marshal(map[string]any{
		"sessionId":  res.SessionID,
		"stopReason": res.StopReason,
		"text":       res.Text,
	})
	sendEvent("prompt_done", doneBytes)
	return nil
}

func (s *server) handleCancel(w http.ResponseWriter, r *http.Request) error {
	sessionID := r.PathValue("sessionId")
	if err := s.svc.Cancel(r.Context(), sessionID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"sessionId": sessionID,
		"state":     "CANCEL_REQUESTED",
	})
	return nil
}
