package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
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

type sessionEntry struct {
	info SessionInfo
	proc *folderProc
}

// Client supervises one ACP v1 child process per workspace folder over stdio.
// Each folder's process starts lazily on its first conversation and stops when
// its last live conversation closes.
type Client struct {
	cfg     Config
	binPath string
	backend string
	log     *slog.Logger

	procWG sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	procs    map[string]*folderProc
	sessions map[string]sessionEntry
	inFlight map[string]*promptSubscriber
}

// NewClient validates configuration and returns a new per-folder ACP stdio Client.
func NewClient(cfg Config, log *slog.Logger) (*Client, error) {
	if strings.TrimSpace(cfg.Binary) == "" {
		return nil, fmt.Errorf("%w: binary is required", ErrInvalidConfig)
	}
	binPath, err := resolveBinary(cfg.Binary)
	if err != nil {
		return nil, err
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
		binPath:  binPath,
		backend:  filepath.Base(binPath),
		log:      log,
		procs:    make(map[string]*folderProc),
		sessions: make(map[string]sessionEntry),
		inFlight: make(map[string]*promptSubscriber),
	}, nil
}

// Start verifies the ACP binary can start and complete the initialize handshake
// without leaving an idle process running when no sessions are open.
func (c *Client) Start(ctx context.Context) error {
	return c.withAnyProc(ctx, func(*folderProc) error { return nil })
}

// Status returns ACP agent readiness and active session count, reusing any live
// folder process without spawning an extra child.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var protoVer int
	var agentInfo AgentInfo
	err := c.withAnyProc(ctx, func(p *folderProc) error {
		protoVer = p.protocolVer
		agentInfo = p.agentInfo
		return nil
	})
	if err != nil {
		return Status{}, err
	}
	c.mu.Lock()
	closed := c.closed
	active := len(c.sessions)
	c.mu.Unlock()
	if closed {
		return Status{}, ErrProcessClosed
	}
	return Status{
		Ready:              true,
		ProtocolVersion:    protoVer,
		Backend:            c.backend,
		AgentInfo:          agentInfo,
		PermissionPolicy:   c.cfg.PermissionPolicy,
		ActiveSessionCount: active,
	}, nil
}

// Close terminates every active ACP child process and waits for all children to exit.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	procs := make([]*folderProc, 0, len(c.procs))
	for _, p := range c.procs {
		procs = append(procs, p)
	}
	clear(c.procs)
	clear(c.sessions)
	clear(c.inFlight)
	c.mu.Unlock()

	for _, p := range procs {
		p.stop()
	}
	c.procWG.Wait()
	return nil
}

// acquireProc returns the live folderProc for dir (starting one lazily if needed)
// with a borrowed reference that the caller must release.
func (c *Client) acquireProc(ctx context.Context, dir string) (*folderProc, func(), error) {
	dirInfo, _ := os.Stat(dir)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, ErrProcessClosed
	}
	p := c.findProcLocked(dir, dirInfo)
	startNew := p == nil
	if startNew {
		p = newFolderProc(c, dir, dirInfo)
		p.refs = 1
		c.procs[dir] = p
		c.procWG.Add(1)
	} else {
		p.refs++
	}
	c.mu.Unlock()

	if startNew {
		go p.start()
	}
	if err := p.waitReady(ctx); err != nil {
		c.releaseProc(p)
		return nil, nil, err
	}
	return p, func() { c.releaseProc(p) }, nil
}

// findProcLocked returns the existing process for dir, matching case-variant
// spellings of the same directory via os.SameFile on case-insensitive filesystems.
func (c *Client) findProcLocked(dir string, dirInfo os.FileInfo) *folderProc {
	if p, ok := c.procs[dir]; ok {
		return p
	}
	if dirInfo == nil {
		return nil
	}
	for _, p := range c.procs {
		if p.dirInfo != nil && os.SameFile(p.dirInfo, dirInfo) {
			return p
		}
	}
	return nil
}

func (c *Client) pickExistingProcLocked() *folderProc {
	var starting *folderProc
	for _, p := range c.procs {
		select {
		case <-p.readyCh:
			if p.readyErr == nil {
				return p
			}
		default:
			if starting == nil {
				starting = p
			}
		}
	}
	return starting
}

// withAnyProc runs fn against an existing live folder process when available,
// falling back to a short-lived process in DefaultWorkspace that stops as soon
// as fn returns if no sessions hold it.
func (c *Client) withAnyProc(ctx context.Context, fn func(*folderProc) error) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrProcessClosed
	}
	p := c.pickExistingProcLocked()
	if p != nil {
		p.refs++
	}
	c.mu.Unlock()

	if p != nil {
		defer c.releaseProc(p)
		if err := p.waitReady(ctx); err != nil {
			return err
		}
		return fn(p)
	}

	p, release, err := c.acquireProc(ctx, c.cfg.DefaultWorkspace)
	if err != nil {
		return err
	}
	defer release()
	return fn(p)
}

// releaseRefLocked drops one reference on p and detaches it from c.procs when
// no sessions or callers remain. Caller holds c.mu.
func (c *Client) releaseRefLocked(p *folderProc) bool {
	p.refs--
	if p.refs > 0 {
		return false
	}
	if c.procs[p.dir] == p {
		delete(c.procs, p.dir)
	}
	return true
}

func (c *Client) releaseProc(p *folderProc) {
	c.mu.Lock()
	stop := c.releaseRefLocked(p)
	c.mu.Unlock()
	if stop {
		p.stop()
	}
}

func (c *Client) removeProc(p *folderProc) {
	c.mu.Lock()
	if c.procs[p.dir] == p {
		delete(c.procs, p.dir)
	}
	c.mu.Unlock()
}

func (c *Client) dropProcSessions(p *folderProc) {
	c.mu.Lock()
	if c.procs[p.dir] == p {
		delete(c.procs, p.dir)
	}
	for id, entry := range c.sessions {
		if entry.proc == p {
			delete(c.sessions, id)
			delete(c.inFlight, id)
			p.refs--
		}
	}
	c.mu.Unlock()
}
