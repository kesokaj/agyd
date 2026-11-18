package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
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
		_ = c.writeEnvelope(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": -32601, "message": "method not supported by client"},
		})
		return
	}
	var params requestPermissionParams
	_ = json.Unmarshal(rawParams, &params)
	outcome := selectPermissionOutcome(c.cfg.PermissionPolicy, params.Options)
	_ = c.writeEnvelope(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  requestPermissionResult{Outcome: outcome},
	})
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
