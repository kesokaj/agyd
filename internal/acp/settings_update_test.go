package acp

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
)

// TestSettingsUpdatesFromAgent: config_option_update and current_mode_update
// notifications agy sends on its own keep the session registry current, so
// GetSession (and the visibility headers and /v1/models built from it) never
// report settings agy has already changed.
func TestSettingsUpdatesFromAgent(t *testing.T) {
	c := &Client{
		log:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		sessions: map[string]sessionEntry{},
		inFlight: map[string]*promptSubscriber{},
	}
	c.sessions["s1"] = sessionEntry{info: SessionInfo{SessionID: "s1", ConfigOptions: []ConfigOption{
		{ID: ConfigModel, Type: "select", CurrentValue: "gemini-4-carbon", Options: []ConfigValue{{Value: "gemini-4-carbon"}}},
		{ID: ConfigMode, Type: "select", CurrentValue: "default", Options: []ConfigValue{{Value: "default"}, {Value: "plan"}}},
	}}}
	send := func(update map[string]any) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"sessionId": "s1", "update": update})
		if err != nil {
			t.Fatal(err)
		}
		c.handleSessionUpdate(nil, raw)
	}

	send(map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": "plan"})
	if got := mustGet(t, c, "s1").Setting(ConfigMode); got != "plan" {
		t.Errorf("mode after current_mode_update = %q, want plan", got)
	}

	send(map[string]any{"sessionUpdate": "config_option_update", "configOptions": []map[string]any{
		{"id": ConfigModel, "type": "select", "currentValue": "gemini-5-new", "options": []map[string]string{{"value": "gemini-4-carbon"}, {"value": "gemini-5-new"}}},
		{"id": ConfigMode, "type": "select", "currentValue": "plan", "options": []map[string]string{{"value": "default"}, {"value": "plan"}}},
	}})
	info := mustGet(t, c, "s1")
	if got := info.Setting(ConfigModel); got != "gemini-5-new" {
		t.Errorf("model after config_option_update = %q, want gemini-5-new", got)
	}
	if n := len(info.ConfigOptions[0].Options); n != 2 {
		t.Errorf("model choices = %d, want 2", n)
	}

	// A malformed update and an update for an unknown session change nothing.
	send(map[string]any{"sessionUpdate": "config_option_update", "configOptions": "nope"})
	send(map[string]any{"sessionUpdate": "current_mode_update"})
	if got := mustGet(t, c, "s1").Setting(ConfigMode); got != "plan" {
		t.Errorf("mode after malformed updates = %q, want plan", got)
	}
	raw, _ := json.Marshal(map[string]any{"sessionId": "ghost", "update": map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": "plan"}})
	c.handleSessionUpdate(nil, raw)
	if _, ok := c.sessions["ghost"]; ok {
		t.Error("update for an unknown session registered it")
	}

	// An update from a different folder process must not mutate or inject chunks into s1.
	owner := &folderProc{dir: "/folder/a"}
	foreign := &folderProc{dir: "/folder/b"}
	entry := c.sessions["s1"]
	entry.proc = owner
	c.sessions["s1"] = entry
	sub := &promptSubscriber{}
	c.inFlight["s1"] = sub

	spoofMode, _ := json.Marshal(map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": "default"}})
	spoofChunk, _ := json.Marshal(map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "spoofed"}}})
	c.handleSessionUpdate(foreign, spoofMode)
	c.handleSessionUpdate(foreign, spoofChunk)
	if got := mustGet(t, c, "s1").Setting(ConfigMode); got != "plan" {
		t.Errorf("foreign proc changed s1 mode to %q, want plan", got)
	}
	if sub.text.String() != "" || len(sub.updates) != 0 {
		t.Errorf("foreign proc injected prompt output %q (%d updates)", sub.text.String(), len(sub.updates))
	}

	// Buffered updates from the owning folder proc preserve multiple verbatim ACP
	// updates in arrival order, keep agent_message_chunk text raw without trimming,
	// and skip malformed notifications that omit the "update" field.
	c.handleSessionUpdate(owner, json.RawMessage(`{"sessionId":"s1"}`))

	wantUpdates := []string{
		`{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"plan step 1"}}`,
		`{"sessionUpdate":"tool_call","toolCallId":"tc_1","kind":"read","title":"Read README.md","status":"in_progress"}`,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"  Raw line 1\n\nLine 2  "}}`,
	}
	for _, u := range wantUpdates {
		c.handleSessionUpdate(owner, json.RawMessage(`{"sessionId":"s1","update":`+u+`}`))
	}

	if len(sub.updates) != len(wantUpdates) {
		t.Fatalf("len(sub.updates) = %d, want %d (got %v)", len(sub.updates), len(wantUpdates), sub.updates)
	}
	for i, want := range wantUpdates {
		if got := string(sub.updates[i]); got != want {
			t.Errorf("sub.updates[%d] = %s, want %s", i, got, want)
		}
	}
	if wantText := "  Raw line 1\n\nLine 2  "; sub.text.String() != wantText {
		t.Errorf("sub.text = %q, want %q", sub.text.String(), wantText)
	}
}

func mustGet(t *testing.T, c *Client, id string) SessionInfo {
	t.Helper()
	info, err := c.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
