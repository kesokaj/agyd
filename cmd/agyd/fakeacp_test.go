package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fakeACPStore persists sessions and prompt turns across ACP child restarts so
// TestRestartPreservesStateAcrossSIGTERM can verify conversation continuation.
type fakeACPStore struct {
	Seq      int                 `json:"seq"`
	Sessions map[string]fakeSess `json:"sessions"`
}

type fakeSess struct {
	Cwd     string   `json:"cwd"`
	Prompts []string `json:"prompts"`
}

func loadACPStore(path string) fakeACPStore {
	var st fakeACPStore
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Sessions == nil {
		st.Sessions = map[string]fakeSess{}
	}
	return st
}

func saveACPStore(path string, st fakeACPStore) {
	b, _ := json.Marshal(st)
	_ = os.WriteFile(path, b, 0o600)
}

var restartChoices = map[string][]string{
	"model":         {"gemini-4-carbon", "claude-sonnet-5-5"},
	"thought_level": {"low", "medium", "high"},
	"mode":          {"default", "accept-edits", "plan"},
}

func defaultFakeConfigMap() map[string]string {
	return map[string]string{"model": "gemini-4-carbon", "thought_level": "medium", "mode": "default"}
}

func buildFakeConfigOptions(vals map[string]string) []map[string]any {
	var out []map[string]any
	for _, id := range []string{"model", "thought_level", "mode"} {
		var opts []map[string]any
		for _, v := range restartChoices[id] {
			opts = append(opts, map[string]any{"value": v, "name": v})
		}
		out = append(out, map[string]any{
			"id": id, "name": id, "category": id, "type": "select",
			"currentValue": vals[id], "options": opts,
		})
	}
	return out
}

func TestHelperServeAndACP(t *testing.T) {
	if slices.Contains(os.Args, "fake-acp") {
		runFakeACP(os.Getenv("AGYD_TEST_ACP_STORE"), os.Getenv("AGYD_TEST_ACP_PIDFILE"))
		os.Exit(0)
	}
	if os.Getenv("AGYD_TEST_ROLE") == "serve" {
		extra := strings.Fields(os.Getenv("AGYD_TEST_SERVE_ARGS"))
		_ = os.Unsetenv("AGYD_TEST_ROLE")
		_ = os.Unsetenv("AGYD_TEST_SERVE_ARGS")
		os.Args = append([]string{"agyd"}, extra...)
		main()
		os.Exit(0)
	}
}

func runFakeACP(storePath, pidPath string) {
	if pidPath != "" {
		_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	live := map[string]map[string]string{}
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = os.Stdout.Write(append(b, '\n'))
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		reply := func(res any) { send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res}) }
		fail := func(code int, msg string) {
			send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": code, "message": msg}})
		}
		handleFakeACPMethod(storePath, live, req.Method, req.Params, send, reply, fail)
	}
}

func handleFakeACPMethod(storePath string, live map[string]map[string]string, method string, raw json.RawMessage, send, reply func(any), fail func(int, string)) {
	var p struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
		ConfigID  string `json:"configId"`
		Value     string `json:"value"`
		Prompt    []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(raw, &p)
	st := loadACPStore(storePath)
	switch method {
	case "initialize":
		reply(map[string]any{"protocolVersion": 1, "agentInfo": map[string]any{"name": "fake-agy", "version": "1.5.11"}})
	case "session/new":
		st.Seq++
		sid := fmt.Sprintf("sess_%d", st.Seq)
		st.Sessions[sid] = fakeSess{Cwd: p.Cwd}
		saveACPStore(storePath, st)
		live[sid] = defaultFakeConfigMap()
		reply(map[string]any{"sessionId": sid, "configOptions": buildFakeConfigOptions(live[sid])})
	case "session/resume":
		if _, ok := st.Sessions[p.SessionID]; !ok {
			fail(-32603, "Failed to read session "+p.SessionID)
			return
		}
		// Mirror agy 1.5.11: resume resets configOptions to agy defaults.
		live[p.SessionID] = defaultFakeConfigMap()
		reply(map[string]any{"sessionId": p.SessionID, "configOptions": buildFakeConfigOptions(live[p.SessionID])})
	case "session/set_config_option":
		choices, ok := restartChoices[p.ConfigID]
		if !ok || !slices.Contains(choices, p.Value) {
			fail(-32602, "Unknown value: "+p.Value)
			return
		}
		live[p.SessionID][p.ConfigID] = p.Value
		reply(map[string]any{"configOptions": buildFakeConfigOptions(live[p.SessionID])})
	case "session/list":
		var items []map[string]any
		for sid, s := range st.Sessions {
			// Mirror agy: unprompted sessions are absent from conversation_summaries.db.
			if len(s.Prompts) > 0 {
				items = append(items, map[string]any{"sessionId": sid, "cwd": s.Cwd, "updatedAt": "2026-11-18T10:00:00Z"})
			}
		}
		reply(map[string]any{"sessions": items})
	case "session/prompt":
		sess := st.Sessions[p.SessionID]
		promptText := ""
		if len(p.Prompt) > 0 {
			promptText = p.Prompt[0].Text
		}
		sess.Prompts = append(sess.Prompts, promptText)
		st.Sessions[p.SessionID] = sess
		saveACPStore(storePath, st)
		out := "history=" + strings.Join(sess.Prompts, ",")
		send(map[string]any{
			"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]any{
				"sessionId": p.SessionID,
				"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": out}},
			},
		})
		reply(map[string]any{"stopReason": "end_turn"})
	case "session/close":
		delete(live, p.SessionID)
		reply(map[string]any{})
	default:
		fail(-32601, "unknown method "+method)
	}
}
