package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func realTempDir(t *testing.T) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return real
}

func newTestClient(t *testing.T, mode string, policy PermissionPolicy) *Client {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cfg := Config{
		Binary:           exe,
		Args:             []string{"-test.run=^TestHelperACPProcess$", "--", mode},
		Env:              append(os.Environ(), "GO_WANT_ACP_HELPER=1", "AGY_API_TOKEN=super-secret-gateway-token"),
		DefaultWorkspace: realTempDir(t),
		PermissionPolicy: policy,
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	c, err := NewClient(cfg, log)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestClientInitializeAndSessionLifecycle(t *testing.T) {
	c := newTestClient(t, "standard", PermissionAllowAll)
	ctx := t.Context()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Ready || st.ProtocolVersion != 1 || st.AgentInfo.Name != "fake-agy" {
		t.Fatalf("Status = %+v, want Ready=true ProtocolVersion=1 Name=fake-agy", st)
	}

	ws := realTempDir(t)
	sess, err := c.NewSession(ctx, ws)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if sess.SessionID == "" || sess.Cwd != ws {
		t.Fatalf("NewSession = %+v, want non-empty SessionID and Cwd=%q", sess, ws)
	}

	// Reject .gemini credential directory as cwd.
	if _, err := c.NewSession(ctx, "/home/agy/.gemini"); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("NewSession(/home/agy/.gemini) err = %v, want ErrInvalidConfig", err)
	}

	// Verify AGY_API_TOKEN is stripped from the ACP child process environment.
	envRes, err := c.Prompt(ctx, sess.SessionID, []ContentBlock{{Type: "text", Text: "check_env"}}, nil)
	if err != nil {
		t.Fatalf("Prompt(check_env): %v", err)
	}
	if envRes.Text != "token=" {
		t.Errorf("child env leaked AGY_API_TOKEN: got %q, want %q", envRes.Text, "token=")
	}

	got, err := c.GetSession(sess.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.SessionID != sess.SessionID {
		t.Errorf("GetSession ID = %q, want %q", got.SessionID, sess.SessionID)
	}

	list := c.ListSessions()
	if len(list) != 1 || list[0].SessionID != sess.SessionID {
		t.Errorf("ListSessions = %+v, want 1 session %q", list, sess.SessionID)
	}

	if err := c.CloseSession(ctx, sess.SessionID); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if _, err := c.GetSession(sess.SessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("GetSession after close = %v, want ErrSessionNotFound", err)
	}
}

func TestClientPromptStreamingAndPermission(t *testing.T) {
	t.Run("allow_all concatenates chunks and approves tool call", func(t *testing.T) {
		c := newTestClient(t, "permission", PermissionAllowAll)
		ctx := t.Context()
		if err := c.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		sess, err := c.NewSession(ctx, realTempDir(t))
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		var streamed []string
		res, err := c.Prompt(ctx, sess.SessionID, []ContentBlock{{Type: "text", Text: "run tool"}}, func(raw []byte) {
			streamed = append(streamed, string(raw))
		})
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if res.StopReason != "end_turn" {
			t.Errorf("StopReason = %q, want end_turn", res.StopReason)
		}
		if res.Text != "Allowed: allow-once!" {
			t.Errorf("Text = %q, want %q", res.Text, "Allowed: allow-once!")
		}
		if len(streamed) != 2 {
			t.Errorf("streamed %d updates, want 2", len(streamed))
		}
		if len(res.Updates) != 0 {
			t.Errorf("streaming Prompt accumulated %d updates in memory, want 0", len(res.Updates))
		}
	})

	t.Run("deny_all selects reject option", func(t *testing.T) {
		c := newTestClient(t, "permission", PermissionDenyAll)
		ctx := t.Context()
		if err := c.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		sess, err := c.NewSession(ctx, realTempDir(t))
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		res, err := c.Prompt(ctx, sess.SessionID, []ContentBlock{{Type: "text", Text: "run tool"}}, nil)
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if res.Text != "Denied: reject-once!" {
			t.Errorf("Text = %q, want %q", res.Text, "Denied: reject-once!")
		}
	})
}

// syncBuffer is an io.Writer safe for the client's read goroutine and the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns every JSON log line whose msg equals msg.
func (b *syncBuffer) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(b.buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func TestPermissionDecisionLogged(t *testing.T) {
	cases := []struct {
		policy      PermissionPolicy
		wantOutcome string
		wantOption  string
		wantKind    string
	}{
		{PermissionAllowAll, "selected", "allow-once", "allow_once"},
		{PermissionDenyAll, "selected", "reject-once", "reject_once"},
	}
	for _, tc := range cases {
		t.Run(string(tc.policy), func(t *testing.T) {
			c := newTestClient(t, "permission", tc.policy)
			logs := &syncBuffer{}
			c.log = slog.New(slog.NewJSONHandler(logs, nil))
			ctx := t.Context()
			if err := c.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			sess, err := c.NewSession(ctx, realTempDir(t))
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			if _, err := c.Prompt(ctx, sess.SessionID, []ContentBlock{{Type: "text", Text: "run tool"}}, nil); err != nil {
				t.Fatalf("Prompt: %v", err)
			}

			recs := logs.records(t, "permission decision")
			if len(recs) != 1 {
				t.Fatalf("got %d permission decision lines, want 1", len(recs))
			}
			rec := recs[0]
			want := map[string]any{
				"level":          "INFO",
				"conversationId": sess.SessionID,
				"toolCallId":     "tc_1",
				"toolKind":       "execute",
				"policy":         string(tc.policy),
				"outcome":        tc.wantOutcome,
				"optionId":       tc.wantOption,
				"optionKind":     tc.wantKind,
			}
			for k, v := range want {
				if rec[k] != v {
					t.Errorf("%s = %v, want %v", k, rec[k], v)
				}
			}
			opts, ok := rec["options"].([]any)
			if !ok || len(opts) != 2 {
				t.Fatalf("options = %v, want 2 entries", rec["options"])
			}
			first, _ := opts[0].(map[string]any)
			if first["optionId"] != "allow-once" || first["kind"] != "allow_once" {
				t.Errorf("options[0] = %v, want optionId=allow-once kind=allow_once", first)
			}
		})
	}
}

func TestClipBoundsAndSanitizes(t *testing.T) {
	long := strings.Repeat("é", maxLoggedField+50)
	if n := len([]rune(clip(long))); n != maxLoggedField+1 {
		t.Errorf("clipped value has %d runes, want %d (limit plus ellipsis)", n, maxLoggedField+1)
	}
	exact := strings.Repeat("a", maxLoggedField)
	if got := clip(exact); got != exact {
		t.Errorf("clip(%d runes) = %q, want unchanged", maxLoggedField, got)
	}
	if got := clip("ls\u202e\u200b\x1b[31m"); got != "ls\ufffd\ufffd\ufffd[31m" {
		t.Errorf("clip(bidi, zero-width, ESC) = %q, want each replaced by U+FFFD", got)
	}
}

// nopWriteCloser lets a test stand in for the child's stdin.
type nopWriteCloser struct{ *syncBuffer }

func (nopWriteCloser) Close() error { return nil }

func TestPermissionDecisionMalformedAndOversized(t *testing.T) {
	logs := &syncBuffer{}
	stdin := &syncBuffer{}
	c := &Client{
		cfg:   Config{PermissionPolicy: PermissionDenyAll},
		log:   slog.New(slog.NewJSONHandler(logs, nil)),
		stdin: nopWriteCloser{stdin},
	}

	// options of the wrong type: the decode fails, deny_all cancels, the line still comes out with err.
	c.handleAgentRequest(7, "session/request_permission", json.RawMessage(`{"sessionId":"s1","toolCall":{"toolCallId":"tc","title":"curl -H 'Authorization: Bearer SECRET'","kind":"execute"},"options":"x"}`))
	recs := logs.records(t, "permission decision")
	if len(recs) != 1 {
		t.Fatalf("got %d permission decision lines, want 1", len(recs))
	}
	if recs[0]["outcome"] != "cancelled" || recs[0]["err"] == nil {
		t.Errorf("record = %v, want outcome=cancelled and err set", recs[0])
	}
	if !strings.Contains(stdin.buf.String(), `"outcome":"cancelled"`) {
		t.Errorf("response = %q, want a cancelled outcome sent to agy", stdin.buf.String())
	}
	if strings.Contains(logs.buf.String(), "SECRET") || strings.Contains(logs.buf.String(), "curl") {
		t.Errorf("tool call title reached the log: %s", logs.buf.String())
	}

	// 20 options with huge ids: the line lists 8, counts 20, and clips every id.
	opts := make([]map[string]string, 20)
	for i := range opts {
		opts[i] = map[string]string{"optionId": strings.Repeat("x", 10000), "name": "n", "kind": "reject_once"}
	}
	raw, _ := json.Marshal(map[string]any{"sessionId": strings.Repeat("s", 10000), "options": opts})
	c.handleAgentRequest(8, "session/request_permission", raw)
	recs = logs.records(t, "permission decision")
	rec := recs[len(recs)-1]
	logged, _ := rec["options"].([]any)
	if len(logged) != maxLoggedOptions || rec["optionCount"] != float64(20) {
		t.Fatalf("options = %d entries, optionCount = %v; want %d and 20", len(logged), rec["optionCount"], maxLoggedOptions)
	}
	for _, k := range []string{"conversationId", "optionId"} {
		if s, _ := rec[k].(string); len([]rune(s)) > maxLoggedField+1 {
			t.Errorf("%s has %d runes, want at most %d", k, len([]rune(s)), maxLoggedField+1)
		}
	}
	first, _ := logged[0].(map[string]any)
	if id, _ := first["optionId"].(string); len([]rune(id)) > maxLoggedField+1 || first["name"] != nil {
		t.Errorf("options[0] = %v, want a clipped optionId and no name", first)
	}
}

func TestUnsupportedAgentRequestLogged(t *testing.T) {
	c := newTestClient(t, "fs_request", PermissionDenyAll)
	logs := &syncBuffer{}
	c.log = slog.New(slog.NewJSONHandler(logs, nil))
	ctx := t.Context()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess, err := c.NewSession(ctx, realTempDir(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	res, err := c.Prompt(ctx, sess.SessionID, []ContentBlock{{Type: "text", Text: "write file"}}, nil)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Text != "code=-32601" {
		t.Errorf("agent saw %q, want code=-32601", res.Text)
	}
	recs := logs.records(t, "unsupported agent request")
	if len(recs) != 1 {
		t.Fatalf("got %d unsupported agent request lines, want 1", len(recs))
	}
	if recs[0]["method"] != "fs/write_text_file" || recs[0]["conversationId"] != sess.SessionID {
		t.Errorf("record = %v, want method=fs/write_text_file conversationId=%s", recs[0], sess.SessionID)
	}
}

func TestClientCancelAndCrashRecovery(t *testing.T) {
	c := newTestClient(t, "slow_and_crash", PermissionAllowAll)
	ctx := t.Context()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess, err := c.NewSession(ctx, realTempDir(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	started := make(chan struct{})
	var once sync.Once
	errCh := make(chan error, 1)
	var promptRes PromptResult
	go func() {
		var pErr error
		promptRes, pErr = c.Prompt(ctx, sess.SessionID, []ContentBlock{{Type: "text", Text: "wait_for_cancel"}}, func(_ []byte) {
			once.Do(func() { close(started) })
		})
		errCh <- pErr
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first session/update")
	}

	// Second concurrent prompt on same session must fail with ErrPromptInFlight.
	if _, err := c.Prompt(ctx, sess.SessionID, []ContentBlock{{Type: "text", Text: "concurrent"}}, nil); !errors.Is(err, ErrPromptInFlight) {
		t.Errorf("concurrent Prompt err = %v, want ErrPromptInFlight", err)
	}

	// Cancel the in-flight prompt.
	if err := c.Cancel(ctx, sess.SessionID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Prompt after Cancel: %v", err)
	}
	if promptRes.StopReason != "cancelled" {
		t.Errorf("StopReason = %q, want cancelled", promptRes.StopReason)
	}

	// Context cancellation on Prompt must automatically send session/cancel to child.
	pCtx, pCancel := context.WithCancel(ctx)
	started2 := make(chan struct{})
	var once2 sync.Once
	go func() {
		_, _ = c.Prompt(pCtx, sess.SessionID, []ContentBlock{{Type: "text", Text: "wait_for_cancel"}}, func(_ []byte) {
			once2.Do(func() { close(started2) })
		})
	}()
	select {
	case <-started2:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for second wait_for_cancel")
	}
	pCancel()
	time.Sleep(50 * time.Millisecond)
	afterCancelRes, err := c.Prompt(ctx, sess.SessionID, []ContentBlock{{Type: "text", Text: "was_cancelled"}}, nil)
	if err != nil {
		t.Fatalf("Prompt(was_cancelled): %v", err)
	}
	if afterCancelRes.Text != "cancel_count=2" {
		t.Errorf("after context cancel Text = %q, want cancel_count=2", afterCancelRes.Text)
	}

	// Trigger unexpected child crash: in-flight prompt returns ErrProcessClosed and session registry is cleared.
	_, err = c.Prompt(ctx, sess.SessionID, []ContentBlock{{Type: "text", Text: "crash_now"}}, nil)
	if !errors.Is(err, ErrProcessClosed) {
		t.Fatalf("Prompt(crash_now) err = %v, want ErrProcessClosed", err)
	}
	if _, err := c.GetSession(sess.SessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("GetSession after crash = %v, want ErrSessionNotFound", err)
	}

	// Subsequent Status and NewSession lazily respawn the child process and succeed.
	st, err := c.Status(ctx)
	if err != nil || !st.Ready {
		t.Fatalf("Status after lazy respawn = %+v, err = %v; want Ready=true", st, err)
	}
	sess2, err := c.NewSession(ctx, realTempDir(t))
	if err != nil || sess2.SessionID == "" {
		t.Fatalf("NewSession after lazy respawn = %+v, err = %v", sess2, err)
	}
}

// TestHelperACPProcess runs inside a child process when GO_WANT_ACP_HELPER=1 is set.
func TestHelperACPProcess(t *testing.T) {
	if os.Getenv("GO_WANT_ACP_HELPER") != "1" {
		return
	}
	mode := "standard"
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	runFakeACPServer(mode, os.Stdin, os.Stdout)
	os.Exit(0)
}

func runFakeACPServer(mode string, r io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(r)
	var writeMu sync.Mutex
	send := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "%s\n", b)
	}

	var cancelCh chan struct{}
	var cancelMu sync.Mutex
	cancelCount := 0
	seq := 0
	cfg := newFakeConfig()

	for scanner.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
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
					"agentInfo": map[string]any{
						"name":    "fake-agy",
						"title":   "Fake Antigravity CLI",
						"version": "1.5.9-test",
					},
				},
			})
		case "session/new":
			seq++
			sid := fmt.Sprintf("sess_%d", seq)
			var np sessionNewParams
			_ = json.Unmarshal(msg.Params, &np)
			cfg.created[sid] = np.Cwd
			cfg.reset(sid)
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result": map[string]any{
					"sessionId":     sid,
					"modes":         map[string]any{"currentModeId": "default"},
					"models":        map[string]any{"currentModelId": "gemini-test"},
					"configOptions": cfg.options(sid),
				},
			})
		case "session/close":
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result":  map[string]any{},
			})
		case "session/cancel":
			cancelMu.Lock()
			cancelCount++
			if cancelCh != nil {
				close(cancelCh)
				cancelCh = nil
			}
			cancelMu.Unlock()
		case "session/prompt":
			var p sessionPromptParams
			_ = json.Unmarshal(msg.Params, &p)
			text := ""
			if len(p.Prompt) > 0 {
				text = p.Prompt[0].Text
			}
			if text == "check_env" {
				send(map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": p.SessionID,
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "token=" + os.Getenv("AGY_API_TOKEN")},
						},
					},
				})
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				})
				continue
			}
			if text == "was_cancelled" {
				cancelMu.Lock()
				cc := cancelCount
				cancelMu.Unlock()
				send(map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": p.SessionID,
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": fmt.Sprintf("cancel_count=%d", cc)},
						},
					},
				})
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				})
				continue
			}
			if text == "crash_now" {
				os.Exit(2)
			}
			if text == "auth_error" {
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"error": map[string]any{
						"code":    -32000,
						"message": "Authentication required: please run agy acp --login",
					},
				})
				continue
			}
			if text == "wait_for_cancel" {
				ch := make(chan struct{})
				cancelMu.Lock()
				cancelCh = ch
				cancelMu.Unlock()
				reqID := *msg.ID
				go func(sid string, id int64, cch chan struct{}) {
					send(map[string]any{
						"jsonrpc": "2.0",
						"method":  "session/update",
						"params": map[string]any{
							"sessionId": sid,
							"update": map[string]any{
								"sessionUpdate": "agent_message_chunk",
								"content":       map[string]any{"type": "text", "text": "working..."},
							},
						},
					})
					<-cch
					send(map[string]any{
						"jsonrpc": "2.0",
						"id":      id,
						"result": map[string]any{
							"stopReason": "cancelled",
						},
					})
				}(p.SessionID, reqID, ch)
				continue
			}
			if mode == "fs_request" {
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      int64(901),
					"method":  "fs/write_text_file",
					"params":  map[string]any{"sessionId": p.SessionID, "path": "/tmp/x", "content": "x"},
				})
				if scanner.Scan() {
					var resp rpcMessage
					_ = json.Unmarshal(scanner.Bytes(), &resp)
					code := 0
					if resp.Error != nil {
						code = resp.Error.Code
					}
					send(map[string]any{
						"jsonrpc": "2.0",
						"method":  "session/update",
						"params": map[string]any{
							"sessionId": p.SessionID,
							"update": map[string]any{
								"sessionUpdate": "agent_message_chunk",
								"content":       map[string]any{"type": "text", "text": fmt.Sprintf("code=%d", code)},
							},
						},
					})
					send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"stopReason": "end_turn"}})
				}
				continue
			}
			if mode == "permission" {
				permID := int64(900)
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      permID,
					"method":  "session/request_permission",
					"params": map[string]any{
						"sessionId": p.SessionID,
						"toolCall":  map[string]any{"toolCallId": "tc_1", "title": "Execute command", "kind": "execute"},
						"options": []map[string]any{
							{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
							{"optionId": "reject-once", "name": "Reject once", "kind": "reject_once"},
						},
					},
				})
				// Read the client's response to session/request_permission.
				if scanner.Scan() {
					var permResp rpcMessage
					_ = json.Unmarshal(scanner.Bytes(), &permResp)
					var out requestPermissionResult
					_ = json.Unmarshal(permResp.Result, &out)
					prefix := "Allowed: "
					if out.Outcome.OptionID == "reject-once" {
						prefix = "Denied: "
					}
					for _, chunk := range []string{prefix, out.Outcome.OptionID + "!"} {
						send(map[string]any{
							"jsonrpc": "2.0",
							"method":  "session/update",
							"params": map[string]any{
								"sessionId": p.SessionID,
								"update": map[string]any{
									"sessionUpdate": "agent_message_chunk",
									"content":       map[string]any{"type": "text", "text": chunk},
								},
							},
						})
					}
					send(map[string]any{
						"jsonrpc": "2.0",
						"id":      msg.ID,
						"result": map[string]any{
							"stopReason": "end_turn",
						},
					})
				}
				continue
			}
			send(map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": p.SessionID,
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": "echo: " + text},
					},
				},
			})
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result": map[string]any{
					"stopReason": "end_turn",
				},
			})
		}
	}
}
