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
	"strconv"
	"strings"
	"syscall"
	"time"

	"agyd/internal/acp"
)

type appConfig struct {
	Host             string
	Port             int
	ACPBin           string
	ACPArgs          []string
	Workspace        string
	PermissionPolicy acp.PermissionPolicy
}

// Addr is the host:port the server listens on.
func (c appConfig) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// usageError carries the flag help text; main prints it and exits 0.
type usageError struct{ text string }

func (e *usageError) Error() string { return "help requested" }

func loadConfig(args []string, getenv func(string) string, getwd func() (string, error), lookPath func(string) (string, error)) (appConfig, error) {
	defaultHost := getenv("AGYD_HOST")
	if defaultHost == "" {
		defaultHost = "127.0.0.1"
	}
	defaultPort := getenv("AGYD_PORT")
	if defaultPort == "" {
		defaultPort = "8080"
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
	hostStr := fs.String("host", defaultHost, "listen address: an IP or localhost; 0.0.0.0 listens on all interfaces (env AGYD_HOST)")
	portStr := fs.String("port", defaultPort, "listen port, 1-65535 (env AGYD_PORT)")
	acpBin := fs.String("agy", defaultBin, "agy executable (env AGYD_AGY)")
	acpArgs := fs.String("acp-args", defaultArgs, "space-separated arguments for agy (env AGYD_ACP_ARGS)")
	workspace := fs.String("workspace", defaultWS, "default session working directory (env AGYD_WORKSPACE)")
	policyStr := fs.String("permission-policy", defaultPolicy, "allow_all approves every agent tool call; deny_all refuses them (env AGYD_PERMISSION_POLICY)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			var b strings.Builder
			b.WriteString("agyd: HTTP + SSE API and Swagger docs in front of agy.\n\nUsage: agyd [flags]\n\nFlags:\n")
			fs.SetOutput(&b)
			fs.PrintDefaults()
			return appConfig{}, &usageError{text: b.String()}
		}
		return appConfig{}, fmt.Errorf("parsing flags: %w", err)
	}
	if fs.NArg() > 0 {
		return appConfig{}, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}

	host := strings.ToLower(strings.TrimSpace(*hostStr))
	if host != "localhost" && net.ParseIP(host) == nil {
		return appConfig{}, fmt.Errorf("invalid host %q: want an IP address or localhost, e.g. 127.0.0.1 or 0.0.0.0", *hostStr)
	}
	port, err := strconv.Atoi(strings.TrimSpace(*portStr))
	if err != nil || port < 1 || port > 65535 {
		return appConfig{}, fmt.Errorf("invalid port %q: want 1-65535", *portStr)
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
		Host:             host,
		Port:             port,
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
	cfg, err := loadConfig(os.Args[1:], os.Getenv, os.Getwd, exec.LookPath)
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
