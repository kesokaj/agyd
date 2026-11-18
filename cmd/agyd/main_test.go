package main

import (
	"errors"
	"strings"
	"testing"
)

func TestConfigFromEnvAndFlags(t *testing.T) {
	mockGetwd := func() (string, error) { return "/mock/cwd", nil }
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
		if cfg.Workspace != "/mock/cwd" {
			t.Errorf("Workspace = %q, want /mock/cwd", cfg.Workspace)
		}
		if cfg.PermissionPolicy != "ALLOW_ALL" {
			t.Errorf("PermissionPolicy = %q, want ALLOW_ALL", cfg.PermissionPolicy)
		}
		if cfg.StateDir != "/mock/home/.config/agyd" {
			t.Errorf("StateDir = %q, want /mock/home/.config/agyd", cfg.StateDir)
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
			"0": false, "65536": false, "-1": false, "http": false, "": false,
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
			for _, f := range []string{"-host", "-port", "-workspace", "-state-dir", "-permission-policy", "-agy", "-acp-args", "AGYD_HOST",
				"AGYD_STATE_DIR", "-model", "-thinking", "-mode", "-no-prompt", "AGYD_MODEL", "AGYD_THINKING", "AGYD_MODE", "AGYD_NO_PROMPT", "state.json"} {
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
}
