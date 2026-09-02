//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// fadvDontNeed asks the kernel to drop a file's clean page cache.
const fadvDontNeed = 4

// evict drops a file's page cache and reports how many of its pages were
// resident before and after.
//
// Targeted rather than dropping the whole machine's cache, which on a shared
// node would charge every other workload for this measurement. The counts are
// returned because a measurement that assumes the eviction worked cannot tell
// a cold read from a fast disk.
func evict(path string) (before, after int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	size := info.Size()
	if size == 0 {
		return 0, 0, fmt.Errorf("%s holds nothing", path)
	}

	if before, err = resident(f, size); err != nil {
		return 0, 0, err
	}
	// Dirty pages are not droppable, so they are written back first.
	if err := f.Sync(); err != nil {
		return 0, 0, err
	}
	if _, _, e := syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, uintptr(size), fadvDontNeed, 0, 0); e != 0 {
		return 0, 0, fmt.Errorf("fadvise on %s: %w", path, e)
	}
	if after, err = resident(f, size); err != nil {
		return 0, 0, err
	}
	return before, after, nil
}

// resident counts how many of a file's pages the page cache holds.
func resident(f *os.File, size int64) (int, error) {
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return 0, fmt.Errorf("mapping %s: %w", f.Name(), err)
	}
	defer syscall.Munmap(data)

	page := int64(os.Getpagesize())
	pages := (size + page - 1) / page
	vec := make([]byte, pages)
	_, _, e := syscall.Syscall(syscall.SYS_MINCORE,
		uintptr(unsafe.Pointer(&data[0])), uintptr(size), uintptr(unsafe.Pointer(&vec[0])))
	if e != 0 {
		return 0, fmt.Errorf("mincore on %s: %w", f.Name(), e)
	}
	var n int
	for _, b := range vec {
		if b&1 == 1 {
			n++
		}
	}
	return n, nil
}
