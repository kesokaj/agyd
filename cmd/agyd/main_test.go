package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func realTempDir(t *testing.T) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return real
}

func TestConfigFromEnvAndFlags(t *testing.T) {
	realWD := realTempDir(t)
	mockGetwd := func() (string, error) { return realWD, nil }
	mockLookPath := func(s string) (string, error) { return s, nil }
	noEnv := func(k string) string {
		if k == "HOME" {
			return "/mock/home"
		}
		return ""
	}

	t.Run("defaults are loopback 8080, cwd, allow_all, home state dir", func(t *testing.T) {
		cfg, err := loadConfig(nil, noEnv, mockGetwd, mockLookPath)
		if err != nil {
			t.Fatalf("loadConfig with default env err = %v, want nil", err)
		}
		if cfg.Addr() != "127.0.0.1:8080" {
			t.Errorf("Addr() = %q, want 127.0.0.1:8080", cfg.Addr())
		}
		if cfg.Workspace != realWD {
			t.Errorf("Workspace = %q, want %q", cfg.Workspace, realWD)
		}
		if cfg.PermissionPolicy != "ALLOW_ALL" {
			t.Errorf("PermissionPolicy = %q, want ALLOW_ALL", cfg.PermissionPolicy)
		}
		if cfg.StateDir != "/mock/home/.config/agyd" {
			t.Errorf("StateDir = %q, want /mock/home/.config/agyd", cfg.StateDir)
		}
		if cfg.LogLevel != slog.LevelInfo {
			t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
		}
	})

	t.Run("state-dir flag and AGYD_STATE_DIR override HOME and reject relative paths", func(t *testing.T) {
		cfg, err := loadConfig(nil, func(k string) string { return map[string]string{"AGYD_STATE_DIR": "/var/lib/agyd"}[k] }, mockGetwd, mockLookPath)
		if err != nil || cfg.StateDir != "/var/lib/agyd" {
			t.Fatalf("AGYD_STATE_DIR without HOME: cfg=%+v, err=%v", cfg, err)
		}
		cfg, err = loadConfig([]string{"-state-dir", "/flag/state"}, func(k string) string { return map[string]string{"AGYD_STATE_DIR": "rel/bad"}[k] }, mockGetwd, mockLookPath)
		if err != nil || cfg.StateDir != "/flag/state" {
			t.Fatalf("-state-dir override: cfg=%+v, err=%v", cfg, err)
		}
		if _, err := loadConfig([]string{"-state-dir", "rel/dir"}, noEnv, mockGetwd, mockLookPath); err == nil {
			t.Error("relative -state-dir: err = nil, want an error")
		}
		if _, err := loadConfig([]string{"-state-dir", ""}, func(k string) string {
			return map[string]string{"AGYD_STATE_DIR": "/var/lib/agyd", "HOME": "/mock/home"}[k]
		}, mockGetwd, mockLookPath); err == nil {
			t.Error("empty -state-dir fell back to env/HOME; want error")
		}
		if _, err := loadConfig([]string{"-state-dir", "   "}, noEnv, mockGetwd, mockLookPath); err == nil {
			t.Error("whitespace -state-dir: err = nil, want an error")
		}
		if _, err := loadConfig([]string{"-state-dir", "/mock/home/.gemini/agyd"}, noEnv, mockGetwd, mockLookPath); err == nil {
			t.Error(".gemini -state-dir: err = nil, want an error")
		}
		if _, err := loadConfig(nil, func(string) string { return "" }, mockGetwd, mockLookPath); err == nil {
			t.Error("missing HOME and AGYD_STATE_DIR: err = nil, want an error")
		}
	})

	t.Run("unexpected positional arguments fail validation", func(t *testing.T) {
		_, err := loadConfig([]string{"agy", "acp", "--login"}, noEnv, mockGetwd, mockLookPath)
		if err == nil || !strings.Contains(err.Error(), "unexpected") {
			t.Fatalf("loadConfig positional args err = %v, want unexpected arguments error", err)
		}
	})

	t.Run("env overrides and no PORT fallback", func(t *testing.T) {
		env := map[string]string{
			"HOME":                   "/mock/home",
			"PORT":                   "9090",
			"AGYD_HOST":              "localhost",
			"AGYD_PORT":              "8081",
			"AGYD_PERMISSION_POLICY": "deny_all",
		}
		cfg, err := loadConfig(nil, func(k string) string { return env[k] }, mockGetwd, mockLookPath)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.Addr() != "localhost:8081" {
			t.Errorf("Addr() = %q, want localhost:8081", cfg.Addr())
		}
	})

	t.Run("flags override env", func(t *testing.T) {
		env := map[string]string{"HOME": "/mock/home", "AGYD_HOST": "localhost", "AGYD_PORT": "8081"}
		cfg, err := loadConfig([]string{"-host", "0.0.0.0", "-port", "9000"}, func(k string) string { return env[k] }, mockGetwd, mockLookPath)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.Addr() != "0.0.0.0:9000" {
			t.Errorf("Addr() = %q, want 0.0.0.0:9000", cfg.Addr())
		}
	})

	t.Run("lookpath fails", func(t *testing.T) {
		failLookPath := func(s string) (string, error) { return "", errors.New("not found") }
		_, err := loadConfig(nil, noEnv, mockGetwd, failLookPath)
		if err == nil || !strings.Contains(err.Error(), "agy not found on PATH") {
			t.Fatalf("expected agy not found on PATH, got %v", err)
		}
	})

	t.Run("host must be an IP or localhost", func(t *testing.T) {
		for host, want := range map[string]string{
			"127.0.0.1":    "127.0.0.1:8080",
			"LOCALHOST":    "localhost:8080",
			"::1":          "[::1]:8080",
			"0.0.0.0":      "0.0.0.0:8080",
			"::":           "[::]:8080",
			"192.168.1.10": "192.168.1.10:8080",
			"":             "",
			"evil.example": "",
			"localhost.":   "",
			"127.0.0.1:80": "",
		} {
			cfg, err := loadConfig([]string{"-host=" + host}, noEnv, mockGetwd, mockLookPath)
			if want == "" {
				if err == nil {
					t.Errorf("host %q: err = nil, want an error", host)
				}
				continue
			}
			if err != nil || cfg.Addr() != want {
				t.Errorf("host %q: Addr() = %q, err = %v, want %q", host, cfg.Addr(), err, want)
			}
		}
	})

	t.Run("port must be 1-65535", func(t *testing.T) {
		for port, ok := range map[string]bool{
			"1": true, "8080": true, "65535": true,
			"0": false, "65536": false, "-1": false, "+8080": false, "http": false, "": false,
		} {
			_, err := loadConfig([]string{"-port=" + port}, noEnv, mockGetwd, mockLookPath)
			if (err == nil) != ok {
				t.Errorf("port %q: err = %v, want ok=%v", port, err, ok)
			}
		}
	})

	t.Run("removed -addr flag is rejected", func(t *testing.T) {
		if _, err := loadConfig([]string{"-addr=127.0.0.1:8080"}, noEnv, mockGetwd, mockLookPath); err == nil {
			t.Fatal("loadConfig -addr err = nil, want an unknown-flag error")
		}
	})

	t.Run("help returns usage listing every flag", func(t *testing.T) {
		for _, arg := range []string{"-h", "--help"} {
			_, err := loadConfig([]string{arg}, noEnv, mockGetwd, mockLookPath)
			ue, ok := errors.AsType[*usageError](err)
			if !ok {
				t.Fatalf("%s: err = %v, want *usageError", arg, err)
			}
			for _, f := range []string{"-host", "-port", "-workspace", "-state-dir", "-permission-policy", "-agy", "-acp-args", "-log-level", "AGYD_HOST",
				"AGYD_STATE_DIR", "AGYD_LOG_LEVEL", "-model", "-thinking", "-mode", "-no-prompt", "AGYD_MODEL", "AGYD_THINKING", "AGYD_MODE", "AGYD_NO_PROMPT", "state.json"} {
				if !strings.Contains(ue.text, f) {
					t.Errorf("%s: usage lacks %q:\n%s", arg, f, ue.text)
				}
			}
			if strings.Contains(ue.text, "config.env") || strings.Contains(ue.text, "./.env") {
				t.Errorf("%s: usage still mentions removed env files:\n%s", arg, ue.text)
			}
		}
	})

	t.Run("settings come from env, flags win, values are validated", func(t *testing.T) {
		env := map[string]string{"HOME": "/mock/home", "AGYD_MODEL": "gemini-4-carbon", "AGYD_THINKING": "low", "AGYD_MODE": "plan", "AGYD_NO_PROMPT": "true"}
		getenv := func(k string) string { return env[k] }
		cfg, err := loadConfig(nil, getenv, mockGetwd, mockLookPath)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.Model != "gemini-4-carbon" || cfg.Thinking != "low" || cfg.Mode != "plan" || !cfg.NoPrompt {
			t.Errorf("from env: %+v", cfg)
		}
		cfg, err = loadConfig([]string{"-model", "claude-sonnet-5-5", "-thinking=high", "-no-prompt=false"}, getenv, mockGetwd, mockLookPath)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.Model != "claude-sonnet-5-5" || cfg.Thinking != "high" || cfg.Mode != "plan" || cfg.NoPrompt {
			t.Errorf("flags over env: %+v", cfg)
		}
		for _, args := range [][]string{
			{"-model", "a b"}, {"-thinking", "x=y"}, {"-mode", "#c"}, {"-model", "a\nAGYD_AGY=/bin/sh"},
		} {
			if _, err := loadConfig(args, noEnv, mockGetwd, mockLookPath); err == nil {
				t.Errorf("loadConfig(%q) err = nil, want an invalid-value error", args)
			}
		}
		if _, err := loadConfig(nil, func(k string) string { return map[string]string{"HOME": "/mock/home", "AGYD_NO_PROMPT": "maybe"}[k] }, mockGetwd, mockLookPath); err == nil {
			t.Error("AGYD_NO_PROMPT=maybe: err = nil, want an error")
		}
	})

	t.Run("unset settings stay empty", func(t *testing.T) {
		cfg, err := loadConfig(nil, noEnv, mockGetwd, mockLookPath)
		if err != nil || cfg.Model != "" || cfg.Thinking != "" || cfg.Mode != "" || cfg.NoPrompt {
			t.Fatalf("cfg = %+v, err = %v; want empty settings", cfg, err)
		}
	})

	t.Run("PortExplicit distinguishes unset port from explicit -port or AGYD_PORT", func(t *testing.T) {
		unsetCfg, err := loadConfig(nil, noEnv, mockGetwd, mockLookPath)
		if err != nil {
			t.Fatalf("unset loadConfig: %v", err)
		}
		if unsetCfg.Port != 8080 || unsetCfg.PortExplicit {
			t.Errorf("unset port: Port=%d PortExplicit=%v, want 8080 and false", unsetCfg.Port, unsetCfg.PortExplicit)
		}

		flag8080, err := loadConfig([]string{"-port", "8080"}, noEnv, mockGetwd, mockLookPath)
		if err != nil || flag8080.Port != 8080 || !flag8080.PortExplicit {
			t.Errorf("-port 8080: Port=%d PortExplicit=%v err=%v, want 8080 and true", flag8080.Port, flag8080.PortExplicit, err)
		}

		env8080, err := loadConfig(nil, func(k string) string {
			return map[string]string{"HOME": "/mock/home", "AGYD_PORT": "8080"}[k]
		}, mockGetwd, mockLookPath)
		if err != nil || env8080.Port != 8080 || !env8080.PortExplicit {
			t.Errorf("AGYD_PORT=8080: Port=%d PortExplicit=%v err=%v, want 8080 and true", env8080.Port, env8080.PortExplicit, err)
		}
	})
}

