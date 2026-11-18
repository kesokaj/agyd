package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"unicode"
)

func (c *Client) readLoop(r io.Reader, cmd *exec.Cmd) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			c.log.Warn("ignoring non-json stdout line from acp child", "err", err)
			continue
		}
		c.dispatchIncoming(msg)
	}
	c.onProcessExit(cmd)
}

func (c *Client) drainStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		c.log.Debug("acp stderr", "line", scanner.Text())
	}
}

func (c *Client) dispatchIncoming(msg rpcMessage) {
	if msg.Method != "" && msg.ID != nil {
		c.handleAgentRequest(*msg.ID, msg.Method, msg.Params)
		return
	}
	if msg.Method == "session/update" {
		c.handleSessionUpdate(msg.Params)
		return
	}
	if msg.ID == nil {
		return
	}
	c.mu.Lock()
	call, ok := c.pending[*msg.ID]
	if ok {
		delete(c.pending, *msg.ID)
	}
	c.mu.Unlock()
	if ok {
		call.res = msg
		close(call.ch)
	}
}

func (c *Client) handleAgentRequest(id int64, method string, rawParams json.RawMessage) {
	if method != "session/request_permission" {
		var p sessionIDParams
		_ = json.Unmarshal(rawParams, &p)
		c.log.Info("unsupported agent request", "conversationId", clip(p.SessionID), "method", clip(method))
		_ = c.writeEnvelope(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": -32601, "message": "method not supported by client"},
		})
		return
	}
	var params requestPermissionParams
	decodeErr := json.Unmarshal(rawParams, &params)
	outcome := selectPermissionOutcome(c.cfg.PermissionPolicy, params.Options)
	c.logPermissionDecision(params, outcome, decodeErr)
	_ = c.writeEnvelope(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  requestPermissionResult{Outcome: outcome},
	})
}

const (
	// maxLoggedField caps every agent-supplied string in a log line, in runes.
	maxLoggedField = 64
	// maxLoggedOptions caps how many permission options one log line lists.
	maxLoggedOptions = 8
)

// loggedOption is the part of a permission option agyd logs; the free-text
// name is left out.
type loggedOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
}

// logPermissionDecision writes one slog line per session/request_permission
// agyd answers: the conversation, the tool call id and kind, the offered
// options, the policy and the chosen outcome. The tool call title is never
// logged: for execute calls it is the shell command (SDD Privacy); HTTP
// clients already see it in the SSE tool_call update.
func (c *Client) logPermissionDecision(p requestPermissionParams, out permissionOutcome, decodeErr error) {
	var tc permissionToolCall
	_ = json.Unmarshal(p.ToolCall, &tc)
	optionKind := ""
	for _, o := range p.Options {
		if out.OptionID != "" && o.OptionID == out.OptionID {
			optionKind = o.Kind
			break
		}
	}
	opts := make([]loggedOption, 0, min(len(p.Options), maxLoggedOptions))
	for _, o := range p.Options[:min(len(p.Options), maxLoggedOptions)] {
		opts = append(opts, loggedOption{OptionID: clip(o.OptionID), Kind: clip(o.Kind)})
	}
	attrs := []any{
		"conversationId", clip(p.SessionID),
		"toolCallId", clip(tc.ToolCallID),
		"toolKind", clip(tc.Kind),
		"options", opts,
		"optionCount", len(p.Options),
		"policy", string(c.cfg.PermissionPolicy),
		"outcome", out.Outcome,
		"optionId", clip(out.OptionID),
		"optionKind", clip(optionKind),
	}
	if decodeErr != nil {
		attrs = append(attrs, "err", clip(decodeErr.Error()))
	}
	c.log.Info("permission decision", attrs...)
}

// clip returns s cut to at most maxLoggedField runes (a cut ends in "…"),
// with control and format runes (bidi overrides, zero-width) replaced by
// U+FFFD so a logged value displays as what it is. It walks s once without
// converting the whole string.
func clip(s string) string {
	var b strings.Builder
	n := 0
	for i, r := range s {
		if n == maxLoggedField {
			if i < len(s) {
				b.WriteString("…")
			}
			break
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			r = unicode.ReplacementChar
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

func selectPermissionOutcome(policy PermissionPolicy, opts []permissionOption) permissionOutcome {
	if policy == PermissionDenyAll {
		for _, want := range []string{"reject_once", "reject_always"} {
			for _, o := range opts {
				if o.Kind == want {
					return permissionOutcome{Outcome: "selected", OptionID: o.OptionID}
				}
			}
		}
		return permissionOutcome{Outcome: "cancelled"}
	}
	for _, want := range []string{"allow_once", "allow_always"} {
		for _, o := range opts {
			if o.Kind == want {
				return permissionOutcome{Outcome: "selected", OptionID: o.OptionID}
			}
		}
	}
	if len(opts) > 0 {
		return permissionOutcome{Outcome: "selected", OptionID: opts[0].OptionID}
	}
	return permissionOutcome{Outcome: "cancelled"}
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.stdin == nil {
		c.mu.Unlock()
		return ErrProcessClosed
	}
	c.nextID++
	id := c.nextID
	pc := &pendingCall{ch: make(chan struct{})}
	c.pending[id] = pc
	c.mu.Unlock()

	if err := c.writeEnvelope(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrProcessClosed, err)
	}

	select {
	case <-pc.ch:
		if pc.err != nil {
			return pc.err
		}
		if pc.res.Error != nil {
			if pc.res.Error.Code == -32602 {
				return fmt.Errorf("%w: %w", ErrInvalidArgument, pc.res.Error)
			}
			return pc.res.Error
		}
		if out != nil && len(pc.res.Result) > 0 {
			if err := json.Unmarshal(pc.res.Result, out); err != nil {
				return fmt.Errorf("decoding %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *Client) notify(method string, params any) error {
	if err := c.writeEnvelope(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrProcessClosed, err)
	}
	return nil
}

func (c *Client) writeEnvelope(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	w := c.stdin
	c.mu.Unlock()
	if w == nil {
		return errors.New("stdin closed")
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}
