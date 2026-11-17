package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"agyd/internal/acp"
)

const errorDomain = "agyd.local"

type apiError struct {
	Status int    `json:"-"`
	Code   string `json:"status"`
	Reason string `json:"-"`
	Field  string `json:"-"`
	Msg    string `json:"message"`
}

func (e *apiError) Error() string { return e.Msg }

func (s *server) classifyError(r *http.Request, err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	if errors.Is(err, acp.ErrSessionNotFound) {
		return &apiError{Status: http.StatusNotFound, Code: "NOT_FOUND", Reason: "SESSION_NOT_FOUND", Msg: "session not found"}
	}
	if errors.Is(err, acp.ErrPromptInFlight) {
		return &apiError{Status: http.StatusConflict, Code: "ABORTED", Reason: "PROMPT_IN_FLIGHT", Msg: "a prompt turn is already in flight for this session"}
	}
	if errors.Is(err, acp.ErrInvalidConfig) {
		return &apiError{Status: http.StatusBadRequest, Code: "INVALID_ARGUMENT", Reason: "INVALID_CWD", Msg: err.Error()}
	}
	var rpcErr *acp.RPCError
	if errors.As(err, &rpcErr) {
		if rpcErr.IsAuthError() {
			return &apiError{
				Status: http.StatusUnauthorized,
				Code:   "UNAUTHENTICATED",
				Reason: "ACP_AUTH_REQUIRED",
				Msg:    "acp agent requires authentication; run: agy acp --login",
			}
		}
		s.log.ErrorContext(r.Context(), "acp rpc error", "code", rpcErr.Code, "msg", rpcErr.Message, "path", r.URL.Path)
		return &apiError{Status: http.StatusBadGateway, Code: "BAD_GATEWAY", Reason: "ACP_RPC_ERROR", Msg: rpcErr.Message}
	}
	if errors.Is(err, acp.ErrProcessClosed) {
		s.log.ErrorContext(r.Context(), "acp process closed", "err", err, "path", r.URL.Path)
		return &apiError{Status: http.StatusBadGateway, Code: "BAD_GATEWAY", Reason: "ACP_PROCESS_EXITED", Msg: "acp child process exited unexpectedly"}
	}
	s.log.ErrorContext(r.Context(), "handler failed", "err", err, "path", r.URL.Path)
	return &apiError{Status: http.StatusInternalServerError, Code: "INTERNAL", Reason: "INTERNAL", Msg: "internal error"}
}

func writeError(w http.ResponseWriter, ae *apiError) {
	writeJSON(w, ae.Status, errorEnvelope(ae))
}

func errorEnvelope(ae *apiError) map[string]any {
	details := []any{map[string]any{
		"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
		"reason": ae.Reason,
		"domain": errorDomain,
	}}
	if ae.Field != "" {
		details = append(details, map[string]any{
			"@type": "type.googleapis.com/google.rpc.BadRequest",
			"fieldViolations": []map[string]string{
				{"field": ae.Field, "description": ae.Msg},
			},
		})
	}
	return map[string]any{
		"error": map[string]any{
			"code":    ae.Status,
			"status":  ae.Code,
			"message": ae.Msg,
			"details": details,
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // response already started
}

func readJSON(r *http.Request, v any, allowEmpty bool) error {
	if r.Body == nil {
		if allowEmpty {
			return nil
		}
		return &apiError{Status: http.StatusBadRequest, Code: "INVALID_ARGUMENT", Reason: "MALFORMED_BODY", Msg: "request body is required"}
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		slog.DebugContext(r.Context(), "malformed json body", "err", err, "path", r.URL.Path)
		return &apiError{Status: http.StatusBadRequest, Code: "INVALID_ARGUMENT", Reason: "MALFORMED_BODY", Msg: "malformed JSON body"}
	}
	return nil
}
