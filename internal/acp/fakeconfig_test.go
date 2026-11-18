package acp

import (
	"encoding/json"
	"fmt"
	"slices"
)

// fakeConfig is the fake agent's configOptions and history, mirroring what
// agy 1.5.11 does (probe 2026-11-18): set_config_option returns the full
// list, list ignores cwd, resume resets options to defaults.
type fakeConfig struct {
	values  map[string]map[string]string // sessionId -> configId -> value
	created map[string]string            // sessionId -> cwd, from session/new
}

var fakeChoices = map[string][]string{
	"model":         {"gemini-4-carbon", "claude-sonnet-5-5", "gemini-3.8-flash"},
	"thought_level": {"low", "medium", "high"},
	"mode":          {"default", "accept-edits", "plan"},
}

var fakeHistory = []map[string]any{
	{"sessionId": "hist_1", "cwd": "/tmp/hist", "title": "Older chat", "updatedAt": "2026-11-01T10:00:00Z"},
	{"sessionId": "hist_2", "cwd": "/tmp/hist2", "title": "Newer chat", "updatedAt": "2026-11-10T10:00:00Z"},
}

func newFakeConfig() *fakeConfig {
	return &fakeConfig{values: map[string]map[string]string{}, created: map[string]string{}}
}

func (f *fakeConfig) reset(sid string) {
	f.values[sid] = map[string]string{"model": "gemini-4-carbon", "thought_level": "medium", "mode": "default"}
}

func (f *fakeConfig) options(sid string) []map[string]any {
	var out []map[string]any
	for _, id := range []string{"model", "thought_level", "mode"} {
		var opts []map[string]any
		for _, v := range fakeChoices[id] {
			opts = append(opts, map[string]any{"value": v, "name": v})
		}
		out = append(out, map[string]any{
			"id": id, "name": id, "category": id, "type": "select",
			"currentValue": f.values[sid][id], "options": opts,
		})
	}
	return out
}

func (f *fakeConfig) known(sid string) (cwd string, ok bool) {
	if cwd, ok := f.created[sid]; ok {
		return cwd, true
	}
	for _, h := range fakeHistory {
		if h["sessionId"] == sid {
			return h["cwd"].(string), true
		}
	}
	return "", false
}

// handle answers the config and history methods; it reports false for any
// other method.
func (f *fakeConfig) handle(msg rpcMessage, send func(any)) bool {
	reply := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result}) }
	fail := func(code int, text string) {
		send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": code, "message": text}})
	}
	var p struct {
		SessionID string `json:"sessionId"`
		ConfigID  string `json:"configId"`
		Value     string `json:"value"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	switch msg.Method {
	case "session/set_config_option":
		choices, ok := fakeChoices[p.ConfigID]
		switch {
		case !ok:
			fail(-32602, "Unknown config option: "+p.ConfigID)
		case !slices.Contains(choices, p.Value):
			fail(-32602, "Unknown model ID: "+p.Value)
		default:
			f.values[p.SessionID][p.ConfigID] = p.Value
			reply(map[string]any{"configOptions": f.options(p.SessionID)})
		}
	case "session/list":
		sessions := slices.Clone(fakeHistory)
		for sid, cwd := range f.created {
			sessions = append(sessions, map[string]any{"sessionId": sid, "cwd": cwd, "title": "", "updatedAt": "2026-11-18T09:00:00Z"})
		}
		reply(map[string]any{"sessions": sessions})
	case "session/resume":
		if _, ok := f.known(p.SessionID); !ok {
			fail(-32603, fmt.Sprintf("Failed to read session %s; retry, or load it with session/load", p.SessionID))
			return true
		}
		f.reset(p.SessionID)
		reply(map[string]any{"sessionId": p.SessionID, "configOptions": f.options(p.SessionID)})
	default:
		return false
	}
	return true
}
