package conversation

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agyd/internal/acp"
	"agyd/internal/config"
)

func TestUpdateRereadsAfterLock(t *testing.T) {
	h := newHarness(t, Settings{})
	info, _, err := h.m.Startup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	id := info.SessionID
	h.agent.afterGet = func(id string) { h.agent.setLive(id, acp.ConfigModel, "claude-sonnet-5-5") }
	got, err := h.m.Update(t.Context(), id, Settings{Model: "gemini-4-carbon"})
	if err != nil {
		t.Fatal(err)
	}
	live := h.agent.liveSetting(id, acp.ConfigModel)
	if got.Setting(acp.ConfigModel) != "gemini-4-carbon" || live != "gemini-4-carbon" {
		t.Errorf("got=%q live=%q, want gemini-4-carbon", got.Setting(acp.ConfigModel), live)
	}
}

// TestSetDefaultsAndCreateSerializeStateSnapshots proves concurrent SetDefaults
// and Create calls serialize full state.json snapshots so neither defaults nor
// conversations are overwritten by an older snapshot.
func TestSetDefaultsAndCreateSerializeStateSnapshots(t *testing.T) {
	var entered atomic.Int32
	st := &memState{
		onSave: func(config.State) {
			if entered.Add(1) <= 3 {
				for deadline := time.Now().Add(50 * time.Millisecond); entered.Load() < 3 && time.Now().Before(deadline); {
					time.Sleep(time.Millisecond)
				}
			}
		},
	}
	m := New(Options{Agent: newFakeAgent(), State: st, Log: slog.New(slog.DiscardHandler), Workspace: "/ws"})
	if _, _, err := m.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := m.SetDefaults(Settings{Model: "gemini-4-carbon"}, true); err != nil {
			t.Error(err)
		}
	})
	wg.Go(func() {
		if _, err := m.SetDefaults(Settings{ThinkingLevel: "high"}, true); err != nil {
			t.Error(err)
		}
	})
	var createdID string
	wg.Go(func() {
		info, err := m.Create(t.Context(), CreateRequest{Cwd: "/other", NoUse: true})
		if err != nil {
			t.Error(err)
			return
		}
		createdID = info.SessionID
	})
	wg.Wait()

	saved := st.get()
	if saved.Defaults.Model != "gemini-4-carbon" || saved.Defaults.ThinkingLevel != "high" {
		t.Errorf("state.json defaults = %+v; want both keys", saved.Defaults)
	}
	if saved.Conversations[createdID].Cwd != "/other" {
		t.Errorf("state.json conversations = %+v; missing created %q", saved.Conversations, createdID)
	}
	if d := m.Defaults(); d.Model != "gemini-4-carbon" || d.ThinkingLevel != "high" {
		t.Errorf("running defaults = %+v; want both keys", d)
	}
}

func TestDeleteNonLiveCurrentClearsPointer(t *testing.T) {
	h := newHarness(t, Settings{})
	info, _, err := h.m.Startup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h.agent.restart()
	if err := h.m.Delete(t.Context(), info.SessionID); err != nil {
		t.Fatalf("Delete non-live current: %v", err)
	}
	if h.m.CurrentID() != "" || h.state.get().CurrentConversationID != "" {
		t.Errorf("pointer not cleared: %q, state %+v", h.m.CurrentID(), h.state.get())
	}
}
