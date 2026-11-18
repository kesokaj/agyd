package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteConfigEnv(t *testing.T) {
	t.Run("creates dir 0700 and file 0600", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "agyd")
		path := filepath.Join(dir, "config.env")
		if err := WriteConfigEnv(path, map[string]string{"AGYD_MODEL": "gemini-4-carbon", "AGYD_THINKING": "high"}); err != nil {
			t.Fatalf("WriteConfigEnv: %v", err)
		}
		if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, err = %v; want 0700", st.Mode().Perm(), err)
		}
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("file mode = %v, err = %v; want 0600", st.Mode().Perm(), err)
		}
		got := readEnvFile(t, path)
		if got["AGYD_MODEL"] != "gemini-4-carbon" || got["AGYD_THINKING"] != "high" {
			t.Errorf("round trip = %v", got)
		}
	})

	t.Run("merges and keeps other lines verbatim", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.env")
		orig := "# my agyd settings\nAGYD_PORT=9000\nAGYD_MODEL=old\n\nAGYD_MODE=plan # keep\n"
		if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := WriteConfigEnv(path, map[string]string{"AGYD_MODEL": "claude-sonnet-5-5", "AGYD_THINKING": "low"}); err != nil {
			t.Fatalf("WriteConfigEnv: %v", err)
		}
		b, _ := os.ReadFile(path)
		text := string(b)
		for _, keep := range []string{"# my agyd settings\n", "AGYD_PORT=9000\n", "AGYD_MODE=plan # keep\n"} {
			if !strings.Contains(text, keep) {
				t.Errorf("lost line %q:\n%s", keep, text)
			}
		}
		if strings.Contains(text, "AGYD_MODEL=old") {
			t.Errorf("old value kept:\n%s", text)
		}
		got := readEnvFile(t, path)
		if got["AGYD_MODEL"] != "claude-sonnet-5-5" || got["AGYD_THINKING"] != "low" || got["AGYD_PORT"] != "9000" {
			t.Errorf("merged = %v", got)
		}
	})

	t.Run("refuses keys outside the settings and invalid values", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.env")
		for name, upd := range map[string]map[string]string{
			"foreign key":  {"AGYD_AGY": "/bin/sh"},
			"newline":      {"AGYD_MODEL": "a\nAGYD_AGY=/bin/sh"},
			"equals":       {"AGYD_MODEL": "a=b"},
			"hash":         {"AGYD_MODEL": "#a"},
			"quote":        {"AGYD_MODEL": `"a"`},
			"space":        {"AGYD_MODEL": " a"},
			"empty update": {},
		} {
			if err := WriteConfigEnv(path, upd); err == nil {
				t.Errorf("%s: WriteConfigEnv(%q) err = nil, want an error", name, upd)
			}
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("a refused write created the file: %v", err)
		}
	})

	t.Run("refuses to merge into a malformed file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.env")
		_ = os.WriteFile(path, []byte("AGYD_MODEL\n"), 0o600)
		if err := WriteConfigEnv(path, map[string]string{"AGYD_MODEL": "x"}); err == nil {
			t.Error("err = nil, want a parse error")
		}
	})
}

func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := ParseEnv(f, path)
	if err != nil {
		t.Fatalf("ParseEnv(%s): %v", path, err)
	}
	return m
}
