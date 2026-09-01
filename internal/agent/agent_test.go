package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		BorrowedDir: filepath.Join(root, "borrowed"),
		DonatedDir:  filepath.Join(root, "donated"),
		JournalPath: filepath.Join(root, "state", "leases.json"),
		Lease:       lease.DefaultConfig(),
	}
}

func acct() pool.Accounting {
	return pool.Accounting{Capacity: 8 * pool.TiB, Compute: 1 * pool.TiB, Donated: 2 * pool.TiB}
}

// extent creates the on-disk capacity a lease stands for.
func extent(t *testing.T, a *Agent, id string) {
	t.Helper()
	p := extentPath(t, a, id)
	if err := os.WriteFile(p, []byte("x"), 0o640); err != nil {
		t.Fatalf("creating extent %s: %v", id, err)
	}
}

// extentPath resolves a lease's extent, failing the test if the identifier is
// one the agent refuses to turn into a path.
func extentPath(t *testing.T, a *Agent, id string) string {
	t.Helper()
	p, err := a.ExtentPath(id)
	if err != nil {
		t.Fatalf("ExtentPath(%q) = %v", id, err)
	}
	return p
}

func TestOpenCreatesPoolsAndAcceptsGrants(t *testing.T) {
	a, rec, err := Open(testConfig(t), acct(), t0)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	defer a.Close()

	if len(rec.OrphanExtents)+len(rec.LeasesWithoutExtents)+len(rec.Expired) != 0 {
		t.Errorf("first start reconciled %+v, want nothing to correct", rec)
	}
	// Grants are only accepted once the journal has been replayed, so this
	// succeeding is what proves startup completed.
	l := lease.Lease{ID: "a", Class: lease.Elastic, Size: 1 * pool.TiB, Term: time.Hour}
	if err := a.Leases().Accept(l, t0); err != nil {
		t.Fatalf("Accept after startup = %v", err)
	}
}

func TestOnlyOneAgentOwnsANode(t *testing.T) {
	cfg := testConfig(t)
	first, _, err := Open(cfg, acct(), t0)
	if err != nil {
		t.Fatalf("first Open = %v", err)
	}

	// Two agents would both journal and both reclaim, and both would be wrong.
	if _, _, err := Open(cfg, acct(), t0); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open = %v, want ErrLocked", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	second, _, err := Open(cfg, acct(), t0)
	if err != nil {
		t.Fatalf("Open after Close = %v, want the lock released", err)
	}
	second.Close()
}

func TestPoolsMustNotShareOrNestDirectories(t *testing.T) {
	// The blunt recoveries that make borrowed capacity safe would be data loss
	// if they could reach donated capacity, so the layout is refused up front.
	root := t.TempDir()
	base := Config{JournalPath: filepath.Join(root, "leases.json"), Lease: lease.DefaultConfig()}

	same := base
	same.BorrowedDir = filepath.Join(root, "pool")
	same.DonatedDir = filepath.Join(root, "pool")
	if _, _, err := Open(same, acct(), t0); !errors.Is(err, ErrSamePool) {
		t.Errorf("shared directory = %v, want ErrSamePool", err)
	}

	nested := base
	nested.BorrowedDir = filepath.Join(root, "pool")
	nested.DonatedDir = filepath.Join(root, "pool", "durable")
	if _, _, err := Open(nested, acct(), t0); !errors.Is(err, ErrNestedPools) {
		t.Errorf("nested directory = %v, want ErrNestedPools", err)
	}

	missing := base
	if _, _, err := Open(missing, acct(), t0); !errors.Is(err, ErrNoPoolDir) {
		t.Errorf("unset directories = %v, want ErrNoPoolDir", err)
	}
}

func TestStartupUnlinksExtentsNoLeaseAccountsFor(t *testing.T) {
	// Capacity nobody has a record of lending has leaked.
	cfg := testConfig(t)
	a, _, err := Open(cfg, acct(), t0)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	extent(t, a, "ghost")
	donated := filepath.Join(cfg.DonatedDir, "durable-data")
	if err := os.WriteFile(donated, []byte("precious"), 0o640); err != nil {
		t.Fatalf("seeding donated data: %v", err)
	}
	a.Close()

	b, rec, err := Open(cfg, acct(), t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("restart = %v", err)
	}
	defer b.Close()

	if len(rec.OrphanExtents) != 1 || rec.OrphanExtents[0] != "ghost" {
		t.Fatalf("OrphanExtents = %v, want the unaccounted extent", rec.OrphanExtents)
	}
	if _, err := os.Stat(extentPath(t, b, "ghost")); !errors.Is(err, os.ErrNotExist) {
		t.Error("orphan extent survived reconciliation")
	}
	// Donated capacity is durable and must never be touched by this.
	if _, err := os.Stat(donated); err != nil {
		t.Errorf("donated data was disturbed: %v", err)
	}
}

