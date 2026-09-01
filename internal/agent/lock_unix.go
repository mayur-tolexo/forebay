//go:build unix

package agent

import (
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an exclusive, non-blocking lock on an open file.
//
// The lock is held by the process and released when the descriptor closes,
// including on a crash, so a dead agent never keeps its successor out. A live
// but wedged one does, which is what liveness exists to break.
func lockFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%w: %v", ErrLocked, err)
	}
	return nil
}

// unlockFile releases the lock.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
