package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

// grantable is a lease small enough that a test can really allocate it.
func grantable(id string, size pool.Bytes) lease.Lease {
	return lease.Lease{ID: id, Class: lease.Elastic, Size: size, Term: time.Hour}
}

// testAccounting is a node with capacity to lend, shared so tests that restart
// an agent describe the same node both times.
func testAccounting() pool.Accounting { return pool.Accounting{Capacity: 64 << 20} }

// openAgent starts an agent over a temporary node with capacity to lend.
func openAgent(t *testing.T) (*Agent, time.Time) {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		BorrowedDir: filepath.Join(root, "borrowed"),
		JournalPath: filepath.Join(root, "state", "leases.json"),
		Lease:       lease.DefaultConfig(),
	}
	cfg.Lease.ReclaimWithin = 30 * time.Second
	now := time.Now()
	a, _, err := Open(cfg, testAccounting(), now)
	if err != nil {
		t.Fatalf("opening agent: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a, now
}

func TestAGrantPutsTheCapacityOnDisk(t *testing.T) {
	// Before this, a lease was pure bookkeeping: the accounting recorded
	// capacity lent and nothing occupied it, so a node could believe it had
	// lent a terabyte with an empty pool underneath.
	a, now := openAgent(t)
	const size = 4 << 20
	if err := a.Grant(grantable("lease-a", size), now); err != nil {
		t.Fatalf("granting: %v", err)
	}

	path, err := a.ExtentPath("lease-a")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the extent was not created: %v", err)
	}
	if info.Size() != size {
		t.Errorf("extent is %d bytes, want %d", info.Size(), size)
	}
	if got := a.Accounting().Borrowed; got != size {
		t.Errorf("borrowed = %s, want %s", got, pool.Bytes(size))
	}
}

func TestReservedBlocksAreReallyCommitted(t *testing.T) {
	// The point of preallocating is that the space is genuinely taken, so
	// compute cannot quietly claim capacity a lease was promised. A sparse
	// file of the right length reserves nothing and would pass a size check
	// while failing the purpose.
	if !ReservesBlocks {
		t.Skip("this build cannot reserve blocks, which it reports at startup")
	}
	a, now := openAgent(t)
	const size = 4 << 20
	if err := a.Grant(grantable("lease-a", size), now); err != nil {
		t.Fatalf("granting: %v", err)
	}
	path, _ := a.ExtentPath("lease-a")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	blocks := allocatedBytes(t, info)
	if blocks < size {
		t.Errorf("extent occupies %d bytes on disk but is %d long, so the blocks were not committed", blocks, size)
	}
}

func TestAFailedReservationLeavesNoAccountingBehind(t *testing.T) {
	// If the bytes cannot be had, the lease must not survive in the
	// accounting. A node that believes it lent more than it did refuses the
	// next honest grant, which is a quiet way to take a node out of service.
	a, now := openAgent(t)
	// Larger than the node's whole capacity, so the manager refuses it and no
	// extent is attempted at all.
	err := a.Grant(grantable("too-big", 1<<40), now)
	if err == nil {
		t.Fatal("a grant beyond capacity was accepted")
	}
	if got := a.Accounting().Borrowed; got != 0 {
		t.Errorf("borrowed = %s after a refused grant, want 0", got)
	}
	if _, statErr := os.Stat(filepath.Join(a.cfg.BorrowedDir, "too-big")); statErr == nil {
		t.Error("an extent was created for a grant that was refused")
	}
}