func TestWorkspaceRejectsSymlinks(t *testing.T) {
	base := realTempDir(t)
	realParent := filepath.Join(base, "real")
	realTarget := filepath.Join(realParent, "sub")
	if err := os.MkdirAll(realTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	leafLink := filepath.Join(base, "leaf-link")
	if err := os.Symlink(realTarget, leafLink); err != nil {
		t.Fatal(err)
	}
	midLink := filepath.Join(base, "mid-link")
	if err := os.Symlink(realParent, midLink); err != nil {
		t.Fatal(err)
	}
	midPath := filepath.Join(midLink, "sub")

	gemDir := filepath.Join(base, ".gemini")
	if err := os.MkdirAll(gemDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gemLink := filepath.Join(base, "gem-link")
	if err := os.Symlink(gemDir, gemLink); err != nil {
		t.Fatal(err)
	}
	plainFile := filepath.Join(base, "file.txt")
	if err := os.WriteFile(plainFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	mockLookPath := func(s string) (string, error) { return s, nil }
	baseEnv := func(k string) string {
		if k == "HOME" {
			return "/mock/home"
		}
		return ""
	}
	realGetwd := func() (string, error) { return realTarget, nil }

	t.Run("symlink at end of -workspace fails naming real path", func(t *testing.T) {
		_, err := loadConfig([]string{"-workspace", leafLink}, baseEnv, realGetwd, mockLookPath)
		if err == nil || !strings.Contains(err.Error(), realTarget) {
			t.Fatalf("-workspace leaf symlink err = %v, want error naming %q", err, realTarget)
		}
	})

	t.Run("symlink in middle of -workspace fails naming real path", func(t *testing.T) {
		_, err := loadConfig([]string{"-workspace", midPath}, baseEnv, realGetwd, mockLookPath)
		if err == nil || !strings.Contains(err.Error(), realTarget) {
			t.Fatalf("-workspace middle symlink err = %v, want error naming %q", err, realTarget)
		}
	})

	t.Run("symlink in AGYD_WORKSPACE and default getwd fail naming real path", func(t *testing.T) {
		_, err := loadConfig(nil, func(k string) string {
			return map[string]string{"HOME": "/mock/home", "AGYD_WORKSPACE": leafLink}[k]
		}, realGetwd, mockLookPath)
		if err == nil || !strings.Contains(err.Error(), realTarget) {
			t.Fatalf("AGYD_WORKSPACE symlink err = %v, want error naming %q", err, realTarget)
		}
		_, err = loadConfig(nil, baseEnv, func() (string, error) { return midPath, nil }, mockLookPath)
		if err == nil || !strings.Contains(err.Error(), realTarget) {
			t.Fatalf("getwd middle symlink err = %v, want error naming %q", err, realTarget)
		}
	})

	t.Run("real path passes including single trailing slash", func(t *testing.T) {
		for _, ws := range []string{realTarget, realTarget + string(filepath.Separator)} {
			cfg, err := loadConfig([]string{"-workspace", ws}, baseEnv, realGetwd, mockLookPath)
			if err != nil || cfg.Workspace != realTarget {
				t.Fatalf("loadConfig(-workspace %q) = %+v, err = %v; want Workspace=%q", ws, cfg, err, realTarget)
			}
		}
	})

	t.Run("invalid workspace shapes fail at startup", func(t *testing.T) {
		for _, bad := range []string{
			"rel/path",
			"",
			"   ",
			filepath.Join(base, "missing"),
			plainFile,
			gemDir,
			gemLink,
			midLink + "/../real/sub",
		} {
			_, err := loadConfig([]string{"-workspace", bad}, baseEnv, realGetwd, mockLookPath)
			if err == nil {
				t.Errorf("loadConfig(-workspace %q) err = nil, want error", bad)
			}
		}
	})
}

func TestParseLogLevel(t *testing.T) {
	valid := []struct {
		raw  string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"  DeBuG ", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{" warn ", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"Warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
	}
	for _, tc := range valid {
		got, err := parseLogLevel(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("parseLogLevel(%q) = %v, %v; want %v, nil", tc.raw, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "   ", "trace", "verbose", "fatal", "info2"} {
		if _, err := parseLogLevel(bad); err == nil || !strings.Contains(err.Error(), "must be debug, info, warn, or error") {
			t.Errorf("parseLogLevel(%q) err = %v, want invalid log level error", bad, err)
		}
	}
}

func TestLogLevelEnvAndFlagPrecedence(t *testing.T) {
	realWD := realTempDir(t)
	mockGetwd := func() (string, error) { return realWD, nil }
	mockLookPath := func(s string) (string, error) { return s, nil }
	envOf := func(m map[string]string) func(string) string {
		return func(k string) string {
			if k == "HOME" {
				return "/mock/home"
			}
			return m[k]
		}
	}

	cfg, err := loadConfig(nil, envOf(map[string]string{"AGYD_LOG_LEVEL": "debug"}), mockGetwd, mockLookPath)
	if err != nil || cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("AGYD_LOG_LEVEL=debug: cfg.LogLevel=%v err=%v", cfg.LogLevel, err)
	}
	cfg, err = loadConfig([]string{"-log-level", "warn"}, envOf(map[string]string{"AGYD_LOG_LEVEL": "debug"}), mockGetwd, mockLookPath)
	if err != nil || cfg.LogLevel != slog.LevelWarn {
		t.Fatalf("-log-level over env: cfg.LogLevel=%v err=%v", cfg.LogLevel, err)
	}
	if _, err := loadConfig(nil, envOf(map[string]string{"AGYD_LOG_LEVEL": "trace"}), mockGetwd, mockLookPath); err == nil {
		t.Error("invalid AGYD_LOG_LEVEL: err = nil, want error")
	}
	if _, err := loadConfig([]string{"-log-level=bogus"}, envOf(nil), mockGetwd, mockLookPath); err == nil {
		t.Error("invalid -log-level: err = nil, want error")
	}
}

func TestNewLoggerLevelFiltering(t *testing.T) {
	parseLines := func(t *testing.T, raw string) []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("log line is not JSON: %q: %v", line, err)
			}
			out = append(out, rec)
		}
		return out
	}

	var nilBuf bytes.Buffer
	nilLog := newLogger(&nilBuf, nil)
	nilLog.Debug("hidden")
	nilLog.Info("shown")
	nilRecs := parseLines(t, nilBuf.String())
	if len(nilRecs) != 1 || nilRecs[0]["severity"] != "INFO" || nilRecs[0]["message"] != "shown" {
		t.Fatalf("newLogger(nil) records = %v, want single INFO shown", nilRecs)
	}

	var buf bytes.Buffer
	var lv slog.LevelVar
	log := newLogger(&buf, &lv)
	log.Debug("suppressed-at-info")
	log.Info("info-visible")
	lv.Set(slog.LevelDebug)
	log.Debug("debug-visible")
	lv.Set(slog.LevelWarn)
	log.Info("suppressed-at-warn")
	log.Warn("warn-visible")
	log.Error("error-visible")

	recs := parseLines(t, buf.String())
	want := []struct{ sev, msg string }{
		{"INFO", "info-visible"},
		{"DEBUG", "debug-visible"},
		{"WARNING", "warn-visible"},
		{"ERROR", "error-visible"},
	}
	if len(recs) != len(want) {
		t.Fatalf("got %d log records (%v), want %d", len(recs), recs, len(want))
	}
	for i, w := range want {
		if recs[i]["severity"] != w.sev || recs[i]["message"] != w.msg {
			t.Errorf("record[%d] = %v, want severity=%s message=%s", i, recs[i], w.sev, w.msg)
		}
	}
}
