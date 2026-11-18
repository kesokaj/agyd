package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agyd/internal/api"
)

// Canned agyd responses, shaped like internal/web/static/openapi.json.
const (
	convBody     = `{"conversationId":"c1","cwd":"/w","live":true,"current":true,"model":"gemini-4-carbon","thinkingLevel":"medium","mode":"default"}` + "\n"
	listBody     = `{"items":[{"conversationId":"c1","cwd":"/w","live":true,"current":true,"model":"gemini-4-carbon","thinkingLevel":"medium"},{"conversationId":"hist_1","cwd":"/hist","title":"old one","live":false,"current":false,"updateTime":"2026-11-01T00:00:00Z"}]}` + "\n"
	modelsBody   = `{"models":[{"value":"gemini-4-carbon","name":"Gemini 4 Carbon"},{"value":"claude-sonnet-5-5","name":"Claude Sonnet 5.5"}],"thinkingLevels":[{"value":"low","name":"Low"},{"value":"medium","name":"Medium"}],"modes":[{"value":"default","name":"Default"}]}` + "\n"
	defaultsBody = `{"model":"gemini-4-carbon","thinkingLevel":"high"}` + "\n"
	promptBody   = `{"conversationId":"c1","model":"gemini-4-carbon","stopReason":"end_turn","text":"Hello, world"}` + "\n"
	noCurrent    = `{"error":{"code":404,"status":"NOT_FOUND","message":"no current conversation; create one or use one","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"NO_CURRENT_CONVERSATION","domain":"agyd.local"}]}}` + "\n"
	infoLine     = "agyd: conversation=c1 model=gemini-4-carbon thinking=medium mode=default\n"
)

type recordedRequest struct {
	method, path, accept, body string
}

// fakeAgyd is an httptest agyd behind the real Host guard and CSRF check,
// so the client is proven to pass both.
type fakeAgyd struct {
	mu        sync.Mutex
	reqs      []recordedRequest
	noCurrent bool                                         // no current conversation: 404s and no Agyd-* headers
	sse       func(w http.ResponseWriter, r *http.Request) // overrides the SSE prompt reply
	srv       *httptest.Server
}

func newFakeAgyd(t *testing.T) *fakeAgyd {
	t.Helper()
	f := &fakeAgyd{}
	f.srv = httptest.NewServer(api.Guard(http.HandlerFunc(f.serve)))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAgyd) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recordedRequest{r.Method, r.URL.EscapedPath(), r.Header.Get("Accept"), string(b)})
	none, sse := f.noCurrent, f.sse
	f.mu.Unlock()
	if !none {
		w.Header().Set(api.HeaderConversation, "c1")
		w.Header().Set(api.HeaderModel, "gemini-4-carbon")
		w.Header().Set(api.HeaderThinkingLevel, "medium")
		w.Header().Set(api.HeaderMode, "default")
	}
	reply := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	switch path := r.URL.Path; {
	case none && path == "/v1/current":
		reply(http.StatusNotFound, noCurrent)
	case strings.HasSuffix(path, "/prompt") && strings.Contains(r.Header.Get("Accept"), "text/event-stream"):
		if sse != nil {
			sse(w, r)
			return
		}
		writeSSE(w, "session_update", `{"sessionId":"c1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking..."}}}`)
		writeSSE(w, "session_update", `{"sessionId":"c1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello, "}}}`)
		writeSSE(w, "session_update", `{"sessionId":"c1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"world"}}}`)
		writeSSE(w, "prompt_done", strings.TrimSpace(promptBody))
	case strings.HasSuffix(path, "/prompt"):
		reply(http.StatusOK, promptBody)
	case path == "/v1/conversations" && r.Method == http.MethodGet:
		reply(http.StatusOK, listBody)
	case path == "/v1/conversations":
		reply(http.StatusCreated, convBody)
	case path == "/v1/models":
		reply(http.StatusOK, modelsBody)
	case path == "/v1/defaults":
		reply(http.StatusOK, defaultsBody)
	default:
		reply(http.StatusOK, convBody)
	}
}

