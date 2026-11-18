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
		sessions: map[string]SessionInfo{},
		inFlight: map[string]*promptSubscriber{},
	}
	c.sessions["s1"] = SessionInfo{SessionID: "s1", ConfigOptions: []ConfigOption{
		{ID: ConfigModel, Type: "select", CurrentValue: "gemini-4-carbon", Options: []ConfigValue{{Value: "gemini-4-carbon"}}},
		{ID: ConfigMode, Type: "select", CurrentValue: "default", Options: []ConfigValue{{Value: "default"}, {Value: "plan"}}},
	}}
	send := func(update map[string]any) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"sessionId": "s1", "update": update})
		if err != nil {
			t.Fatal(err)
		}
		c.handleSessionUpdate(raw)
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
	c.handleSessionUpdate(raw)
	if _, ok := c.sessions["ghost"]; ok {
		t.Error("update for an unknown session registered it")
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
