package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStateStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agyd")
	a, b := NewStateStore(dir, 8080), NewStateStore(dir, 9090)

	if st, err := a.Load(); err != nil || st != (ConversationState{}) {
		t.Fatalf("Load with no file = %+v, %v; want zero, nil", st, err)
	}
	want := ConversationState{CurrentConversationID: "c1", Cwd: "/w", Model: "m", ThinkingLevel: "high", Mode: "plan"}
	if err := a.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := b.Save(ConversationState{CurrentConversationID: "c2"}); err != nil {
		t.Fatalf("Save other port: %v", err)
	}
	if got, err := a.Load(); err != nil || got != want {
		t.Errorf("Load 8080 = %+v, %v; want %+v (the other port's write must merge, not replace)", got, err, want)
	}
	if got, _ := b.Load(); got.CurrentConversationID != "c2" {
		t.Errorf("Load 9090 = %+v", got)
	}
	st, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("state.json mode = %v, err = %v; want 0600", st.Mode().Perm(), err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if !strings.Contains(string(raw), `"instances"`) || !strings.Contains(string(raw), `"8080"`) {
		t.Errorf("state.json layout = %s", raw)
	}
}

func TestStateStoreCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStateStore(dir, 8080)
	if st, err := s.Load(); err == nil || st != (ConversationState{}) {
		t.Errorf("Load corrupt = %+v, %v; want zero and an error", st, err)
	}
	if err := s.Save(ConversationState{CurrentConversationID: "fresh"}); err != nil {
		t.Fatalf("Save over a corrupt file: %v", err)
	}
	if st, err := s.Load(); err != nil || st.CurrentConversationID != "fresh" {
		t.Errorf("Load after repair = %+v, %v", st, err)
	}
}

func TestStateStoreConcurrentSaves(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	stores := []*StateStore{NewStateStore(dir, 1), NewStateStore(dir, 2)}
	for i := range 20 {
		wg.Go(func() {
			s := stores[i%2]
			_ = s.Save(ConversationState{CurrentConversationID: "x"})
		})
	}
	wg.Wait()
	for _, s := range stores {
		if st, err := s.Load(); err != nil || st.CurrentConversationID != "x" {
			t.Errorf("port %s: %+v, %v", s.key, st, err)
		}
	}
}
