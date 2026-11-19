package conversation

import (
	"slices"
	"testing"

	"github.com/kesokaj/agyd/internal/acp"
)

// TestModelsFollowLiveCurrent: a model list agy changes on its own
// (config_option_update on the live current conversation) reaches Models
// without a new conversation being created.
func TestModelsFollowLiveCurrent(t *testing.T) {
	h := newHarness(t, Settings{})
	info, _, err := h.m.Startup(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	h.agent.mu.Lock()
	live := h.agent.live[info.SessionID]
	opts := slices.Clone(live.ConfigOptions)
	for i := range opts {
		if opts[i].ID == acp.ConfigModel {
			opts[i].Options = append(slices.Clone(opts[i].Options), acp.ConfigValue{Value: "gemini-5-new", Name: "Gemini 5 New"})
		}
	}
	live.ConfigOptions = opts
	h.agent.live[info.SessionID] = live
	h.agent.mu.Unlock()

	models, err := h.m.Models()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range models {
		if o.ID == acp.ConfigModel && slices.ContainsFunc(o.Options, func(v acp.ConfigValue) bool { return v.Value == "gemini-5-new" }) {
			return
		}
	}
	t.Errorf("Models = %+v; want the model agy added to the live current conversation", models)
}
