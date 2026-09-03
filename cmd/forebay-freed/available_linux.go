//go:build linux

package main

import (
	"fmt"
	"syscall"
)

// available reports free bytes with one syscall.
//
// Not the topology package's description, which re-reads the mount table on
// every call: at a millisecond apart that is a thousand parses a second, and a
// measurement of when the filesystem reflects an unlink should not be doing
// that much of its own IO while it waits.
func available(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
