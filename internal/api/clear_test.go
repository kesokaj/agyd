package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kesokaj/agyd/internal/acp"
	"github.com/kesokaj/agyd/internal/conversation"
)

func TestClearEndpoints(t *testing.T) {
	e := newTestEnv(t)

	t.Run("POST /v1/clear with no current conversation is 404", func(t *testing.T) {
		rec := e.do(http.MethodPost, "/v1/clear", "")
		want(t, rec, http.StatusNotFound, "NO_CURRENT_CONVERSATION")
		if v, ok := rec.Header()[HeaderConversation]; ok {
			t.Errorf("expected no %s header, got %v", HeaderConversation, v)
		}
	})

	dirA := realTempDir(t)
	dirB := realTempDir(t)
	orig := createConv(t, e, `{"cwd":"`+dirA+`","model":"claude-sonnet-5-5","thinkingLevel":"high","mode":"plan"}`)

	t.Run("POST /v1/clear creates fresh conversation in same folder with same settings and keeps old", func(t *testing.T) {
		// Change defaults afterwards to confirm /clear preserves the conversation's
		// own settings instead of reverting to defaults.
		want(t, e.do(http.MethodPut, "/v1/defaults", `{"model":"gemini-4-carbon","thinkingLevel":"low","mode":"default"}`), http.StatusOK, "")

		rec := e.do(http.MethodPost, "/v1/clear", "")
		want(t, rec, http.StatusCreated, "")
		cleared := decode[conversation.Conversation](t, rec)

		if cleared.ConversationID == "" || cleared.ConversationID == orig.ConversationID {
			t.Fatalf("cleared id = %q, want new id != %q", cleared.ConversationID, orig.ConversationID)
		}
		if cleared.Cwd != dirA || cleared.Model != "claude-sonnet-5-5" || cleared.ThinkingLevel != "high" || cleared.Mode != "plan" || !cleared.Live || !cleared.Current {
			t.Errorf("cleared = %+v", cleared)
		}
		if rec.Header().Get(HeaderConversation) != cleared.ConversationID ||
			rec.Header().Get(HeaderModel) != "claude-sonnet-5-5" ||
			rec.Header().Get(HeaderThinkingLevel) != "high" ||
			rec.Header().Get(HeaderMode) != "plan" {
			t.Errorf("headers = %v", rec.Header())
		}

		// Current pointer and /v1/prompt now target the new conversation; literal "/clear" was never sent to ACP.
		cur := decode[conversation.Conversation](t, e.do(http.MethodGet, "/v1/current", ""))
		if cur.ConversationID != cleared.ConversationID {
			t.Errorf("GET /v1/current = %q, want %q", cur.ConversationID, cleared.ConversationID)
		}
		e.svc.mu.Lock()
		promptedBefore := len(e.svc.prompted)
		e.svc.mu.Unlock()
		if promptedBefore != 0 {
			t.Errorf("clear sent %d prompt(s) to ACP, want 0", promptedBefore)
		}
		pRes := decode[promptResponse](t, e.do(http.MethodPost, "/v1/prompt", `{"prompt":"fresh turn"}`))
		if pRes.ConversationID != cleared.ConversationID {
			t.Errorf("prompt after clear went to %q, want %q", pRes.ConversationID, cleared.ConversationID)
		}

		// Old conversation stays in list/history and can still be switched back to.
		oldRec := e.do(http.MethodGet, "/v1/conversations/"+orig.ConversationID, "")
		want(t, oldRec, http.StatusOK, "")
		oldView := decode[conversation.Conversation](t, oldRec)
		if oldView.Current || oldView.Cwd != dirA {
			t.Errorf("old conversation after clear = %+v", oldView)
		}
		list := decode[listConversationsResponse](t, e.do(http.MethodGet, "/v1/conversations?cwd="+dirA, ""))
		if len(list.Items) != 2 {
			t.Errorf("list for dirA has %d items, want 2: %+v", len(list.Items), list.Items)
		}
		want(t, e.do(http.MethodPost, "/v1/conversations/"+orig.ConversationID+"/use", ""), http.StatusOK, "")
		if e.mgr.CurrentID() != orig.ConversationID {
			t.Errorf("use old id failed to switch back: current = %q", e.mgr.CurrentID())
		}
	})

	t.Run("POST /v1/conversations/{id}/clear clears non-current live and history conversations", func(t *testing.T) {
		other := createConv(t, e, `{"cwd":"`+dirB+`","model":"gemini-4-carbon","thinkingLevel":"low","mode":"accept-edits","use":false}`)
		rec := e.do(http.MethodPost, "/v1/conversations/"+other.ConversationID+"/clear", "")
		want(t, rec, http.StatusCreated, "")
		cleared := decode[conversation.Conversation](t, rec)
		if cleared.ConversationID == other.ConversationID || cleared.Cwd != dirB ||
			cleared.Model != "gemini-4-carbon" || cleared.ThinkingLevel != "low" || cleared.Mode != "accept-edits" || !cleared.Current {
			t.Errorf("cleared other = %+v", cleared)
		}
		if rec.Header().Get(HeaderConversation) != cleared.ConversationID || rec.Header().Get(HeaderMode) != "accept-edits" {
			t.Errorf("headers = %v", rec.Header())
		}

		histRec := e.do(http.MethodPost, "/v1/conversations/hist_1/clear", "")
		want(t, histRec, http.StatusCreated, "")
		clearedHist := decode[conversation.Conversation](t, histRec)
		if clearedHist.ConversationID == "hist_1" || clearedHist.Cwd != e.histCwd || !clearedHist.Current {
			t.Errorf("cleared hist_1 = %+v, want new id in %q", clearedHist, e.histCwd)
		}
	})

	t.Run("unknown id and symlink history cwd fail without moving current", func(t *testing.T) {
		beforeCur := e.mgr.CurrentID()
		beforeLive := len(e.svc.ListSessions())
		want(t, e.do(http.MethodPost, "/v1/conversations/nope/clear", ""), http.StatusNotFound, "CONVERSATION_NOT_FOUND")

		base := realTempDir(t)
		realTarget := filepath.Join(base, "real")
		if err := os.MkdirAll(realTarget, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(realTarget, link); err != nil {
			t.Fatal(err)
		}
		e.svc.mu.Lock()
		e.svc.history["hist_link"] = acp.AgentSession{SessionID: "hist_link", Cwd: link, Title: "linked"}
		e.svc.mu.Unlock()

		rec := e.do(http.MethodPost, "/v1/conversations/hist_link/clear", "")
		want(t, rec, http.StatusBadRequest, "INVALID_CWD")
		if !strings.Contains(rec.Body.String(), realTarget) {
			t.Errorf("symlink clear error %s does not name real path %q", rec.Body, realTarget)
		}
		if e.mgr.CurrentID() != beforeCur || len(e.svc.ListSessions()) != beforeLive {
			t.Errorf("failed clears changed state: cur %q->%q, live %d->%d", beforeCur, e.mgr.CurrentID(), beforeLive, len(e.svc.ListSessions()))
		}
	})

	t.Run("wrong method returns 405 with Allow POST and cross-origin POST is 403", func(t *testing.T) {
		for _, p := range []string{"/v1/clear", "/v1/conversations/abc/clear"} {
			rec := e.do(http.MethodGet, p, "")
			want(t, rec, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
			if got := rec.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("GET %s Allow = %q, want POST", p, got)
			}
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/clear", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		Guard(e.h).ServeHTTP(rec, req)
		want(t, rec, http.StatusForbidden, "CROSS_ORIGIN_REQUEST")
	})

	t.Run("clear during in-flight turn creates new current without aborting running turn", func(t *testing.T) {
		busy := createConv(t, e, `{"cwd":"`+dirA+`","model":"claude-sonnet-5-5"}`)
		done := make(chan int, 1)
		go func() {
			done <- e.do(http.MethodPost, "/v1/prompt", `{"prompt":"block_until_cancel"}`).Code
		}()
		e.svc.waitInFlight(t, busy.ConversationID)

		rec := e.do(http.MethodPost, "/v1/clear", "")
		want(t, rec, http.StatusCreated, "")
		cleared := decode[conversation.Conversation](t, rec)
		if cleared.ConversationID == busy.ConversationID || cleared.Cwd != dirA || cleared.Model != "claude-sonnet-5-5" || !cleared.Current {
			t.Errorf("cleared during turn = %+v", cleared)
		}

		want(t, e.do(http.MethodPost, "/v1/conversations/"+busy.ConversationID+"/cancel", ""), http.StatusOK, "")
		if code := <-done; code != http.StatusOK {
			t.Fatalf("in-flight prompt on old conversation exited %d, want 200", code)
		}
	})
}
