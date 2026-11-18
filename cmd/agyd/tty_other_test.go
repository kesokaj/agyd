//go:build !darwin && !linux

package main

import (
	"os"
	"testing"
)

func openTestTTY(t *testing.T) *os.File {
	t.Helper()
	tty, err := os.Open("/dev/tty")
	if err != nil {
		t.Skipf("open /dev/tty: %v", err)
	}
	t.Cleanup(func() { _ = tty.Close() })
	return tty
}
