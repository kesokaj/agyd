package api

import (
	"net/http"
	"strings"
	"testing"

	"agyd/internal/acp"
)

func TestStatusAndHealthEndpoints(t *testing.T) {
	e := newTestEnv(t)

	t.Run("healthz returns 204", func(t *testing.T) {
		want(t, e.do(http.MethodGet, "/healthz", ""), http.StatusNoContent, "")
	})

	t.Run("status returns 200 with the workspace", func(t *testing.T) {
		rec := e.do(http.MethodGet, "/v1/status", "")
		want(t, rec, http.StatusOK, "")
		st := decode[statusResponse](t, rec)
		if !st.Ready || st.ProtocolVersion != 1 || st.Workspace != "/workspace" {
			t.Errorf("status = %+v, want ready, protocol 1, /workspace", st)
		}
	})
}

func TestPromptCurrentAutoCreates(t *testing.T) {
	e := newTestEnv(t)

	// A bad body never creates a conversation.
	want(t, e.do(http.MethodPost, "/v1/prompt", `{"prompt":""}`), http.StatusBadRequest, "FIELD_REQUIRED")
	want(t, e.do(http.MethodPost, "/v1/prompt", `{"prompt":"hi","extra":1}`), http.StatusBadRequest, "MALFORMED_BODY")
	if id := e.mgr.CurrentID(); id != "" {
		t.Fatalf("a rejected prompt created conversation %q", id)
	}

	rec := e.do(http.MethodPost, "/v1/prompt", `{"prompt":"hello"}`)
	want(t, rec, http.StatusOK, "")
	res := decode[promptResponse](t, rec)
	if res.ConversationID == "" || res.ConversationID != e.mgr.CurrentID() {
		t.Fatalf("prompt went to %q, current is %q", res.ConversationID, e.mgr.CurrentID())
	}
	if res.Text != "Hello from ACP" || res.StopReason != "end_turn" || res.Model != "gemini-4-carbon" || res.ThinkingLevel != "medium" {
		t.Errorf("prompt result = %+v", res)
	}
	if got := rec.Header().Get(HeaderConversation); got != res.ConversationID {
		t.Errorf("%s = %q, want %q", HeaderConversation, got, res.ConversationID)
	}

	// A second prompt reuses the same conversation.
	again := decode[promptResponse](t, e.do(http.MethodPost, "/v1/prompt", `{"prompt":"again"}`))
	if again.ConversationID != res.ConversationID {
		t.Errorf("second prompt went to %q, want %q", again.ConversationID, res.ConversationID)
	}
}

func TestPromptSSEAndErrors(t *testing.T) {
	e := newTestEnv(t)
	id := decode[map[string]any](t, e.do(http.MethodPost, "/v1/conversations", `{}`))["conversationId"].(string)

	t.Run("SSE streaming response names the conversation", func(t *testing.T) {
		rec := e.do(http.MethodPost, "/v1/conversations/"+id+"/prompt", `{"prompt":"hello"}`, "Accept", "text/event-stream")
		want(t, rec, http.StatusOK, "")
		body := rec.Body.String()
		if !strings.Contains(body, "event: session_update\n") || !strings.Contains(body, "event: prompt_done\n") ||
			!strings.Contains(body, `"conversationId":"`+id+`"`) {
			t.Errorf("SSE body:\n%s", body)
		}
		if got := rec.Header().Get(HeaderModel); got != "gemini-4-carbon" {
			t.Errorf("%s = %q on the stream", HeaderModel, got)
		}
	})

	t.Run("unknown conversation is 404 CONVERSATION_NOT_FOUND", func(t *testing.T) {
		want(t, e.do(http.MethodPost, "/v1/conversations/nope/prompt", `{"prompt":"x"}`), http.StatusNotFound, "CONVERSATION_NOT_FOUND")
	})

	t.Run("auth required error maps to 401 ACP_AUTH_REQUIRED", func(t *testing.T) {
		want(t, e.do(http.MethodPost, "/v1/prompt", `{"prompt":"auth_error"}`), http.StatusUnauthorized, "ACP_AUTH_REQUIRED")
	})
}

