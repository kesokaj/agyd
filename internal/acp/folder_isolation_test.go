package acp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
)

type procInspect struct {
	PID    int      `json:"pid"`
	CmdDir string   `json:"cmdDir"`
	PWDEnv string   `json:"pwdEnv"`
	Roots  []string `json:"roots"`
}

type procSpawnRecord struct {
	PID int    `json:"pid"`
	Dir string `json:"dir"`
}

func TestHelperFolderACP(t *testing.T) {
	if os.Getenv("GO_WANT_FOLDER_ACP") != "1" {
		return
	}
	runFolderFakeACP(os.Stdin, os.Stdout)
	os.Exit(0)
}

// runFolderFakeACP reproduces agy 1.5.11's workspace-root behavior (BUG-002):
// each process starts with its own working directory (exec.Cmd.Dir) as an
// initial workspace root and appends every session/new or session/resume cwd.
func runFolderFakeACP(r io.Reader, w io.Writer) {
	wd, _ := os.Getwd()
	pid := os.Getpid()
	if logPath := os.Getenv("AGYD_TEST_PROC_LOG"); logPath != "" {
		rec, _ := json.Marshal(procSpawnRecord{PID: pid, Dir: wd})
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
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

	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "%s\n", b)
	}

	cfg := newFakeConfig()
	seq := 0
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var msg rpcMessage
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		if msg.Method == "session/resume" {
			var rp struct {
				SessionID string `json:"sessionId"`
				Cwd       string `json:"cwd"`
			}
			_ = json.Unmarshal(msg.Params, &rp)
			addRoot(rp.Cwd)
			cfg.reset(rp.SessionID)
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result":  map[string]any{"sessionId": rp.SessionID, "configOptions": cfg.options(rp.SessionID)},
			})
			continue
		}
		if cfg.handle(msg, send) {
			continue
		}
		switch msg.Method {
		case "initialize":
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result": map[string]any{
					"protocolVersion": 1,
					"agentInfo":       map[string]any{"name": "fake-agy", "version": "1.5.11"},
				},
			})
		case "session/new":
			var np sessionNewParams
			_ = json.Unmarshal(msg.Params, &np)
			if strings.Contains(np.Cwd, "fail-new") {
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"error":   map[string]any{"code": -32603, "message": "simulated session/new failure"},
				})
				continue
			}
			seq++
			sid := fmt.Sprintf("sess_%d_%d", pid, seq)
			addRoot(np.Cwd)
			cfg.created[sid] = np.Cwd
			cfg.reset(sid)
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result":  map[string]any{"sessionId": sid, "configOptions": cfg.options(sid)},
			})
		case "session/close":
			send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{}})
		case "session/prompt":
			var pp sessionPromptParams
			_ = json.Unmarshal(msg.Params, &pp)
			payload, _ := json.Marshal(procInspect{
				PID:    pid,
				CmdDir: wd,
				PWDEnv: os.Getenv("PWD"),
				Roots:  roots,
			})
			send(map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": pp.SessionID,
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": string(payload)},
					},
				},
			})
			send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"stopReason": "end_turn"}})
		}
	}
}

func readSpawnLog(t *testing.T, path string) []procSpawnRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var out []procSpawnRecord
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var rec procSpawnRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("Unmarshal spawn line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func inspectSession(t *testing.T, c *Client, sessionID string) procInspect {
	t.Helper()
	res, err := c.Prompt(t.Context(), sessionID, []ContentBlock{{Type: "text", Text: "inspect"}}, nil)
	if err != nil {
		t.Fatalf("Prompt(%s): %v", sessionID, err)
	}
	var got procInspect
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatalf("decode inspect %q: %v", res.Text, err)
	}
	return got
}

