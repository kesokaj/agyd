package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// ConversationState is what one agyd instance remembers across restarts: the
// current conversation and the settings to re-apply after resuming it (agy
// resets them on session/resume).
type ConversationState struct {
	CurrentConversationID string `json:"currentConversationId"`
	Cwd                   string `json:"cwd,omitempty"`
	Model                 string `json:"model,omitempty"`
	ThinkingLevel         string `json:"thinkingLevel,omitempty"`
	Mode                  string `json:"mode,omitempty"`
}

type stateFile struct {
	Instances map[string]ConversationState `json:"instances"`
}

// stateMu serializes read-merge-write cycles of every StateStore in this
// process (several stores may share one file). Two agyd processes on the
// same port are unsupported.
var stateMu sync.Mutex

// StateStore keeps one instance's entry, keyed by listen port, in
// <dir>/state.json.
type StateStore struct {
	path string
	key  string
}

// NewStateStore returns the store for the instance listening on port.
func NewStateStore(dir string, port int) *StateStore {
	return &StateStore{path: filepath.Join(dir, "state.json"), key: strconv.Itoa(port)}
}

// Path is the state file location.
func (s *StateStore) Path() string { return s.path }

// Load returns this instance's state. A missing file or entry is the zero
// state; a corrupt file is the zero state plus an error.
func (s *StateStore) Load() (ConversationState, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	f, err := s.read()
	if err != nil {
		return ConversationState{}, err
	}
	return f.Instances[s.key], nil
}

// Save replaces this instance's entry and keeps the others. A corrupt file is
// replaced.
func (s *StateStore) Save(st ConversationState) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	f, err := s.read()
	if err != nil {
		f = stateFile{}
	}
	if f.Instances == nil {
		f.Instances = map[string]ConversationState{}
	}
	f.Instances[s.key] = st
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path, append(b, '\n'))
}

func (s *StateStore) read() (stateFile, error) {
	var f stateFile
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("state: %w", err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return stateFile{}, fmt.Errorf("state: %s is corrupt: %w", s.path, err)
	}
	return f, nil
}
