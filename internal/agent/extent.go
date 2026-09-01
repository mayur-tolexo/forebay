package agent

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

// invalidSuffix marks an extent that is on its way out.
//
// It is deliberately not a valid lease identifier, which does two things at
// once. Reconciliation already unlinks anything in the borrowed pool that no
// live lease claims, so a crash between invalidating and unlinking leaves a
// file the next startup removes without needing to know why it is there. And a
// reader resolving a lease to a path can never reach it, because ExtentPath
// builds the name from the identifier and validLeaseID refuses a dot.
const invalidSuffix = ".invalid"

// ErrNoExtent reports a lease whose capacity was never materialised.
var ErrNoExtent = errors.New("agent: lease has no extent")

// Grant accepts a lease and materialises the capacity it describes.
//
// The order is the safe one. The lease manager decides first whether the node
// can honour the grant, because it is the authority on the node's own capacity
// and refusing is cheaper than allocating. Only then are the bytes reserved.
//
// If reserving fails the lease is released again, so the accounting never
// records capacity that does not exist on disk. A node that believes it lent
// more than it did will refuse the next honest grant.
func (a *Agent) Grant(l lease.Lease, now time.Time) error {
	if err := a.leases.Accept(l, now); err != nil {
		return err
	}
	if err := a.allocate(l.ID, l.Size); err != nil {
		if _, rerr := a.leases.Release(l.ID, now); rerr != nil {
			return fmt.Errorf("%w, and releasing the accounting after that failed: %v", err, rerr)
		}
		return err
	}
	return nil
}

// allocate creates a lease's extent and reserves its bytes.
//
// One extent per lease, large and few, which is what makes reclamation an
// unlink rather than a compaction. Reclaiming happens when the node is under
// pressure, and any path that has to write in order to free space is slowest
// exactly when it is needed most.
func (a *Agent) allocate(leaseID string, size pool.Bytes) error {
	path, err := a.ExtentPath(leaseID)
	if err != nil {
		return err
	}
	// O_EXCL because an extent that already exists belongs to a lease the
	// manager did not know about, and silently adopting it would hand a new
	// tenant whatever the last one left there.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("agent: creating extent for %s: %w", leaseID, err)
	}
	if err := reserve(f, int64(size)); err != nil {
		f.Close()
		// The half-made extent is removed rather than left for
		// reconciliation, so a failed grant does not occupy the pool until the
		// next restart.
		if rmErr := os.Remove(path); rmErr != nil {
			return fmt.Errorf("agent: %w, and removing the partial extent failed: %v", err, rmErr)
		}
		return fmt.Errorf("agent: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("agent: closing extent for %s: %w", leaseID, err)
	}
	return nil
}

// Release returns one lease's capacity, extent first.
//
// The extent is invalidated and unlinked before the accounting is freed, so
// the node never reports capacity as available while the bytes are still on
// disk. Reporting it early would let the next grant be accepted against space
// that is not there yet, which is the overcommit the pool arithmetic exists to
// prevent.
func (a *Agent) Release(leaseID string, now time.Time) (pool.Bytes, error) {
	if err := a.discard(leaseID); err != nil {
		return 0, err
	}
	return a.leases.Release(leaseID, now)
}

// discard invalidates an extent and then unlinks it.
//
// Invalidate before unlink, from RFC-0005: an extent becomes unreachable
// before its bytes are released, so no reader can be handed a range that is
// being freed underneath it. The rename is the invalidation, and it is atomic,
// which is what makes the two steps safe to be interrupted between.
//
// Draining readers is not built. Nothing serves an extent yet, so there is
// nobody to wait for, and when the access layer exists it has to wait between
// these two steps rather than around them.
func (a *Agent) discard(leaseID string) error {
	path, err := a.ExtentPath(leaseID)
	if err != nil {
		return err
	}
	invalid := path + invalidSuffix
	if err := os.Rename(path, invalid); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrNoExtent, leaseID)
		}
		return fmt.Errorf("agent: invalidating extent for %s: %w", leaseID, err)
	}
	if err := os.Remove(invalid); err != nil {
		// Left behind deliberately: the name no lease claims, so the next
		// reconciliation unlinks it. Reporting the error matters more than
		// retrying it, because the capacity is already unreachable.
		return fmt.Errorf("agent: unlinking invalidated extent for %s: %w", leaseID, err)
	}
	return nil
}

// ReclaimCapacity frees at least need bytes and unlinks what it freed.
//
// The lease manager chooses which leases go, in reclaim order, since it owns
// the ladder and the protections around it. This walks the leases it released
// and removes their extents.
//
// The window between those two things is real and is the reason this returns
// the leases whose extents outlived their accounting: the manager reports the
// capacity free before the bytes are gone, so a grant arriving in between could
// be accepted against space still occupied. Reclaim was measured at
// milliseconds, which makes the window small rather than absent.
func (a *Agent) ReclaimCapacity(need pool.Bytes, now time.Time) (lease.Result, []string, error) {
	res := a.leases.Reclaim(need, now)
	var failed []string
	for _, id := range res.Dropped {
		if err := a.discard(id); err != nil && !errors.Is(err, ErrNoExtent) {
			failed = append(failed, id)
		}
	}
	if len(failed) > 0 {
		return res, failed, fmt.Errorf("agent: %d reclaimed extents could not be unlinked and await reconciliation", len(failed))
	}
	return res, nil, nil
}

// invalidatedExtents lists extents left mid-discard by an interrupted run, so
// startup can report them rather than only silently cleaning them up.
func invalidatedExtents(entries []os.DirEntry) []string {
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), invalidSuffix) {
			out = append(out, e.Name())
		}
	}
	return out
}
