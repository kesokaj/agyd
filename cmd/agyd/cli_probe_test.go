package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func serverPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", rawURL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("Port(%q): %v", rawURL, err)
	}
	return p
}

func probeCLIEnv(t *testing.T, ports []int, extraEnv map[string]string) (cliEnv, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	env := map[string]string{"HOME": t.TempDir()}
	for k, v := range extraEnv {
		env[k] = v
	}
	var stdout, stderr bytes.Buffer
	return cliEnv{
		stdout:       &stdout,
		stderr:       &stderr,
		getenv:       func(k string) string { return env[k] },
		getwd:        func() (string, error) { return "/work/here", nil },
		httpClient:   osCLIEnv().httpClient,
		probePorts:   ports,
		probeTimeout: 100 * time.Millisecond,
	}, &stdout, &stderr
}

func TestCLIProbeDiscovery(t *testing.T) {
	t.Run("skips non-agyd listener on first port without sending command, uses agyd on fallback port", func(t *testing.T) {
		var nonAgydPaths []string
		gnubby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			nonAgydPaths = append(nonAgydPaths, r.Method+" "+r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"service":"gnubby-ss"}`)
		}))
		defer gnubby.Close()

		agydSrv := newFakeAgyd(t)
		env, stdout, stderr := probeCLIEnv(t, []int{serverPort(t, gnubby.URL), serverPort(t, agydSrv.srv.URL)}, nil)

		if code := runCLI(t, t.Context(), env, "model", "claude-sonnet-5-5"); code != 0 {
			t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
		}
		if got := strings.Join(nonAgydPaths, ","); got != "GET /healthz" {
			t.Fatalf("non-agyd server saw requests %q, want GET /healthz only", got)
		}
		reqs := agydSrv.requests()
		if len(reqs) != 2 || reqs[0].path != "/healthz" || reqs[1].method != http.MethodPatch || reqs[1].path != "/v1/current" {
			t.Fatalf("agyd requests = %+v, want /healthz then PATCH /v1/current", reqs)
		}
		if !strings.Contains(stdout.String(), "conversation  c1") {
			t.Errorf("stdout = %q, want conversation  c1", stdout)
		}
		if stderr.String() != infoLine {
			t.Errorf("stderr = %q, want single info line %q", stderr, infoLine)
		}
	})

	t.Run("prefers first port when agyd answers there and never touches second", func(t *testing.T) {
		first := newFakeAgyd(t)
		second := newFakeAgyd(t)
		env, stdout, _ := probeCLIEnv(t, []int{serverPort(t, first.srv.URL), serverPort(t, second.srv.URL)}, nil)

		if code := runCLI(t, t.Context(), env, "current"); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if len(second.requests()) != 0 {
			t.Errorf("second port saw %d requests, want 0", len(second.requests()))
		}
		if !strings.Contains(stdout.String(), "conversation  c1") {
			t.Errorf("stdout = %q", stdout)
		}
	})

	t.Run("skips closed port, 204 healthz, HTML healthz, redirect healthz, and hung TCP listener", func(t *testing.T) {
		closedPort := freeLoopbackPort(t)

		old204 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer old204.Close()

		htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `{"service":"agyd"}`)
		}))
		defer htmlSrv.Close()

		agydSrv := newFakeAgyd(t)
		redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, agydSrv.srv.URL+r.URL.Path, http.StatusFound)
		}))
		defer redir.Close()

		hungLn, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer hungLn.Close()
		doneHung := make(chan struct{})
		defer close(doneHung)
		go func() {
			for {
				conn, err := hungLn.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					<-doneHung
					_ = c.Close()
				}(conn)
			}
		}()

		ports := []int{
			closedPort,
			serverPort(t, old204.URL),
			serverPort(t, htmlSrv.URL),
			serverPort(t, redir.URL),
			hungLn.Addr().(*net.TCPAddr).Port,
			serverPort(t, agydSrv.srv.URL),
		}
		env, _, stderr := probeCLIEnv(t, ports, nil)
		// Even if a caller supplies an http.Client with nil CheckRedirect, probe must not follow redirects.
		env.httpClient = &http.Client{}
		if code := runCLI(t, t.Context(), env, "current"); code != 0 {
			t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
		}
		reqs := agydSrv.requests()
		if len(reqs) != 2 || reqs[0].path != "/healthz" || reqs[1].path != "/v1/current" {
			t.Fatalf("agyd requests = %+v, want direct /healthz then /v1/current", reqs)
		}
	})

	t.Run("when neither port answers as agyd, exits 1 without sending command to non-agyd", func(t *testing.T) {
		var sawCommand atomic.Bool
		gnubby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				sawCommand.Store(true)
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer gnubby.Close()
		p1 := serverPort(t, gnubby.URL)
		p2 := freeLoopbackPort(t)

		env, _, stderr := probeCLIEnv(t, []int{p1, p2}, nil)
		code := runCLI(t, t.Context(), env, "model", "claude-sonnet-5-5")
		if code != 1 {
			t.Fatalf("exit = %d, want 1; stderr: %s", code, stderr)
		}
		if sawCommand.Load() {
			t.Fatal("CLI sent command to non-agyd listener when probe failed")
		}
		for _, want := range []string{strconv.Itoa(p1), strconv.Itoa(p2)} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr %q missing probed port %s", stderr.String(), want)
			}
		}
	})

	t.Run("AGYD_URL dials directly without probing candidate ports", func(t *testing.T) {
		var probed atomic.Bool
		decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			probed.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"service":"agyd"}`)
		}))
		defer decoy.Close()

		direct := newFakeAgyd(t)
		env, stdout, stderr := probeCLIEnv(t, []int{serverPort(t, decoy.URL)}, map[string]string{
			"AGYD_URL": direct.srv.URL + "/",
		})
		if code := runCLI(t, t.Context(), env, "current"); code != 0 {
			t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
		}
		if probed.Load() {
			t.Error("AGYD_URL set, but candidate port was still probed")
		}
		if reqs := direct.requests(); len(reqs) != 1 || reqs[0].path != "/v1/current" {
			t.Errorf("direct requests = %+v, want single GET /v1/current", reqs)
		}
		if !strings.Contains(stdout.String(), "conversation  c1") {
			t.Errorf("stdout = %q", stdout)
		}
	})

	t.Run("usage errors in default probe mode exit 2 with zero network probes", func(t *testing.T) {
		var probed atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			probed.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"service":"agyd"}`)
		}))
		defer srv.Close()

		env, _, _ := probeCLIEnv(t, []int{serverPort(t, srv.URL)}, nil)
		if code := runCLI(t, t.Context(), env, "use"); code != 2 {
			t.Fatalf("empty use exit = %d, want 2", code)
		}
		if code := runCLI(t, t.Context(), env, "model"); code != 2 {
			t.Fatalf("empty model exit = %d, want 2", code)
		}
		if code := runCLI(t, t.Context(), env, "current", "--port", ""); code != 2 {
			t.Fatalf("empty --port exit = %d, want 2", code)
		}
		if code := runCLI(t, t.Context(), env, "current", "--host", ""); code != 2 {
			t.Fatalf("empty --host exit = %d, want 2", code)
		}
		if probed.Load() {
			t.Error("usage error triggered /healthz network probe")
		}
	})
}