func writeSSE(w http.ResponseWriter, event, data string) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	w.(http.Flusher).Flush()
}

func (f *fakeAgyd) requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// testCLIEnv points the client at addr through AGYD_HOST/AGYD_PORT with an
// empty HOME, so no real config.env is read.
func testCLIEnv(t *testing.T, addr, stdin string, extraEnv map[string]string) (cliEnv, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	env := map[string]string{"HOME": t.TempDir(), "AGYD_HOST": host, "AGYD_PORT": port}
	for k, v := range extraEnv {
		env[k] = v
	}
	var stdout, stderr bytes.Buffer
	return cliEnv{
		stdin: strings.NewReader(stdin), stdout: &stdout, stderr: &stderr,
		getenv:     func(k string) string { return env[k] },
		getwd:      func() (string, error) { return "/work/here", nil },
		readFile:   os.ReadFile,
		httpClient: &http.Client{},
	}, &stdout, &stderr
}

// runCLI goes through dispatch exactly as main does.
func runCLI(t *testing.T, ctx context.Context, env cliEnv, args ...string) int {
	t.Helper()
	cmd, rest, err := dispatch(args)
	if err != nil || cmd == "serve" {
		t.Fatalf("dispatch(%q) = %q, %v; want a client command", args, cmd, err)
	}
	return runClient(ctx, cmd, rest, env)
}

func fakeAddr(f *fakeAgyd) string {
	u, _ := url.Parse(f.srv.URL)
	return u.Host
}

func TestCLICommandsHitTheirEndpoints(t *testing.T) {
	tests := []struct {
		args                 []string
		method, path, accept string
		body                 string
		stdout               []string // substrings of the text output
	}{
		{[]string{"ask", "hello", "world"}, "POST", "/v1/prompt", "text/event-stream", `{"prompt":"hello world"}`, []string{"Hello, world\n"}},
		{[]string{"ask", "--conversation", "c/2", "hi"}, "POST", "/v1/conversations/c%2F2/prompt", "text/event-stream", `{"prompt":"hi"}`, nil},
		{[]string{"new", "--model", "claude-sonnet-5-5", "--thinking", "high"}, "POST", "/v1/conversations", "application/json",
			`{"cwd":"/work/here","model":"claude-sonnet-5-5","thinkingLevel":"high"}`, []string{"conversation  c1", "model         gemini-4-carbon"}},
		{[]string{"list"}, "GET", "/v1/conversations", "application/json", "", []string{"* c1", "  hist_1", "old one"}},
		{[]string{"use", "hist_1"}, "POST", "/v1/conversations/hist_1/use", "application/json", "", []string{"conversation  c1"}},
		{[]string{"current"}, "GET", "/v1/current", "application/json", "", []string{"cwd           /w"}},
		{[]string{"model", "claude-sonnet-5-5", "--thinking", "low"}, "PATCH", "/v1/current", "application/json",
			`{"model":"claude-sonnet-5-5","thinkingLevel":"low"}`, nil},
		{[]string{"model", "--mode", "plan"}, "PATCH", "/v1/current", "application/json", `{"mode":"plan"}`, nil},
		{[]string{"models"}, "GET", "/v1/models", "application/json", "", []string{"* gemini-4-carbon", "  claude-sonnet-5-5", "* medium", "THINKING LEVELS"}},
		{[]string{"defaults"}, "GET", "/v1/defaults", "application/json", "", []string{"thinking  high", "mode      (agy default)"}},
		{[]string{"defaults", "--model", "gemini-4-carbon", "--mode", "plan"}, "PUT", "/v1/defaults", "application/json",
			`{"model":"gemini-4-carbon","mode":"plan"}`, []string{"saved to config.env"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			f := newFakeAgyd(t)
			env, stdout, stderr := testCLIEnv(t, fakeAddr(f), "", nil)
			if code := runCLI(t, t.Context(), env, tt.args...); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
			}
			reqs := f.requests()
			if len(reqs) != 1 {
				t.Fatalf("got %d requests, want 1: %+v", len(reqs), reqs)
			}
			want := recordedRequest{tt.method, tt.path, tt.accept, tt.body}
			if reqs[0] != want {
				t.Errorf("request = %+v, want %+v", reqs[0], want)
			}
			if stderr.String() != infoLine {
				t.Errorf("stderr = %q, want the info line %q", stderr, infoLine)
			}
			for _, s := range tt.stdout {
				if !strings.Contains(stdout.String(), s) {
					t.Errorf("stdout missing %q:\n%s", s, stdout)
				}
			}
		})
	}
}

