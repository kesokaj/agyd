package api

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/kesokaj/agyd/internal/acp"
	"github.com/kesokaj/agyd/internal/conversation"
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
	// ErrInvalidArgument also wraps the agent's *acp.RPCError, so it is
	// matched before the generic RPC case below.
	if errors.Is(err, conversation.ErrInvalidSetting) || errors.Is(err, acp.ErrInvalidArgument) {
		return &apiError{Status: http.StatusBadRequest, Code: "INVALID_ARGUMENT", Reason: "INVALID_SETTING", Msg: err.Error()}
	}
	if errors.Is(err, acp.ErrSessionNotFound) {
		return &apiError{Status: http.StatusNotFound, Code: "NOT_FOUND", Reason: "CONVERSATION_NOT_FOUND", Msg: "conversation not found"}
	}
	if errors.Is(err, conversation.ErrNoCurrent) {
		return &apiError{Status: http.StatusNotFound, Code: "NOT_FOUND", Reason: "NO_CURRENT_CONVERSATION", Msg: "no current conversation; create one or use one"}
	}
	if errors.Is(err, conversation.ErrNoModels) {
		return &apiError{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Reason: "MODELS_UNKNOWN", Msg: err.Error()}
	}
	if errors.Is(err, acp.ErrPromptInFlight) {
		return &apiError{Status: http.StatusConflict, Code: "ABORTED", Reason: "PROMPT_IN_FLIGHT", Msg: "a prompt turn is in flight for this conversation"}
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

// maxBodyBytes caps every JSON request body.
const maxBodyBytes = 1 << 20

func readJSON(r *http.Request, v any, allowEmpty bool) error {
	if r.Body == nil {
		if allowEmpty {
			return nil
		}
		return &apiError{Status: http.StatusBadRequest, Code: "INVALID_ARGUMENT", Reason: "MALFORMED_BODY", Msg: "request body is required"}
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if allowEmpty && errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		// Exactly one JSON value: trailing data is as malformed as a bad value.
		if extra := dec.Decode(&json.RawMessage{}); extra != io.EOF {
			err = cmp.Or(extra, errors.New("trailing data after JSON value"))
		}
	}
	if err == nil {
		return nil
	}
	if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
		return &apiError{Status: http.StatusRequestEntityTooLarge, Code: "INVALID_ARGUMENT", Reason: "BODY_TOO_LARGE", Msg: "request body exceeds 1 MiB"}
	}
	slog.DebugContext(r.Context(), "malformed json body", "err", err, "path", r.URL.Path)
	return &apiError{Status: http.StatusBadRequest, Code: "INVALID_ARGUMENT", Reason: "MALFORMED_BODY", Msg: "malformed JSON body"}
}
