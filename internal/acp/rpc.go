package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

func (p *folderProc) dispatchIncoming(msg rpcMessage) {
	if msg.Method != "" && msg.ID != nil {
		p.handleAgentRequest(*msg.ID, msg.Method, msg.Params)
		return
	}
	if msg.Method == "session/update" {
		p.client.handleSessionUpdate(p, msg.Params)
		return
	}
	if msg.ID == nil {
		return
	}
	p.mu.Lock()
	call, ok := p.pending[*msg.ID]
	if ok {
		delete(p.pending, *msg.ID)
	}
	p.mu.Unlock()
	if ok {
		call.res = msg
		close(call.ch)
	}
}

func (p *folderProc) ownsSession(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	p.client.mu.Lock()
	entry, ok := p.client.sessions[sessionID]
	p.client.mu.Unlock()
	return ok && entry.proc == p
}

func (p *folderProc) handleAgentRequest(id int64, method string, rawParams json.RawMessage) {
	if method != "session/request_permission" {
		var sp sessionIDParams
		_ = json.Unmarshal(rawParams, &sp)
		attrs := []any{"method", clip(method)}
		if p.ownsSession(sp.SessionID) {
			attrs = append(attrs, "conversationId", clip(sp.SessionID))
		} else if sp.SessionID != "" {
			attrs = append(attrs, "claimedConversationId", clip(sp.SessionID), "dir", p.dir)
		}
		p.client.log.Info("unsupported agent request", attrs...)
		_ = p.writeEnvelope(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": -32601, "message": "method not supported by client"},
		})
		return
	}
	var params requestPermissionParams
	decodeErr := json.Unmarshal(rawParams, &params)
	if !p.ownsSession(params.SessionID) {
		p.client.log.Warn("unowned agent request",
			"dir", p.dir,
			"method", clip(method),
			"claimedConversationId", clip(params.SessionID),
		)
		_ = p.writeEnvelope(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": -32602, "message": "session does not belong to this process"},
		})
		return
	}
	outcome := selectPermissionOutcome(p.client.cfg.PermissionPolicy, params.Options)
	p.client.logPermissionDecision(params, outcome, decodeErr)
	_ = p.writeEnvelope(map[string]any{
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

func (p *folderProc) call(ctx context.Context, method string, params any, out any) error {
	p.mu.Lock()
	if p.stdin == nil {
		p.mu.Unlock()
		return ErrProcessClosed
	}
	p.nextID++
	id := p.nextID
	pc := &pendingCall{ch: make(chan struct{})}
	p.pending[id] = pc
	p.mu.Unlock()

	if err := p.writeEnvelope(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
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
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return ctx.Err()
	}
}

func (p *folderProc) notify(method string, params any) error {
	if err := p.writeEnvelope(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrProcessClosed, err)
	}
	return nil
}

func (p *folderProc) writeEnvelope(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	w := p.stdin
	p.mu.Unlock()
	if w == nil {
		return errors.New("stdin closed")
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}
