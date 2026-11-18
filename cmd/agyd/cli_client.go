package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agyd/internal/api"
)

// cli is one client command's connection to a running agyd.
type cli struct {
	env        cliEnv
	base       string      // http(s)://host[:port] once pinned or probed
	candidates []string    // candidate base URLs to probe on /healthz when base == ""
	json       bool        // --json: raw API responses on stdout
	header     http.Header // headers of the last response
}

// apiErr is an AIP-193 error envelope returned by agyd.
type apiErr struct {
	Code    int    `json:"code"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Details []struct {
		Reason string `json:"reason"`
	} `json:"details"`
}

func (e *apiErr) Error() string {
	reason := ""
	for _, d := range e.Details {
		if d.Reason != "" {
			reason = " (" + d.Reason + ")"
			break
		}
	}
	return fmt.Sprintf("%d %s: %s%s", e.Code, e.Status, e.Message, reason)
}

// decodeAPIErr turns an error response body into an *apiErr, falling back to
// the HTTP status when the body is not an envelope.
func decodeAPIErr(status int, body []byte) error {
	var env struct {
		Error *apiErr `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error != nil && env.Error.Message != "" {
		return env.Error
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &apiErr{Code: status, Status: http.StatusText(status), Message: msg}
}

// send issues one request. The stdlib client sends Host as host:port, which
// agyd's Host guard accepts for loopback, and no Origin, which passes CSRF.
func (c *cli) send(ctx context.Context, method, path string, body any, accept string) (*http.Response, error) {
	base, err := c.resolveBase(ctx)
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.env.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("interrupted: %w", ctx.Err())
		}
		return nil, fmt.Errorf("cannot reach agyd at %s (is it running? start it with: agyd): %w", base, err)
	}
	c.header = resp.Header
	c.printInfo(resp.Header)
	return resp, nil
}

// call runs a JSON request. With --json the body is copied to stdout as is
// (error bodies too); otherwise a success body is decoded into out.
func (c *cli) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.send(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if c.json && len(data) > 0 {
		_, _ = c.env.stdout.Write(data)
		if data[len(data)-1] != '\n' {
			_, _ = io.WriteString(c.env.stdout, "\n")
		}
	}
	if resp.StatusCode >= 400 {
		return decodeAPIErr(resp.StatusCode, data)
	}
	if c.json || out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// printInfo writes the one-line visibility summary (D8) from the Agyd-*
// headers to stderr; agyd omits a header whose value is unknown.
func (c *cli) printInfo(h http.Header) {
	id := h.Get(api.HeaderConversation)
	if id == "" {
		fmt.Fprintln(c.env.stderr, "agyd: conversation=none")
		return
	}
	or := func(v string) string {
		if v == "" {
			return "-"
		}
		return v
	}
	line := fmt.Sprintf("agyd: conversation=%s model=%s thinking=%s", id,
		or(h.Get(api.HeaderModel)), or(h.Get(api.HeaderThinkingLevel)))
	if m := h.Get(api.HeaderMode); m != "" {
		line += " mode=" + m
	}
	fmt.Fprintln(c.env.stderr, line)
}

// promptResult is the part of a finished turn the CLI uses.
type promptResult struct {
	ConversationID string `json:"conversationId"`
	StopReason     string `json:"stopReason"`
	Text           string `json:"text"`
}

// streamPrompt posts a prompt with Accept: text/event-stream and writes the
// agent's text chunks to stdout as they arrive. Cancelling ctx (Ctrl-C)
// drops the connection, which makes agyd cancel the turn.
func (c *cli) streamPrompt(ctx context.Context, path, prompt string) error {
	resp, err := c.send(ctx, http.MethodPost, path, map[string]string{"prompt": prompt}, "text/event-stream")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return decodeAPIErr(resp.StatusCode, data)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var res promptResult
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
		return c.finishTurn(res, res.Text)
	}
	var written strings.Builder
	err = readSSE(resp.Body, func(event string, data []byte) (bool, error) {
		switch event {
		case "session_update":
			if text := chunkText(data); text != "" {
				written.WriteString(text)
				_, _ = io.WriteString(c.env.stdout, text)
			}
		case "prompt_done":
			var res promptResult
			if err := json.Unmarshal(data, &res); err != nil {
				return true, fmt.Errorf("decoding prompt_done: %w", err)
			}
			return true, c.finishTurn(res, written.String())
		case "prompt_error":
			return true, decodeAPIErr(http.StatusBadGateway, data)
		}
		return false, nil
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("interrupted: %w", ctx.Err())
	}
	return err
}

// finishTurn ends the reply with a newline and reports an unusual stop.
func (c *cli) finishTurn(res promptResult, shown string) error {
	if shown == "" && res.Text != "" {
		_, _ = io.WriteString(c.env.stdout, res.Text)
		shown = res.Text
	}
	if shown != "" && !strings.HasSuffix(shown, "\n") {
		_, _ = io.WriteString(c.env.stdout, "\n")
	}
	if res.StopReason != "" && res.StopReason != "end_turn" {
		fmt.Fprintf(c.env.stderr, "agyd: turn stopped: %s\n", res.StopReason)
	}
	return nil
}

// chunkText returns the text of an agent_message_chunk session update
// ({sessionId, update: {sessionUpdate, content: {type, text}}}), else "".
func chunkText(data []byte) string {
	var p struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	if json.Unmarshal(data, &p) != nil || p.Update.SessionUpdate != "agent_message_chunk" || p.Update.Content.Type != "text" {
		return ""
	}
	return p.Update.Content.Text
}

var errStreamEnded = errors.New("the stream ended before the turn finished")

// readSSE calls fn for each event until fn reports done. Lines are read
// without a length limit, since one update can carry a large tool result.
func readSSE(r io.Reader, fn func(event string, data []byte) (done bool, err error)) error {
	br := bufio.NewReader(r)
	var event string
	var data []byte
	hasData := false
	for {
		line, err := br.ReadString('\n')
		if errors.Is(err, io.EOF) { // an unterminated last event is incomplete too
			return errStreamEnded
		}
		if err != nil {
			return fmt.Errorf("reading stream: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if event != "" || hasData {
				done, ferr := fn(event, data)
				if done || ferr != nil {
					return ferr
				}
			}
			event, data, hasData = "", nil, false
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if hasData {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
			hasData = true
		}
	}
}
