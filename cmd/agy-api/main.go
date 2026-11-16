// Command agy-api runs the HTTP + SSE gateway in front of an ACP stdio agent (agy acp).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agy-api/internal/acp"
	"agy-api/internal/api"
)

type appConfig struct {
	Addr             string
	Token            string
	ACPBin           string
	ACPArgs          []string
	Workspace        string
	PermissionPolicy acp.PermissionPolicy
}

func loadConfig(args []string, getenv func(string) string) (appConfig, error) {
	defaultAddr := getenv("AGY_API_ADDR")
	if defaultAddr == "" {
		if port := getenv("PORT"); port != "" {
			defaultAddr = ":" + port
		} else {
			defaultAddr = "127.0.0.1:8080"
		}
	}
	defaultBin := getenv("AGY_ACP_BIN")
	if defaultBin == "" {
		defaultBin = "agy"
	}
	defaultArgs := getenv("AGY_ACP_ARGS")
	if defaultArgs == "" {
		defaultArgs = "acp"
	}
	defaultWS := getenv("AGY_WORKSPACE")
	if defaultWS == "" {
		defaultWS = "/workspace"
	}
	defaultPolicy := getenv("AGY_PERMISSION_POLICY")
	if defaultPolicy == "" {
		defaultPolicy = "allow_all"
	}

	fs := flag.NewFlagSet("agy-api", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", defaultAddr, "HTTP listen address")
	acpBin := fs.String("acp-bin", defaultBin, "ACP agent executable")
	acpArgs := fs.String("acp-args", defaultArgs, "space-separated arguments for ACP agent")
	workspace := fs.String("workspace", defaultWS, "default session working directory")
	policyStr := fs.String("permission-policy", defaultPolicy, "permission policy (allow_all or deny_all)")

	if err := fs.Parse(args); err != nil {
		return appConfig{}, fmt.Errorf("parsing flags: %w", err)
	}

	token := strings.TrimSpace(getenv("AGY_API_TOKEN"))
	if token == "" {
		return appConfig{}, errors.New("AGY_API_TOKEN environment variable is required")
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
		Token:            token,
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
		Handler:           api.NewHandler(log, acpClient, api.Config{Token: cfg.Token, DefaultWorkspace: cfg.Workspace}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Addr, "acpBin", cfg.ACPBin, "workspace", cfg.Workspace)

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
	cfg, err := loadConfig(os.Args[1:], os.Getenv)
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
