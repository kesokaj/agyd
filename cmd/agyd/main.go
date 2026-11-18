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
	"syscall"
	"time"

	"agyd/internal/acp"
	"agyd/internal/config"
)

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
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

	initCtx, cancelInit := context.WithTimeout(ctx, 15*time.Second)
	err = acpClient.Start(initCtx)
	cancelInit()
	if err != nil {
		log.Warn("initial acp startup failed; will retry lazily on next request", "err", err)
	}

	extraHosts := exposedHosts(cfg.Host, net.InterfaceAddrs, os.Hostname)
	if !isLoopbackHost(cfg.Host) {
		log.Warn("listening beyond loopback: anyone who can reach this address can drive agy as you; there is no API token",
			"host", cfg.Host, "permissionPolicy", cfg.PermissionPolicy, "allowedHosts", extraHosts)
	}

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           newMux(log, acpClient, cfg.Workspace, extraHosts...),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen(listenNetwork(cfg.Host), cfg.Addr())
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Addr(), err)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	log.Info("listening", "addr", cfg.Addr(), "url", localURL(cfg), "acpBin", cfg.ACPBin, "workspace", cfg.Workspace)

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
	log := newLogger(os.Stdout)
	cfg, err := loadServeConfig(log, os.Args[1:])
	if ue, ok := errors.AsType[*usageError](err); ok {
		fmt.Print(ue.text)
		return
	}
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, cfg); err != nil {
		log.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// loadServeConfig layers ./.env, the shell env and config.env (never touching
// the process environment), then applies the flags.
func loadServeConfig(log *slog.Logger, args []string) (appConfig, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return appConfig{}, fmt.Errorf("getting current directory: %w", err)
	}
	layers, err := config.Load(cwd, os.Getenv, os.ReadFile)
	if err != nil {
		return appConfig{}, err
	}
	if len(layers.Ignored) > 0 {
		log.Warn("ignoring keys a project .env may not set; put them in config.env or the shell env",
			"file", layers.DotEnvFile, "keys", layers.Ignored)
	}
	cfg, err := loadConfig(args, layers.Getenv, func() (string, error) { return cwd, nil }, exec.LookPath)
	if err == nil {
		log.Info("settings", "model", cfg.Model, "thinking", cfg.Thinking, "mode", cfg.Mode,
			"configFile", layers.ConfigFile, "dotEnvFile", layers.DotEnvFile)
	}
	return cfg, err
}
