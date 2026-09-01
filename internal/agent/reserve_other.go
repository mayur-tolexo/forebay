//go:build !linux

package agent

import (
	"fmt"
	"os"
)

// reserve sets the file's length without committing blocks to it.
//
// Only Linux has fallocate, and this fallback exists so the package builds and
// its tests run on a development machine. It is not equivalent: the file reads
// as the right size while the blocks behind it are still free, so nothing stops
// compute taking capacity a lease has been promised.
//
// The agent says so rather than implying otherwise, because a node that
// believes it reserved space it did not is exactly the failure the extent
// design exists to prevent.
func reserve(f *os.File, size int64) error {
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("sizing %s to %d bytes: %w", f.Name(), size, err)
	}
	return nil
}

// ReservesBlocks reports whether this build actually commits blocks when it
// reserves an extent, so an operator is told rather than left to assume it.
const ReservesBlocks = false
