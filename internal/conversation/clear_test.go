package conversation

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kesokaj/agyd/internal/acp"
	"github.com/kesokaj/agyd/internal/config"
)

func TestClearCurrentPreservesFolderAndSettings(t *testing.T) {
	// Defaults differ from the current conversation's settings (including a
	// setting that matches agy's own default model) so we prove Clear copies
	// the conversation's live settings rather than falling back to defaults.
	h := newHarness(t, Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "low", Mode: "default"})
	orig, err := h.m.Create(t.Context(), CreateRequest{
		Cwd:      "/proj/alpha",
		Settings: Settings{Model: "gemini-4-carbon", ThinkingLevel: "high", Mode: "default"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Simulate an agent-side mode change not yet written to state.json memos:
	// Clear must read the live session settings rather than stale memos.
	h.agent.setLive(orig.SessionID, acp.ConfigMode, "plan")

	cleared, err := h.m.Clear(t.Context(), "")
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if cleared.SessionID == "" || cleared.SessionID == orig.SessionID {
		t.Fatalf("cleared SessionID = %q, want new id distinct from %q", cleared.SessionID, orig.SessionID)
	}
	if cleared.Cwd != "/proj/alpha" {
		t.Errorf("cleared Cwd = %q, want /proj/alpha", cleared.Cwd)
	}
	if got := SettingsOf(cleared); got != (Settings{Model: "gemini-4-carbon", ThinkingLevel: "high", Mode: "plan"}) {
		t.Errorf("cleared settings = %+v, want gemini-4-carbon/high/plan", got)
	}
	if h.m.CurrentID() != cleared.SessionID {
		t.Errorf("CurrentID = %q, want %q", h.m.CurrentID(), cleared.SessionID)
	}

	// Old conversation stays live, intact in history, and remembered in state.json.
	if _, err := h.agent.GetSession(orig.SessionID); err != nil {
		t.Errorf("old session %q no longer live: %v", orig.SessionID, err)
	}
	st := h.state.get()
	if st.CurrentConversationID != cleared.SessionID {
		t.Errorf("state.CurrentConversationID = %q, want %q", st.CurrentConversationID, cleared.SessionID)
	}
	wantRec := config.ConversationRecord{Cwd: "/proj/alpha", Model: "gemini-4-carbon", ThinkingLevel: "high", Mode: "plan"}
	if st.Conversations[orig.SessionID] != wantRec {
		t.Errorf("state old record = %+v, want %+v", st.Conversations[orig.SessionID], wantRec)
	}
	if st.Conversations[cleared.SessionID] != wantRec {
		t.Errorf("state new record = %+v, want %+v", st.Conversations[cleared.SessionID], wantRec)
	}
	list, err := h.m.List(t.Context(), "/proj/alpha")
	if err != nil || len(list) != 2 {
		t.Fatalf("List(/proj/alpha) = %+v, %v; want both old and new conversations", list, err)
	}
}

func TestClearByIDLiveRememberedAndHistory(t *testing.T) {
	t.Run("non-current live conversation becomes current with its folder and settings", func(t *testing.T) {
		h := newHarness(t, Settings{})
		a, err := h.m.Create(t.Context(), CreateRequest{
			Cwd:      "/proj/a",
			Settings: Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "high", Mode: "plan"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.m.Create(t.Context(), CreateRequest{Cwd: "/proj/b"}); err != nil {
			t.Fatal(err)
		}

		cleared, err := h.m.Clear(t.Context(), a.SessionID)
		if err != nil {
			t.Fatalf("Clear(%s): %v", a.SessionID, err)
		}
		if cleared.SessionID == a.SessionID || cleared.Cwd != "/proj/a" {
			t.Fatalf("cleared = %+v, want new id in /proj/a", cleared)
		}
		if got := SettingsOf(cleared); got != (Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "high", Mode: "plan"}) {
			t.Errorf("cleared settings = %+v", got)
		}
		if h.m.CurrentID() != cleared.SessionID {
			t.Errorf("CurrentID = %q, want %q", h.m.CurrentID(), cleared.SessionID)
		}
	})

	t.Run("non-live remembered conversation uses state folder and settings without resuming", func(t *testing.T) {
		h := newHarness(t, Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "low"})
		orig, err := h.m.Create(t.Context(), CreateRequest{
			Cwd:      "/proj/mem",
			Settings: Settings{ThinkingLevel: "high", Mode: "plan"},
		})
		if err != nil {
			t.Fatal(err)
		}
		h.agent.restart()

		cleared, err := h.m.Clear(t.Context(), orig.SessionID)
		if err != nil {
			t.Fatalf("Clear non-live remembered: %v", err)
		}
		if cleared.SessionID == orig.SessionID || cleared.Cwd != "/proj/mem" {
			t.Fatalf("cleared = %+v, want new id in /proj/mem", cleared)
		}
		if got := SettingsOf(cleared); got != (Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "high", Mode: "plan"}) {
			t.Errorf("cleared settings = %+v", got)
		}
		if _, resumed := h.agent.resumedCwds[orig.SessionID]; resumed {
			t.Errorf("Clear resumed old conversation %q instead of starting fresh", orig.SessionID)
		}
	})

	t.Run("history-only conversation uses history cwd and running defaults", func(t *testing.T) {
		h := newHarness(t, Settings{Model: "claude-sonnet-5-5", Mode: "plan"})
		h.agent.history["h_empty"] = acp.AgentSession{
			SessionID:  "h_empty",
			Cwd:        "",
			UpdateTime: time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC),
		}

		cleared, err := h.m.Clear(t.Context(), "h2")
		if err != nil {
			t.Fatalf("Clear(h2): %v", err)
		}
		if cleared.SessionID == "h2" || cleared.Cwd != "/hist/two" {
			t.Fatalf("cleared = %+v, want new id in /hist/two", cleared)
		}
		if got := SettingsOf(cleared); got.Model != "claude-sonnet-5-5" || got.Mode != "plan" {
			t.Errorf("cleared settings = %+v, want running defaults applied", got)
		}

		clearedEmpty, err := h.m.Clear(t.Context(), "h_empty")
		if err != nil {
			t.Fatalf("Clear(h_empty): %v", err)
		}
		if clearedEmpty.Cwd != "/ws" {
			t.Errorf("cleared h_empty Cwd = %q, want fallback /ws", clearedEmpty.Cwd)
		}
	})

	t.Run("remembered retired model degrades leniently with warning", func(t *testing.T) {
		h := newHarness(t, Settings{})
		h.state.st = config.State{
			Conversations: map[string]config.ConversationRecord{
				"h1": {Cwd: "/proj/old", Model: "retired-model", ThinkingLevel: "high"},
			},
		}
		h.m.loadState(h.state.st)

		cleared, err := h.m.Clear(t.Context(), "h1")
		if err != nil {
			t.Fatalf("Clear retired model: %v", err)
		}
		if cleared.Cwd != "/proj/old" || cleared.Setting(acp.ConfigModel) != "gemini-4-carbon" || cleared.Setting(acp.ConfigThinking) != "high" {
			t.Errorf("cleared = %+v, want /proj/old, agy default model, and high thinking", cleared)
		}
		if !strings.Contains(h.logs.String(), "retired-model") {
			t.Errorf("expected warning for retired-model in logs:\n%s", h.logs)
		}
	})
}

