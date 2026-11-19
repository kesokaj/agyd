package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

type isoProcInspect struct {
	PID    int      `json:"pid"`
	CmdDir string   `json:"cmdDir"`
	PWDEnv string   `json:"pwdEnv"`
	Roots  []string `json:"roots"`
}

type isoSpawnEntry struct {
	PID int    `json:"pid"`
	Dir string `json:"dir"`
}

type isoSessionRecord struct {
	ListCwd string   `json:"listCwd"`
	Prompts []string `json:"prompts"`
}

type isoStore struct {
	Seq      int                         `json:"seq"`
	Sessions map[string]isoSessionRecord `json:"sessions"`
}

func TestHelperIsolationACP(t *testing.T) {
	if os.Getenv("AGYD_TEST_ISO_ACP") != "1" {
		return
	}
	runIsolationFakeACP(os.Getenv("AGYD_TEST_ISO_STORE"), os.Getenv("AGYD_TEST_ISO_SPAWNS"))
	os.Exit(0)
}

func withIsoStoreLock(storePath string, fn func(*isoStore)) {
	lockDir := storePath + ".lock"
	for {
		if err := os.Mkdir(lockDir, 0o700); err == nil {
			break
		}
	}
	defer func() { _ = os.Remove(lockDir) }()

	var st isoStore
	if b, err := os.ReadFile(storePath); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Sessions == nil {
		st.Sessions = map[string]isoSessionRecord{}
	}
	fn(&st)
	b, _ := json.Marshal(st)
	tmp := fmt.Sprintf("%s.tmp.%d", storePath, os.Getpid())
	_ = os.WriteFile(tmp, b, 0o600)
	_ = os.Rename(tmp, storePath)
}

func runIsolationFakeACP(storePath, spawnLogPath string) {
	wd, _ := os.Getwd()
	pid := os.Getpid()
	if spawnLogPath != "" {
		rec, _ := json.Marshal(isoSpawnEntry{PID: pid, Dir: wd})
		if f, err := os.OpenFile(spawnLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			_, _ = f.Write(append(rec, '\n'))
			_ = f.Close()
		}
	}

	roots := []string{wd}
	addRoot := func(cwd string) {
		if cwd != "" && !slices.Contains(roots, cwd) {
			roots = append(roots, cwd)
		}
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

		var p struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			ConfigID  string `json:"configId"`
			Value     string `json:"value"`
		}
		_ = json.Unmarshal(req.Params, &p)

		switch req.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentInfo": map[string]any{"name": "fake-agy", "version": "1.5.11"}})
		case "session/new":
			if strings.Contains(p.Cwd, "fail-new") {
				fail(-32603, "simulated session/new failure")
				continue
			}
			addRoot(p.Cwd)
			var sid string
			withIsoStoreLock(storePath, func(st *isoStore) {
				st.Seq++
				sid = fmt.Sprintf("sess_new_%d", st.Seq)
				st.Sessions[sid] = isoSessionRecord{ListCwd: p.Cwd}
			})
			live[sid] = defaultFakeConfigMap()
			reply(map[string]any{"sessionId": sid, "configOptions": buildFakeConfigOptions(live[sid])})
		case "session/resume":
			var exists bool
			withIsoStoreLock(storePath, func(st *isoStore) {
				_, exists = st.Sessions[p.SessionID]
			})
			if !exists {
				fail(-32603, "Failed to read session "+p.SessionID)
				continue
			}
			addRoot(p.Cwd)
			live[p.SessionID] = defaultFakeConfigMap()
			reply(map[string]any{"sessionId": p.SessionID, "configOptions": buildFakeConfigOptions(live[p.SessionID])})
		case "session/set_config_option":
			choices, ok := restartChoices[p.ConfigID]
			if !ok || !slices.Contains(choices, p.Value) {
				fail(-32602, "Unknown value: "+p.Value)
				continue
			}
			if live[p.SessionID] == nil {
				live[p.SessionID] = defaultFakeConfigMap()
			}
			live[p.SessionID][p.ConfigID] = p.Value
			reply(map[string]any{"configOptions": buildFakeConfigOptions(live[p.SessionID])})
		case "session/list":
			var items []map[string]any
			withIsoStoreLock(storePath, func(st *isoStore) {
				for sid, s := range st.Sessions {
					if len(s.Prompts) > 0 {
						items = append(items, map[string]any{"sessionId": sid, "cwd": s.ListCwd, "updatedAt": "2026-11-18T10:00:00Z"})
					}
				}
			})
			reply(map[string]any{"sessions": items})
		case "session/prompt":
			payload, _ := json.Marshal(isoProcInspect{
				PID:    pid,
				CmdDir: wd,
				PWDEnv: os.Getenv("PWD"),
				Roots:  roots,
			})
			send(map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": p.SessionID,
					"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": string(payload)}},
				},
			})
			reply(map[string]any{"stopReason": "end_turn"})
		case "session/close":
			delete(live, p.SessionID)
			reply(map[string]any{})
		default:
			fail(-32601, "unknown method "+req.Method)
		}
	}
}

