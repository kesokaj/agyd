//go:build !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !linux

package main

import "os"

func fileIsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if nullFi, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, nullFi) {
		return false
	}
	return true
}