func TestClearErrorsAndConcurrency(t *testing.T) {
	h := newHarness(t, Settings{})
	if _, err := h.m.Clear(t.Context(), ""); !errors.Is(err, ErrNoCurrent) {
		t.Fatalf("Clear('') with no current = %v, want ErrNoCurrent", err)
	}
	if h.agent.newCount != 0 {
		t.Fatalf("failed Clear created %d sessions", h.agent.newCount)
	}

	orig, err := h.m.Create(t.Context(), CreateRequest{Cwd: "/proj/keep"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Clear(t.Context(), "nope"); !errors.Is(err, acp.ErrSessionNotFound) {
		t.Fatalf("Clear('nope') = %v, want ErrSessionNotFound", err)
	}
	if h.m.CurrentID() != orig.SessionID {
		t.Errorf("failed Clear moved current to %q, want %q", h.m.CurrentID(), orig.SessionID)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			info, err := h.m.Clear(t.Context(), "")
			if err != nil {
				t.Errorf("concurrent Clear: %v", err)
			}
			if info.Cwd != "/proj/keep" {
				t.Errorf("concurrent Clear Cwd = %q, want /proj/keep", info.Cwd)
			}
		})
	}
	wg.Wait()
	if cur, saved := h.m.CurrentID(), h.state.get().CurrentConversationID; cur == "" || cur == orig.SessionID || cur != saved {
		t.Errorf("post-concurrent current=%q, state=%q", cur, saved)
	}
}
