// Package api implements the HTTP + Server-Sent Events (SSE) gateway for agyd.
package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"agyd/internal/acp"
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

// Guard applies the Host guard and CrossOriginProtection to the mux.
func Guard(next http.Handler) http.Handler {
	csrf := http.NewCrossOriginProtection()
	csrfHandler := csrf.Handler(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
		host = strings.ToLower(host)

		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			writeError(w, &apiError{
				Status: http.StatusForbidden,
				Code:   "PERMISSION_DENIED",
				Reason: "FORBIDDEN_HOST",
				Msg:    "Host not allowed",
			})
			return
		}
		csrfHandler.ServeHTTP(w, r)
	})
}

// NewHandler constructs the HTTP mux for agyd.
func NewHandler(log *slog.Logger, svc acp.Service, cfg Config) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	s := &server{log: log, svc: svc, cfg: cfg}

	mux := http.NewServeMux()
	for _, r := range s.routes() {
		mux.Handle(r.Method+" "+r.Pattern, s.adapt(r.handler))
	}

	// Default handler for 404s
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, &apiError{
			Status: http.StatusNotFound,
			Code:   "NOT_FOUND",
			Reason: "NOT_FOUND",
			Msg:    "Not found",
		})
	})

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

type statusResponse struct {
	acp.Status
	Workspace string `json:"workspace"`
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) error {
	st, err := s.svc.Status(r.Context())
	if err != nil {
		return err
	}
	res := statusResponse{
		Status:    st,
		Workspace: s.cfg.DefaultWorkspace,
	}
	writeJSON(w, http.StatusOK, res)
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
