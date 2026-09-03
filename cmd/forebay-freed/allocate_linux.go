//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// allocate commits the blocks, the way the agent does when it lends capacity.
// A sparse file would be unlinked just as fast and would have taken nothing to
// give back.
func allocate(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, size); err != nil {
		os.Remove(path)
		return fmt.Errorf("allocating %s: %w", path, err)
	}
	return f.Sync()
}
