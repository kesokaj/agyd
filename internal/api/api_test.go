package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kesokaj/agyd/internal/acp"
)

func TestStatusAndHealthEndpoints(t *testing.T) {
	e := newTestEnv(t)

	t.Run("healthz returns 200 with service agyd", func(t *testing.T) {
		rec := e.do(http.MethodGet, "/healthz", "")
		want(t, rec, http.StatusOK, "")
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		got := decode[map[string]any](t, rec)
		if len(got) != 1 || got["service"] != "agyd" {
			t.Errorf("healthz body = %+v, want {\"service\":\"agyd\"}", got)
		}
	})

	t.Run("status returns 200 with the workspace", func(t *testing.T) {
		rec := e.do(http.MethodGet, "/v1/status", "")
		want(t, rec, http.StatusOK, "")
		st := decode[statusResponse](t, rec)
		if !st.Ready || st.ProtocolVersion != 1 || st.Workspace != e.workspace {
			t.Errorf("status = %+v, want ready, protocol 1, %s", st, e.workspace)
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

func TestPromptVerbatimACPEnvelope(t *testing.T) {
	wantUpdates := []string{
		`{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"plan step 1"}}`,
		`{"sessionUpdate":"tool_call","toolCallId":"tc_1","kind":"read","title":"Read README.md","status":"in_progress"}`,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"  Raw line 1\n\nLine 2  "}}`,
	}
	wantText := "  Raw line 1\n\nLine 2  "
	wantUsage := `{"input":12,"output":7}`

	for _, ep := range []struct {
		name string
		byID bool
	}{
		{name: "current", byID: false},
		{name: "by_id", byID: true},
	} {
		t.Run(ep.name, func(t *testing.T) {
			setup := func(t *testing.T) (*testEnv, string, func() string) {
				t.Helper()
				e := newTestEnv(t)
				if ep.byID {
					id := decode[map[string]any](t, e.do(http.MethodPost, "/v1/conversations", `{}`))["conversationId"].(string)
					return e, "/v1/conversations/" + id + "/prompt", func() string { return id }
				}
				return e, "/v1/prompt", e.mgr.CurrentID
			}

			t.Run("buffered_empty_updates", func(t *testing.T) {
				e, path, wantID := setup(t)
				rec := e.do(http.MethodPost, path, `{"prompt":"empty_updates"}`)
				want(t, rec, http.StatusOK, "")

				fields := decodeRawObject(t, rec.Body.Bytes())
				assertPromptMetadata(t, fields, wantID(), "", "")
				rawUpdates, ok := fields["updates"]
				if !ok || string(rawUpdates) != "[]" {
					t.Errorf("buffered empty updates = %q (present=%v), want []", string(rawUpdates), ok)
				}
			})

			t.Run("buffered_multi_updates", func(t *testing.T) {
				e, path, wantID := setup(t)
				rec := e.do(http.MethodPost, path, `{"prompt":"multi_updates"}`)
				want(t, rec, http.StatusOK, "")

				fields := decodeRawObject(t, rec.Body.Bytes())
				assertPromptMetadata(t, fields, wantID(), wantText, wantUsage)
				var gotUpdates []json.RawMessage
				if err := json.Unmarshal(fields["updates"], &gotUpdates); err != nil {
					t.Fatalf("unmarshal updates: %v", err)
				}
				if len(gotUpdates) != len(wantUpdates) {
					t.Fatalf("len(updates) = %d, want %d", len(gotUpdates), len(wantUpdates))
				}
				for i, want := range wantUpdates {
					if got := string(gotUpdates[i]); got != want {
						t.Errorf("updates[%d] = %s, want %s", i, got, want)
					}
				}
			})

			t.Run("sse_multi_updates", func(t *testing.T) {
				e, path, wantID := setup(t)
				rec := e.do(http.MethodPost, path, `{"prompt":"multi_updates"}`, "Accept", "text/event-stream")
				want(t, rec, http.StatusOK, "")

				events := parseSSE(t, rec.Body.String())
				if len(events) != len(wantUpdates)+1 {
					t.Fatalf("len(events) = %d, want %d:\n%s", len(events), len(wantUpdates)+1, rec.Body.String())
				}
				id := wantID()
				for i, u := range wantUpdates {
					wantData := `{"sessionId":"` + id + `","update":` + u + `}`
					if events[i].event != "session_update" || events[i].data != wantData {
						t.Errorf("event[%d] = (%q, %s), want (session_update, %s)", i, events[i].event, events[i].data, wantData)
					}
				}
				assertPromptDoneSSE(t, events[len(wantUpdates)], id, wantText, wantUsage)
			})

			t.Run("sse_empty_updates", func(t *testing.T) {
				e, path, wantID := setup(t)
				rec := e.do(http.MethodPost, path, `{"prompt":"empty_updates"}`, "Accept", "text/event-stream")
				want(t, rec, http.StatusOK, "")

				events := parseSSE(t, rec.Body.String())
				if len(events) != 1 {
					t.Fatalf("len(events) = %d, want 1 (%+v)", len(events), events)
				}
				assertPromptDoneSSE(t, events[0], wantID(), "", "")
			})
		})
	}
}

type sseEvent struct {
	event string
	data  string
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev.event = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				ev.data = v
			}
		}
		out = append(out, ev)
	}
	return out
}

func decodeRawObject(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("unmarshal object %s: %v", data, err)
	}
	return fields
}

func decodeStringField(t *testing.T, fields map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := fields[key]
	if !ok {
		t.Fatalf("missing JSON field %q in %v", key, fields)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal field %q (%s): %v", key, raw, err)
	}
	return s
}

func assertPromptDoneSSE(t *testing.T, ev sseEvent, wantID, wantText, wantUsage string) {
	t.Helper()
	if ev.event != "prompt_done" {
		t.Fatalf("event = %q, want prompt_done", ev.event)
	}
	fields := decodeRawObject(t, []byte(ev.data))
	if raw, ok := fields["updates"]; ok {
		t.Errorf("prompt_done unexpectedly contains updates: %s", raw)
	}
	assertPromptMetadata(t, fields, wantID, wantText, wantUsage)
}

func assertPromptMetadata(t *testing.T, fields map[string]json.RawMessage, wantID, wantText, wantUsage string) {
	t.Helper()
	if raw, ok := fields["output"]; ok {
		t.Errorf("unexpected custom output field: %s", raw)
	}
	if wantID == "" {
		t.Fatal("expected non-empty conversationId")
	}
	for _, tc := range []struct{ key, want string }{
		{"conversationId", wantID},
		{"model", "gemini-4-carbon"},
		{"thinkingLevel", "medium"},
		{"mode", "default"},
		{"stopReason", "end_turn"},
		{"text", wantText},
	} {
		if got := decodeStringField(t, fields, tc.key); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}
	if wantUsage == "" {
		if raw, ok := fields["usage"]; ok {
			t.Errorf("unexpected usage field: %s", raw)
		}
	} else if raw, ok := fields["usage"]; !ok || string(raw) != wantUsage {
		t.Errorf("usage = %s (present=%v), want %s", raw, ok, wantUsage)
	}
}

func TestCancelEndpoints(t *testing.T) {
	e := newTestEnv(t)

	want(t, e.do(http.MethodPost, "/v1/cancel", ""), http.StatusNotFound, "NO_CURRENT_CONVERSATION")

	id := decode[map[string]any](t, e.do(http.MethodPost, "/v1/conversations", `{}`))["conversationId"].(string)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- e.do(http.MethodPost, "/v1/prompt", `{"prompt":"block_until_cancel"}`)
	}()
	e.svc.waitInFlight(t, id)

	want(t, e.do(http.MethodPost, "/v1/prompt", `{"prompt":"second"}`), http.StatusConflict, "PROMPT_IN_FLIGHT")

	rec := e.do(http.MethodPost, "/v1/cancel", "")
	want(t, rec, http.StatusOK, "")
	if !strings.Contains(rec.Body.String(), `"conversationId":"`+id+`"`) || !strings.Contains(rec.Body.String(), "CANCEL_REQUESTED") {
		t.Errorf("cancel body = %s", rec.Body)
	}
	cancelledRec := <-done
	if cancelledRec.Code != http.StatusOK {
		t.Fatalf("cancelled prompt status = %d, want 200", cancelledRec.Code)
	}
	fields := decodeRawObject(t, cancelledRec.Body.Bytes())
	if got := decodeStringField(t, fields, "stopReason"); got != "cancelled" {
		t.Errorf("cancelled stopReason = %q, want cancelled", got)
	}
	if rawUpdates, ok := fields["updates"]; !ok || string(rawUpdates) != "[]" {
		t.Errorf("cancelled updates = %q (present=%v), want []", string(rawUpdates), ok)
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
