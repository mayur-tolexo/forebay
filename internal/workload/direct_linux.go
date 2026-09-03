//go:build linux

package workload

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// alignment is what O_DIRECT wants of a buffer and of a length. 4096 covers
// every device this runs on, and asking for more than the device needs costs
// nothing.
const alignment = 4096

// openDirect opens a file whose writes go past the page cache.
func openDirect(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC|syscall.O_DIRECT, 0o600)
	if err != nil {
		return nil, fmt.Errorf("workload: opening %s for direct IO: %w", path, err)
	}
	return f, nil
}

// alignedBuffer returns a buffer O_DIRECT will accept, since an unaligned one
// fails the write with EINVAL rather than falling back to buffered.
func alignedBuffer(size int) ([]byte, error) {
	if size%alignment != 0 {
		return nil, fmt.Errorf("workload: a block of %d is not a multiple of %d, which direct IO requires", size, alignment)
	}
	buf := make([]byte, size+alignment)
	off := int(uintptr(unsafe.Pointer(&buf[0])) % alignment)
	if off != 0 {
		off = alignment - off
	}
	return buf[off : off+size], nil
}
