package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadEnvFile(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.env")
	if err := os.WriteFile(ok, []byte("AGYD_MODEL=m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadEnvFile(ok); err != nil || string(b) != "AGYD_MODEL=m\n" {
		t.Fatalf("regular file = %q, %v", b, err)
	}
	if _, err := ReadEnvFile(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file err = %v, want ErrNotExist (Load skips it)", err)
	}
	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(ok, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEnvFile(link); err != nil {
		t.Errorf("symlink to a regular file: %v", err)
	}

	// A directory or a FIFO named .env must fail fast, never block or stream.
	fifo := filepath.Join(dir, "fifo.env")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, fifo} {
		if _, err := ReadEnvFile(p); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("ReadEnvFile(%s) err = %v, want 'not a regular file'", p, err)
		}
	}

	big := filepath.Join(dir, "big.env")
	if err := os.WriteFile(big, make([]byte, MaxEnvFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEnvFile(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized file err = %v, want 'larger than'", err)
	}
}

// TestOpenEnvFileNeverBlocks covers the Stat/Open race: a regular file
// swapped for a FIFO after any type check must not hang startup, so the
// open itself has to be non-blocking and the type check runs on the open
// descriptor.
func TestOpenEnvFileNeverBlocks(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo.env")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := openEnvFile(fifo)
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("openEnvFile(fifo) err = %v, want 'not a regular file'", err)
		}
	case <-time.After(3 * time.Second):
		// Unblock the stuck open so the test binary can exit.
		if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		t.Fatal("openEnvFile blocked on a FIFO")
	}
}
