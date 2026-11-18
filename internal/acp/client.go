package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Service defines the ACP operations consumed by the HTTP gateway.
type Service interface {
	Status(ctx context.Context) (Status, error)
	NewSession(ctx context.Context, cwd string) (SessionInfo, error)
	GetSession(sessionID string) (SessionInfo, error)
	ListSessions() []SessionInfo
	CloseSession(ctx context.Context, sessionID string) error
	Prompt(ctx context.Context, sessionID string, blocks []ContentBlock, onUpdate func(rawUpdate []byte)) (PromptResult, error)
	Cancel(ctx context.Context, sessionID string) error
	SetConfigOption(ctx context.Context, sessionID, configID, value string) (SessionInfo, error)
	ListAgentSessions(ctx context.Context) ([]AgentSession, error)
	ResumeSession(ctx context.Context, sessionID, cwd string) (SessionInfo, error)
	Close() error
}

type pendingCall struct {
	res rpcMessage
	err error
	ch  chan struct{}
}

type promptSubscriber struct {
	onUpdate func(rawUpdate []byte)
	updates  []json.RawMessage
	text     strings.Builder
}

// Client manages an ACP v1 child process over stdio.
type Client struct {
	cfg     Config
	backend string
	log     *slog.Logger

	procMu  sync.Mutex
	writeMu sync.Mutex

	mu          sync.Mutex
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	ready       bool
	closed      bool
	protocolVer int
	agentInfo   AgentInfo
	nextID      int64
	pending     map[int64]*pendingCall
	sessions    map[string]SessionInfo
	inFlight    map[string]*promptSubscriber
}

// NewClient validates configuration and returns a new ACP stdio Client.
func NewClient(cfg Config, log *slog.Logger) (*Client, error) {
	if strings.TrimSpace(cfg.Binary) == "" {
		return nil, fmt.Errorf("%w: binary is required", ErrInvalidConfig)
	}
	policy, err := ParsePermissionPolicy(string(cfg.PermissionPolicy))
	if err != nil {
		return nil, err
	}
	cfg.PermissionPolicy = policy
	if cfg.DefaultWorkspace == "" {
		cfg.DefaultWorkspace = "/workspace"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		cfg:      cfg,
		backend:  filepath.Base(cfg.Binary),
		log:      log,
		pending:  make(map[int64]*pendingCall),
		sessions: make(map[string]SessionInfo),
		inFlight: make(map[string]*promptSubscriber),
	}, nil
}

// Start launches the ACP child process and completes the initialize handshake.
func (c *Client) Start(ctx context.Context) error {
	return c.ensureReady(ctx)
}

// Status returns the current ACP agent status, lazily restarting the child process if it exited.
func (c *Client) Status(ctx context.Context) (Status, error) {
	if err := c.ensureReady(ctx); err != nil {
		return Status{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{
		Ready:              c.ready,
		ProtocolVersion:    c.protocolVer,
		Backend:            c.backend,
		AgentInfo:          c.agentInfo,
		PermissionPolicy:   c.cfg.PermissionPolicy,
		ActiveSessionCount: len(c.sessions),
	}, nil
}

// Close terminates the ACP child process and releases resources.
func (c *Client) Close() error {
	c.procMu.Lock()
	defer c.procMu.Unlock()

	c.mu.Lock()
	c.closed = true
	cmd := c.cmd
	stdin := c.stdin
	c.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}

func (c *Client) ensureReady(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrProcessClosed
	}
	if c.ready {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	c.procMu.Lock()
	defer c.procMu.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrProcessClosed
	}
	if c.ready {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	return c.spawnAndInit(ctx)
}

func (c *Client) spawnAndInit(ctx context.Context) error {
	cmd := exec.Command(c.cfg.Binary, c.cfg.Args...)
	baseEnv := c.cfg.Env
	if len(baseEnv) == 0 {
		baseEnv = os.Environ()
	}
	cmd.Env = filterSecretEnv(baseEnv)
	stdin, stdout, stderr, err := openStdioPipes(cmd)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("%w: starting %s: %v", ErrProcessClosed, c.cfg.Binary, err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.pending = make(map[int64]*pendingCall)
	c.sessions = make(map[string]SessionInfo)
	c.inFlight = make(map[string]*promptSubscriber)
	c.mu.Unlock()

	go c.readLoop(stdout, cmd)
	go c.drainStderr(stderr)

	var initRes initializeResult
	params := initializeParams{
		ProtocolVersion: ProtocolVersion,
		ClientCapabilities: clientCapabilities{
			FS:       fsCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
		ClientInfo: clientInfo{Name: "agyd", Version: "0.1.0"},
	}
	if err := c.call(ctx, "initialize", params, &initRes); err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		return fmt.Errorf("acp initialize handshake: %w", err)
	}

	c.mu.Lock()
	c.protocolVer = initRes.ProtocolVersion
	c.agentInfo = initRes.AgentInfo
	c.ready = true
	c.mu.Unlock()
	return nil
}

func filterSecretEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "AGY_API_TOKEN=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func openStdioPipes(cmd *exec.Cmd) (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating acp stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, nil, fmt.Errorf("creating acp stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, nil, fmt.Errorf("creating acp stderr pipe: %w", err)
	}
	return stdin, stdout, stderr, nil
}

func (c *Client) onProcessExit(cmd *exec.Cmd) {
	c.mu.Lock()
	if c.cmd == cmd {
		c.ready = false
		if c.stdin != nil {
			_ = c.stdin.Close()
			c.stdin = nil
		}
		for id, call := range c.pending {
			call.err = ErrProcessClosed
			close(call.ch)
			delete(c.pending, id)
		}
		clear(c.sessions)
		clear(c.inFlight)
	}
	c.mu.Unlock()
	_ = cmd.Wait()
}