func TestStartupDropsLeasesWhoseExtentIsGone(t *testing.T) {
	cfg := testConfig(t)
	a, _, err := Open(cfg, acct(), t0)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	for _, id := range []string{"kept", "vanished"} {
		l := lease.Lease{ID: id, Class: lease.Elastic, Size: 1 * pool.TiB, Term: time.Hour}
		if err := a.Leases().Accept(l, t0); err != nil {
			t.Fatalf("Accept(%s) = %v", id, err)
		}
		extent(t, a, id)
	}
	// The extent disappears while the node is down.
	if err := os.Remove(extentPath(t, a, "vanished")); err != nil {
		t.Fatalf("removing extent: %v", err)
	}
	a.Close()

	b, rec, err := Open(cfg, acct(), t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("restart = %v", err)
	}
	defer b.Close()

	if len(rec.LeasesWithoutExtents) != 1 || rec.LeasesWithoutExtents[0] != "vanished" {
		t.Fatalf("LeasesWithoutExtents = %v, want the lease with no extent", rec.LeasesWithoutExtents)
	}
	if got := b.Accounting().Borrowed; got != 1*pool.TiB {
		t.Errorf("Borrowed = %s, want only the surviving lease counted", got)
	}
	if err := b.Accounting().Validate(); err != nil {
		t.Errorf("accounting after reconciliation = %v, want it to balance", err)
	}
}

