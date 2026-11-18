package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agyd/internal/api"
)

const (
	defaultProbeTimeout = time.Second
	maxHealthzBytes     = 4096
)

type clientTarget struct {
	base       string   // pinned URL when --port, AGYD_PORT, or AGYD_URL is set
	candidates []string // candidate URLs probed via GET /healthz when no port/URL is set
}

// resolveClientTarget resolves where the CLI looks for agyd (ADR-006):
// explicit --host/--port flags win over AGYD_URL, which wins over
// AGYD_HOST/AGYD_PORT; when no port or AGYD_URL is set, it returns candidate
// URLs on ports 8080 then 3000 for lazy /healthz probing.
func resolveClientTarget(o *cliOpts, env cliEnv) (clientTarget, error) {
	if !o.hostSet && !o.portSet {
		if rawURL := env.getenv("AGYD_URL"); rawURL != "" {
			u, err := parseAgydURL(rawURL)
			if err != nil {
				return clientTarget{}, err
			}
			return clientTarget{base: u}, nil
		}
	}
	rawHost := o.host
	if !o.hostSet {
		rawHost = env.getenv("AGYD_HOST")
		if rawHost == "" {
			rawHost = "127.0.0.1"
		}
	}
	host, err := normalizeDialHost(rawHost)
	if err != nil {
		return clientTarget{}, err
	}
	rawPort := o.port
	if !o.portSet {
		rawPort = env.getenv("AGYD_PORT")
	}
	if o.portSet || rawPort != "" {
		port, err := parsePort(rawPort)
		if err != nil {
			return clientTarget{}, err
		}
		return clientTarget{base: "http://" + net.JoinHostPort(host, strconv.Itoa(port))}, nil
	}
	ports := env.probePorts
	if len(ports) == 0 {
		ports = defaultServePorts
	}
	candidates := make([]string, len(ports))
	for i, p := range ports {
		candidates[i] = "http://" + net.JoinHostPort(host, strconv.Itoa(p))
	}
	return clientTarget{candidates: candidates}, nil
}

func normalizeDialHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	ip := net.ParseIP(host)
	switch {
	case host == "localhost":
		return host, nil
	case ip == nil:
		return "", fmt.Errorf("invalid host %q: want an IP address or localhost", raw)
	case ip.IsUnspecified() && ip.To4() != nil:
		return "127.0.0.1", nil
	case ip.IsUnspecified():
		return "::1", nil
	default:
		return host, nil
	}
}

func parseAgydURL(raw string) (string, error) {
	const errMsg = "invalid AGYD_URL: want http://host[:port] or https://host[:port] with no userinfo, path, query, or fragment"
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New(errMsg)
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New(errMsg)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || (u.RawPath != "" && u.RawPath != "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New(errMsg)
	}
	host := strings.ToLower(u.Hostname())
	if !validURLHost(host) {
		return "", errors.New(errMsg)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") && port == "" {
		return "", errors.New(errMsg)
	}
	if port != "" {
		n, err := parsePort(port)
		if err != nil {
			return "", fmt.Errorf("invalid AGYD_URL: %w", err)
		}
		return u.Scheme + "://" + net.JoinHostPort(host, strconv.Itoa(n)), nil
	}
	if strings.Contains(host, ":") {
		return u.Scheme + "://[" + host + "]", nil
	}
	return u.Scheme + "://" + host, nil
}

func validURLHost(h string) bool {
	if net.ParseIP(h) != nil {
		return true
	}
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
	}
	return true
}

func (c *cli) resolveBase(ctx context.Context) (string, error) {
	if c.base != "" {
		return c.base, nil
	}
	timeout := c.env.probeTimeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	client := probeHTTPClient(c.env.httpClient)
	for _, cand := range c.candidates {
		if ctx.Err() != nil {
			return "", fmt.Errorf("interrupted: %w", ctx.Err())
		}
		if probeHealthz(ctx, client, cand, timeout) {
			c.base = cand
			return cand, nil
		}
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("interrupted: %w", ctx.Err())
	}
	return "", fmt.Errorf("cannot find a running agyd at %s (is it running? start it with: agyd, or set AGYD_URL / --port)",
		strings.Join(c.candidates, " or "))
}

func probeHTTPClient(hc *http.Client) *http.Client {
	if hc == nil {
		hc = &http.Client{}
	}
	if hc.CheckRedirect != nil {
		return hc
	}
	cp := *hc
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}

func probeHealthz(ctx context.Context, client *http.Client, base string, timeout time.Duration) bool {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthzBytes+1))
	if err != nil || len(body) > maxHealthzBytes {
		return false
	}
	var h struct {
		Service string `json:"service"`
	}
	return json.Unmarshal(body, &h) == nil && h.Service == api.ServiceName
}
