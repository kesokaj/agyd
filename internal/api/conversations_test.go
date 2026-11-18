package api

import (
	"net/http"
	"testing"

	"agyd/internal/conversation"
)

func createConv(t *testing.T, e *testEnv, body string) conversation.Conversation {
	t.Helper()
	rec := e.do(http.MethodPost, "/v1/conversations", body)
	want(t, rec, http.StatusCreated, "")
	return decode[conversation.Conversation](t, rec)
}

func TestCreateConversation(t *testing.T) {
	e := newTestEnv(t)

	work := t.TempDir()
	c := createConv(t, e, `{"cwd":"`+work+`","model":"claude-sonnet-5-5","mode":"plan"}`)
	if c.Cwd != work || c.Model != "claude-sonnet-5-5" || c.ThinkingLevel != "medium" || c.Mode != "plan" || !c.Live || !c.Current {
		t.Errorf("created = %+v", c)
	}

	t.Run("use false keeps the pointer", func(t *testing.T) {
		other := createConv(t, e, `{"use":false}`)
		if other.Current || other.Cwd != "/workspace" || e.mgr.CurrentID() != c.ConversationID {
			t.Errorf("use:false moved the pointer: other=%+v current=%q", other, e.mgr.CurrentID())
		}
	})

	t.Run("bad cwd is 400 INVALID_CWD", func(t *testing.T) {
		want(t, e.do(http.MethodPost, "/v1/conversations", `{"cwd":"relative/path"}`), http.StatusBadRequest, "INVALID_CWD")
		want(t, e.do(http.MethodPost, "/v1/conversations", `{"cwd":"/home/agy/.gemini"}`), http.StatusBadRequest, "INVALID_CWD")
	})

	t.Run("bad model is 400 and leaves no conversation behind", func(t *testing.T) {
		before := len(e.svc.ListSessions())
		want(t, e.do(http.MethodPost, "/v1/conversations", `{"model":"no-such-model"}`), http.StatusBadRequest, "INVALID_SETTING")
		want(t, e.do(http.MethodPost, "/v1/conversations", `{"model":"bad model\nAGYD_AGY=x"}`), http.StatusBadRequest, "INVALID_SETTING")
		if after := len(e.svc.ListSessions()); after != before || e.mgr.CurrentID() != c.ConversationID {
			t.Errorf("live %d -> %d, current %q; want unchanged", before, after, e.mgr.CurrentID())
		}
	})
}

func TestListAndGetConversations(t *testing.T) {
	e := newTestEnv(t)
	workA, workB := t.TempDir(), t.TempDir()
	a := createConv(t, e, `{"cwd":"`+workA+`"}`)
	b := createConv(t, e, `{"cwd":"`+workB+`"}`)

	list := decode[listConversationsResponse](t, e.do(http.MethodGet, "/v1/conversations", ""))
	if len(list.Items) != 3 || list.Items[0].ConversationID != b.ConversationID || !list.Items[0].Current ||
		list.Items[2].ConversationID != "hist_1" || list.Items[2].Live {
		t.Errorf("list = %+v, want b (current), a, hist_1 (not live)", list.Items)
	}

	filtered := decode[listConversationsResponse](t, e.do(http.MethodGet, "/v1/conversations?cwd="+workA, ""))
	if len(filtered.Items) != 1 || filtered.Items[0].ConversationID != a.ConversationID {
		t.Errorf("cwd filter = %+v, want only a", filtered.Items)
	}

	got := decode[conversation.Conversation](t, e.do(http.MethodGet, "/v1/conversations/"+a.ConversationID, ""))
	if got.ConversationID != a.ConversationID || !got.Live || got.Current || got.Model == "" {
		t.Errorf("get live = %+v", got)
	}
	hist := decode[conversation.Conversation](t, e.do(http.MethodGet, "/v1/conversations/hist_1", ""))
	if hist.Live || hist.Cwd != "/hist/one" || hist.Model != "" {
		t.Errorf("get history = %+v, want not live, no settings", hist)
	}
	want(t, e.do(http.MethodGet, "/v1/conversations/nope", ""), http.StatusNotFound, "CONVERSATION_NOT_FOUND")
}

