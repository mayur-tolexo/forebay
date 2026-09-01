//go:build linux

package agent

import (
	"fmt"
	"os"
	"syscall"
)

// reserve makes size bytes genuinely belong to the file.
//
// fallocate allocates blocks rather than recording a length, which is the
// whole point: a sparse file of the right size reserves nothing, so compute
// could take the space Forebay has already promised to a lease and the node
// would discover it at write time, under pressure, which is the one moment the
// design cannot afford a surprise.
func reserve(f *os.File, size int64) error {
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, size); err != nil {
		// ENOSPC arrives here like any other failure and needs no special
		// case: the caller removes the half-made extent either way, and the
		// wrapped error already says which one it was.
		return fmt.Errorf("reserving %d bytes for %s: %w", size, f.Name(), err)
	}
	return nil
}

// ReservesBlocks reports whether this build actually commits blocks when it
// reserves an extent, so an operator is told rather than left to assume it.
const ReservesBlocks = true
