package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// folderProc supervises one ACP child process bound to a single workspace
// directory via exec.Cmd.Dir. Client owns folderProc membership and refs under
// Client.mu; folderProc.mu is a leaf lock (never acquire Client.mu while
// holding folderProc.mu).
type folderProc struct {
	client  *Client
	dir     string
	dirInfo os.FileInfo

	initCtx    context.Context
	initCancel context.CancelFunc

	readyCh     chan struct{}
	readyErr    error
	protocolVer int
	agentInfo   AgentInfo

	exited chan struct{}

	// refs counts live sessions plus active callers borrowing this process.
	// Guarded exclusively by client.mu.
	refs int

	writeMu sync.Mutex

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stopped bool
	nextID  int64
	pending map[int64]*pendingCall
}

func newFolderProc(c *Client, dir string, dirInfo os.FileInfo) *folderProc {
	initCtx, initCancel := context.WithCancel(context.Background())
	return &folderProc{
		client:     c,
		dir:        dir,
		dirInfo:    dirInfo,
		initCtx:    initCtx,
		initCancel: initCancel,
		readyCh:    make(chan struct{}),
		exited:     make(chan struct{}),
		pending:    make(map[int64]*pendingCall),
	}
}

// resolveBinary pins bin to an absolute path at client construction time so
// setting exec.Cmd.Dir to a conversation folder can never resolve a relative
// binary path or relative PATH entry inside an untrusted workspace directory.
func resolveBinary(bin string) (string, error) {
	bin = strings.TrimSpace(bin)
	if filepath.IsAbs(bin) {
		return filepath.Clean(bin), nil
	}
	looked, err := exec.LookPath(bin)
	if err != nil && !errors.Is(err, exec.ErrDot) {
		if strings.ContainsRune(bin, filepath.Separator) {
			abs, absErr := filepath.Abs(bin)
			if absErr != nil {
				return "", fmt.Errorf("%w: resolving binary %q: %w", ErrInvalidConfig, bin, absErr)
			}
			return abs, nil
		}
		return "", fmt.Errorf("%w: resolving binary %q: %w", ErrInvalidConfig, bin, err)
	}
	abs, err := filepath.Abs(looked)
	if err != nil {
		return "", fmt.Errorf("%w: resolving binary %q: %w", ErrInvalidConfig, bin, err)
	}
	return abs, nil
}

func buildProcEnv(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "AGY_API_TOKEN=") || strings.HasPrefix(kv, "PWD=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PWD="+dir)
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
		_ = stdout.Close()
		return nil, nil, nil, fmt.Errorf("creating acp stderr pipe: %w", err)
	}
	return stdin, stdout, stderr, nil
}

func (p *folderProc) start() {
	defer p.client.procWG.Done()
	if err := p.spawnAndInit(); err != nil {
		p.client.removeProc(p)
		p.readyErr = err
		close(p.readyCh)
		return
	}
	close(p.readyCh)
}

func (p *folderProc) startCommand() (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	cmd := exec.Command(p.client.binPath, p.client.cfg.Args...)
	cmd.Dir = p.dir
	baseEnv := p.client.cfg.Env
	if len(baseEnv) == 0 {
		baseEnv = os.Environ()
	}
	cmd.Env = buildProcEnv(baseEnv, p.dir)

	stdin, stdout, stderr, err := openStdioPipes(cmd)
	if err != nil {
		return nil, nil, nil, err
	}

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, nil, nil, ErrProcessClosed
	}
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, nil, nil, fmt.Errorf("%w: starting %s: %v", ErrProcessClosed, p.client.cfg.Binary, err)
	}
	p.cmd = cmd
	p.stdin = stdin
	p.mu.Unlock()
	return cmd, stdout, stderr, nil
}

func (p *folderProc) spawnAndInit() error {
	cmd, stdout, stderr, err := p.startCommand()
	if err != nil {
		close(p.exited)
		return err
	}

	stderrDone := make(chan struct{})
	p.client.procWG.Add(1)
	go p.readLoop(stdout, cmd, stderrDone)
	go p.drainStderr(stderr, stderrDone)

	var initRes initializeResult
	params := initializeParams{
		ProtocolVersion: ProtocolVersion,
		ClientCapabilities: clientCapabilities{
			FS:       fsCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
		ClientInfo: clientInfo{Name: "agyd", Version: "0.1.0"},
	}
	if err := p.call(p.initCtx, "initialize", params, &initRes); err != nil {
		p.stop()
		return fmt.Errorf("acp initialize handshake: %w", err)
	}

	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		<-p.exited
		return ErrProcessClosed
	}
	p.protocolVer = initRes.ProtocolVersion
	p.agentInfo = initRes.AgentInfo
	return nil
}

func (p *folderProc) waitReady(ctx context.Context) error {
	select {
	case <-p.readyCh:
		return p.readyErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stop terminates the child process and waits until cmd.Wait() has reaped it.
// Must not be called while holding Client.mu or folderProc.mu.
func (p *folderProc) stop() {
	p.initCancel()
	p.mu.Lock()
	p.stopped = true
	stdin := p.stdin
	p.stdin = nil
	cmd := p.cmd
	p.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	<-p.exited
}

func (p *folderProc) readLoop(r io.Reader, cmd *exec.Cmd, stderrDone <-chan struct{}) {
	defer func() {
		p.onExit(cmd, stderrDone)
		close(p.exited)
		p.client.procWG.Done()
	}()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			p.client.log.Warn("ignoring non-json stdout line from acp child", "dir", p.dir, "err", err)
			continue
		}
		p.dispatchIncoming(msg)
	}
}

func (p *folderProc) drainStderr(r io.Reader, done chan<- struct{}) {
	defer close(done)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		p.client.log.Debug("acp stderr", "dir", p.dir, "line", scanner.Text())
	}
}

func (p *folderProc) onExit(cmd *exec.Cmd, stderrDone <-chan struct{}) {
	// Drop client sessions before waking pending calls so a caller receiving
	// ErrProcessClosed immediately observes GetSession -> ErrSessionNotFound.
	p.client.dropProcSessions(p)

	p.mu.Lock()
	p.stopped = true
	if p.stdin != nil {
		_ = p.stdin.Close()
		p.stdin = nil
	}
	for id, call := range p.pending {
		call.err = ErrProcessClosed
		close(call.ch)
		delete(p.pending, id)
	}
	p.mu.Unlock()

	_ = cmd.Wait()
	<-stderrDone
}