// use switches the target of /v1/prompt, for live and history conversations.
func TestUseSwitchesPromptTarget(t *testing.T) {
	e := newTestEnv(t)
	a := createConv(t, e, `{"cwd":"`+t.TempDir()+`"}`)
	b := createConv(t, e, `{"cwd":"`+t.TempDir()+`"}`)

	promptTarget := func() string {
		t.Helper()
		rec := e.do(http.MethodPost, "/v1/prompt", `{"prompt":"hello"}`)
		want(t, rec, http.StatusOK, "")
		return decode[promptResponse](t, rec).ConversationID
	}
	if got := promptTarget(); got != b.ConversationID {
		t.Fatalf("before use: prompt went to %q, want %q", got, b.ConversationID)
	}

	rec := e.do(http.MethodPost, "/v1/conversations/"+a.ConversationID+"/use", "")
	want(t, rec, http.StatusOK, "")
	if rec.Header().Get(HeaderConversation) != a.ConversationID || !decode[conversation.Conversation](t, rec).Current {
		t.Fatalf("use response = %s %v", rec.Body, rec.Header())
	}
	if got := promptTarget(); got != a.ConversationID {
		t.Fatalf("after use: prompt went to %q, want %q", got, a.ConversationID)
	}
	cur := decode[conversation.Conversation](t, e.do(http.MethodGet, "/v1/current", ""))
	if cur.ConversationID != a.ConversationID {
		t.Errorf("GET /v1/current = %q, want %q", cur.ConversationID, a.ConversationID)
	}

	// A history conversation is resumed in its own cwd and becomes the target.
	rec = e.do(http.MethodPost, "/v1/conversations/hist_1/use", "")
	want(t, rec, http.StatusOK, "")
	if h := decode[conversation.Conversation](t, rec); !h.Live || h.Cwd != "/hist/one" {
		t.Errorf("resumed = %+v", h)
	}
	if got := promptTarget(); got != "hist_1" {
		t.Errorf("after use hist_1: prompt went to %q", got)
	}

	want(t, e.do(http.MethodPost, "/v1/conversations/nope/use", ""), http.StatusNotFound, "CONVERSATION_NOT_FOUND")
	if e.mgr.CurrentID() != "hist_1" {
		t.Errorf("a failed use moved the pointer to %q", e.mgr.CurrentID())
	}
}

func TestPatchSettings(t *testing.T) {
	e := newTestEnv(t)
	want(t, e.do(http.MethodPatch, "/v1/current", `{"model":"claude-sonnet-5-5"}`), http.StatusNotFound, "NO_CURRENT_CONVERSATION")
	want(t, e.do(http.MethodGet, "/v1/current", ""), http.StatusNotFound, "NO_CURRENT_CONVERSATION")

	c := createConv(t, e, `{}`)

	rec := e.do(http.MethodPatch, "/v1/current", `{"model":"claude-sonnet-5-5","thinkingLevel":"high"}`)
	want(t, rec, http.StatusOK, "")
	got := decode[conversation.Conversation](t, rec)
	if got.Model != "claude-sonnet-5-5" || got.ThinkingLevel != "high" || rec.Header().Get(HeaderModel) != "claude-sonnet-5-5" {
		t.Errorf("patched = %+v headers %v", got, rec.Header())
	}

	rec = e.do(http.MethodPatch, "/v1/conversations/"+c.ConversationID, `{"mode":"plan"}`)
	want(t, rec, http.StatusOK, "")
	if got := decode[conversation.Conversation](t, rec); got.Mode != "plan" || got.Model != "claude-sonnet-5-5" {
		t.Errorf("patched by id = %+v", got)
	}

	t.Run("bad model is 400 and keeps the old value", func(t *testing.T) {
		want(t, e.do(http.MethodPatch, "/v1/current", `{"model":"no-such-model"}`), http.StatusBadRequest, "INVALID_SETTING")
		want(t, e.do(http.MethodPatch, "/v1/current", `{"thinkingLevel":"x=y"}`), http.StatusBadRequest, "INVALID_SETTING")
		want(t, e.do(http.MethodPatch, "/v1/current", `{}`), http.StatusBadRequest, "INVALID_SETTING")
		want(t, e.do(http.MethodPatch, "/v1/current", `{"temperature":"1"}`), http.StatusBadRequest, "MALFORMED_BODY")
		cur := decode[conversation.Conversation](t, e.do(http.MethodGet, "/v1/current", ""))
		if cur.Model != "claude-sonnet-5-5" {
			t.Errorf("model after rejected patch = %q", cur.Model)
		}
	})

	t.Run("unknown conversation is 404", func(t *testing.T) {
		want(t, e.do(http.MethodPatch, "/v1/conversations/nope", `{"mode":"plan"}`), http.StatusNotFound, "CONVERSATION_NOT_FOUND")
	})
}