func readIsoSpawns(t *testing.T, path string) []isoSpawnEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var out []isoSpawnEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var rec isoSpawnEntry
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("Unmarshal(%q): %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func promptInspect(t *testing.T, baseURL, convID string) isoProcInspect {
	t.Helper()
	url := baseURL + "/v1/prompt"
	if convID != "" {
		url = baseURL + "/v1/conversations/" + convID + "/prompt"
	}
	res := httpJSON(t, http.MethodPost, url, `{"prompt":"inspect"}`, http.StatusOK)
	text, _ := res["text"].(string)
	var got isoProcInspect
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode inspect %q: %v", text, err)
	}
	return got
}

func TestServerPerFolderIsolationAndStateRouting(t *testing.T) {
	tmp := realTempDir(t)
	home := filepath.Join(tmp, "home")
	stateDir := filepath.Join(tmp, "state")
	dirA := filepath.Join(tmp, "dirA")
	dirB := filepath.Join(tmp, "dirB")
	poisonDir := filepath.Join(tmp, "poisonDir")
	failDir := filepath.Join(tmp, "fail-new")
	for _, d := range []string{home, stateDir, dirA, dirB, poisonDir, failDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	storePath := filepath.Join(tmp, "iso-store.json")
	spawnPath := filepath.Join(tmp, "iso-spawns.jsonl")
	initialStore := isoStore{
		Seq: 10,
		Sessions: map[string]isoSessionRecord{
			"sess_saved":  {ListCwd: poisonDir, Prompts: []string{"prior"}},
			"sess_other":  {ListCwd: poisonDir, Prompts: []string{"prior"}},
			"sess_legacy": {ListCwd: "", Prompts: []string{"prior"}},
		},
	}
	rawStore, _ := json.Marshal(initialStore)
	if err := os.WriteFile(storePath, rawStore, 0o600); err != nil {
		t.Fatal(err)
	}

	// Pre-populate state.json: current conversation is in dirB while -workspace is dirA.
	stateJSON := fmt.Sprintf(`{
  "currentConversationId": "sess_saved",
  "conversations": {
    "sess_saved": {"cwd": %q},
    "sess_other": {"cwd": %q}
  }
}`, dirB, dirA)
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), []byte(stateJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	srv := startAgydProcess(t, dirA, map[string]string{
		"HOME":                 home,
		"AGYD_STATE_DIR":       stateDir,
		"AGYD_HOST":            "127.0.0.1",
		"AGYD_PORT":            strconv.Itoa(freeLoopbackPort(t)),
		"AGYD_WORKSPACE":       dirA,
		"AGYD_AGY":             exe,
		"AGYD_ACP_ARGS":        "-test.run=^TestHelperIsolationACP$",
		"AGYD_TEST_ISO_ACP":    "1",
		"AGYD_TEST_ISO_STORE":  storePath,
		"AGYD_TEST_ISO_SPAWNS": spawnPath,
	})
	defer srv.stopSIGTERM(t)

	// Startup resumed sess_saved in dirB from state.json: only dirB's process must have started (not dirA).
	spawns := readIsoSpawns(t, spawnPath)
	if len(spawns) != 1 || spawns[0].Dir != dirB {
		t.Fatalf("startup spawns = %+v, want single process in %s", spawns, dirB)
	}

	inSaved := promptInspect(t, srv.base, "")
	if inSaved.CmdDir != dirB || inSaved.PWDEnv != dirB || !slices.Equal(inSaved.Roots, []string{dirB}) {
		t.Fatalf("sess_saved inspect = %+v, want CmdDir/PWDEnv=%s and roots=[%s]", inSaved, dirB, dirB)
	}

	// Second conversation in dirB shares pidB.
	cB2 := httpJSON(t, http.MethodPost, srv.base+"/v1/conversations", fmt.Sprintf(`{"cwd":%q}`, dirB), http.StatusCreated)
	cB2ID := cB2["conversationId"].(string)
	inB2 := promptInspect(t, srv.base, cB2ID)
	if inB2.PID != inSaved.PID || !slices.Equal(inB2.Roots, []string{dirB}) {
		t.Fatalf("second dirB conversation inspect = %+v, want PID=%d roots=[%s]", inB2, inSaved.PID, dirB)
	}

	// Switching to sess_other routes via state.json (dirA), spawning dirA's process without leaking into dirB.
	usedOther := httpJSON(t, http.MethodPost, srv.base+"/v1/conversations/sess_other/use", "", http.StatusOK)
	if usedOther["cwd"] != dirA {
		t.Fatalf("use sess_other cwd = %v, want %s (from state.json)", usedOther["cwd"], dirA)
	}
	inOther := promptInspect(t, srv.base, "sess_other")
	if inOther.PID == inSaved.PID || inOther.CmdDir != dirA || !slices.Equal(inOther.Roots, []string{dirA}) {
		t.Fatalf("sess_other inspect = %+v, want distinct PID in %s with roots=[%s]", inOther, dirA, dirA)
	}
	if recheckB := promptInspect(t, srv.base, cB2ID); !slices.Equal(recheckB.Roots, []string{dirB}) {
		t.Fatalf("dirB roots after dirA spawn = %v, want [%s]", recheckB.Roots, dirB)
	}

	// Legacy session absent from state.json with empty session/list cwd falls back to -workspace (dirA), sharing pidA.
	usedLegacy := httpJSON(t, http.MethodPost, srv.base+"/v1/conversations/sess_legacy/use", "", http.StatusOK)
	if usedLegacy["cwd"] != dirA {
		t.Fatalf("use sess_legacy cwd = %v, want fallback %s", usedLegacy["cwd"], dirA)
	}
	inLegacy := promptInspect(t, srv.base, "sess_legacy")
	if inLegacy.PID != inOther.PID || !slices.Equal(inLegacy.Roots, []string{dirA}) {
		t.Fatalf("sess_legacy inspect = %+v, want PID=%d roots=[%s]", inLegacy, inOther.PID, dirA)
	}

	// Failed session/new in failDir stops and reaps the temporary failDir process.
	_ = httpJSON(t, http.MethodPost, srv.base+"/v1/conversations", fmt.Sprintf(`{"cwd":%q}`, failDir), http.StatusBadGateway)
	spawnsAfterFail := readIsoSpawns(t, spawnPath)
	failPID := spawnsAfterFail[len(spawnsAfterFail)-1].PID
	if err := syscall.Kill(failPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("failDir PID %d still alive after failed create (kill(0) = %v), want ESRCH", failPID, err)
	}

	// Closing sess_saved leaves cB2 open in dirB: pidB stays alive.
	_ = httpJSON(t, http.MethodDelete, srv.base+"/v1/conversations/sess_saved", "", http.StatusNoContent)
	if err := syscall.Kill(inSaved.PID, 0); err != nil {
		t.Fatalf("dirB PID %d died while %s still open: %v", inSaved.PID, cB2ID, err)
	}

	// Closing cB2 (last in dirB) stops pidB while pidA stays alive.
	_ = httpJSON(t, http.MethodDelete, srv.base+"/v1/conversations/"+cB2ID, "", http.StatusNoContent)
	if err := syscall.Kill(inSaved.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("dirB PID %d still alive after last dirB conversation closed (kill(0) = %v), want ESRCH", inSaved.PID, err)
	}
	if err := syscall.Kill(inOther.PID, 0); err != nil {
		t.Errorf("dirA PID %d died when dirB closed: %v", inOther.PID, err)
	}

	// Closing sess_other and sess_legacy (last in dirA) stops pidA.
	_ = httpJSON(t, http.MethodDelete, srv.base+"/v1/conversations/sess_other", "", http.StatusNoContent)
	if err := syscall.Kill(inOther.PID, 0); err != nil {
		t.Fatalf("dirA PID %d died while sess_legacy still open: %v", inOther.PID, err)
	}
	_ = httpJSON(t, http.MethodDelete, srv.base+"/v1/conversations/sess_legacy", "", http.StatusNoContent)
	if err := syscall.Kill(inOther.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("dirA PID %d still alive after last dirA conversation closed (kill(0) = %v), want ESRCH", inOther.PID, err)
	}
}

func TestServerClearReusesFolderProcAndPreservesSettings(t *testing.T) {
	tmp := realTempDir(t)
	home := filepath.Join(tmp, "home")
	stateDir := filepath.Join(tmp, "state")
	dirA := filepath.Join(tmp, "dirA")
	dirB := filepath.Join(tmp, "dirB")
	for _, d := range []string{home, stateDir, dirA, dirB} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	storePath := filepath.Join(tmp, "iso-store.json")
	spawnPath := filepath.Join(tmp, "iso-spawns.jsonl")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	srv := startAgydProcess(t, dirA, map[string]string{
		"HOME":                 home,
		"AGYD_STATE_DIR":       stateDir,
		"AGYD_HOST":            "127.0.0.1",
		"AGYD_PORT":            strconv.Itoa(freeLoopbackPort(t)),
		"AGYD_WORKSPACE":       dirA,
		"AGYD_AGY":             exe,
		"AGYD_ACP_ARGS":        "-test.run=^TestHelperIsolationACP$",
		"AGYD_TEST_ISO_ACP":    "1",
		"AGYD_TEST_ISO_STORE":  storePath,
		"AGYD_TEST_ISO_SPAWNS": spawnPath,
	})
	defer srv.stopSIGTERM(t)

	// Create a conversation in dirB with non-default settings and verify its process.
	orig := httpJSON(t, http.MethodPost, srv.base+"/v1/conversations",
		fmt.Sprintf(`{"cwd":%q,"model":"claude-sonnet-5-5","thinkingLevel":"high","mode":"plan"}`, dirB), http.StatusCreated)
	origID := orig["conversationId"].(string)
	inOrig := promptInspect(t, srv.base, origID)
	spawnsBefore := len(readIsoSpawns(t, spawnPath))

	// POST /v1/clear creates a fresh conversation on the same dirB process with the same settings.
	cleared := httpJSON(t, http.MethodPost, srv.base+"/v1/clear", "", http.StatusCreated)
	clearedID := cleared["conversationId"].(string)
	if clearedID == origID || cleared["cwd"] != dirB || cleared["model"] != "claude-sonnet-5-5" ||
		cleared["thinkingLevel"] != "high" || cleared["mode"] != "plan" || cleared["current"] != true {
		t.Fatalf("cleared = %+v, want new id in %s with claude-sonnet-5-5/high/plan", cleared, dirB)
	}
	if spawnsAfter := len(readIsoSpawns(t, spawnPath)); spawnsAfter != spawnsBefore {
		t.Errorf("clear spawned extra process: spawns %d -> %d", spawnsBefore, spawnsAfter)
	}
	inCleared := promptInspect(t, srv.base, "")
	if inCleared.PID != inOrig.PID || inCleared.CmdDir != dirB || !slices.Equal(inCleared.Roots, []string{dirB}) {
		t.Fatalf("cleared inspect = %+v, want same PID=%d and roots=[%s]", inCleared, inOrig.PID, dirB)
	}

	// Old conversation remains reachable and intact in state.json alongside the new current one.
	rawState, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatalf("ReadFile(state.json): %v", err)
	}
	var st struct {
		CurrentConversationID string `json:"currentConversationId"`
		Conversations         map[string]struct {
			Cwd           string `json:"cwd"`
			Model         string `json:"model"`
			ThinkingLevel string `json:"thinkingLevel"`
			Mode          string `json:"mode"`
		} `json:"conversations"`
	}
	if err := json.Unmarshal(rawState, &st); err != nil {
		t.Fatalf("unmarshal state.json: %v", err)
	}
	if st.CurrentConversationID != clearedID {
		t.Errorf("state.CurrentConversationID = %q, want %q", st.CurrentConversationID, clearedID)
	}
	for _, id := range []string{origID, clearedID} {
		rec := st.Conversations[id]
		if rec.Cwd != dirB || rec.Model != "claude-sonnet-5-5" || rec.ThinkingLevel != "high" || rec.Mode != "plan" {
			t.Errorf("state.Conversations[%q] = %+v, want %s claude-sonnet-5-5/high/plan", id, rec, dirB)
		}
	}
}
