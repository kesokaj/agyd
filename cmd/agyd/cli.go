package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// The agyd CLI (D8): bare `agyd` or `agyd serve` runs the server; the
// subcommands below are clients of a running agyd's HTTP API.

// cliEnv is everything a client command touches, so tests can inject it.
type cliEnv struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
	getwd          func() (string, error)
	httpClient     *http.Client
}

func osCLIEnv() cliEnv {
	return cliEnv{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		getenv: os.Getenv, getwd: os.Getwd,
		httpClient: &http.Client{ // no timeout: ask streams; Ctrl-C cancels the context
			// agyd never redirects; whatever else answers on the port must not
			// steer a prompt to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// cliOpts holds the parsed flags of one client command.
type cliOpts struct {
	json                  bool
	host, port            string
	model, thinking, mode string
	conversation          string
}

func (o *cliOpts) hasSettings() bool { return o.model != "" || o.thinking != "" || o.mode != "" }

type cliCommand struct {
	args, summary string
	minArgs       int
	maxArgs       int // -1: any
	flags         func(fs *flag.FlagSet, o *cliOpts)
	run           func(ctx context.Context, c *cli, o *cliOpts, args []string) error
}

func settingFlags(withModel bool) func(fs *flag.FlagSet, o *cliOpts) {
	return func(fs *flag.FlagSet, o *cliOpts) {
		if withModel {
			fs.StringVar(&o.model, "model", "", "model family, e.g. gemini-4-carbon (see agyd models)")
		}
		fs.StringVar(&o.thinking, "thinking", "", "thinking level, e.g. low, medium or high")
		fs.StringVar(&o.mode, "mode", "", "agent mode, e.g. default, accept-edits or plan")
	}
}

// cliCommands is the client command table; dispatch routes these names here.
var cliCommands = map[string]cliCommand{
	"ask": {
		args: `"<prompt>"`, summary: "prompt the current conversation and stream the reply (no prompt: read stdin)",
		maxArgs: -1, run: runAsk,
		flags: func(fs *flag.FlagSet, o *cliOpts) {
			fs.StringVar(&o.conversation, "conversation", "", "prompt this live conversation instead of the current one")
		},
	},
	"new": {
		summary: "create a conversation in this folder and make it current",
		flags:   settingFlags(true), run: runNew,
	},
	"list":    {summary: "list conversations (live and agy history; * marks current)", run: runList},
	"use":     {args: "<id>", summary: "make a conversation current (resumes it if needed)", minArgs: 1, maxArgs: 1, run: runUse},
	"current": {summary: "show the current conversation", run: runCurrent},
	"model": {
		args: "[<family>]", summary: "change the current conversation's model, thinking level or mode",
		maxArgs: 1, flags: settingFlags(false), run: runModel,
	},
	"models": {summary: "list the models, thinking levels and modes agy offers", run: runModels},
	"defaults": {
		summary: "show the defaults for new conversations; with flags, change and save them",
		flags:   settingFlags(true), run: runDefaults,
	},
}

var cliOrder = []string{"ask", "new", "list", "use", "current", "model", "models", "defaults"}

// dispatch picks the serve path or a client command from the arguments.
// Bare `agyd`, `agyd serve ...` and `agyd -flag ...` serve.
func dispatch(args []string) (cmd string, rest []string, err error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "serve", args, nil
	}
	switch name := args[0]; {
	case name == "serve":
		return "serve", args[1:], nil
	case name == "help":
		return "help", args[1:], nil
	default:
		if _, ok := cliCommands[name]; ok {
			return name, args[1:], nil
		}
		return "", nil, fmt.Errorf("unknown command %q; run agyd help", name)
	}
}

func clientUsage() string {
	var b strings.Builder
	b.WriteString("Usage:\n  agyd [serve] [flags]      run the server (agyd -help lists its flags)\n")
	b.WriteString("  agyd <command> [flags]    talk to a running agyd\n\nCommands:\n")
	for _, name := range cliOrder {
		c := cliCommands[name]
		fmt.Fprintf(&b, "  %-24s %s\n", strings.TrimSpace(name+" "+c.args), c.summary)
	}
	b.WriteString("\nEvery command prints the conversation, model and thinking level to stderr,\n")
	b.WriteString("and takes --json (raw API response on stdout), --host and --port\n")
	b.WriteString("(default: AGYD_HOST/AGYD_PORT from the environment, else 127.0.0.1:8080).\n")
	return b.String()
}

// runClient runs one client command and returns the process exit code:
// 0 ok, 1 failed, 2 usage error.
func runClient(ctx context.Context, name string, args []string, env cliEnv) int {
	if name == "help" {
		fmt.Fprint(env.stdout, clientUsage())
		return 0
	}
	cmd := cliCommands[name]
	o, pos, err := parseClientArgs(name, cmd, args, env)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "agyd %s: %v\n", name, err)
		return 2
	}
	base, err := clientBaseURL(o, env)
	if err != nil {
		fmt.Fprintf(env.stderr, "agyd %s: %v\n", name, err)
		return 2
	}
	c := &cli{env: env, base: base, json: o.json}
	if err := cmd.run(ctx, c, o, pos); err != nil {
		var ue usageErr
		if errors.As(err, &ue) {
			fmt.Fprintf(env.stderr, "agyd %s: %v\n", name, err)
			return 2
		}
		fmt.Fprintf(env.stderr, "agyd: %v\n", err)
		return 1
	}
	return 0
}

