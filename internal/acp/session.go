package acp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// isValidCwd reports whether cwd is a clean absolute path outside the .gemini credential store.
// The segment match ignores case because macOS and Windows file systems do.
// It reads only the path text; ValidateDir and ValidateCwd also enforce existence and reject symlinks.
func isValidCwd(cwd string) bool {
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return false
	}
	for _, seg := range strings.Split(cwd, string(filepath.Separator)) {
		if strings.EqualFold(seg, ".gemini") {
			return false
		}
	}
	return true
}

type invalidDirError struct{ msg string }

func (e invalidDirError) Error() string { return e.msg }
func (e invalidDirError) Unwrap() error { return ErrInvalidConfig }

// ValidateDir checks that dir is a clean absolute path to an existing real
// directory outside .gemini with no symlinks in any path component. When dir
// traverses a symlink to a directory outside .gemini, the error names the
// resolved real path. Returned errors match errors.Is(err, ErrInvalidConfig).
func ValidateDir(label, dir string) error {
	if !isValidCwd(dir) {
		return invalidDirError{fmt.Sprintf("%s %q must be a clean absolute path outside .gemini", label, dir)}
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return invalidDirError{fmt.Sprintf("%s %q must be an existing directory", label, dir)}
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return invalidDirError{fmt.Sprintf("%s %q must be an existing directory", label, dir)}
	}
	if lfi, err := os.Lstat(real); err != nil || !lfi.IsDir() {
		return invalidDirError{fmt.Sprintf("%s %q must be an existing directory", label, dir)}
	}
	if !isValidCwd(real) {
		return invalidDirError{fmt.Sprintf("%s %q must not resolve into .gemini", label, dir)}
	}
	if real != dir {
		return invalidDirError{fmt.Sprintf("%s %q must not contain symlinks; real path is %q", label, dir, real)}
	}
	return nil
}

// ValidateCwd checks a conversation working directory with ValidateDir("cwd", cwd).
func ValidateCwd(cwd string) error {
	return ValidateDir("cwd", cwd)
}

// NewSession starts or reuses the folder's ACP child, calls session/new, and registers the session.
func (c *Client) NewSession(ctx context.Context, cwd string) (SessionInfo, error) {
	if cwd == "" {
		cwd = c.cfg.DefaultWorkspace
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
	params := sessionNewParams{Cwd: p.dir, MCPServers: []any{}}
	if err := p.call(ctx, "session/new", params, &res); err != nil {
		return SessionInfo{}, err
	}

	info := SessionInfo{
		SessionID:     res.SessionID,
		Cwd:           p.dir,
		Backend:       c.backend,
		CreateTime:    time.Now().UTC(),
		Modes:         res.Modes,
		Models:        res.Models,
		ConfigOptions: parseConfigOptions(res.ConfigOptions),
	}
	c.mu.Lock()
	if c.closed || c.procs[p.dir] != p {
		c.mu.Unlock()
		return SessionInfo{}, ErrProcessClosed
	}
	if _, exists := c.sessions[info.SessionID]; exists {
		c.mu.Unlock()
		return SessionInfo{}, fmt.Errorf("duplicate acp session id %q", info.SessionID)
	}
	c.sessions[info.SessionID] = sessionEntry{info: info, proc: p}
	p.refs++
	c.mu.Unlock()
	return info, nil
}

// GetSession returns metadata for a tracked session.
func (c *Client) GetSession(sessionID string) (SessionInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.sessions[sessionID]
	if !ok {
		return SessionInfo{}, ErrSessionNotFound
	}
	return entry.info, nil
}

// ListSessions returns all active sessions ordered by creation time.
func (c *Client) ListSessions() []SessionInfo {
	c.mu.Lock()
	out := make([]SessionInfo, 0, len(c.sessions))
	for _, entry := range c.sessions {
		out = append(out, entry.info)
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

// CloseSession calls session/close on the folder's ACP child, removes the
// session from memory, and stops the child when no conversations remain in its
// folder. A session in a turn (or a settings change) is refused with ErrPromptInFlight.
func (c *Client) CloseSession(ctx context.Context, sessionID string) error {
	p, release, err := c.markBusy(sessionID)
	if err != nil {
		return err
	}
	var res map[string]any
	if err := p.call(ctx, "session/close", sessionIDParams{SessionID: sessionID}, &res); err != nil {
		release()
		return err
	}
	c.mu.Lock()
	var toStop *folderProc
	if entry, ok := c.sessions[sessionID]; ok {
		delete(c.sessions, sessionID)
		if entry.proc != nil && c.releaseRefLocked(entry.proc) {
			toStop = entry.proc
		}
	}
	c.mu.Unlock()
	release()
	if toStop != nil {
		toStop.stop()
	}
	return nil
}

// Prompt executes a session/prompt turn, invoking onUpdate for each session/update notification.
func (c *Client) Prompt(ctx context.Context, sessionID string, blocks []ContentBlock, onUpdate func(rawUpdate []byte)) (PromptResult, error) {
	sub := &promptSubscriber{onUpdate: onUpdate}
	c.mu.Lock()
	entry, ok := c.sessions[sessionID]
	if !ok {
		c.mu.Unlock()
		return PromptResult{}, ErrSessionNotFound
	}
	if _, busy := c.inFlight[sessionID]; busy {
		c.mu.Unlock()
		return PromptResult{}, ErrPromptInFlight
	}
	c.inFlight[sessionID] = sub
	p := entry.proc
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
	if err := p.call(ctx, "session/prompt", params, &rpcRes); err != nil {
		if ctx.Err() != nil {
			_ = p.notify("session/cancel", sessionIDParams{SessionID: sessionID})
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
	c.mu.Lock()
	entry, ok := c.sessions[sessionID]
	c.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}
	return entry.proc.notify("session/cancel", sessionIDParams{SessionID: sessionID})
}

func (c *Client) handleSessionUpdate(p *folderProc, rawParams json.RawMessage) {
	var up sessionUpdateParams
	if err := json.Unmarshal(rawParams, &up); err != nil || up.SessionID == "" {
		return
	}
	var env sessionUpdateEnvelope
	_ = json.Unmarshal(up.Update, &env)

	c.mu.Lock()
	entry, tracked := c.sessions[up.SessionID]
	if !tracked || entry.proc != p {
		c.mu.Unlock()
		return
	}
	c.applySettingsUpdate(up.SessionID, entry, env)
	sub, ok := c.inFlight[up.SessionID]
	if ok {
		if sub.onUpdate == nil {
			sub.updates = append(sub.updates, up.Update)
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

// applySettingsUpdate keeps a live session's configOptions in step with the
// settings agy changes on its own (config_option_update carries the full
// list, current_mode_update the new mode). The caller holds c.mu.
func (c *Client) applySettingsUpdate(sessionID string, entry sessionEntry, env sessionUpdateEnvelope) {
	info := entry.info
	switch env.SessionUpdate {
	case "config_option_update":
		if opts := parseConfigOptions(env.ConfigOptions); opts != nil {
			info.ConfigOptions = opts
		}
	case "current_mode_update":
		if env.CurrentModeID == "" {
			return
		}
		opts := slices.Clone(info.ConfigOptions)
		for i := range opts {
			if opts[i].ID == ConfigMode {
				opts[i].CurrentValue = env.CurrentModeID
			}
		}
		info.ConfigOptions = opts
	default:
		return
	}
	entry.info = info
	c.sessions[sessionID] = entry
}
