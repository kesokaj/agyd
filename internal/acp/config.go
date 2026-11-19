package acp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// configOptions ids agy uses for the settings agyd exposes.
const (
	ConfigModel    = "model"
	ConfigThinking = "thought_level"
	ConfigMode     = "mode"
)

// ConfigOption is one select-type session setting reported by the agent.
type ConfigOption struct {
	ID           string        `json:"id"`
	Name         string        `json:"name,omitempty"`
	Category     string        `json:"category,omitempty"`
	Type         string        `json:"type"`
	CurrentValue string        `json:"currentValue"`
	Options      []ConfigValue `json:"options,omitempty"`
}

// ConfigValue is one allowed value of a ConfigOption.
type ConfigValue struct {
	Value string `json:"value"`
	Name  string `json:"name,omitempty"`
}

// AgentSession is one conversation the agent knows about (session/list),
// live in this process or not.
type AgentSession struct {
	SessionID  string    `json:"sessionId"`
	Cwd        string    `json:"cwd"`
	Title      string    `json:"title,omitempty"`
	UpdateTime time.Time `json:"updateTime"`
}

// Setting returns the current value of configOptions id, or "".
func (s SessionInfo) Setting(id string) string {
	for _, o := range s.ConfigOptions {
		if o.ID == id {
			return o.CurrentValue
		}
	}
	return ""
}

// parseConfigOptions keeps the options agyd understands and skips any entry
// of another shape, so a new agy option type cannot break session/new.
func parseConfigOptions(raw json.RawMessage) []ConfigOption {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []ConfigOption
	for _, it := range items {
		var o ConfigOption
		if json.Unmarshal(it, &o) == nil && o.ID != "" {
			out = append(out, o)
		}
	}
	return out
}

// markBusy claims sessionID for a non-prompt operation so no turn, settings
// change or close can overlap it. release must be called once.
func (c *Client) markBusy(sessionID string) (proc *folderProc, release func(), err error) {
	sub := &promptSubscriber{}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.sessions[sessionID]
	if !ok {
		return nil, nil, ErrSessionNotFound
	}
	if _, busy := c.inFlight[sessionID]; busy {
		return nil, nil, ErrPromptInFlight
	}
	c.inFlight[sessionID] = sub
	return entry.proc, func() {
		c.mu.Lock()
		if c.inFlight[sessionID] == sub {
			delete(c.inFlight, sessionID)
		}
		c.mu.Unlock()
	}, nil
}

// SetConfigOption sets one configOptions value on a live session and stores
// the full list agy returns. A bad id or value is ErrInvalidArgument; a
// session in a turn is ErrPromptInFlight.
func (c *Client) SetConfigOption(ctx context.Context, sessionID, configID, value string) (SessionInfo, error) {
	p, release, err := c.markBusy(sessionID)
	if err != nil {
		return SessionInfo{}, err
	}
	defer release()
	var res struct {
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	params := map[string]string{"sessionId": sessionID, "configId": configID, "value": value}
	if err := p.call(ctx, "session/set_config_option", params, &res); err != nil {
		return SessionInfo{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.sessions[sessionID]
	if !ok {
		return SessionInfo{}, ErrSessionNotFound
	}
	if opts := parseConfigOptions(res.ConfigOptions); opts != nil {
		entry.info.ConfigOptions = opts
		c.sessions[sessionID] = entry
	}
	return entry.info, nil
}

// ListAgentSessions returns every conversation agy has on this host
// (session/list), oldest first. agy ignores a cwd filter and does not page.
func (c *Client) ListAgentSessions(ctx context.Context) ([]AgentSession, error) {
	var res struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Title     string `json:"title"`
			UpdatedAt string `json:"updatedAt"`
		} `json:"sessions"`
	}
	err := c.withAnyProc(ctx, func(p *folderProc) error {
		return p.call(ctx, "session/list", map[string]any{}, &res)
	})
	if err != nil {
		return nil, err
	}
	out := make([]AgentSession, 0, len(res.Sessions))
	for _, s := range res.Sessions {
		t, _ := time.Parse(time.RFC3339Nano, s.UpdatedAt)
		out = append(out, AgentSession{SessionID: s.SessionID, Cwd: s.Cwd, Title: s.Title, UpdateTime: t})
	}
	slices.SortStableFunc(out, func(a, b AgentSession) int {
		return cmp.Or(a.UpdateTime.Compare(b.UpdateTime), cmp.Compare(a.SessionID, b.SessionID))
	})
	return out, nil
}

// ResumeSession attaches to an existing agy conversation (session/resume, no
// history replay) on its folder's ACP child and registers it. A live session
// is returned as is. agy resets configOptions on resume, so callers re-apply
// remembered settings. session/load is never used: it reports success for
// unknown ids.
func (c *Client) ResumeSession(ctx context.Context, sessionID, cwd string) (SessionInfo, error) {
	if info, err := c.GetSession(sessionID); err == nil {
		return info, nil
	}
	if err := ValidateCwd(cwd); err != nil {
		return SessionInfo{}, err
	}
	p, release, err := c.acquireProc(ctx, cwd)
	if err != nil {
		return SessionInfo{}, err
	}
	defer release()

	var res sessionNewResult
	params := map[string]any{"sessionId": sessionID, "cwd": p.dir, "mcpServers": []any{}}
	if err := p.call(ctx, "session/resume", params, &res); err != nil {
		if rpcErr, ok := errors.AsType[*RPCError](err); ok && strings.Contains(strings.ToLower(rpcErr.Message), "failed to read session") {
			return SessionInfo{}, fmt.Errorf("%w: %w", ErrSessionNotFound, err)
		}
		return SessionInfo{}, err
	}
	info := SessionInfo{
		SessionID:     sessionID,
		Cwd:           p.dir,
		Backend:       c.backend,
		CreateTime:    time.Now().UTC(),
		Modes:         res.Modes,
		Models:        res.Models,
		ConfigOptions: parseConfigOptions(res.ConfigOptions),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if live, ok := c.sessions[sessionID]; ok {
		return live.info, nil // a concurrent resume won
	}
	if c.closed || c.procs[p.dir] != p {
		return SessionInfo{}, ErrProcessClosed
	}
	c.sessions[sessionID] = sessionEntry{info: info, proc: p}
	p.refs++
	return info, nil
}