// usageErr is a command-line mistake found after flag parsing (exit 2).
type usageErr string

func (e usageErr) Error() string { return string(e) }

// parseClientArgs parses flags anywhere among the positional arguments, so
// `agyd ask "hi" --json` works; "--" ends flag parsing.
func parseClientArgs(name string, cmd cliCommand, args []string, env cliEnv) (*cliOpts, []string, error) {
	o := &cliOpts{}
	fs := flag.NewFlagSet("agyd "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&o.json, "json", false, "print the raw API response as JSON on stdout")
	fs.StringVar(&o.host, "host", "", "agyd address (env AGYD_HOST; 0.0.0.0 dials 127.0.0.1)")
	fs.StringVar(&o.port, "port", "", "agyd port (env AGYD_PORT)")
	if cmd.flags != nil {
		cmd.flags(fs, o)
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintf(env.stdout, "Usage: agyd %s [flags]\n\n%s.\n\nFlags:\n",
					strings.TrimSpace(name+" "+cmd.args), cmd.summary)
				fs.SetOutput(env.stdout)
				fs.PrintDefaults()
			}
			return nil, nil, err
		}
		rest := fs.Args()
		consumed := args[:len(args)-len(rest)]
		if len(consumed) > 0 && consumed[len(consumed)-1] == "--" {
			pos = append(pos, rest...)
			break
		}
		if len(rest) == 0 {
			break
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
	if len(pos) < cmd.minArgs || (cmd.maxArgs >= 0 && len(pos) > cmd.maxArgs) {
		return nil, nil, fmt.Errorf("want %s, got %d argument(s); usage: agyd %s",
			argCount(cmd), len(pos), strings.TrimSpace(name+" "+cmd.args))
	}
	return o, pos, nil
}

func argCount(cmd cliCommand) string {
	switch {
	case cmd.maxArgs < 0:
		return fmt.Sprintf("at least %d argument(s)", cmd.minArgs)
	case cmd.minArgs == cmd.maxArgs:
		return fmt.Sprintf("%d argument(s)", cmd.minArgs)
	default:
		return fmt.Sprintf("%d to %d arguments", cmd.minArgs, cmd.maxArgs)
	}
}

// clientBaseURL resolves where agyd listens (ADR-006): --host/--port, then
// AGYD_HOST/AGYD_PORT in the environment, else 127.0.0.1:8080. An unspecified
// bind address (0.0.0.0, ::) dials the loopback address of the same family.
func clientBaseURL(o *cliOpts, env cliEnv) (string, error) {
	pick := func(flagVal, key, fallback string) string {
		if flagVal != "" {
			return flagVal
		}
		if v := env.getenv(key); v != "" {
			return v
		}
		return fallback
	}
	host := strings.ToLower(strings.TrimSpace(pick(o.host, "AGYD_HOST", "127.0.0.1")))
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	port := strings.TrimSpace(pick(o.port, "AGYD_PORT", "8080"))
	ip := net.ParseIP(host)
	switch {
	case host == "localhost":
	case ip == nil:
		return "", fmt.Errorf("invalid host %q: want an IP address or localhost", host)
	case ip.IsUnspecified() && ip.To4() != nil:
		host = "127.0.0.1"
	case ip.IsUnspecified():
		host = "::1"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid port %q: want 1-65535", port)
	}
	return "http://" + net.JoinHostPort(host, port), nil
}