func TestAnExtentIsNeverSilentlyAdopted(t *testing.T) {
	// A file already sitting at a lease's path belongs to something the
	// manager does not know about. Reusing it would hand a new tenant whatever
	// the last one left in it.
	a, now := openAgent(t)
	path := filepath.Join(a.cfg.BorrowedDir, "lease-a")
	if err := os.WriteFile(path, []byte("someone else's bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := a.Grant(grantable("lease-a", 1<<20), now); err == nil {
		t.Fatal("a grant adopted an extent that was already there")
	}
	if got := a.Accounting().Borrowed; got != 0 {
		t.Errorf("borrowed = %s after the grant failed, want 0", got)
	}
}

func TestReleaseFreesTheDiskBeforeTheAccounting(t *testing.T) {
	// Reporting capacity available while its bytes are still on disk would let
	// the next grant be accepted against space that is not there.
	a, now := openAgent(t)
	const size = 4 << 20
	if err := a.Grant(grantable("lease-a", size), now); err != nil {
		t.Fatalf("granting: %v", err)
	}
	if _, err := a.Release("lease-a", now); err != nil {
		t.Fatalf("releasing: %v", err)
	}
	if got := a.Accounting().Borrowed; got != 0 {
		t.Errorf("borrowed = %s after release, want 0", got)
	}
	entries, err := os.ReadDir(a.cfg.BorrowedDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		// The prefix rather than a list of names, so a file the agent keeps
		// for itself later does not break an assertion about extents.
		if !strings.HasPrefix(e.Name(), agentFilePrefix) {
			t.Errorf("%s survived the release", e.Name())
		}
	}
}

func TestReleasingALeaseWithNoExtentSaysSo(t *testing.T) {
	a, now := openAgent(t)
	if err := a.Grant(grantable("lease-a", 1<<20), now); err != nil {
		t.Fatalf("granting: %v", err)
	}
	path, _ := a.ExtentPath("lease-a")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Release("lease-a", now); !errors.Is(err, ErrNoExtent) {
		t.Errorf("release = %v, want ErrNoExtent", err)
	}
}

func TestAnInterruptedDiscardIsFinishedByTheNextStartup(t *testing.T) {
	// Invalidating renames before unlinking, so a crash in between leaves a
	// file under a name no lease can claim. Reconciliation already removes
	// anything unaccounted for, so recovery costs nothing extra, but it is
	// reported separately because it means a reclaim was interrupted rather
	// than that capacity leaked.
	a, now := openAgent(t)
	cfg := a.cfg
	if err := a.Grant(grantable("lease-a", 1<<20), now); err != nil {
		t.Fatalf("granting: %v", err)
	}
	path, _ := a.ExtentPath("lease-a")
	if err := os.Rename(path, path+invalidSuffix); err != nil {
		t.Fatal(err)
	}
	a.Close()

	restarted, rec, err := Open(cfg, testAccounting(), now)
	if err != nil {
		t.Fatalf("restarting: %v", err)
	}
	defer restarted.Close()

	if len(rec.InvalidatedExtents) != 1 || !strings.HasSuffix(rec.InvalidatedExtents[0], invalidSuffix) {
		t.Errorf("InvalidatedExtents = %v, want the interrupted extent named", rec.InvalidatedExtents)
	}
	// Reported once. The same event used to appear as leaked capacity and as
	// a lease that lost its extent as well, so one interrupted reclaim read
	// as three problems on the startup line.
	if len(rec.OrphanExtents) != 0 {
		t.Errorf("OrphanExtents = %v, want the interrupted extent counted only once", rec.OrphanExtents)
	}
	if len(rec.LeasesWithoutExtents) != 0 {
		t.Errorf("LeasesWithoutExtents = %v, want the interrupted reclaim to explain it", rec.LeasesWithoutExtents)
	}
	if _, err := os.Stat(path + invalidSuffix); !os.IsNotExist(err) {
		t.Error("the invalidated extent survived startup")
	}
	if got := restarted.Accounting().Borrowed; got != 0 {
		t.Errorf("borrowed = %s, want the lease dropped with its extent", got)
	}
}

func TestReclaimUnlinksWhatItFreed(t *testing.T) {
	a, now := openAgent(t)
	const size = 8 << 20
	for _, id := range []string{"lease-a", "lease-b"} {
		if err := a.Grant(grantable(id, size), now); err != nil {
			t.Fatalf("granting %s: %v", id, err)
		}
	}

	rec, err := a.ReclaimCapacity(size, now)
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}
	if len(rec.Failed) != 0 {
		t.Errorf("extents left behind: %v", rec.Failed)
	}
	if rec.Result.Reclaimed < size {
		t.Errorf("reclaimed %s, want at least %s", rec.Result.Reclaimed, pool.Bytes(size))
	}
	for _, id := range rec.Result.Dropped {
		path := filepath.Join(a.cfg.BorrowedDir, id)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("extent for reclaimed lease %s survived", id)
		}
	}
}

func TestAnExtentCannotEscapeTheBorrowedPool(t *testing.T) {
	// Identifiers arrive from a control plane, so a grant naming a path
	// outside the pool must be refused rather than cleaned, since blunt
	// deletion is confined to that directory by construction.
	a, now := openAgent(t)
	for _, id := range []string{"../escape", "a/b", ".", ""} {
		if err := a.Grant(grantable(id, 1<<20), now); err == nil {
			t.Errorf("a grant with id %q was accepted", id)
		}
	}
}