func TestCLIJSONPrintsTheRawResponse(t *testing.T) {
	tests := []struct {
		args       []string
		wantAccept string
		want       string
	}{
		{[]string{"current", "--json"}, "application/json", convBody},
		{[]string{"list", "--json"}, "application/json", listBody},
		{[]string{"models", "--json"}, "application/json", modelsBody},
		{[]string{"defaults", "--json"}, "application/json", defaultsBody},
		{[]string{"ask", "hi", "--json"}, "application/json", promptBody}, // flag after the prompt; buffered, not SSE
		{[]string{"new", "--json"}, "application/json", convBody},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			f := newFakeAgyd(t)
			env, stdout, stderr := testCLIEnv(t, fakeAddr(f), "", nil)
			if code := runCLI(t, t.Context(), env, tt.args...); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
			}
			if stdout.String() != tt.want {
				t.Errorf("stdout = %q, want the raw body %q", stdout, tt.want)
			}
			if got := f.requests()[0].accept; got != tt.wantAccept {
				t.Errorf("Accept = %q, want %q", got, tt.wantAccept)
			}
			if stderr.String() != infoLine {
				t.Errorf("stderr = %q, want the info line on stderr even with --json", stderr)
			}
		})
	}
}

func TestCLIErrorsShowInfoLineAndEnvelope(t *testing.T) {
	f := newFakeAgyd(t)
	f.noCurrent = true

	env, stdout, stderr := testCLIEnv(t, fakeAddr(f), "", nil)
	if code := runCLI(t, t.Context(), env, "current"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	want := "agyd: conversation=none\nagyd: 404 NOT_FOUND: no current conversation; create one or use one (NO_CURRENT_CONVERSATION)\n"
	if stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout)
	}

	env, stdout, _ = testCLIEnv(t, fakeAddr(f), "", nil)
	if code := runCLI(t, t.Context(), env, "current", "--json"); code != 1 {
		t.Fatalf("--json exit = %d, want 1", code)
	}
	if stdout.String() != noCurrent {
		t.Errorf("--json stdout = %q, want the error envelope", stdout)
	}
}

