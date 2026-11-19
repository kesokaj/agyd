package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kesokaj/agyd/internal/acp"
	"github.com/kesokaj/agyd/internal/config"
)

type appConfig struct {
	Host             string
	Port             int
	PortExplicit     bool
	ACPBin           string
	ACPArgs          []string
	Workspace        string
	PermissionPolicy acp.PermissionPolicy
	LogLevel         slog.Level

	// Model, Thinking and Mode are the requested defaults for new
	// conversations ("" means not set at this layer). NoPrompt skips the
	// startup picker.
	Model, Thinking, Mode string
	NoPrompt              bool

	// StateDir holds state.json.
	StateDir string
}

// Addr is the host:port the server listens on.
func (c appConfig) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// usageError carries the flag help text; main prints it and exits 0.
type usageError struct{ text string }

func (e *usageError) Error() string { return "help requested" }

// rawFlags holds the unvalidated flag values.
type rawFlags struct {
	host, port, bin, args, workspace, stateDir, policy, logLevel string
	model, thinking, mode                                        string
	noPrompt                                                     bool
	portSet, stateDirSet                                         bool
}

func envOr(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}

// newServeFlags binds the serve flags; defaults come from AGYD_* env vars or
// built-in defaults.
func newServeFlags(getenv func(string) string, wd string, noPrompt bool) (*flag.FlagSet, *rawFlags) {
	r := &rawFlags{}
	fs := flag.NewFlagSet("agyd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&r.host, "host", envOr(getenv, "AGYD_HOST", "127.0.0.1"), "listen address: an IP or localhost; 0.0.0.0 listens on all interfaces (env AGYD_HOST)")
	fs.StringVar(&r.port, "port", envOr(getenv, "AGYD_PORT", "8080"), "listen port, 1-65535; when unset, tries 8080 then 3000 (env AGYD_PORT)")
	fs.StringVar(&r.bin, "agy", envOr(getenv, "AGYD_AGY", "agy"), "agy executable (env AGYD_AGY)")
	fs.StringVar(&r.args, "acp-args", envOr(getenv, "AGYD_ACP_ARGS", "acp"), "space-separated arguments for agy (env AGYD_ACP_ARGS)")
	fs.StringVar(&r.workspace, "workspace", envOr(getenv, "AGYD_WORKSPACE", wd), "default conversation working directory (env AGYD_WORKSPACE)")
	fs.StringVar(&r.stateDir, "state-dir", getenv("AGYD_STATE_DIR"), "directory holding state.json (env AGYD_STATE_DIR; default ~/.config/agyd)")
	fs.StringVar(&r.policy, "permission-policy", envOr(getenv, "AGYD_PERMISSION_POLICY", "allow_all"), "allow_all approves every agent tool call; deny_all refuses them (env AGYD_PERMISSION_POLICY)")
	fs.StringVar(&r.logLevel, "log-level", envOr(getenv, "AGYD_LOG_LEVEL", "info"), "log level: debug, info, warn or error (env AGYD_LOG_LEVEL)")
	fs.StringVar(&r.model, "model", getenv("AGYD_MODEL"), "default model for new conversations, e.g. gemini-4-carbon (env AGYD_MODEL)")
	fs.StringVar(&r.thinking, "thinking", getenv("AGYD_THINKING"), "default thinking level: low, medium or high (env AGYD_THINKING)")
	fs.StringVar(&r.mode, "mode", getenv("AGYD_MODE"), "default agent mode: default, accept-edits or plan (env AGYD_MODE)")
	fs.BoolVar(&r.noPrompt, "no-prompt", noPrompt, "never ask for the default model at startup (env AGYD_NO_PROMPT)")
	return fs, r
}

const usageHeader = `agyd: HTTP + SSE API and Swagger docs in front of agy.

Usage: agyd [serve] [flags]
       agyd <command> [flags]   client of a running agyd (agyd help lists commands)

Settings, highest first: flags > AGYD_* environment variables > saved defaults
in state.json (in AGYD_STATE_DIR, default ~/.config/agyd) > built-in defaults.

Flags:
`

func loadConfig(args []string, getenv func(string) string, getwd func() (string, error), lookPath func(string) (string, error)) (appConfig, error) {
	wd := getenv("AGYD_WORKSPACE")
	if wd == "" {
		var err error
		if wd, err = getwd(); err != nil {
			return appConfig{}, fmt.Errorf("getting current directory: %w", err)
		}
	}
	noPrompt := false
	if s := getenv("AGYD_NO_PROMPT"); s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return appConfig{}, fmt.Errorf("invalid AGYD_NO_PROMPT %q: want true or false", s)
		}
		noPrompt = b
	}
	fs, r := newServeFlags(getenv, wd, noPrompt)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			var b strings.Builder
			b.WriteString(usageHeader)
			fs.SetOutput(&b)
			fs.PrintDefaults()
			return appConfig{}, &usageError{text: b.String()}
		}
		return appConfig{}, fmt.Errorf("parsing flags: %w", err)
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			r.portSet = true
		case "state-dir":
			r.stateDirSet = true
		}
	})
	if fs.NArg() > 0 {
		return appConfig{}, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	return r.validate(getenv, lookPath)
}

func resolveStateDir(explicit string, explicitSet bool, getenv func(string) string) (string, error) {
	if explicitSet {
		return config.ValidateStateDir(explicit)
	}
	return config.StateDir(getenv)
}

func parsePort(raw string) (int, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid port %q: want 1-65535", raw)
	}
	return int(n), nil
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q: must be debug, info, warn, or error", raw)
	}
}

func (r *rawFlags) validate(getenv func(string) string, lookPath func(string) (string, error)) (appConfig, error) {
	host := strings.ToLower(strings.TrimSpace(r.host))
	if host != "localhost" && net.ParseIP(host) == nil {
		return appConfig{}, fmt.Errorf("invalid host %q: want an IP address or localhost, e.g. 127.0.0.1 or 0.0.0.0", r.host)
	}
	port, err := parsePort(r.port)
	if err != nil {
		return appConfig{}, err
	}
	if _, err := lookPath(r.bin); err != nil {
		return appConfig{}, fmt.Errorf("agy not found on PATH; install agy-cli or pass -agy /path/to/agy")
	}
	policy, err := acp.ParsePermissionPolicy(r.policy)
	if err != nil {
		return appConfig{}, err
	}
	logLevel, err := parseLogLevel(r.logLevel)
	if err != nil {
		return appConfig{}, err
	}
	ws := strings.TrimSpace(r.workspace)
	if len(ws) > 1 {
		ws = strings.TrimSuffix(ws, string(filepath.Separator))
	}
	if err := acp.ValidateDir("workspace", ws); err != nil {
		return appConfig{}, err
	}
	stateDir, err := resolveStateDir(r.stateDir, r.stateDirSet, getenv)
	if err != nil {
		return appConfig{}, err
	}
	for name, v := range map[string]string{"model": r.model, "thinking": r.thinking, "mode": r.mode} {
		if v != "" && !config.ValidSettingValue(v) {
			return appConfig{}, fmt.Errorf("invalid %s %q: want 1-64 of [A-Za-z0-9._-]", name, v)
		}
	}
	return appConfig{
		Host: host, Port: port, PortExplicit: r.portSet || getenv("AGYD_PORT") != "",
		ACPBin: strings.TrimSpace(r.bin), ACPArgs: strings.Fields(r.args),
		Workspace: ws, StateDir: stateDir, PermissionPolicy: policy, LogLevel: logLevel,
		Model: r.model, Thinking: r.thinking, Mode: r.mode, NoPrompt: r.noPrompt,
	}, nil
}