func TestLeasesSurviveARestart(t *testing.T) {
	cfg := testConfig(t)
	a, _, err := Open(cfg, acct(), t0)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	l := lease.Lease{ID: "kept", Class: lease.Elastic, Size: 2 * pool.TiB, Term: time.Hour}
	if err := a.Leases().Accept(l, t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	extent(t, a, "kept")
	a.Close()

	b, rec, err := Open(cfg, acct(), t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("restart = %v", err)
	}
	defer b.Close()

	if len(rec.OrphanExtents)+len(rec.LeasesWithoutExtents) != 0 {
		t.Errorf("clean restart corrected %+v, want nothing", rec)
	}
	if got := b.Accounting().Borrowed; got != 2*pool.TiB {
		t.Errorf("Borrowed after restart = %s, want 2.00TiB", got)
	}
}

func TestExpiredLeasesAreDroppedAndTheirExtentsRemoved(t *testing.T) {
	cfg := testConfig(t)
	a, _, err := Open(cfg, acct(), t0)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	l := lease.Lease{ID: "stale", Class: lease.Elastic, Size: 1 * pool.TiB, Term: time.Minute}
	if err := a.Leases().Accept(l, t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	extent(t, a, "stale")
	a.Close()

	b, rec, err := Open(cfg, acct(), t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("restart = %v", err)
	}
	defer b.Close()

	if len(rec.Expired) != 1 || rec.Expired[0] != "stale" {
		t.Fatalf("Expired = %v, want the lapsed lease", rec.Expired)
	}
	// Its extent is then unaccounted for, so reconciliation removes it too.
	if len(rec.OrphanExtents) != 1 || rec.OrphanExtents[0] != "stale" {
		t.Fatalf("OrphanExtents = %v, want the lapsed lease's extent", rec.OrphanExtents)
	}
	if got := b.Accounting().Borrowed; got != 0 {
		t.Errorf("Borrowed = %s, want the capacity returned", got)
	}
}

func TestUnreadableJournalStartsEmptyAndClearsThePool(t *testing.T) {
	// Everything the journal described is regenerable, so the node discards
	// the borrowed pool and carries on rather than refusing to start.
	cfg := testConfig(t)
	a, _, err := Open(cfg, acct(), t0)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	if err := a.Leases().Accept(lease.Lease{ID: "a", Class: lease.Elastic, Size: 1 * pool.TiB, Term: time.Hour}, t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	extent(t, a, "a")
	a.Close()

	if err := os.WriteFile(cfg.JournalPath, []byte("{{{ not json"), 0o640); err != nil {
		t.Fatalf("corrupting journal: %v", err)
	}

	b, rec, err := Open(cfg, acct(), t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("Open with a corrupt journal = %v, want a started agent and no error", err)
	}
	defer b.Close()
	// A returned error always means no agent and no lock, so a recovered
	// journal problem is reported on the result instead.
	if !errors.Is(rec.JournalRecovered, lease.ErrCorrupt) {
		t.Fatalf("JournalRecovered = %v, want ErrCorrupt", rec.JournalRecovered)
	}

	if len(rec.OrphanExtents) != 1 || rec.OrphanExtents[0] != "a" {
		t.Errorf("OrphanExtents = %v, want the now-unaccounted extent", rec.OrphanExtents)
	}
	if got := b.Accounting().Borrowed; got != 0 {
		t.Errorf("Borrowed = %s, want the borrowed pool discarded", got)
	}
}

func TestLeaseIdentifiersCannotEscapeTheBorrowedPool(t *testing.T) {
	// Identifiers arrive from the control plane and become paths. The whole
	// design rests on blunt deletion staying inside one directory, so an
	// identifier that could leave it is refused rather than cleaned.
	a, _, err := Open(testConfig(t), acct(), t0)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	defer a.Close()

	for _, id := range []string{"", ".", "..", "../escape", "a/b", "sub/../../x", lockName} {
		if p, err := a.ExtentPath(id); !errors.Is(err, ErrBadLeaseID) {
			t.Errorf("ExtentPath(%q) = %q, %v; want ErrBadLeaseID", id, p, err)
		}
	}
	if _, err := a.ExtentPath("shard-00104"); err != nil {
		t.Errorf("ExtentPath on an ordinary id = %v, want nil", err)
	}
}

func TestANodeThatCameBackSmallerIsReportedApartFromAgeing(t *testing.T) {
	// A lapsed term and a node losing capacity are different events, and an
	// operator reading one should not conclude the other.
	cfg := testConfig(t)
	big := pool.Accounting{Capacity: 8 * pool.TiB, Compute: 1 * pool.TiB, Donated: 2 * pool.TiB}
	a, _, err := Open(cfg, big, t0)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	l := lease.Lease{ID: "roomy", Class: lease.Elastic, Size: 4 * pool.TiB, Term: time.Hour}
	if err := a.Leases().Accept(l, t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	extent(t, a, "roomy")
	a.Close()

	// The workload grew while the node was down, so the lease no longer fits.
	smaller := pool.Accounting{Capacity: 8 * pool.TiB, Compute: 6 * pool.TiB, Donated: 2 * pool.TiB}
	b, rec, err := Open(cfg, smaller, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("restart = %v", err)
	}
	defer b.Close()

	if len(rec.Unfittable) != 1 || rec.Unfittable[0] != "roomy" {
		t.Fatalf("Unfittable = %v, want the lease that no longer fits", rec.Unfittable)
	}
	if len(rec.Expired) != 0 {
		t.Errorf("Expired = %v, want nothing reported as aged out", rec.Expired)
	}
	if err := b.Accounting().Validate(); err != nil {
		t.Errorf("accounting after restart = %v, want it to balance", err)
	}
}

func TestReportedListsAreStable(t *testing.T) {
	// Built by iterating a map, these used to change order between runs, which
	// is the same defect the reclaim ladder had.
	cfg := testConfig(t)
	a, _, err := Open(cfg, acct(), t0)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	for _, id := range []string{"c", "a", "b"} {
		l := lease.Lease{ID: id, Class: lease.Elastic, Size: 1 * pool.TiB, Term: time.Hour}
		if err := a.Leases().Accept(l, t0); err != nil {
			t.Fatalf("Accept(%s) = %v", id, err)
		}
	}
	a.Close() // every extent is missing, so all three are reported

	b, rec, err := Open(cfg, acct(), t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("restart = %v", err)
	}
	defer b.Close()

	want := []string{"a", "b", "c"}
	if len(rec.LeasesWithoutExtents) != len(want) {
		t.Fatalf("LeasesWithoutExtents = %v, want %v", rec.LeasesWithoutExtents, want)
	}
	for i := range want {
		if rec.LeasesWithoutExtents[i] != want[i] {
			t.Fatalf("LeasesWithoutExtents = %v, want sorted %v", rec.LeasesWithoutExtents, want)
		}
	}
}
