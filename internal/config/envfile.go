// Package config resolves agyd settings from flags, env files and the shell,
// writes the operator's config.env, and keeps the per-port state file.
//
// Env files are read with a deliberately small parser: only AGYD_* keys are
// ever parsed, and parsed values never reach the process environment (and so
// never reach the agy child).
package config

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var (
	agydKey      = regexp.MustCompile(`^AGYD_[A-Z0-9_]+$`)
	settingValue = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// ValidSettingValue reports whether v is safe as a model, thinking level or
// mode value: 1-64 characters of [A-Za-z0-9._-]. It is the gate for every
// value agyd writes to config.env or passes to agy as a setting.
func ValidSettingValue(v string) bool { return settingValue.MatchString(v) }

// ParseEnv reads KEY=VALUE lines and returns the AGYD_* entries. Comments,
// blank lines and lines that do not start with AGYD_ (after an optional
// "export ") are skipped unparsed. A malformed AGYD line is an error naming
// name:line. Later duplicates win.
func ParseEnv(r io.Reader, name string) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxEnvFileSize)
	for n := 1; sc.Scan(); n++ {
		key, val, ours, err := parseLine(sc.Text())
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", name, n, err)
		}
		if ours {
			out[key] = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// parseLine returns ours=false for any line that is not an AGYD assignment.
func parseLine(line string) (key, val string, ours bool, err error) {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "export ")
	s = strings.TrimLeft(s, " \t")
	if !strings.HasPrefix(s, "AGYD_") {
		return "", "", false, nil
	}
	if strings.ContainsRune(s, 0) {
		return "", "", false, fmt.Errorf("NUL byte in line")
	}
	k, v, found := strings.Cut(s, "=")
	if !found {
		return "", "", false, fmt.Errorf("missing '=' after %q", truncate(k))
	}
	k = strings.TrimSpace(k)
	if !agydKey.MatchString(k) {
		return "", "", false, fmt.Errorf("invalid key %q (want AGYD_[A-Z0-9_]+)", truncate(k))
	}
	v, err = parseValue(strings.TrimSpace(v))
	if err != nil {
		return "", "", false, fmt.Errorf("%s: %w", k, err)
	}
	return k, v, true, nil
}

// parseValue strips matching quotes (no escapes) or an unquoted " #" comment.
func parseValue(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if q := v[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(v[1:], q)
		if end < 0 {
			return "", fmt.Errorf("unterminated %c quote", q)
		}
		rest := strings.TrimSpace(v[end+2:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", fmt.Errorf("unexpected text after closing quote")
		}
		return v[1 : end+1], nil
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v, nil
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
