package conversation

import (
	"testing"
	"time"

	"github.com/kesokaj/agyd/internal/acp"
	"github.com/kesokaj/agyd/internal/config"
)

func TestResumeRoutesFromStateFolderAndFallback(t *testing.T) {
	h := newHarness(t, Settings{})
	h.agent.history["h_empty"] = acp.AgentSession{
		SessionID:  "h_empty",
		Cwd:        "",
		Title:      "outside agyd without cwd",
		UpdateTime: time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC),
	}
	// state.json records h1 in /state/folder-b and h2 in /state/folder-c,
	// whereas agy's session/list reports /hist/one and /hist/two.
	h.state.st = config.State{
		CurrentConversationID: "h1",
		Conversations: map[string]config.ConversationRecord{
			"h1": {Cwd: "/state/folder-b", ThinkingLevel: "high"},
			"h2": {Cwd: "/state/folder-c", Mode: "plan"},
		},
	}

	info, fresh, err := h.m.Startup(t.Context())
	if err != nil || fresh {
		t.Fatalf("Startup = %+v, fresh=%v, err=%v", info, fresh, err)
	}
	if got := h.agent.resumedCwds["h1"]; got != "/state/folder-b" {
		t.Errorf("Startup routed h1 to %q, want /state/folder-b from state.json", got)
	}

	used2, err := h.m.Use(t.Context(), "h2")
	if err != nil || used2.Cwd != "/state/folder-c" {
		t.Fatalf("Use(h2) = %+v, err=%v", used2, err)
	}
	if got := h.agent.resumedCwds["h2"]; got != "/state/folder-c" {
		t.Errorf("Use(h2) routed to %q, want /state/folder-c from state.json", got)
	}

	// History conversation not in state.json with empty session/list cwd falls back to default workspace.
	usedEmpty, err := h.m.Use(t.Context(), "h_empty")
	if err != nil || usedEmpty.Cwd != "/ws" {
		t.Fatalf("Use(h_empty) = %+v, err=%v; want fallback to default workspace /ws", usedEmpty, err)
	}
	if got := h.agent.resumedCwds["h_empty"]; got != "/ws" {
		t.Errorf("Use(h_empty) routed to %q, want /ws", got)
	}
}
