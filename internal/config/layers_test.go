package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeFS maps absolute paths to file contents for Load.
type fakeFS map[string]string

func (f fakeFS) read(p string) ([]byte, error) {
	if s, ok := f[p]; ok {
		return []byte(s), nil
	}
	return nil, fs.ErrNotExist
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDir(t *testing.T) {
	if got, _ := Dir(envOf(map[string]string{"XDG_CONFIG_HOME": "/x", "HOME": "/h"})); got != "/x/agyd" {
		t.Errorf("Dir with XDG_CONFIG_HOME = %q, want /x/agyd", got)
	}
	if got, _ := Dir(envOf(map[string]string{"HOME": "/h"})); got != "/h/.config/agyd" {
		t.Errorf("Dir with HOME = %q, want /h/.config/agyd", got)
	}
	if got, _ := Dir(envOf(map[string]string{"XDG_CONFIG_HOME": "rel", "HOME": "/h"})); got != "/h/.config/agyd" {
		t.Errorf("Dir ignores a relative XDG_CONFIG_HOME: got %q", got)
	}
	if _, err := Dir(envOf(nil)); err == nil {
		t.Error("Dir with no HOME: err = nil, want an error")
	}
}

func TestFindDotEnv(t *testing.T) {
	files := fakeFS{"/h/proj/.env": "", "/h/.env": "", "/.env": "", "/opt/.env": ""}
	cases := []struct{ cwd, home, want string }{
		{"/h/proj/sub/deep", "/h", "/h/proj/.env"},
		{"/h/proj", "/h", "/h/proj/.env"},
		{"/h/other", "/h", "/h/.env"},
		{"/opt/app", "/h", "/opt/.env"},
		{"/srv/app", "/h", "/.env"},
		{"/h/proj", "", "/h/proj/.env"},
	}
	for _, c := range cases {
		if got := FindDotEnv(c.cwd, c.home, files.read); got != c.want {
			t.Errorf("FindDotEnv(%q, %q) = %q, want %q", c.cwd, c.home, got, c.want)
		}
	}
	// $HOME is the upper bound inside $HOME: nothing above it is read.
	if got := FindDotEnv("/h/a/b", "/h", fakeFS{"/.env": ""}.read); got != "" {
		t.Errorf("FindDotEnv crossed $HOME: got %q, want none", got)
	}
}

func TestLoadPrecedence(t *testing.T) {
	const home = "/h"
	cfgEnv := filepath.Join(home, ".config", "agyd", "config.env")
	files := fakeFS{
		cfgEnv: "AGYD_MODEL=from-config\nAGYD_THINKING=low\nAGYD_MODE=plan\nAGYD_PORT=7000\nAGYD_AGY=/opt/agy\n",
		"/h/proj/.env": "AGYD_MODEL=from-dotenv\n" +
			"AGYD_AGY=./evil.sh\nAGYD_ACP_ARGS=x\nAGYD_HOST=0.0.0.0\nAGYD_PORT=1\n" +
			"AGYD_PERMISSION_POLICY=allow_all\nAGYD_WORKSPACE=/\nSECRET=hunter2\n",
	}
	shell := map[string]string{"HOME": home, "AGYD_MODEL": "from-shell", "AGYD_THINKING": "medium"}

	l, err := Load("/h/proj", envOf(shell), files.read)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for key, want := range map[string]string{
		"AGYD_MODEL":     "from-dotenv", // ./.env > shell > config.env
		"AGYD_THINKING":  "medium",      // shell > config.env
		"AGYD_MODE":      "plan",        // config.env only
		"AGYD_PORT":      "7000",        // ./.env may not set it (D9)
		"AGYD_AGY":       "/opt/agy",    // ./.env may not set it (D9)
		"AGYD_HOST":      "",
		"AGYD_WORKSPACE": "",
		"SECRET":         "",
		"HOME":           home, // non-AGYD keys pass through from the shell only
	} {
		if got := l.Getenv(key); got != want {
			t.Errorf("Getenv(%s) = %q, want %q", key, got, want)
		}
	}
	if got := l.Source("AGYD_MODEL"); got != "/h/proj/.env" {
		t.Errorf("Source(AGYD_MODEL) = %q, want /h/proj/.env", got)
	}
	if got := l.Source("AGYD_THINKING"); got != "env" {
		t.Errorf("Source(AGYD_THINKING) = %q, want env", got)
	}
	if got := l.Source("AGYD_MODE"); got != cfgEnv {
		t.Errorf("Source(AGYD_MODE) = %q, want %q", got, cfgEnv)
	}
	refused := []string{"AGYD_ACP_ARGS", "AGYD_AGY", "AGYD_HOST", "AGYD_PERMISSION_POLICY", "AGYD_PORT", "AGYD_WORKSPACE"}
	if !slices.Equal(l.Ignored, refused) {
		t.Errorf("Ignored = %v, want %v", l.Ignored, refused)
	}
	if l.ConfigFile != cfgEnv || l.DotEnvFile != "/h/proj/.env" {
		t.Errorf("files = %q, %q", l.ConfigFile, l.DotEnvFile)
	}
}

func TestLoadMissingFilesAndErrors(t *testing.T) {
	l, err := Load("/h/p", envOf(map[string]string{"HOME": "/h", "AGYD_MODE": "x"}), fakeFS{}.read)
	if err != nil {
		t.Fatalf("Load with no files: %v", err)
	}
	if l.Getenv("AGYD_MODE") != "x" || l.Getenv("AGYD_MODEL") != "" {
		t.Errorf("unexpected values: mode=%q model=%q", l.Getenv("AGYD_MODE"), l.Getenv("AGYD_MODEL"))
	}

	bad := fakeFS{"/h/p/.env": "AGYD_MODEL\n"}
	if _, err := Load("/h/p", envOf(map[string]string{"HOME": "/h"}), bad.read); err == nil {
		t.Error("Load with a malformed .env: err = nil, want an error")
	}

	denied := func(string) ([]byte, error) { return nil, fs.ErrPermission }
	if _, err := Load("/h/p", envOf(map[string]string{"HOME": "/h"}), denied); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("Load with an unreadable config.env: err = %v, want ErrPermission", err)
	}
}

func TestLoadNeverTouchesProcessEnv(t *testing.T) {
	before := os.Environ()
	files := fakeFS{"/h/p/.env": "AGYD_MODEL=leak-check\n", "/h/.config/agyd/config.env": "AGYD_MODE=leak-check\n"}
	if _, err := Load("/h/p", envOf(map[string]string{"HOME": "/h"}), files.read); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if after := os.Environ(); !slices.Equal(before, after) {
		t.Fatal("Load changed the process environment; .env values could reach the agy child")
	}
	if os.Getenv("AGYD_MODEL") == "leak-check" || os.Getenv("AGYD_MODE") == "leak-check" {
		t.Fatal("file values leaked into os.Getenv")
	}
}
