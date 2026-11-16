package main

import (
	"reflect"
	"strings"
	"testing"

	"agy-api/internal/acp"
)

func TestConfigFromEnvAndFlags(t *testing.T) {
	t.Run("missing token fails validation", func(t *testing.T) {
		_, err := loadConfig(nil, func(string) string { return "" })
		if err == nil || !strings.Contains(err.Error(), "AGY_API_TOKEN") {
			t.Fatalf("loadConfig err = %v, want missing AGY_API_TOKEN error", err)
		}
	})

	t.Run("env defaults and PORT fallback", func(t *testing.T) {
		env := map[string]string{
			"AGY_API_TOKEN":         "secret-123",
			"PORT":                  "9090",
			"AGY_PERMISSION_POLICY": "deny_all",
		}
		cfg, err := loadConfig(nil, func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.Addr != ":9090" {
			t.Errorf("Addr = %q, want :9090", cfg.Addr)
		}
		if cfg.ACPBin != "agy" || !reflect.DeepEqual(cfg.ACPArgs, []string{"acp"}) {
			t.Errorf("ACP = %q %v, want agy [acp]", cfg.ACPBin, cfg.ACPArgs)
		}
		if cfg.Workspace != "/workspace" {
			t.Errorf("Workspace = %q, want /workspace", cfg.Workspace)
		}
		if cfg.PermissionPolicy != acp.PermissionDenyAll {
			t.Errorf("PermissionPolicy = %q, want DENY_ALL", cfg.PermissionPolicy)
		}
	})

	t.Run("flags override env vars", func(t *testing.T) {
		env := map[string]string{
			"AGY_API_TOKEN": "secret-123",
			"AGY_API_ADDR":  "127.0.0.1:8080",
		}
		args := []string{
			"-addr=127.0.0.1:7070",
			"-acp-bin=claude-agent-acp",
			"-acp-args=--stdio --verbose",
			"-workspace=/tmp/work",
			"-permission-policy=allow_all",
		}
		cfg, err := loadConfig(args, func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.Addr != "127.0.0.1:7070" {
			t.Errorf("Addr = %q, want 127.0.0.1:7070", cfg.Addr)
		}
		if cfg.ACPBin != "claude-agent-acp" {
			t.Errorf("ACPBin = %q, want claude-agent-acp", cfg.ACPBin)
		}
		if !reflect.DeepEqual(cfg.ACPArgs, []string{"--stdio", "--verbose"}) {
			t.Errorf("ACPArgs = %v, want [--stdio --verbose]", cfg.ACPArgs)
		}
		if cfg.Workspace != "/tmp/work" {
			t.Errorf("Workspace = %q, want /tmp/work", cfg.Workspace)
		}
		if cfg.PermissionPolicy != acp.PermissionAllowAll {
			t.Errorf("PermissionPolicy = %q, want ALLOW_ALL", cfg.PermissionPolicy)
		}
	})

	t.Run("invalid permission policy fails", func(t *testing.T) {
		env := map[string]string{
			"AGY_API_TOKEN":         "secret-123",
			"AGY_PERMISSION_POLICY": "invalid_policy",
		}
		if _, err := loadConfig(nil, func(k string) string { return env[k] }); err == nil {
			t.Fatal("expected error for invalid permission policy, got nil")
		}
	})
}
