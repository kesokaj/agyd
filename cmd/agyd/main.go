// Command agyd runs the HTTP + SSE gateway in front of an ACP stdio agent (agy acp) with Swagger docs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agyd/internal/acp"
)

type appConfig struct {
	Addr             string
	ACPBin           string
	ACPArgs          []string
	Workspace        string
	PermissionPolicy acp.PermissionPolicy
}

func loadConfig(args []string, getenv func(string) string, getwd func() (string, error), lookPath func(string) (string, error)) (appConfig, error) {
	defaultAddr := getenv("AGYD_ADDR")
	if defaultAddr == "" {
		defaultAddr = "127.0.0.1:8080"
	}
	defaultBin := getenv("AGYD_AGY")
	if defaultBin == "" {
		defaultBin = "agy"
	}
	defaultArgs := getenv("AGYD_ACP_ARGS")
	if defaultArgs == "" {
		defaultArgs = "acp"
	}
	defaultWS := getenv("AGYD_WORKSPACE")
	if defaultWS == "" {
		wd, err := getwd()
		if err != nil {
			return appConfig{}, fmt.Errorf("getting current directory: %w", err)
		}
		defaultWS = wd
	}
	defaultPolicy := getenv("AGYD_PERMISSION_POLICY")
	if defaultPolicy == "" {
		defaultPolicy = "allow_all"
	}

	fs := flag.NewFlagSet("agyd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", defaultAddr, "HTTP listen address")
	acpBin := fs.String("agy", defaultBin, "ACP agent executable")
	acpArgs := fs.String("acp-args", defaultArgs, "space-separated arguments for ACP agent")
	workspace := fs.String("workspace", defaultWS, "default session working directory")
	policyStr := fs.String("permission-policy", defaultPolicy, "permission policy (allow_all or deny_all)")

	if err := fs.Parse(args); err != nil {
		return appConfig{}, fmt.Errorf("parsing flags: %w", err)
	}
	if fs.NArg() > 0 {
		return appConfig{}, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}

	host, _, err := net.SplitHostPort(strings.TrimSpace(*addr))
	if err != nil {
		return appConfig{}, fmt.Errorf("invalid addr %q: want host:port, e.g. 127.0.0.1:8080", *addr)
	}
	host = strings.ToLower(host)
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return appConfig{}, fmt.Errorf("invalid addr %q: must be loopback (127.0.0.1, localhost, or ::1)", host)
	}

	if _, err := lookPath(*acpBin); err != nil {
		return appConfig{}, fmt.Errorf("agy not found on PATH; install agy-cli or pass -agy /path/to/agy")
	}

	policy, err := acp.ParsePermissionPolicy(*policyStr)
	if err != nil {
		return appConfig{}, err
	}
	cleanWS := filepath.Clean(strings.TrimSpace(*workspace))
	if !filepath.IsAbs(cleanWS) {
		return appConfig{}, fmt.Errorf("workspace %q must be an absolute path", *workspace)
	}

	return appConfig{
		Addr:             strings.TrimSpace(*addr),
		ACPBin:           strings.TrimSpace(*acpBin),
		ACPArgs:          strings.Fields(*acpArgs),
		Workspace:        cleanWS,
		PermissionPolicy: policy,
	}, nil
}

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

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           newMux(log, acpClient, cfg.Workspace),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	log.Info("listening", "addr", cfg.Addr, "url", "http://"+cfg.Addr, "acpBin", cfg.ACPBin, "workspace", cfg.Workspace)

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
	cfg, err := loadConfig(os.Args[1:], os.Getenv, os.Getwd, exec.LookPath)
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
