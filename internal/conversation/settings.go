package conversation

import (
	"errors"
	"fmt"

	"agyd/internal/acp"
	"agyd/internal/config"
)

// Errors the API maps to status codes.
var (
	// ErrNoCurrent: no current conversation is set (404 NO_CURRENT_CONVERSATION).
	ErrNoCurrent = errors.New("no current conversation")
	// ErrNoModels: agy has not reported configOptions yet (503).
	ErrNoModels = errors.New("no model list yet: agy has not reported its config options")
	// ErrInvalidSetting: a setting value failed agyd's own validation (400).
	ErrInvalidSetting = errors.New("invalid setting")
)

// Settings are the per-conversation agy settings agyd exposes. "" means
// "not set" (keep agy's value).
type Settings struct {
	Model         string `json:"model,omitempty"`
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	Mode          string `json:"mode,omitempty"`
}

// IsZero reports whether no setting is given.
func (s Settings) IsZero() bool { return s == Settings{} }

// overlay returns s with every non-empty field of o applied.
func (s Settings) overlay(o Settings) Settings {
	if o.Model != "" {
		s.Model = o.Model
	}
	if o.ThinkingLevel != "" {
		s.ThinkingLevel = o.ThinkingLevel
	}
	if o.Mode != "" {
		s.Mode = o.Mode
	}
	return s
}

type setting struct{ id, value string }

// pairs maps the fields to agy configOptions ids, in apply order.
func (s Settings) pairs() []setting {
	return []setting{{acp.ConfigModel, s.Model}, {acp.ConfigThinking, s.ThinkingLevel}, {acp.ConfigMode, s.Mode}}
}

// validate checks every non-empty value with config.ValidSettingValue.
func (s Settings) validate() error {
	for _, p := range s.pairs() {
		if p.value != "" && !config.ValidSettingValue(p.value) {
			return fmt.Errorf("%w: %s %q (want 1-64 of [A-Za-z0-9._-])", ErrInvalidSetting, p.id, p.value)
		}
	}
	return nil
}

// SettingsOf returns a live conversation's current settings as agy reports
// them.
func SettingsOf(info acp.SessionInfo) Settings {
	return Settings{
		Model:         info.Setting(acp.ConfigModel),
		ThinkingLevel: info.Setting(acp.ConfigThinking),
		Mode:          info.Setting(acp.ConfigMode),
	}
}

// Defaults returns the running defaults for new and resumed conversations.
func (m *Manager) Defaults() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.defaults
}

// SetDefaults validates s (format, and membership in agy's reported options
// when known), optionally persists the given keys to config.env, and merges
// them into the running defaults.
func (m *Manager) SetDefaults(s Settings, persist bool) (Settings, error) {
	if s.IsZero() {
		return Settings{}, fmt.Errorf("%w: give at least one of model, thinkingLevel, mode", ErrInvalidSetting)
	}
	if err := s.validate(); err != nil {
		return Settings{}, err
	}
	m.defaultsMu.Lock()
	defer m.defaultsMu.Unlock()
	if err := m.checkKnown(s); err != nil {
		return Settings{}, err
	}
	if persist && m.saveDefaults != nil {
		if err := m.saveDefaults(s); err != nil {
			return Settings{}, fmt.Errorf("saving defaults: %w", err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaults = m.defaults.overlay(s)
	return m.defaults, nil
}

// checkKnown rejects values absent from the last configOptions agy reported.
func (m *Manager) checkKnown(s Settings) error {
	m.mu.Lock()
	opts := m.options
	m.mu.Unlock()
	for _, p := range s.pairs() {
		if p.value == "" {
			continue
		}
		for _, o := range opts {
			if o.ID == p.id && len(o.Options) > 0 && !hasValue(o, p.value) {
				return fmt.Errorf("%w: %s %q is not offered by agy", ErrInvalidSetting, p.id, p.value)
			}
		}
	}
	return nil
}

func hasValue(o acp.ConfigOption, v string) bool {
	for _, c := range o.Options {
		if c.Value == v {
			return true
		}
	}
	return false
}

// Models returns the configOptions agy last reported for any conversation,
// refreshed from the live current conversation first so settings agy changed
// on its own (config_option_update) show up without a new conversation.
func (m *Manager) Models() ([]acp.ConfigOption, error) {
	if id := m.CurrentID(); id != "" {
		if info, err := m.agent.GetSession(id); err == nil {
			m.noteOptions(info.ConfigOptions)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.options) == 0 {
		return nil, ErrNoModels
	}
	return append([]acp.ConfigOption(nil), m.options...), nil
}

func (m *Manager) noteOptions(opts []acp.ConfigOption) {
	if len(opts) == 0 {
		return
	}
	m.mu.Lock()
	m.options = opts
	m.mu.Unlock()
}
