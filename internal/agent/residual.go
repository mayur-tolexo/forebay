package agent

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrResidualData reports a pool that hands a new holder bytes the last one
// left. A node that cannot prove otherwise must donate nothing.
var ErrResidualData = errors.New("agent: freshly reserved capacity is not zeroed")

// probeName is the extent the check reserves. It carries the invalid suffix so
// that a crash mid-check leaves a file reconciliation already unlinks, and so
// that no lease identifier can ever resolve to it.
const probeName = "residual-probe" + invalidSuffix

// verifyPool is the check Open runs, replaceable in a test.
var verifyPool = VerifyNoResidualData

// probeSize is what the check reserves.
//
// Large enough to span more than one filesystem allocation unit, so a stale
// block would have to be missed rather than merely not sampled, and small
// enough that the check costs a fraction of a second on a node that is
// starting up.
const probeSize = 4 << 20

// VerifyNoResidualData proves that capacity this node reserves reads as zeros
// before the node lends any of it.
//
// Reclamation is an unlink, because a path that has to write in order to free
// space is slowest exactly when compute is waiting for it. That leaves the
// whole residual-data guarantee resting on allocation: fallocate commits
// blocks as unwritten extents, which read as zeros rather than as whatever was
// on them. It is a filesystem property, it holds on ext4 and XFS, and its
// failure mode is silent, so the node checks it rather than assuming it.
//
// Refusing to lend is the correct failure. An operator gets a node that stayed
// out of the pool, not a cluster that leaked.
func VerifyNoResidualData(dir string) error {
	path := filepath.Join(dir, probeName)
	// Removed first: a probe left by an interrupted startup would otherwise
	// fail O_EXCL and report a residual-data problem that is really a crash.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("agent: removing a stale residual probe: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o640)
	if err != nil {
		return fmt.Errorf("agent: creating the residual probe: %w", err)
	}
	defer os.Remove(path)
	defer f.Close()

	// Reserved exactly as a real extent is. A check that allocated differently
	// would be checking something the agent does not do.
	if err := reserve(f, probeSize); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return scanZeros(f, probeSize)
}

// scanZeros reads the whole probe and reports the first byte that is not zero,
// since where it is says more than that one exists.
//
// Taken as an interface so a reader that cannot occur on a filesystem can
// still be driven in a test.
func scanZeros(f io.ReaderAt, size int64) error {
	buf := make([]byte, 1<<20)
	var at int64
	for at < size {
		n, err := f.ReadAt(buf, at)
		// Bounded to what was asked for, since a reader may hold more than the
		// probe and bytes past it are not this check's to judge.
		if over := at + int64(n) - size; over > 0 {
			n -= int(over)
		}
		for i := 0; i < n; i++ {
			if buf[i] != 0 {
				return fmt.Errorf("%w: byte %d of a fresh %d-byte extent is %#x",
					ErrResidualData, at+int64(i), size, buf[i])
			}
		}
		at += int64(n)
		// Nothing read means no progress, whatever the reason given. It is the
		// loop's only exit besides reaching the size: a reservation that
		// stopped short arrives here too, and that is its own defect rather
		// than a clean pool. Looping instead would hang the node's startup
		// without ever saying why, which is worse than failing.
		if n == 0 {
			return fmt.Errorf("agent: the residual probe stopped at %d of %d bytes: %w",
				at, size, cmp.Or(err, io.ErrUnexpectedEOF))
		}
	}
	return nil
}
