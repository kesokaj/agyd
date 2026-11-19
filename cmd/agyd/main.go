// Command agyd runs the HTTP + SSE gateway in front of an ACP stdio agent (agy acp) with Swagger docs.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"agyd/internal/acp"
)

func newLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	if level == nil {
		level = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.LevelKey:
				lvl := a.Value.Any().(slog.Level)
				s := lvl.String()
				if lvl == slog.LevelWarn {
					s = "WARNING"
				}
				return slog.String("severity", s)
			case slog.MessageKey:
				a.Key = "message"
			case slog.TimeKey:
				a.Key = "timestamp"
			}
			return a
		},
	}))
}

func run(ctx context.Context, log *slog.Logger, cfg appConfig) error {
	return runWithListen(ctx, log, cfg, net.Listen)
}

func runWithListen(ctx context.Context, log *slog.Logger, cfg appConfig, listen func(network, address string) (net.Listener, error)) error {
	requestedPort := cfg.Port
	ln, boundPort, err := listenServe(cfg, listen)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	if boundPort != requestedPort {
		log.Warn("default port in use, using fallback", "busyPort", requestedPort, "port", boundPort)
	}
	cfg.Port = boundPort

	extraHosts := exposedHosts(cfg.Host, net.InterfaceAddrs, os.Hostname)
	if !isLoopbackHost(cfg.Host) {
		log.Warn("listening beyond loopback: anyone who can reach this address can drive agy as you; there is no API token",
			"host", cfg.Host, "permissionPolicy", cfg.PermissionPolicy, "allowedHosts", extraHosts)
	}

	acpClient, err := acp.NewClient(acp.Config{
		Binary:           cfg.ACPBin,
		Args:             cfg.ACPArgs,
		DefaultWorkspace: cfg.Workspace,
		PermissionPolicy: cfg.PermissionPolicy,
	}, log)
	if err != nil {
		return fmt.Errorf("initializing acp client: %w", err)
	}
	defer func() { _ = acpClient.Close() }()

	mgr := newManager(log, cfg, acpClient)
	startConversation(ctx, log, cfg, mgr, os.Stdin, os.Stderr, stdinIsTerminal())
	if ctx.Err() != nil {
		return nil
	}

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           newMux(log, acpClient, mgr, cfg.Workspace, extraHosts...),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	log.Info("listening", "addr", cfg.Addr(), "url", localURL(cfg), "acpBin", cfg.ACPBin, "workspace", cfg.Workspace, "stateDir", cfg.StateDir)

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving http: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down http server: %w", err)
	}
	return nil
}

func main() {
	cmd, args, err := dispatch(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "agyd:", err)
		os.Exit(2)
	}
	if cmd != "serve" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		code := runClient(ctx, cmd, args, osCLIEnv())
		stop()
		os.Exit(code)
	}
	serve(args)
}

// serve runs the server: bare `agyd`, `agyd serve` or `agyd -flag ...`.
func serve(args []string) {
	var levelVar slog.LevelVar
	log := newLogger(os.Stdout, &levelVar)
	cfg, err := loadServeConfig(args)
	if ue, ok := errors.AsType[*usageError](err); ok {
		fmt.Print(ue.text)
		return
	}
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(1)
	}
	levelVar.Set(cfg.LogLevel)
	log.Info("settings", "model", cfg.Model, "thinking", cfg.Thinking, "mode", cfg.Mode,
		"stateFile", filepath.Join(cfg.StateDir, "state.json"), "logLevel", cfg.LogLevel.String())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, cfg); err != nil {
		log.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// loadServeConfig resolves configuration from flags and AGYD_* environment
// variables only (ADR-006).
func loadServeConfig(args []string) (appConfig, error) {
	return loadConfig(args, os.Getenv, os.Getwd, exec.LookPath)
}