// PATCH and DELETE during a turn are 409 PROMPT_IN_FLIGHT (D11).
func TestPatchAndDeleteDuringTurn(t *testing.T) {
	e := newTestEnv(t)
	c := createConv(t, e, `{}`)
	done := make(chan int, 1)
	go func() {
		done <- e.do(http.MethodPost, "/v1/prompt", `{"prompt":"block_until_cancel"}`).Code
	}()
	e.svc.waitInFlight(t, c.ConversationID)

	want(t, e.do(http.MethodPatch, "/v1/current", `{"model":"claude-sonnet-5-5"}`), http.StatusConflict, "PROMPT_IN_FLIGHT")
	want(t, e.do(http.MethodPatch, "/v1/conversations/"+c.ConversationID, `{"thinkingLevel":"low"}`), http.StatusConflict, "PROMPT_IN_FLIGHT")
	want(t, e.do(http.MethodDelete, "/v1/conversations/"+c.ConversationID, ""), http.StatusConflict, "PROMPT_IN_FLIGHT")

	want(t, e.do(http.MethodPost, "/v1/cancel", ""), http.StatusOK, "")
	if code := <-done; code != http.StatusOK {
		t.Fatalf("cancelled prompt = %d", code)
	}
	cur := decode[conversation.Conversation](t, e.do(http.MethodGet, "/v1/current", ""))
	if cur.Model != "gemini-4-carbon" || cur.ThinkingLevel != "medium" || !cur.Live {
		t.Errorf("settings changed by a 409 patch: %+v", cur)
	}
	want(t, e.do(http.MethodPatch, "/v1/current", `{"model":"claude-sonnet-5-5"}`), http.StatusOK, "")
}

func TestDeleteCurrentClearsPointer(t *testing.T) {
	e := newTestEnv(t)
	c := createConv(t, e, `{}`)

	rec := e.do(http.MethodDelete, "/v1/conversations/"+c.ConversationID, "")
	want(t, rec, http.StatusNoContent, "")
	if v, ok := rec.Header()[HeaderConversation]; ok {
		t.Errorf("delete of current still names %v", v)
	}
	want(t, e.do(http.MethodGet, "/v1/current", ""), http.StatusNotFound, "NO_CURRENT_CONVERSATION")
	want(t, e.do(http.MethodDelete, "/v1/conversations/"+c.ConversationID, ""), http.StatusNotFound, "CONVERSATION_NOT_FOUND")

	// The next prompt creates a fresh conversation.
	res := decode[promptResponse](t, e.do(http.MethodPost, "/v1/prompt", `{"prompt":"hi"}`))
	if res.ConversationID == "" || res.ConversationID == c.ConversationID {
		t.Errorf("prompt after delete went to %q", res.ConversationID)
	}
}
