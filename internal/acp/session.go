package acp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// IsValidCwd reports whether cwd is a clean absolute path outside the .gemini credential store.
func IsValidCwd(cwd string) bool {
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return false
	}
	for _, seg := range strings.Split(cwd, string(filepath.Separator)) {
		if seg == ".gemini" {
			return false
		}
	}
	return true
}

// NewSession calls session/new on the ACP agent and registers the session in memory.
func (c *Client) NewSession(ctx context.Context, cwd string) (SessionInfo, error) {
	if err := c.ensureReady(ctx); err != nil {
		return SessionInfo{}, err
	}
	if cwd == "" {
		cwd = c.cfg.DefaultWorkspace
	}
	if !IsValidCwd(cwd) {
		return SessionInfo{}, fmt.Errorf("%w: cwd %q must be a clean absolute path outside .gemini", ErrInvalidConfig, cwd)
	}

	var res sessionNewResult
	params := sessionNewParams{Cwd: cwd, MCPServers: []any{}}
	if err := c.call(ctx, "session/new", params, &res); err != nil {
		return SessionInfo{}, err
	}

	info := SessionInfo{
		SessionID:     res.SessionID,
		Cwd:           cwd,
		Backend:       c.backend,
		CreateTime:    time.Now().UTC(),
		Modes:         res.Modes,
		Models:        res.Models,
		ConfigOptions: parseConfigOptions(res.ConfigOptions),
	}
	c.mu.Lock()
	c.sessions[info.SessionID] = info
	c.mu.Unlock()
	return info, nil
}

// GetSession returns metadata for a tracked session.
func (c *Client) GetSession(sessionID string) (SessionInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, ok := c.sessions[sessionID]
	if !ok {
		return SessionInfo{}, ErrSessionNotFound
	}
	return info, nil
}

// ListSessions returns all active sessions ordered by creation time.
func (c *Client) ListSessions() []SessionInfo {
	c.mu.Lock()
	out := make([]SessionInfo, 0, len(c.sessions))
	for _, s := range c.sessions {
		out = append(out, s)
	}
	c.mu.Unlock()
	slices.SortFunc(out, func(a, b SessionInfo) int {
		if c := a.CreateTime.Compare(b.CreateTime); c != 0 {
			return c
		}
		return cmp.Compare(a.SessionID, b.SessionID)
	})
	return out
}

// CloseSession calls session/close on the ACP agent and removes the session
// from memory. A session in a turn (or a settings change) is refused with
// ErrPromptInFlight.
func (c *Client) CloseSession(ctx context.Context, sessionID string) error {
	release, err := c.markBusy(sessionID)
	if err != nil {
		return err
	}
	var res map[string]any
	if err := c.call(ctx, "session/close", sessionIDParams{SessionID: sessionID}, &res); err != nil {
		release()
		return err
	}
	c.mu.Lock()
	delete(c.sessions, sessionID)
	c.mu.Unlock()
	release()
	return nil
}

// Prompt executes a session/prompt turn, invoking onUpdate for each session/update notification.
func (c *Client) Prompt(ctx context.Context, sessionID string, blocks []ContentBlock, onUpdate func(rawUpdate []byte)) (PromptResult, error) {
	sub := &promptSubscriber{onUpdate: onUpdate}
	c.mu.Lock()
	if _, ok := c.sessions[sessionID]; !ok {
		c.mu.Unlock()
		return PromptResult{}, ErrSessionNotFound
	}
	if _, busy := c.inFlight[sessionID]; busy {
		c.mu.Unlock()
		return PromptResult{}, ErrPromptInFlight
	}
	c.inFlight[sessionID] = sub
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		if c.inFlight[sessionID] == sub {
			delete(c.inFlight, sessionID)
		}
		c.mu.Unlock()
	}()

	var rpcRes sessionPromptResult
	params := sessionPromptParams{SessionID: sessionID, Prompt: blocks}
	if err := c.call(ctx, "session/prompt", params, &rpcRes); err != nil {
		if ctx.Err() != nil {
			_ = c.notify("session/cancel", sessionIDParams{SessionID: sessionID})
		}
		return PromptResult{}, err
	}

	c.mu.Lock()
	text := sub.text.String()
	updates := slices.Clone(sub.updates)
	c.mu.Unlock()

	return PromptResult{
		SessionID:  sessionID,
		StopReason: rpcRes.StopReason,
		Text:       text,
		Updates:    updates,
		Usage:      rpcRes.Usage,
	}, nil
}

// Cancel sends a session/cancel notification for an active session.
func (c *Client) Cancel(_ context.Context, sessionID string) error {
	if _, err := c.GetSession(sessionID); err != nil {
		return err
	}
	return c.notify("session/cancel", sessionIDParams{SessionID: sessionID})
}

func (c *Client) handleSessionUpdate(rawParams json.RawMessage) {
	var p sessionUpdateParams
	if err := json.Unmarshal(rawParams, &p); err != nil || p.SessionID == "" {
		return
	}
	var env sessionUpdateEnvelope
	_ = json.Unmarshal(p.Update, &env)

	c.mu.Lock()
	sub, ok := c.inFlight[p.SessionID]
	if ok {
		if sub.onUpdate == nil {
			sub.updates = append(sub.updates, p.Update)
		}
		if env.SessionUpdate == "agent_message_chunk" && env.Content.Type == "text" {
			sub.text.WriteString(env.Content.Text)
		}
	}
	c.mu.Unlock()

	if ok && sub.onUpdate != nil {
		sub.onUpdate(rawParams)
	}
}
