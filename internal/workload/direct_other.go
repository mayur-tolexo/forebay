//go:build !linux

package workload

import (
	"fmt"
	"os"
)

// alignment keeps the block check the same shape as the Linux one, so a
// configuration that would be refused there is refused here too.
const alignment = 4096

// openDirect has no direct IO to offer off Linux, and says so rather than
// opening a buffered file whose numbers would be memory bandwidth.
func openDirect(path string) (*os.File, error) {
	return nil, fmt.Errorf("workload: direct IO needs Linux, so %s cannot be measured against the device here", path)
}

// alignedBuffer keeps the same contract, so a test can exercise the check.
func alignedBuffer(size int) ([]byte, error) {
	if size%alignment != 0 {
		return nil, fmt.Errorf("workload: a block of %d is not a multiple of %d, which direct IO requires", size, alignment)
	}
	return make([]byte, size), nil
}