func TestALeasedExtentCanOnlyBeMadeThroughGrant(t *testing.T) {
	// The agent used to hand out its lease manager, so a caller could accept a
	// lease straight on it and get capacity recorded as lent with nothing on
	// disk. Such a lease is silently dropped by the next startup, which made
	// two ways to create a lease where only one survived a restart.
	//
	// This is a compile-time property rather than a runtime one, so the test
	// is the assertion that no exported accessor returns the manager.
	a, _ := openAgent(t)
	if _, hasAccessor := any(a).(interface{ Leases() *lease.Manager }); hasAccessor {
		t.Error("Agent exposes its lease manager, so a caller can create a lease with no extent")
	}
}

func TestALeaseCannotShadowAnExtentBeingDiscarded(t *testing.T) {
	// Discarding renames an extent to the invalid suffix, and a rename
	// overwrites whatever is already there. With the suffix unreserved, two
	// leases could exist whose names collided under that rule, and releasing
	// the first destroyed the second's extent and said nothing: the victim
	// kept its accounting with no bytes behind it, which is the state the
	// extent lifecycle exists to prevent, reached by releasing something else.
	a, now := openAgent(t)
	if err := a.Grant(grantable("a", 1<<20), now); err != nil {
		t.Fatal(err)
	}
	err := a.Grant(grantable("a"+invalidSuffix, 2<<20), now)
	if !errors.Is(err, ErrBadLeaseID) {
		t.Fatalf("granting a lease named a%s = %v, want ErrBadLeaseID", invalidSuffix, err)
	}
	// The refused grant must leave the first lease and its extent untouched.
	if got := a.Accounting().Borrowed; got != 1<<20 {
		t.Errorf("borrowed = %s, want only the first lease", got)
	}
	if _, err := a.Release("a", now); err != nil {
		t.Errorf("releasing the surviving lease: %v", err)
	}
}

func TestReclaimIsTimedAgainstThePromiseItMade(t *testing.T) {
	// The elastic class exists to promise capacity back within a deadline, and
	// until now nothing measured whether it was. A promise nobody times is a
	// promise nobody keeps.
	a, now := openAgent(t)
	const size = 4 << 20
	if err := a.Grant(grantable("elastic-a", size), now); err != nil {
		t.Fatal(err)
	}
	rec, err := a.ReclaimCapacity(size, now)
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}
	if rec.Elapsed <= 0 {
		t.Error("Elapsed was not measured")
	}
	if rec.Deadline != a.cfg.Lease.ReclaimWithin {
		t.Errorf("Deadline = %s, want the configured %s", rec.Deadline, a.cfg.Lease.ReclaimWithin)
	}
	if rec.Overran {
		t.Errorf("a %s reclaim overran a %s deadline", rec.Elapsed, rec.Deadline)
	}
}

func TestOpportunisticCapacityHasNoDeadlineToOverrun(t *testing.T) {
	// It is taken first and without warning, so it promises nothing. Measuring
	// it against the elastic deadline would invent a promise the class does
	// not make, and then report it as kept.
	a, now := openAgent(t)
	const size = 4 << 20
	l := grantable("opp-a", size)
	l.Class = lease.Opportunistic
	if err := a.Grant(l, now); err != nil {
		t.Fatal(err)
	}
	rec, err := a.ReclaimCapacity(size, now)
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}
	if rec.Deadline != 0 {
		t.Errorf("Deadline = %s for an opportunistic reclaim, want none", rec.Deadline)
	}
	if rec.Overran {
		t.Error("an opportunistic reclaim was reported as overrunning a deadline it does not have")
	}
}

func TestAMissedDeadlineIsAnError(t *testing.T) {
	// A caller that gets a nil error keeps offering the same promise. Missing
	// the deadline is the elastic class failing at the one thing it does, so
	// it has to be impossible to ignore by accident.
	a, now := openAgent(t)
	// A deadline no real reclaim can meet, since the point is the reporting
	// rather than provoking a genuinely slow unlink.
	a.cfg.Lease.ReclaimWithin = time.Nanosecond
	const size = 4 << 20
	if err := a.Grant(grantable("elastic-a", size), now); err != nil {
		t.Fatal(err)
	}
	rec, err := a.ReclaimCapacity(size, now)
	if !errors.Is(err, ErrDeadlineMissed) {
		t.Fatalf("reclaim = %v, want ErrDeadlineMissed", err)
	}
	if !rec.Overran {
		t.Error("Overran was not set on a reclaim that missed its deadline")
	}
	// The capacity still came back. A missed deadline is a broken promise, not
	// a failed reclamation, and reporting it must not undo the work.
	if got := a.Accounting().Borrowed; got != 0 {
		t.Errorf("borrowed = %s after a late reclaim, want the capacity returned anyway", got)
	}
}
