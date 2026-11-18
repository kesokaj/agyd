package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// returnsWithin runs f and reports whether it returned within 3 s. A reader
// stuck on the FIFO is left blocked; the test binary exits regardless.
func returnsWithin(f func() error) (bool, error) {
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return true, err
	case <-time.After(3 * time.Second):
		return false, nil
	}
}

// TestServeAndCLIReadEnvSafely pins both env-file readers to
// config.ReadEnvFile: a FIFO named .env must never block startup or a CLI
// call (os.ReadFile would hang on it).
func TestServeAndCLIReadEnvSafely(t *testing.T) {
	home := t.TempDir()
	fifo := filepath.Join(home, ".env")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Chdir(home)

	ok, err := returnsWithin(func() error {
		_, err := osCLIEnv().readFile(fifo)
		return err
	})
	if !ok {
		t.Fatal("CLI readFile blocked on a FIFO .env")
	}
	if err == nil {
		t.Error("CLI readFile(fifo) = nil error, want a refusal")
	}
	if ok, _ := returnsWithin(func() error {
		// The result depends on agy being installed; only the return matters.
		_, err := loadServeConfig(slog.New(slog.NewTextHandler(io.Discard, nil)), []string{"-no-prompt"})
		return err
	}); !ok {
		t.Fatal("loadServeConfig blocked on a FIFO .env")
	}
}