func TestCLIAskStreaming(t *testing.T) {
	t.Run("prompt from stdin", func(t *testing.T) {
		f := newFakeAgyd(t)
		env, stdout, _ := testCLIEnv(t, fakeAddr(f), "from a pipe\n", nil)
		if code := runCLI(t, t.Context(), env, "ask"); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if got := f.requests()[0].body; got != `{"prompt":"from a pipe\n"}` {
			t.Errorf("body = %s", got)
		}
		if stdout.String() != "Hello, world\n" {
			t.Errorf("stdout = %q", stdout)
		}
	})

	t.Run("prompt_error mid-stream fails after the partial text", func(t *testing.T) {
		f := newFakeAgyd(t)
		f.sse = func(w http.ResponseWriter, _ *http.Request) {
			writeSSE(w, "session_update", `{"sessionId":"c1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"partial"}}}`)
			writeSSE(w, "prompt_error", `{"error":{"code":502,"status":"BAD_GATEWAY","message":"acp child process exited unexpectedly","details":[{"reason":"ACP_PROCESS_EXITED"}]}}`)
		}
		env, stdout, stderr := testCLIEnv(t, fakeAddr(f), "", nil)
		if code := runCLI(t, t.Context(), env, "ask", "hi"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if stdout.String() != "partial" || !strings.Contains(stderr.String(), "502 BAD_GATEWAY: acp child process exited unexpectedly (ACP_PROCESS_EXITED)") {
			t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
		}
	})

	t.Run("stream that ends without prompt_done fails", func(t *testing.T) {
		f := newFakeAgyd(t)
		f.sse = func(w http.ResponseWriter, _ *http.Request) {
			writeSSE(w, "session_update", `{"sessionId":"c1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"cut"}}}`)
		}
		env, _, stderr := testCLIEnv(t, fakeAddr(f), "", nil)
		if code := runCLI(t, t.Context(), env, "ask", "hi"); code != 1 || !strings.Contains(stderr.String(), errStreamEnded.Error()) {
			t.Errorf("exit = %d, stderr = %q; want 1 and %q", code, stderr, errStreamEnded)
		}
	})

	t.Run("interrupt drops the connection so agyd cancels the turn", func(t *testing.T) {
		f := newFakeAgyd(t)
		ctx, cancel := context.WithCancel(t.Context())
		serverSawCancel := make(chan struct{})
		f.sse = func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, "session_update", `{"sessionId":"c1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"long"}}}`)
			cancel() // the operator presses Ctrl-C mid-turn
			<-r.Context().Done()
			close(serverSawCancel)
		}
		env, _, stderr := testCLIEnv(t, fakeAddr(f), "", nil)
		if code := runCLI(t, ctx, env, "ask", "hi"); code != 1 || !strings.Contains(stderr.String(), "interrupted") {
			t.Fatalf("exit = %d, stderr = %q; want 1 and interrupted", code, stderr)
		}
		select {
		case <-serverSawCancel:
		case <-time.After(5 * time.Second):
			t.Error("agyd never saw the request context cancelled")
		}
	})
}

func TestCLIUsageErrorsSendNothing(t *testing.T) {
	for _, args := range [][]string{
		{"use"},
		{"use", "a", "b"},
		{"model"},
		{"ask", "--bogus", "hi"},
		{"current", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := newFakeAgyd(t)
			env, _, stderr := testCLIEnv(t, fakeAddr(f), "", nil)
			if code := runCLI(t, t.Context(), env, args...); code != 2 {
				t.Errorf("exit = %d, want 2; stderr: %s", code, stderr)
			}
			if n := len(f.requests()); n != 0 {
				t.Errorf("sent %d requests, want 0", n)
			}
		})
	}
	t.Run("ask with empty stdin", func(t *testing.T) {
		f := newFakeAgyd(t)
		env, _, _ := testCLIEnv(t, fakeAddr(f), "  \n", nil)
		if code := runCLI(t, t.Context(), env, "ask"); code != 2 || len(f.requests()) != 0 {
			t.Errorf("exit = %d, requests = %d; want 2 and none", code, len(f.requests()))
		}
	})
}