func TestPerFolderProcessIsolation(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := realTempDir(t)
	defaultWs := filepath.Join(root, "default")
	folderA := filepath.Join(root, "FolderA")
	folderB := filepath.Join(root, "FolderB")
	failDir := filepath.Join(root, "fail-new")
	for _, d := range []string{defaultWs, folderA, folderB, failDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	spawnLog := filepath.Join(root, "spawns.jsonl")

	c, err := NewClient(Config{
		Binary:           exe,
		Args:             []string{"-test.run=^TestHelperFolderACP$"},
		Env:              append(os.Environ(), "GO_WANT_FOLDER_ACP=1", "AGYD_TEST_PROC_LOG="+spawnLog, "PWD=/stale/parent/pwd"),
		DefaultWorkspace: defaultWs,
		PermissionPolicy: PermissionAllowAll,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := t.Context()

	// Before the first conversation, zero agy processes are running.
	if spawns := readSpawnLog(t, spawnLog); len(spawns) != 0 {
		t.Fatalf("spawns before first conversation = %+v, want none", spawns)
	}

	// First conversation in folderA starts folderA's process with cmd.Dir = folderA (not defaultWs).
	sA1, err := c.NewSession(ctx, folderA)
	if err != nil {
		t.Fatalf("NewSession(folderA): %v", err)
	}
	inA1 := inspectSession(t, c, sA1.SessionID)
	if inA1.CmdDir != folderA || inA1.PWDEnv != folderA {
		t.Fatalf("folderA inspect = %+v; want CmdDir=%q and PWDEnv=%q", inA1, folderA, folderA)
	}
	if !slices.Equal(inA1.Roots, []string{folderA}) {
		t.Fatalf("folderA roots = %v, want [%s] only", inA1.Roots, folderA)
	}

	// Second conversation in folderA shares the same process.
	sA2, err := c.NewSession(ctx, folderA)
	if err != nil {
		t.Fatalf("second NewSession(folderA): %v", err)
	}
	inA2 := inspectSession(t, c, sA2.SessionID)
	if inA2.PID != inA1.PID || !slices.Equal(inA2.Roots, []string{folderA}) {
		t.Fatalf("second session in folderA = %+v, want PID=%d and roots=[%s]", inA2, inA1.PID, folderA)
	}

	// Conversation in folderB starts a second process with cmd.Dir = folderB; neither folder leaks into the other.
	sB1, err := c.NewSession(ctx, folderB)
	if err != nil {
		t.Fatalf("NewSession(folderB): %v", err)
	}
	inB1 := inspectSession(t, c, sB1.SessionID)
	if inB1.PID == inA1.PID {
		t.Fatalf("folderB reused folderA PID %d; want distinct per-folder processes", inB1.PID)
	}
	if inB1.CmdDir != folderB || inB1.PWDEnv != folderB || !slices.Equal(inB1.Roots, []string{folderB}) {
		t.Fatalf("folderB inspect = %+v, want CmdDir/PWDEnv=%q and roots=[%s]", inB1, folderB, folderB)
	}
	if againA := inspectSession(t, c, sA2.SessionID); !slices.Equal(againA.Roots, []string{folderA}) {
		t.Fatalf("folderA roots after folderB creation = %v, want [%s]", againA.Roots, folderA)
	}

	st, err := c.Status(ctx)
	if err != nil || !st.Ready || st.ActiveSessionCount != 3 {
		t.Fatalf("Status = %+v, err = %v; want Ready=true ActiveSessionCount=3", st, err)
	}
	if spawns := readSpawnLog(t, spawnLog); len(spawns) != 2 {
		t.Fatalf("spawns after Status with live procs = %+v, want 2", spawns)
	}

	// Case-insensitive filesystem check (e.g. macOS APFS): case-variant spelling reuses folderB's process.
	lowerB := filepath.Join(root, "folderb")
	if fi1, err1 := os.Stat(folderB); err1 == nil {
		if fi2, err2 := os.Stat(lowerB); err2 == nil && os.SameFile(fi1, fi2) {
			sB2, err := c.NewSession(ctx, lowerB)
			if err != nil {
				t.Fatalf("NewSession(case-variant %q): %v", lowerB, err)
			}
			inB2 := inspectSession(t, c, sB2.SessionID)
			if inB2.PID != inB1.PID || !slices.Equal(inB2.Roots, []string{folderB}) {
				t.Errorf("case-variant inspect = %+v, want PID=%d roots=[%s]", inB2, inB1.PID, folderB)
			}
			if err := c.CloseSession(ctx, sB2.SessionID); err != nil {
				t.Fatalf("CloseSession(sB2): %v", err)
			}
		}
	}

	// Failed session/new in a fresh folder stops that folder's process immediately.
	if _, err := c.NewSession(ctx, failDir); err == nil {
		t.Fatal("NewSession(failDir) succeeded, want error")
	}
	spawnsAfterFail := readSpawnLog(t, spawnLog)
	failPID := spawnsAfterFail[len(spawnsAfterFail)-1].PID
	if err := syscall.Kill(failPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("failed-folder PID %d still alive (kill(0) = %v), want ESRCH", failPID, err)
	}

	// Closing sA1 leaves sA2 in folderA: pidA stays alive.
	if err := c.CloseSession(ctx, sA1.SessionID); err != nil {
		t.Fatalf("CloseSession(sA1): %v", err)
	}
	if err := syscall.Kill(inA1.PID, 0); err != nil {
		t.Fatalf("folderA PID %d died while sA2 still live: %v", inA1.PID, err)
	}

	// Closing sB1 (last in folderB) stops and reaps pidB immediately while pidA stays alive.
	if err := c.CloseSession(ctx, sB1.SessionID); err != nil {
		t.Fatalf("CloseSession(sB1): %v", err)
	}
	if err := syscall.Kill(inB1.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("folderB PID %d still alive after last session closed (kill(0) = %v), want ESRCH", inB1.PID, err)
	}
	if err := syscall.Kill(inA1.PID, 0); err != nil {
		t.Errorf("folderA PID %d died when folderB closed: %v", inA1.PID, err)
	}

	// Closing sA2 (last in folderA) stops and reaps pidA immediately.
	if err := c.CloseSession(ctx, sA2.SessionID); err != nil {
		t.Fatalf("CloseSession(sA2): %v", err)
	}
	if err := syscall.Kill(inA1.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("folderA PID %d still alive after last session closed (kill(0) = %v), want ESRCH", inA1.PID, err)
	}

	// Resuming sB1 in folderB spawns a fresh process for folderB only.
	resumedB, err := c.ResumeSession(ctx, sB1.SessionID, folderB)
	if err != nil {
		t.Fatalf("ResumeSession(sB1, folderB): %v", err)
	}
	inResumedB := inspectSession(t, c, resumedB.SessionID)
	if inResumedB.PID == inB1.PID || inResumedB.CmdDir != folderB || !slices.Equal(inResumedB.Roots, []string{folderB}) {
		t.Errorf("resumed folderB inspect = %+v, want new PID in %s with roots=[%s]", inResumedB, folderB, folderB)
	}
}

func TestRelativeBinaryNotHijackedByWorkspaceDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := realTempDir(t)
	callerDir := filepath.Join(root, "caller")
	defaultWs := filepath.Join(root, "default")
	folderA := filepath.Join(root, "folderA")
	for _, d := range []string{filepath.Join(callerDir, "bin"), defaultWs, filepath.Join(folderA, "bin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	legit := filepath.Join(callerDir, "bin", "fake-agy")
	legitScript := fmt.Sprintf("#!/bin/sh\nexec %q \"$@\"\n", exe)
	if err := os.WriteFile(legit, []byte(legitScript), 0o700); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(root, "hijacked")
	poisonScript := fmt.Sprintf("#!/bin/sh\necho hijacked > %q\nexit 1\n", marker)
	for _, p := range []string{filepath.Join(folderA, "bin", "fake-agy"), filepath.Join(folderA, "fake-agy")} {
		if err := os.WriteFile(p, []byte(poisonScript), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(callerDir)
	t.Setenv("PATH", "./bin:"+os.Getenv("PATH"))

	for _, binSpec := range []string{"./bin/fake-agy", "fake-agy"} {
		c, err := NewClient(Config{
			Binary:           binSpec,
			Args:             []string{"-test.run=^TestHelperFolderACP$"},
			Env:              append(os.Environ(), "GO_WANT_FOLDER_ACP=1"),
			DefaultWorkspace: defaultWs,
			PermissionPolicy: PermissionAllowAll,
		}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("NewClient(%q): %v", binSpec, err)
		}
		sess, err := c.NewSession(t.Context(), folderA)
		if err != nil {
			_ = c.Close()
			t.Fatalf("NewSession(%q): %v", binSpec, err)
		}
		got := inspectSession(t, c, sess.SessionID)
		_ = c.Close()
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("workspace binary hijacked execution for %q", binSpec)
		}
		if got.CmdDir != folderA {
			t.Fatalf("NewSession(%q) CmdDir = %q, want %q", binSpec, got.CmdDir, folderA)
		}
	}
}

func TestConcurrentFolderProcessLifecycle(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := realTempDir(t)
	dirs := []string{filepath.Join(root, "dir1"), filepath.Join(root, "dir2")}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	spawnLog := filepath.Join(root, "concurrent-spawns.jsonl")

	c, err := NewClient(Config{
		Binary:           exe,
		Args:             []string{"-test.run=^TestHelperFolderACP$"},
		Env:              append(os.Environ(), "GO_WANT_FOLDER_ACP=1", "AGYD_TEST_PROC_LOG="+spawnLog),
		DefaultWorkspace: dirs[0],
		PermissionPolicy: PermissionAllowAll,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx := t.Context()
	var wg sync.WaitGroup
	for i := range 8 {
		dir := dirs[i%len(dirs)]
		wg.Go(func() {
			s, err := c.NewSession(ctx, dir)
			if err != nil {
				t.Errorf("NewSession(%s): %v", dir, err)
				return
			}
			got := inspectSession(t, c, s.SessionID)
			if got.CmdDir != dir || !slices.Equal(got.Roots, []string{dir}) {
				t.Errorf("inspect(%s) = %+v, want CmdDir=%s roots=[%s]", s.SessionID, got, dir, dir)
			}
			if _, err := c.Status(ctx); err != nil {
				t.Errorf("Status: %v", err)
			}
			if err := c.CloseSession(ctx, s.SessionID); err != nil {
				t.Errorf("CloseSession(%s): %v", s.SessionID, err)
			}
		})
	}
	wg.Wait()

	if got := c.ListSessions(); len(got) != 0 {
		t.Fatalf("ListSessions after all closed = %+v, want empty", got)
	}
	for _, rec := range readSpawnLog(t, spawnLog) {
		if err := syscall.Kill(rec.PID, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("spawned PID %d (%s) still alive after all sessions closed: %v", rec.PID, rec.Dir, err)
		}
	}
}
