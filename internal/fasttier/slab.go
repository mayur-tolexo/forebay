package fasttier

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
)

// ErrRevoked reports a slab whose capacity has been taken back. A reader sees
// it as a miss to retry, never as an error to report.
var ErrRevoked = errors.New("fasttier: the capacity holding this block was reclaimed")

// slab is one lease's extent, divided into fixed-size slots.
//
// Blocks live inside an extent rather than as files of their own, because
// RFC-0005 allocates borrowed capacity as extents that are large and few: that
// is what makes reclaiming an unlink instead of a walk over many small files.
type slab struct {
	// lease is the grant this extent belongs to, which is also the unit
	// reclamation takes away.
	lease string
	// file is the preallocated extent.
	file *os.File
	// slots is how many blocks fit.
	slots int
	// free lists slots holding nothing.
	free []int
	// revoked marks the slab unreadable before its bytes are released, so no
	// reader is handed a range being freed underneath it. Atomic because reads
	// happen outside the cache lock.
	revoked atomic.Bool
}

// openSlab divides an existing extent into slots.
func openSlab(lease, path string, blockSize int64) (*slab, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("fasttier: opening extent for %s: %w", lease, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("fasttier: measuring extent for %s: %w", lease, err)
	}
	slots := int(info.Size() / blockSize)
	if slots == 0 {
		f.Close()
		return nil, fmt.Errorf("fasttier: extent for %s holds no whole block of %d bytes", lease, blockSize)
	}
	s := &slab{lease: lease, file: f, slots: slots, free: make([]int, 0, slots)}
	for i := slots - 1; i >= 0; i-- {
		s.free = append(s.free, i)
	}
	return s, nil
}

// take claims a free slot, reporting whether one was available.
func (s *slab) take() (int, bool) {
	if s.revoked.Load() || len(s.free) == 0 {
		return 0, false
	}
	slot := s.free[len(s.free)-1]
	s.free = s.free[:len(s.free)-1]
	return slot, true
}

// release returns a slot to the free list.
func (s *slab) release(slot int) { s.free = append(s.free, slot) }

// write puts a block in a slot.
func (s *slab) write(slot int, data []byte, blockSize int64) error {
	if s.revoked.Load() {
		return ErrRevoked
	}
	if int64(len(data)) > blockSize {
		return fmt.Errorf("fasttier: block is %d bytes, slot holds %d", len(data), blockSize)
	}
	if _, err := s.file.WriteAt(data, int64(slot)*blockSize); err != nil {
		return fmt.Errorf("fasttier: writing slot %d: %w", slot, err)
	}
	return nil
}

// read returns length bytes from a slot.
func (s *slab) read(slot int, length int, blockSize int64) ([]byte, error) {
	if s.revoked.Load() {
		return nil, ErrRevoked
	}
	buf := make([]byte, length)
	if _, err := s.file.ReadAt(buf, int64(slot)*blockSize); err != nil {
		return nil, fmt.Errorf("fasttier: reading slot %d: %w", slot, err)
	}
	return buf, nil
}

// revoke makes the slab unreadable. Called before the agent unlinks the
// extent, so a read in flight becomes a miss rather than a range that is being
// freed underneath it.
func (s *slab) revoke() { s.revoked.Store(true) }

// close releases the descriptor.
func (s *slab) close() error { return s.file.Close() }
