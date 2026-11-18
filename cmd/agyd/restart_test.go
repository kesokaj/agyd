package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) WriteString(str string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.WriteString(str)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type runningAgyd struct {
	cmd     *exec.Cmd
	base    string
	acpPID  func() int
	logs    *syncBuf
	drained <-chan struct{}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func startAgydProcess(t *testing.T, cwd string, env map[string]string) *runningAgyd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	port := env["AGYD_PORT"]
	addr := "127.0.0.1:" + port
	cmd := exec.Command(exe, "-test.run=^TestHelperServeAndACP$")
	cmd.Dir = cwd
	// Nil Stdin attaches /dev/null (as in detached containers and background daemons)
	// with no -no-prompt: proves /dev/null is not misdetected as a terminal.
	cmd.Stdin = nil
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "AGYD_TEST_ROLE=serve"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	logs := &syncBuf{}
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start agyd: %v", err)
	}

	listening := make(chan error, 1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		sc := bufio.NewScanner(stdout)
		sent := false
		for sc.Scan() {
			line := sc.Text()
			_, _ = logs.WriteString(line + "\n")
			if !sent && strings.Contains(line, `"message":"listening"`) && strings.Contains(line, `"addr":"`+addr+`"`) {
				sent = true
				listening <- nil
			}
		}
		if !sent {
			listening <- fmt.Errorf("agyd exited before listening on %s:\n%s", addr, logs.String())
		}
	}()

	select {
	case err := <-listening:
		if err != nil {
			<-drained
			_ = cmd.Wait()
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-drained
		_ = cmd.Wait()
		t.Fatalf("timed out waiting for agyd to listen on %s:\n%s", addr, logs.String())
	}

	pidFile := env["AGYD_TEST_ACP_PIDFILE"]
	r := &runningAgyd{
		cmd:  cmd,
		base: "http://" + addr,
		acpPID: func() int {
			b, _ := os.ReadFile(pidFile)
			n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			return n
		},
		logs:    logs,
		drained: drained,
	}
	t.Cleanup(func() {
		if r.cmd.ProcessState == nil {
			_ = r.cmd.Process.Kill()
			<-r.drained
			_ = r.cmd.Wait()
		}
	})
	return r
}

func (r *runningAgyd) stopSIGTERM(t *testing.T) {
	t.Helper()
	childPID := r.acpPID()
	if err := r.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM agyd: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		<-r.drained
		done <- r.cmd.Wait()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agyd exited with %v after SIGTERM:\n%s", err, r.logs.String())
		}
	case <-time.After(10 * time.Second):
		_ = r.cmd.Process.Kill()
		<-done
		t.Fatalf("agyd did not stop within 10s of SIGTERM:\n%s", r.logs.String())
	}
	if childPID > 0 {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if syscall.Kill(childPID, 0) == syscall.ESRCH {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("acp child pid %d still alive after agyd SIGTERM", childPID)
	}
}