func TestCLIUnreachableServer(t *testing.T) {
	f := newFakeAgyd(t)
	addr := fakeAddr(f)
	f.srv.Close()
	env, _, stderr := testCLIEnv(t, addr, "", nil)
	if code := runCLI(t, t.Context(), env, "current"); code != 1 || !strings.Contains(stderr.String(), "cannot reach agyd at http://"+addr) {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestDispatchServeVersusClient(t *testing.T) {
	tests := []struct {
		args     []string
		wantCmd  string
		wantRest []string
		wantErr  bool
	}{
		{nil, "serve", nil, false},
		{[]string{"-port", "9090"}, "serve", []string{"-port", "9090"}, false},
		{[]string{"--host=0.0.0.0"}, "serve", []string{"--host=0.0.0.0"}, false},
		{[]string{"-help"}, "serve", []string{"-help"}, false},
		{[]string{"serve"}, "serve", []string{}, false},
		{[]string{"serve", "-port", "9090"}, "serve", []string{"-port", "9090"}, false},
		{[]string{"help"}, "help", []string{}, false},
		{[]string{"ask", "hi", "--json"}, "ask", []string{"hi", "--json"}, false},
		{[]string{"bogus"}, "", nil, true},
		{[]string{"Ask"}, "", nil, true},
	}
	for _, name := range cliOrder {
		tests = append(tests, struct {
			args     []string
			wantCmd  string
			wantRest []string
			wantErr  bool
		}{[]string{name}, name, []string{}, false})
	}
	for _, tt := range tests {
		cmd, rest, err := dispatch(tt.args)
		if (err != nil) != tt.wantErr || cmd != tt.wantCmd || !slices.Equal(rest, tt.wantRest) {
			t.Errorf("dispatch(%q) = %q, %q, %v; want %q, %q, err %v", tt.args, cmd, rest, err, tt.wantCmd, tt.wantRest, tt.wantErr)
		}
	}
	if len(cliOrder) != len(cliCommands) {
		t.Errorf("cliOrder has %d names, cliCommands %d", len(cliOrder), len(cliCommands))
	}
}

func TestClientHelp(t *testing.T) {
	env, stdout, _ := testCLIEnv(t, "127.0.0.1:1", "", nil)
	if code := runClient(t.Context(), "help", nil, env); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
	for _, name := range cliOrder {
		if !strings.Contains(stdout.String(), "\n  "+name) {
			t.Errorf("help does not list %q:\n%s", name, stdout)
		}
	}
	env, stdout, _ = testCLIEnv(t, "127.0.0.1:1", "", nil)
	if code := runCLI(t, t.Context(), env, "new", "-h"); code != 0 || !strings.Contains(stdout.String(), "Usage: agyd new") || !strings.Contains(stdout.String(), "-thinking") {
		t.Errorf("new -h exit = %d, stdout:\n%s", code, stdout)
	}
}

// TestClientBaseURL covers D15: flag > shell env > config.env, never ./.env,
// and an unspecified bind address dials loopback.
func TestClientBaseURL(t *testing.T) {
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "agyd")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.env"), []byte("AGYD_PORT=7000\nAGYD_HOST=0.0.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".env"), []byte("AGYD_PORT=6000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		home    string
		env     map[string]string
		opts    cliOpts
		want    string
		wantErr bool
	}{
		{"defaults", t.TempDir(), nil, cliOpts{}, "http://127.0.0.1:8080", false},
		{"config.env, unspecified host dials loopback, ./.env ignored", home, nil, cliOpts{}, "http://127.0.0.1:7000", false},
		{"shell env beats config.env", home, map[string]string{"AGYD_PORT": "9000"}, cliOpts{}, "http://127.0.0.1:9000", false},
		{"flags beat env", home, map[string]string{"AGYD_PORT": "9000"}, cliOpts{host: "localhost", port: "9100"}, "http://localhost:9100", false},
		{"IPv6 unspecified dials ::1", t.TempDir(), map[string]string{"AGYD_HOST": "::"}, cliOpts{}, "http://[::1]:8080", false},
		{"bracketed IPv6", t.TempDir(), nil, cliOpts{host: "[::1]"}, "http://[::1]:8080", false},
		{"bad host", t.TempDir(), nil, cliOpts{host: "evil.example"}, "", true},
		{"bad port", t.TempDir(), nil, cliOpts{port: "70000"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vals := map[string]string{"HOME": tt.home}
			for k, v := range tt.env {
				vals[k] = v
			}
			env := cliEnv{
				getenv:   func(k string) string { return vals[k] },
				getwd:    func() (string, error) { return project, nil },
				readFile: os.ReadFile,
			}
			got, err := clientBaseURL(&tt.opts, env)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("clientBaseURL = %q, %v; want %q, err %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// The client never follows a redirect: a 3xx from whatever answers on the
// port is shown as an error, and the redirect target is never contacted.
func TestCLIDoesNotFollowRedirects(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	env, _, stderr := testCLIEnv(t, strings.TrimPrefix(redirector.URL, "http://"), "", nil)
	env.httpClient = osCLIEnv().httpClient
	if code := runCLI(t, t.Context(), env, "current"); code == 0 {
		t.Errorf("exit code 0 on a redirect; stderr %q", stderr.String())
	}
	if hit {
		t.Error("the client followed the redirect")
	}
}
