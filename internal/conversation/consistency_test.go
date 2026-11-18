package conversation

import (
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agyd/internal/acp"
)

// A change landing between Update's read and its opMu must not be skipped as
// "already set": the result, agy and the state file all agree.
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
	if got.Setting(acp.ConfigModel) != "gemini-4-carbon" || live != "gemini-4-carbon" || h.state.get().Model != "gemini-4-carbon" {
		t.Errorf("returned %q, agy %q, state %q; want gemini-4-carbon everywhere",
			got.Setting(acp.ConfigModel), live, h.state.get().Model)
	}
}

// Two concurrent SetDefaults(persist) calls must not lose either key in
// config.env: the read-merge-write of the file is serialized.
func TestSetDefaultsSerializesPersist(t *testing.T) {
	var (
		fileMu  sync.Mutex
		file    = map[string]string{}
		entered atomic.Int32
	)
	save := func(s Settings) error {
		entered.Add(1)
		fileMu.Lock()
		snapshot := maps.Clone(file)
		fileMu.Unlock()
		// Give a second unserialized caller time to read the same snapshot.
		for deadline := time.Now().Add(100 * time.Millisecond); entered.Load() < 2 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		if s.Model != "" {
			snapshot["AGYD_MODEL"] = s.Model
		}
		if s.ThinkingLevel != "" {
			snapshot["AGYD_THINKING"] = s.ThinkingLevel
		}
		fileMu.Lock()
		file = snapshot
		fileMu.Unlock()
		return nil
	}
	m := New(Options{Agent: newFakeAgent(), State: &memState{}, Log: slog.New(slog.DiscardHandler), Workspace: "/ws", SaveDefaults: save})
	var wg sync.WaitGroup
	for _, s := range []Settings{{Model: "gemini-4-carbon"}, {ThinkingLevel: "high"}} {
		wg.Go(func() {
			if _, err := m.SetDefaults(s, true); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if file["AGYD_MODEL"] != "gemini-4-carbon" || file["AGYD_THINKING"] != "high" {
		t.Errorf("config.env = %v; want both keys", file)
	}
	if d := m.Defaults(); d.Model != "gemini-4-carbon" || d.ThinkingLevel != "high" {
		t.Errorf("running defaults = %+v; want both keys", d)
	}
}

// Deleting the current conversation after agy restarted (so it is no longer
// live) still clears the pointer instead of leaving it dangling.
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
