//go:build darwin

package main

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

func openTestTTY(t *testing.T) *os.File {
	t.Helper()
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close() })
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCPTYGRANT, 0); errno != 0 {
		t.Fatalf("TIOCPTYGRANT: %v", errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCPTYUNLK, 0); errno != 0 {
		t.Fatalf("TIOCPTYUNLK: %v", errno)
	}
	var raw [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&raw[0]))); errno != 0 {
		t.Fatalf("TIOCPTYGNAME: %v", errno)
	}
	n := bytes.IndexByte(raw[:], 0)
	if n <= 0 {
		t.Fatal("empty slave name from TIOCPTYGNAME")
	}
	slave, err := os.OpenFile(string(raw[:n]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open slave %q: %v", string(raw[:n]), err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return slave
}
