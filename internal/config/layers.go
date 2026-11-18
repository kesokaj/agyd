package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
)

// DotEnvKeys are the only keys a project's ./.env may set (D9, D24). Anything
// else there (the agy binary, its args, bind address, permission policy,
// workspace, the agent mode) could redirect what agyd executes or exposes, or
// loosen its permission posture, so it is ignored.
var DotEnvKeys = []string{"AGYD_MODEL", "AGYD_THINKING"}

// SettingKeys are the conversation defaults agyd itself writes to config.env
// (the startup picker, PUT /v1/defaults), in file order.
var SettingKeys = []string{"AGYD_MODEL", "AGYD_THINKING", "AGYD_MODE"}

// SourceEnv is what Source reports for a value taken from the shell.
const SourceEnv = "env"

// Layered is the resolved view of ./.env (allowlisted) > shell env >
// config.env. Flags sit above it and are applied by the caller.
type Layered struct {
	ConfigFile string   // path of config.env, read or not
	DotEnvFile string   // path of the ./.env used, or ""
	Ignored    []string // sorted ./.env keys refused by D9

	getenv func(string) string
	dotEnv map[string]string // allowlisted ./.env values
	cfg    map[string]string // config.env values
}

// Getenv returns the layered value of key. Non-AGYD keys come from the shell
// only.
func (l *Layered) Getenv(key string) string {
	v, _ := l.lookup(key)
	return v
}

// Source names where key's value came from: a file path, "env", or "" when
// unset.
func (l *Layered) Source(key string) string {
	_, src := l.lookup(key)
	return src
}

func (l *Layered) lookup(key string) (value, source string) {
	if !strings.HasPrefix(key, "AGYD_") {
		if v := l.getenv(key); v != "" {
			return v, SourceEnv
		}
		return "", ""
	}
	if v, ok := l.dotEnv[key]; ok {
		return v, l.DotEnvFile
	}
	if v := l.getenv(key); v != "" {
		return v, SourceEnv
	}
	if v, ok := l.cfg[key]; ok {
		return v, l.ConfigFile
	}
	return "", ""
}

// Dir is agyd's config directory: $XDG_CONFIG_HOME/agyd when that is an
// absolute path, else $HOME/.config/agyd.
func Dir(getenv func(string) string) (string, error) {
	if x := getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "agyd"), nil
	}
	home := getenv("HOME")
	if !filepath.IsAbs(home) {
		return "", errors.New("config dir: HOME is not set to an absolute path")
	}
	return filepath.Join(home, ".config", "agyd"), nil
}

// FindDotEnv returns the nearest .env walking up from cwd. Inside home the
// walk stops at home; outside it, at the filesystem root. "" when none.
func FindDotEnv(cwd, home string, read func(string) ([]byte, error)) string {
	dir := filepath.Clean(cwd)
	home = filepath.Clean(home)
	inHome := home != "." && home != "/" && (dir == home || strings.HasPrefix(dir, home+string(filepath.Separator)))
	for {
		p := filepath.Join(dir, ".env")
		if _, err := read(p); err == nil {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir || (inHome && dir == home) {
			return ""
		}
		dir = parent
	}
}

// Load resolves the layers for a process started in cwd. A missing file is
// skipped; any other read error or a malformed AGYD line is returned. Load
// never modifies the process environment.
func Load(cwd string, getenv func(string) string, read func(string) ([]byte, error)) (*Layered, error) {
	dir, err := Dir(getenv)
	if err != nil {
		return nil, err
	}
	l := &Layered{ConfigFile: filepath.Join(dir, "config.env"), getenv: getenv, dotEnv: map[string]string{}}
	if l.cfg, err = loadEnvFile(l.ConfigFile, read); err != nil {
		return nil, err
	}
	l.DotEnvFile = FindDotEnv(cwd, getenv("HOME"), read)
	if l.DotEnvFile == "" {
		return l, nil
	}
	vals, err := loadEnvFile(l.DotEnvFile, read)
	if err != nil {
		return nil, err
	}
	for k, v := range vals {
		if slices.Contains(DotEnvKeys, k) {
			l.dotEnv[k] = v
		} else {
			l.Ignored = append(l.Ignored, k)
		}
	}
	slices.Sort(l.Ignored)
	return l, nil
}

// loadEnvFile parses path's AGYD_* lines; a missing file yields nil, nil.
func loadEnvFile(path string, read func(string) ([]byte, error)) (map[string]string, error) {
	b, err := read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseEnv(bytes.NewReader(b), path)
}
