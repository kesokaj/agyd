package acp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionConfigOptions(t *testing.T) {
	c := newTestClient(t, "standard", PermissionAllowAll)
	ctx := t.Context()
	sess, err := c.NewSession(ctx, realTempDir(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if len(sess.ConfigOptions) != 3 {
		t.Fatalf("ConfigOptions = %+v, want 3 options", sess.ConfigOptions)
	}
	if sess.Setting(ConfigModel) != "gemini-4-carbon" || sess.Setting(ConfigThinking) != "medium" || sess.Setting(ConfigMode) != "default" {
		t.Errorf("settings = %q/%q/%q", sess.Setting(ConfigModel), sess.Setting(ConfigThinking), sess.Setting(ConfigMode))
	}

	got, err := c.SetConfigOption(ctx, sess.SessionID, ConfigModel, "claude-sonnet-5-5")
	if err != nil {
		t.Fatalf("SetConfigOption: %v", err)
	}
	if got.Setting(ConfigModel) != "claude-sonnet-5-5" {
		t.Errorf("returned model = %q", got.Setting(ConfigModel))
	}
	if again, _ := c.GetSession(sess.SessionID); again.Setting(ConfigModel) != "claude-sonnet-5-5" {
		t.Errorf("registry model = %q, want the value agy returned", again.Setting(ConfigModel))
	}

	_, err = c.SetConfigOption(ctx, sess.SessionID, ConfigModel, "no-such-model")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("bad value err = %v, want ErrInvalidArgument", err)
	}
	if rpcErr, ok := errors.AsType[*RPCError](err); !ok || rpcErr.Code != -32602 {
		t.Errorf("bad value err = %v, want the -32602 RPCError kept", err)
	}
	if _, err := c.SetConfigOption(ctx, "nope", ConfigModel, "x"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("unknown session err = %v, want ErrSessionNotFound", err)
	}
}

func TestSetConfigAndCloseRefusedDuringTurn(t *testing.T) {
	c := newTestClient(t, "standard", PermissionAllowAll)
	ctx := t.Context()
	sess, err := c.NewSession(ctx, realTempDir(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	started := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		_, err := c.Prompt(ctx, sess.SessionID, []ContentBlock{{Type: "text", Text: "wait_for_cancel"}}, func([]byte) {
			once.Do(func() { close(started) })
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the turn to start")
	}
	if _, err := c.SetConfigOption(ctx, sess.SessionID, ConfigThinking, "high"); !errors.Is(err, ErrPromptInFlight) {
		t.Errorf("SetConfigOption mid-turn err = %v, want ErrPromptInFlight", err)
	}
	if err := c.CloseSession(ctx, sess.SessionID); !errors.Is(err, ErrPromptInFlight) {
		t.Errorf("CloseSession mid-turn err = %v, want ErrPromptInFlight", err)
	}
	if err := c.Cancel(ctx, sess.SessionID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if _, err := c.SetConfigOption(ctx, sess.SessionID, ConfigThinking, "high"); err != nil {
		t.Errorf("SetConfigOption after the turn: %v", err)
	}
}

func TestListAndResumeAgentSessions(t *testing.T) {
	c := newTestClient(t, "standard", PermissionAllowAll)
	ctx := t.Context()
	list, err := c.ListAgentSessions(ctx)
	if err != nil {
		t.Fatalf("ListAgentSessions: %v", err)
	}
	if len(list) != 2 || list[0].SessionID != "hist_1" || list[0].Cwd != "/tmp/hist" || list[0].Title != "Older chat" {
		t.Fatalf("list = %+v", list)
	}
	if want := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC); !list[0].UpdateTime.Equal(want) {
		t.Errorf("UpdateTime = %v, want %v", list[0].UpdateTime, want)
	}

	histDir := realTempDir(t)
	info, err := c.ResumeSession(ctx, "hist_1", histDir)
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if info.SessionID != "hist_1" || info.Cwd != histDir || info.Setting(ConfigThinking) != "medium" {
		t.Errorf("resumed = %+v", info)
	}
	if _, err := c.GetSession("hist_1"); err != nil {
		t.Errorf("resumed session not registered: %v", err)
	}
	if again, err := c.ResumeSession(ctx, "hist_1", histDir); err != nil || again.SessionID != "hist_1" {
		t.Errorf("resuming a live session = %+v, %v; want it returned as is", again, err)
	}
	if _, err := c.ResumeSession(ctx, "missing", histDir); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("resume unknown err = %v, want ErrSessionNotFound", err)
	}
	if _, err := c.ResumeSession(ctx, "hist_2", "relative"); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("resume with a bad cwd err = %v, want ErrInvalidConfig", err)
	}
	base := realTempDir(t)
	gemDir := filepath.Join(base, ".gemini")
	if err := os.MkdirAll(gemDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sneaky := filepath.Join(base, "sneaky")
	if err := os.Symlink(gemDir, sneaky); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResumeSession(ctx, "hist_2", sneaky); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("resume with symlink to .gemini err = %v, want ErrInvalidConfig", err)
	}
}

func TestSessionRejectsSymlinkCwd(t *testing.T) {
	c := newTestClient(t, "standard", PermissionAllowAll)
	ctx := t.Context()

	base := realTempDir(t)
	realParent := filepath.Join(base, "real")
	realTarget := filepath.Join(realParent, "sub")
	if err := os.MkdirAll(realTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	leafLink := filepath.Join(base, "leaf-link")
	if err := os.Symlink(realTarget, leafLink); err != nil {
		t.Fatal(err)
	}
	midLink := filepath.Join(base, "mid-link")
	if err := os.Symlink(realParent, midLink); err != nil {
		t.Fatal(err)
	}
	midPath := filepath.Join(midLink, "sub")

	for _, tc := range []struct {
		name, path string
	}{
		{"leaf symlink", leafLink},
		{"middle symlink", midPath},
	} {
		t.Run("NewSession "+tc.name, func(t *testing.T) {
			_, err := c.NewSession(ctx, tc.path)
			if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), realTarget) {
				t.Fatalf("NewSession(%q) err = %v, want ErrInvalidConfig naming %q", tc.path, err, realTarget)
			}
		})
		t.Run("ResumeSession "+tc.name, func(t *testing.T) {
			_, err := c.ResumeSession(ctx, "hist_2", tc.path)
			if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), realTarget) {
				t.Fatalf("ResumeSession(%q) err = %v, want ErrInvalidConfig naming %q", tc.path, err, realTarget)
			}
		})
	}

	t.Run("real path passes", func(t *testing.T) {
		sess, err := c.NewSession(ctx, realTarget)
		if err != nil || sess.Cwd != realTarget {
			t.Fatalf("NewSession(%q) = %+v, err = %v", realTarget, sess, err)
		}
		resumed, err := c.ResumeSession(ctx, "hist_2", realTarget)
		if err != nil || resumed.Cwd != realTarget {
			t.Fatalf("ResumeSession(%q) = %+v, err = %v", realTarget, resumed, err)
		}
	})
}

func TestParseConfigOptionsSkipsUnknownShapes(t *testing.T) {
	raw := json.RawMessage(`[{"id":"model","type":"select","currentValue":"m","options":[{"value":"m","name":"M"}]},
		{"id":"flag","type":"boolean","currentValue":true},"junk"]`)
	got := parseConfigOptions(raw)
	if len(got) != 1 || got[0].ID != "model" || got[0].CurrentValue != "m" || got[0].Options[0].Name != "M" {
		t.Fatalf("parseConfigOptions = %+v", got)
	}
	if parseConfigOptions(nil) != nil || parseConfigOptions(json.RawMessage(`{}`)) != nil {
		t.Error("non-array input must give nil")
	}
}
