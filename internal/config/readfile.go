package config

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// MaxEnvFileSize caps how much of an env file agyd reads, and how long one
// line may be.
const MaxEnvFileSize = 1 << 20

// ReadEnvFile reads an env file for Load: it follows symlinks but refuses
// anything that is not a regular file (a FIFO would block startup, a device
// could stream forever) and anything larger than MaxEnvFileSize. A missing
// file is fs.ErrNotExist, which Load skips.
func ReadEnvFile(path string) ([]byte, error) {
	f, err := openEnvFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxEnvFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxEnvFileSize {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, MaxEnvFileSize)
	}
	return b, nil
}

// openEnvFile opens path without blocking (O_NONBLOCK, so a FIFO swapped in
// cannot hang startup) and checks the type on the open descriptor, which
// leaves no window between the check and the read.
func openEnvFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	return f, nil
}
