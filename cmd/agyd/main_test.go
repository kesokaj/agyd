package main

import (
	"errors"
	"strings"
	"testing"
)

func TestConfigFromEnvAndFlags(t *testing.T) {
	mockGetwd := func() (string, error) { return "/mock/cwd", nil }
	mockLookPath := func(s string) (string, error) { return s, nil }
	noEnv := func(string) string { return "" }

	t.Run("defaults are loopback 8080, cwd, allow_all", func(t *testing.T) {
		cfg, err := loadConfig(nil, noEnv, mockGetwd, mockLookPath)
		if err != nil {
			t.Fatalf("loadConfig with empty env err = %v, want nil", err)
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
	})

	t.Run("unexpected positional arguments fail validation", func(t *testing.T) {
		_, err := loadConfig([]string{"agy", "acp", "--login"}, noEnv, mockGetwd, mockLookPath)
		if err == nil || !strings.Contains(err.Error(), "unexpected") {
			t.Fatalf("loadConfig positional args err = %v, want unexpected arguments error", err)
		}
	})

	t.Run("env overrides and no PORT fallback", func(t *testing.T) {
		env := map[string]string{
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
		env := map[string]string{"AGYD_HOST": "localhost", "AGYD_PORT": "8081"}
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
			for _, f := range []string{"-host", "-port", "-workspace", "-permission-policy", "-agy", "-acp-args", "AGYD_HOST"} {
				if !strings.Contains(ue.text, f) {
					t.Errorf("%s: usage lacks %q:\n%s", arg, f, ue.text)
				}
			}
		}
	})
}