func TestCancelEndpoints(t *testing.T) {
	e := newTestEnv(t)

	want(t, e.do(http.MethodPost, "/v1/cancel", ""), http.StatusNotFound, "NO_CURRENT_CONVERSATION")

	id := decode[map[string]any](t, e.do(http.MethodPost, "/v1/conversations", `{}`))["conversationId"].(string)
	done := make(chan int, 1)
	go func() {
		done <- e.do(http.MethodPost, "/v1/prompt", `{"prompt":"block_until_cancel"}`).Code
	}()
	e.svc.waitInFlight(t, id)

	want(t, e.do(http.MethodPost, "/v1/prompt", `{"prompt":"second"}`), http.StatusConflict, "PROMPT_IN_FLIGHT")

	rec := e.do(http.MethodPost, "/v1/cancel", "")
	want(t, rec, http.StatusOK, "")
	if !strings.Contains(rec.Body.String(), `"conversationId":"`+id+`"`) || !strings.Contains(rec.Body.String(), "CANCEL_REQUESTED") {
		t.Errorf("cancel body = %s", rec.Body)
	}
	if code := <-done; code != http.StatusOK {
		t.Errorf("cancelled prompt status = %d, want 200", code)
	}

	want(t, e.do(http.MethodPost, "/v1/conversations/unknown/cancel", ""), http.StatusNotFound, "CONVERSATION_NOT_FOUND")
	want(t, e.do(http.MethodPost, "/v1/conversations/"+id+"/cancel", ""), http.StatusOK, "")
}

// Every /healthz and /v1 response carries the visibility headers of the
// current conversation, errors and unknown paths included (D5, D17).
func TestHeadersOnEveryResponse(t *testing.T) {
	e := newTestEnv(t)

	rec := e.do(http.MethodGet, "/healthz", "")
	if v, ok := rec.Header()[HeaderConversation]; ok {
		t.Errorf("no current conversation, but %s = %q", HeaderConversation, v)
	}

	created := decode[map[string]any](t, e.do(http.MethodPost, "/v1/conversations", `{"model":"claude-sonnet-5-5","thinkingLevel":"high"}`))
	id := created["conversationId"].(string)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/healthz", ""},
		{http.MethodGet, "/v1/status", ""},
		{http.MethodGet, "/v1/defaults", ""},
		{http.MethodGet, "/v1/typo", ""},
		{http.MethodPost, "/v1/prompt", `{"prompt":""}`},       // 400
		{http.MethodGet, "/v1/conversations/missing", ""},      // 404
		{http.MethodPatch, "/v1/current", `{"model":"bogus"}`}, // 400 from agy
	} {
		rec := e.do(tc.method, tc.path, tc.body)
		h := rec.Header()
		if h.Get(HeaderConversation) != id || h.Get(HeaderModel) != "claude-sonnet-5-5" ||
			h.Get(HeaderThinkingLevel) != "high" || h.Get(HeaderMode) != "default" {
			t.Errorf("%s %s (%d): headers %v", tc.method, tc.path, rec.Code, h)
		}
	}
}

func TestClassifyErrorMapping(t *testing.T) {
	s := &server{log: discardLog()}
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	for _, tc := range []struct {
		err    error
		status int
		reason string
	}{
		{acp.ErrPromptInFlight, http.StatusConflict, "PROMPT_IN_FLIGHT"},
		{acp.ErrSessionNotFound, http.StatusNotFound, "CONVERSATION_NOT_FOUND"},
		{&acp.RPCError{Code: -32603, Message: "boom"}, http.StatusBadGateway, "ACP_RPC_ERROR"},
	} {
		ae := s.classifyError(req, tc.err)
		if ae.Status != tc.status || ae.Reason != tc.reason {
			t.Errorf("%v -> %d %s, want %d %s", tc.err, ae.Status, ae.Reason, tc.status, tc.reason)
		}
	}
}
