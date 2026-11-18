// Package config resolves the state directory, validates setting values, and
// reads and writes the single port-independent state.json file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
)

// MaxStateFileSize caps how much of state.json agyd reads or writes.
const MaxStateFileSize = 4 << 20

var (
	settingValue = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	stateMu      sync.Mutex
)

// ValidSettingValue reports whether v is a safe non-empty setting value
// (1-64 characters of [A-Za-z0-9._-]).
func ValidSettingValue(v string) bool { return settingValue.MatchString(v) }

// ValidateStateDir validates an explicit state directory (-state-dir or
// AGYD_STATE_DIR): it must be an absolute path outside .gemini.
func ValidateStateDir(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if !filepath.IsAbs(s) {
		return "", fmt.Errorf("state dir %q must be an absolute path", raw)
	}
	clean := filepath.Clean(s)
	if hasDotGemini(clean) {
		return "", fmt.Errorf("state dir %q must be outside .gemini", raw)
	}
	return clean, nil
}

// StateDir resolves the directory holding state.json: AGYD_STATE_DIR (must be
// absolute and outside .gemini when set), else $XDG_CONFIG_HOME/agyd (when
// absolute and outside .gemini), else $HOME/.config/agyd.
func StateDir(getenv func(string) string) (string, error) {
	if raw := getenv("AGYD_STATE_DIR"); raw != "" {
		return ValidateStateDir(raw)
	}
	if x := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); filepath.IsAbs(x) && !hasDotGemini(filepath.Clean(x)) {
		return filepath.Join(x, "agyd"), nil
	}
	home := strings.TrimSpace(getenv("HOME"))
	if !filepath.IsAbs(home) || hasDotGemini(filepath.Clean(home)) {
		return "", errors.New("state dir: HOME is not set to a valid absolute path (set AGYD_STATE_DIR or HOME)")
	}
	return filepath.Join(home, ".config", "agyd"), nil
}

// Settings holds conversation defaults persisted in state.json.
type Settings struct {
	Model         string `json:"model,omitempty"`
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	Mode          string `json:"mode,omitempty"`
}

// ConversationRecord holds the folder and settings remembered per conversation
// in state.json so agyd can resume and route it after a restart.
type ConversationRecord struct {
	Cwd           string `json:"cwd,omitempty"`
	Model         string `json:"model,omitempty"`
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	Mode          string `json:"mode,omitempty"`
}

// State is the flat, port-independent contents of state.json.
type State struct {
	CurrentConversationID string                        `json:"currentConversationId,omitempty"`
	Defaults              Settings                      `json:"defaults,omitzero"`
	Conversations         map[string]ConversationRecord `json:"conversations,omitempty"`
}

// StateStore reads and atomically writes <dir>/state.json.
type StateStore struct {
	path string
}

// NewStateStore returns a store backed by <dir>/state.json.
func NewStateStore(dir string) *StateStore {
	return &StateStore{path: filepath.Join(dir, "state.json")}
}

// Path returns the state file path.
func (s *StateStore) Path() string { return s.path }

// Load reads state.json. A missing file returns the zero State; a corrupt or
// invalid file returns the zero State and an error.
func (s *StateStore) Load() (State, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	b, err := readRegularNonblock(s.path, MaxStateFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("state: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("state: %s is corrupt: %w", s.path, err)
	}
	if err := validateState(st); err != nil {
		return State{}, fmt.Errorf("state: %s is corrupt: %w", s.path, err)
	}
	return st, nil
}

// Save validates st and atomically writes it to state.json with mode 0600
// (creating parent directories with mode 0700).
func (s *StateStore) Save(st State) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	if err := validateState(st); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data := append(b, '\n')
	if int64(len(data)) > MaxStateFileSize {
		return fmt.Errorf("state: %s: larger than %d bytes", s.path, MaxStateFileSize)
	}
	return writeAtomic(s.path, data)
}

func validateState(st State) error {
	if st.CurrentConversationID != "" && !validConversationID(st.CurrentConversationID) {
		return fmt.Errorf("invalid currentConversationId %q", st.CurrentConversationID)
	}
	if err := validateSettings("defaults", st.Defaults.Model, st.Defaults.ThinkingLevel, st.Defaults.Mode); err != nil {
		return err
	}
	for id, rec := range st.Conversations {
		if !validConversationID(id) {
			return fmt.Errorf("invalid conversation id %q", id)
		}
		if rec.Cwd != "" && !validCwdPath(rec.Cwd) {
			return fmt.Errorf("conversation %q cwd %q must be a clean absolute path outside .gemini", id, rec.Cwd)
		}
		if err := validateSettings("conversation "+id, rec.Model, rec.ThinkingLevel, rec.Mode); err != nil {
			return err
		}
	}
	return nil
}

func validConversationID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func validCwdPath(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && !hasDotGemini(p)
}

func hasDotGemini(p string) bool {
	for _, seg := range strings.Split(p, string(filepath.Separator)) {
		if strings.EqualFold(seg, ".gemini") {
			return true
		}
	}
	return false
}

func validateSettings(scope, model, thinking, mode string) error {
	for _, p := range []struct{ name, val string }{
		{"model", model}, {"thinkingLevel", thinking}, {"mode", mode},
	} {
		if p.val != "" && !ValidSettingValue(p.val) {
			return fmt.Errorf("%s: invalid %s %q", scope, p.name, p.val)
		}
	}
	return nil
}

func readRegularNonblock(path string, maxBytes int64) ([]byte, error) {
	lfi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !lfi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(lfi, fi) {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, maxBytes)
	}
	return b, nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if lfi, err := os.Lstat(dir); err == nil && !lfi.IsDir() {
		return fmt.Errorf("%s: state dir is not a directory", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
