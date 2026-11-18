//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func fileIsTerminal(f *os.File) bool {
	var ws [4]uint16
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	return errno == 0
}
