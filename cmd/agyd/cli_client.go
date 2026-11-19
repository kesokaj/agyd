package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// send issues one JSON request. The stdlib client sends Host as host:port, which
// agyd's Host guard accepts for loopback, and no Origin, which passes CSRF.
func (c *cli) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
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
	req.Header.Set("Accept", "application/json")
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
	resp, err := c.send(ctx, method, path, body)
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
