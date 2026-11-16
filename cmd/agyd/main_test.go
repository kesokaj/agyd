package main

import (
	"errors"
	"strings"
	"testing"
)

func TestConfigFromEnvAndFlags(t *testing.T) {
	mockGetwd := func() (string, error) { return "/mock/cwd", nil }
	mockLookPath := func(s string) (string, error) { return s, nil }

	t.Run("succeeds with empty env", func(t *testing.T) {
		cfg, err := loadConfig(nil, func(string) string { return "" }, mockGetwd, mockLookPath)
		if err != nil {
			t.Fatalf("loadConfig with empty env err = %v, want nil", err)
		}
		if cfg.Addr != "127.0.0.1:8080" {
			t.Errorf("Addr = %q, want 127.0.0.1:8080", cfg.Addr)
		}
		if cfg.Workspace != "/mock/cwd" {
			t.Errorf("Workspace = %q, want /mock/cwd", cfg.Workspace)
		}
	})

	t.Run("unexpected positional arguments fail validation", func(t *testing.T) {
		_, err := loadConfig([]string{"agy", "acp", "--login"}, func(string) string { return "" }, mockGetwd, mockLookPath)
		if err == nil || !strings.Contains(err.Error(), "unexpected") {
			t.Fatalf("loadConfig positional args err = %v, want unexpected arguments error", err)
		}
	})

	t.Run("env overrides and no PORT fallback", func(t *testing.T) {
		env := map[string]string{
			"PORT":                   "9090",
			"AGYD_ADDR":              "localhost:8081",
			"AGYD_PERMISSION_POLICY": "deny_all",
		}
		cfg, err := loadConfig(nil, func(k string) string { return env[k] }, mockGetwd, mockLookPath)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.Addr != "localhost:8081" {
			t.Errorf("Addr = %q, want localhost:8081", cfg.Addr)
		}
	})

	t.Run("non-loopback addr fails", func(t *testing.T) {
		args := []string{"-addr=0.0.0.0:8080"}
		_, err := loadConfig(args, func(string) string { return "" }, mockGetwd, mockLookPath)
		if err == nil || !strings.Contains(err.Error(), "must be loopback") {
			t.Fatalf("expected loopback error, got %v", err)
		}
	})

	t.Run("lookpath fails", func(t *testing.T) {
		failLookPath := func(s string) (string, error) { return "", errors.New("not found") }
		_, err := loadConfig(nil, func(string) string { return "" }, mockGetwd, failLookPath)
		if err == nil || !strings.Contains(err.Error(), "agy not found on PATH") {
			t.Fatalf("expected agy not found on PATH, got %v", err)
		}
	})

	t.Run("addr must be loopback host:port", func(t *testing.T) {
		for addr, ok := range map[string]bool{
			"127.0.0.1:8080":          true,
			"localhost:9000":          true,
			"[::1]:8080":              true,
			":8080":                   false,
			"0.0.0.0:8080":            false,
			"[::]:8080":               false,
			"127.0.0.1":               false,
			"localhost.:8080":         false,
			"[::ffff:127.0.0.1]:8080": false,
			"192.168.1.10:8080":       false,
		} {
			_, err := loadConfig([]string{"-addr=" + addr}, func(string) string { return "" }, mockGetwd, mockLookPath)
			if (err == nil) != ok {
				t.Errorf("addr %q: err = %v, want ok=%v", addr, err, ok)
			}
		}
	})
}