func httpJSON(t *testing.T, method, url, body string, wantCode int) map[string]any {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantCode {
		t.Fatalf("%s %s = %d %s, want %d", method, url, resp.StatusCode, raw, wantCode)
	}
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func TestRestartPreservesStateAcrossSIGTERM(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	stateDir := filepath.Join(tmp, "custom-state")
	dirA := filepath.Join(tmp, "dirA")
	dirB := filepath.Join(tmp, "dirB")
	cfgDir := filepath.Join(home, ".config", "agyd")
	for _, d := range []string{cfgDir, dirA, dirB} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Poison .env and config.env: Phase 1 ignores both completely.
	if err := os.WriteFile(filepath.Join(dirA, ".env"), []byte("AGYD_MODEL=claude-sonnet-5-5\nAGYD_THINKING=low\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	poisonCfg := "AGYD_MODEL=claude-sonnet-5-5\nAGYD_MODE=plan\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.env"), []byte(poisonCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(tmp, "acp-store.json")
	pidPath := filepath.Join(tmp, "acp.pid")
	envForPort := func(port int) map[string]string {
		return map[string]string{
			"HOME":                  home,
			"AGYD_STATE_DIR":        stateDir,
			"AGYD_HOST":             "127.0.0.1",
			"AGYD_PORT":             strconv.Itoa(port),
			"AGYD_WORKSPACE":        dirA,
			"AGYD_AGY":              exe,
			"AGYD_ACP_ARGS":         "-test.run=^TestHelperServeAndACP$ -- fake-acp",
			"AGYD_TEST_ACP_STORE":   storePath,
			"AGYD_TEST_ACP_PIDFILE": pidPath,
		}
	}

	// Instance 1: env-only startup, set defaults, create two conversations in two folders, use one, prompt it, SIGTERM.
	srv1 := startAgydProcess(t, dirA, envForPort(freeLoopbackPort(t)))

	// Poison .env and config.env must not have been read into defaults.
	if d0 := httpJSON(t, http.MethodGet, srv1.base+"/v1/defaults", "", http.StatusOK); len(d0) != 0 {
		t.Fatalf("initial defaults = %v, want {} (.env and config.env must be ignored)", d0)
	}

	dPut := httpJSON(t, http.MethodPut, srv1.base+"/v1/defaults", `{"model":"claude-sonnet-5-5","thinkingLevel":"high"}`, http.StatusOK)
	if dPut["model"] != "claude-sonnet-5-5" || dPut["thinkingLevel"] != "high" {
		t.Fatalf("PUT /v1/defaults = %v", dPut)
	}

	// Poison config.env in ~/.config/agyd must not have been touched, and stateDir/config.env must not exist.
	if got, _ := os.ReadFile(filepath.Join(cfgDir, "config.env")); string(got) != poisonCfg {
		t.Errorf("~/.config/agyd/config.env was modified:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "config.env")); !os.IsNotExist(err) {
		t.Errorf("stateDir/config.env should not exist: %v", err)
	}

	c1 := httpJSON(t, http.MethodPost, srv1.base+"/v1/conversations", fmt.Sprintf(`{"cwd":%q,"mode":"plan"}`, dirA), http.StatusCreated)
	c1ID := c1["conversationId"].(string)
	p1 := httpJSON(t, http.MethodPost, srv1.base+"/v1/prompt", `{"prompt":"alpha-turn"}`, http.StatusOK)
	if p1["text"] != "history=alpha-turn" {
		t.Fatalf("first prompt text = %v, want history=alpha-turn", p1["text"])
	}

	c2 := httpJSON(t, http.MethodPost, srv1.base+"/v1/conversations", fmt.Sprintf(`{"cwd":%q,"thinkingLevel":"low"}`, dirB), http.StatusCreated)
	c2ID := c2["conversationId"].(string)

	// Switch back to c1 before SIGTERM.
	used := httpJSON(t, http.MethodPost, srv1.base+"/v1/conversations/"+c1ID+"/use", "", http.StatusOK)
	if used["conversationId"] != c1ID {
		t.Fatalf("use c1 = %v", used)
	}

	// Inspect state.json on disk: in AGYD_STATE_DIR, mode 0600, flat (not keyed by port).
	statePath := filepath.Join(stateDir, "state.json")
	fi, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", statePath, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state.json perm = %v, want 0600", fi.Mode().Perm())
	}
	rawState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawState), `"instances"`) {
		t.Errorf("state.json must not be keyed by port:\n%s", rawState)
	}

	srv1.stopSIGTERM(t)

	// Instance 2: start on a DIFFERENT port with the same AGYD_STATE_DIR.
	srv2 := startAgydProcess(t, dirA, envForPort(freeLoopbackPort(t)))
	defer srv2.stopSIGTERM(t)

	dAfter := httpJSON(t, http.MethodGet, srv2.base+"/v1/defaults", "", http.StatusOK)
	if dAfter["model"] != "claude-sonnet-5-5" || dAfter["thinkingLevel"] != "high" {
		t.Errorf("defaults after restart = %v, want claude-sonnet-5-5/high", dAfter)
	}

	curAfter := httpJSON(t, http.MethodGet, srv2.base+"/v1/current", "", http.StatusOK)
	if curAfter["conversationId"] != c1ID || curAfter["cwd"] != dirA ||
		curAfter["model"] != "claude-sonnet-5-5" || curAfter["thinkingLevel"] != "high" || curAfter["mode"] != "plan" {
		t.Errorf("current after restart = %v", curAfter)
	}

	listAfter := httpJSON(t, http.MethodGet, srv2.base+"/v1/conversations", "", http.StatusOK)
	items, _ := listAfter["items"].([]any)
	foundFolders := map[string]string{}
	for _, raw := range items {
		m := raw.(map[string]any)
		foundFolders[m["conversationId"].(string)] = m["cwd"].(string)
	}
	if foundFolders[c1ID] != dirA || foundFolders[c2ID] != dirB {
		t.Errorf("conversation folders after restart = %v; want c1=%s, c2=%s", foundFolders, dirA, dirB)
	}

	// Prompting continues the same conversation from before SIGTERM.
	p2 := httpJSON(t, http.MethodPost, srv2.base+"/v1/prompt", `{"prompt":"beta-turn"}`, http.StatusOK)
	if p2["conversationId"] != c1ID || p2["text"] != "history=alpha-turn,beta-turn" {
		t.Errorf("continuation prompt after restart = %v, want c1 history=alpha-turn,beta-turn", p2)
	}

	// Switching to c2 (which was never prompted before SIGTERM) resumes it in dirB with its remembered settings.
	useC2 := httpJSON(t, http.MethodPost, srv2.base+"/v1/conversations/"+c2ID+"/use", "", http.StatusOK)
	if useC2["conversationId"] != c2ID || useC2["cwd"] != dirB || useC2["thinkingLevel"] != "low" || useC2["model"] != "claude-sonnet-5-5" {
		t.Errorf("use c2 after restart = %v; want cwd=%s thinkingLevel=low model=claude-sonnet-5-5", useC2, dirB)
	}
}
