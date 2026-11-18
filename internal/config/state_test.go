package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestStateDir(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{"AGYD_STATE_DIR wins over XDG and HOME", map[string]string{"AGYD_STATE_DIR": "/custom/state/", "XDG_CONFIG_HOME": "/xdg", "HOME": "/home/u"}, "/custom/state", false},
		{"AGYD_STATE_DIR works without HOME", map[string]string{"AGYD_STATE_DIR": "/var/lib/agyd"}, "/var/lib/agyd", false},
		{"relative AGYD_STATE_DIR rejected", map[string]string{"AGYD_STATE_DIR": "rel/state", "HOME": "/home/u"}, "", true},
		{"XDG_CONFIG_HOME used when absolute", map[string]string{"XDG_CONFIG_HOME": "/xdg", "HOME": "/home/u"}, "/xdg/agyd", false},
		{"XDG_CONFIG_HOME works without HOME", map[string]string{"XDG_CONFIG_HOME": "/xdg"}, "/xdg/agyd", false},
		{"relative XDG_CONFIG_HOME ignored for HOME", map[string]string{"XDG_CONFIG_HOME": "rel", "HOME": "/home/u"}, "/home/u/.config/agyd", false},
		{"HOME fallback", map[string]string{"HOME": "/home/u"}, "/home/u/.config/agyd", false},
		{"missing HOME and overrides errors", map[string]string{}, "", true},
		{"relative HOME errors", map[string]string{"HOME": "rel"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := StateDir(envOf(tc.env))
			if (err != nil) != tc.wantErr {
				t.Fatalf("StateDir(%v) err = %v, wantErr %v", tc.env, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("StateDir(%v) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

func TestValidSettingValue(t *testing.T) {
	for v, ok := range map[string]bool{
		"gemini-4-carbon": true, "claude-sonnet-5-5": true, "gemini-3.8-flash": true,
		"high": true, "accept-edits": true, "a_b": true,
		"": false, "has space": false, "a=b": false, "a\nb": false, "#x": false,
		`"q"`: false, "a/b": false, strings.Repeat("a", 65): false, strings.Repeat("a", 64): true,
	} {
		if got := ValidSettingValue(v); got != ok {
			t.Errorf("ValidSettingValue(%q) = %v, want %v", v, got, ok)
		}
	}
}

func TestStateStoreFlatSchemaAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "agyd")
	s := NewStateStore(dir)

	if st, err := s.Load(); err != nil || !reflect.DeepEqual(st, State{}) {
		t.Fatalf("Load with no file = %+v, %v; want zero, nil", st, err)
	}

	want := State{
		CurrentConversationID: "c1",
		Defaults:              Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "high", Mode: "plan"},
		Conversations: map[string]ConversationRecord{
			"c1": {Cwd: "/work/a", Model: "claude-sonnet-5-5", ThinkingLevel: "high", Mode: "plan"},
			"c2": {Cwd: "/work/b", Model: "gemini-4-carbon", ThinkingLevel: "low"},
		},
	}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Load = %+v, %v; want %+v", got, err, want)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode = %v, err = %v; want 0700", dirInfo.Mode().Perm(), err)
	}
	fileInfo, err := os.Stat(s.Path())
	if err != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Errorf("state.json mode = %v, err = %v; want 0600", fileInfo.Mode().Perm(), err)
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"instances"`) {
		t.Errorf("state.json must not contain port-keyed instances:\n%s", raw)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Errorf("temp file left behind in %s: %v (%v)", dir, entries, err)
	}
}

func TestStateStoreUpgradesLegacyInstancesFileAsEmpty(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"instances":{"8080":{"currentConversationId":"old","cwd":"/w","model":"m"}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStateStore(dir)
	got, err := s.Load()
	if err != nil || !reflect.DeepEqual(got, State{}) {
		t.Fatalf("Load legacy instances file = %+v, %v; want empty State{}, nil", got, err)
	}
}

func TestStateStoreCorruptAndValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"malformed json", []byte("{not json")},
		{"invalid default model", []byte(`{"defaults":{"model":"bad value"}}`)},
		{"invalid conversation mode", []byte(`{"conversations":{"c1":{"cwd":"/w","mode":"bad/mode"}}}`)},
		{"relative conversation cwd", []byte(`{"conversations":{"c1":{"cwd":"rel/path"}}}`)},
		{"oversize file", append([]byte(`{"currentConversationId":"`), append(bytes.Repeat([]byte("a"), MaxStateFileSize), []byte(`"}`)...)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			s := NewStateStore(dir)
			if st, err := s.Load(); err == nil || !reflect.DeepEqual(st, State{}) {
				t.Fatalf("Load(%s) = %+v, %v; want empty State and error", tc.name, st, err)
			}
			if err := s.Save(State{CurrentConversationID: "fresh"}); err != nil {
				t.Fatalf("Save over %s: %v", tc.name, err)
			}
			if st, err := s.Load(); err != nil || st.CurrentConversationID != "fresh" {
				t.Errorf("Load after repair = %+v, %v", st, err)
			}
		})
	}
}

func TestStateStoreRefusesFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "state.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("Mkfifo unsupported: %v", err)
	}
	s := NewStateStore(dir)
	done := make(chan error, 1)
	go func() {
		_, err := s.Load()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Load on FIFO returned nil error, want refusal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Load blocked on FIFO")
	}
}

func TestStateStoreConcurrentSaves(t *testing.T) {
	dir := t.TempDir()
	s := NewStateStore(dir)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_ = s.Save(State{
				CurrentConversationID: "x",
				Defaults:              Settings{Model: "gemini-4-carbon"},
				Conversations:         map[string]ConversationRecord{"x": {Cwd: "/w"}},
			})
		})
	}
	wg.Wait()
	st, err := s.Load()
	if err != nil || st.CurrentConversationID != "x" || st.Defaults.Model != "gemini-4-carbon" {
		t.Errorf("Load after concurrent saves = %+v, %v", st, err)
	}
}

func TestStateStoreValidationAndSymlinkHardening(t *testing.T) {
	if _, err := StateDir(func(k string) string { return map[string]string{"AGYD_STATE_DIR": "/home/u/.gemini/agyd"}[k] }); err == nil {
		t.Error("StateDir inside .gemini: err = nil, want error")
	}
	if _, err := StateDir(func(k string) string { return map[string]string{"AGYD_STATE_DIR": "   "}[k] }); err == nil {
		t.Error("StateDir whitespace: err = nil, want error")
	}
	if got, err := StateDir(func(k string) string { return map[string]string{"AGYD_STATE_DIR": " /var/lib/agyd "}[k] }); err != nil || got != "/var/lib/agyd" {
		t.Errorf("StateDir trimmed = %q, %v; want /var/lib/agyd", got, err)
	}

	dir := t.TempDir()
	s := NewStateStore(dir)
	for name, raw := range map[string]string{
		"unclean cwd":    `{"conversations":{"c1":{"cwd":"/a/../b"}}}`,
		".gemini cwd":    `{"conversations":{"c1":{"cwd":"/home/u/.gemini/sub"}}}`,
		"control cur id": "{\"currentConversationId\":\"bad\\nid\"}",
	} {
		if err := os.WriteFile(s.Path(), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Load(); err == nil {
			t.Errorf("Load(%s): err = nil, want error", name)
		}
	}

	// Symlinked state.json must be refused without opening the target.
	target := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(target, []byte(`{"currentConversationId":"ok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(s.Path())
	if err := os.Symlink(target, s.Path()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Error("Load on symlinked state.json: err = nil, want error")
	}

	// Save must enforce the same validation as Load and reject symlinked state dir.
	if err := NewStateStore(t.TempDir()).Save(State{Conversations: map[string]ConversationRecord{"c1": {Cwd: "/home/u/.gemini"}}}); err == nil {
		t.Error("Save(.gemini cwd): err = nil, want error")
	}
	realDir := t.TempDir()
	symDir := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(realDir, symDir); err != nil {
		t.Fatal(err)
	}
	if err := NewStateStore(symDir).Save(State{CurrentConversationID: "ok"}); err == nil {
		t.Error("Save into symlinked state dir: err = nil, want error")
	}
}
