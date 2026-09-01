// Package agent brings a node's capacity under management: it owns the pool
// directories, holds the lock that makes one agent the only writer, replays
// the lease journal, and reconciles what the journal claims against what is
// actually on disk.
//
// It does not serve data. The read path is not built.
package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

// Errors that stop an agent starting. Each is a refusal to run in a state
// where it could do damage, rather than a condition to degrade through.
var (
	ErrLocked      = errors.New("agent: another agent holds this node's pool")
	ErrSamePool    = errors.New("agent: borrowed and donated pools must be different directories")
	ErrNoPoolDir   = errors.New("agent: pool directories must be configured")
	ErrNestedPools = errors.New("agent: one pool directory contains the other")
	ErrBadLeaseID  = errors.New("agent: lease id cannot be used as a path")
)

// lockName is the file whose descriptor carries the node lock. It lives in the
// borrowed directory so that the lock and the capacity it guards cannot be
// configured to different volumes by accident.
const lockName = agentFilePrefix + "node.lock"

// agentFilePrefix marks files the agent keeps for itself in the borrowed pool,
// so no lease can be named over one. A prefix rather than a list of names,
// because the list kept growing and one addition was already forgotten.
const agentFilePrefix = ".forebay-"

// Config is what an agent needs to take charge of a node's capacity.
type Config struct {
	// BorrowedDir holds capacity lent revocably. Everything in it is
	// regenerable, which is what lets the agent delete anything it cannot
	// account for.
	BorrowedDir string
	// DonatedDir holds durable data. The agent never deletes from it, and it
	// must be a separate directory, because the blunt recoveries that make
	// borrowed capacity safe would be data loss here.
	DonatedDir string
	// JournalPath records live leases so a restart knows what was lent.
	JournalPath string
	// Lease tunes the lease manager. Its reclaim deadline is not optional:
	// without one every elastic grant is refused.
	Lease lease.Config
}

// Agent owns one node's capacity for the lifetime of the process.
type Agent struct {
	cfg    Config
	lock   *os.File
	leases *lease.Manager
}

// Reconciliation reports what startup had to correct.
type Reconciliation struct {
	// InvalidatedExtents are extents a previous run had begun discarding and
	// did not finish, found by their suffix. They are unlinked as orphans like
	// anything else unaccounted for, and named separately because they mean
	// something different: not leaked capacity, but a reclaim interrupted
	// between invalidating an extent and removing it.
	InvalidatedExtents []string
	// Leftovers are the agent's own temporary files from a run that was killed
	// mid-write. They hold nothing and are removed.
	Leftovers []string
	// OrphanExtents are extents on disk that no lease accounted for. Capacity
	// nobody has a record of lending has leaked, so they are unlinked.
	OrphanExtents []string
	// LeasesWithoutExtents are leases whose extent is gone. The lease
	// describes capacity that is not there, so it is dropped.
	LeasesWithoutExtents []string
	// Expired are leases whose term ran out while the node was down.
	Expired []string
	// Unfittable are leases the accounting could no longer accommodate, which
	// means the node came back smaller than it went away. Kept apart from
	// Expired because an operator reading one should not conclude the other.
	Unfittable []string
	// JournalRecovered is set when the journal could not be read and the node
	// started empty. Recoverable rather than fatal, because everything the
	// journal described is regenerable, but never silent.
	JournalRecovered error
}

// Validate rejects configurations that would let a blunt recovery touch
// durable data. This is checked before anything is created or deleted.
//
// It is exported so a caller can check the layout before doing any work of its
// own. Measuring the filesystem under a pool that was never configured, or
// under a pair the agent is about to reject, produces an answer about the
// wrong directory and reports it in place of the real problem.
func (c Config) Validate() error {
	if c.BorrowedDir == "" || c.DonatedDir == "" || c.JournalPath == "" {
		return ErrNoPoolDir
	}
	b, err := filepath.Abs(c.BorrowedDir)
	if err != nil {
		return fmt.Errorf("agent: resolving borrowed dir: %w", err)
	}
	d, err := filepath.Abs(c.DonatedDir)
	if err != nil {
		return fmt.Errorf("agent: resolving donated dir: %w", err)
	}
	if b == d {
		return ErrSamePool
	}
	// Nesting is as dangerous as sharing: deleting everything unaccounted for
	// in the borrowed directory would walk into a donated pool underneath it.
	if within(b, d) || within(d, b) {
		return fmt.Errorf("%w: %s and %s", ErrNestedPools, b, d)
	}
	return nil
}

// within reports whether child is inside parent.
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !hasDotDotPrefix(rel)
}

// hasDotDotPrefix reports whether a relative path escapes its base.
func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 2 && rel[0] == '.' && rel[1] == '.'
}

// Open starts an agent: it validates the layout, takes the node lock, replays
// the journal and reconciles it against the disk, in that order.
//
// Nothing is lent until all of it has succeeded. The lease manager refuses
// grants until its journal has been replayed, so a partial startup lends
// nothing rather than lending against a state it does not know.
func Open(cfg Config, acct pool.Accounting, now time.Time) (*Agent, Reconciliation, error) {
	var rec Reconciliation

	if err := cfg.Validate(); err != nil {
		return nil, rec, err
	}
	for _, dir := range []string{cfg.BorrowedDir, cfg.DonatedDir, filepath.Dir(cfg.JournalPath)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, rec, fmt.Errorf("agent: creating %s: %w", dir, err)
		}
	}

	lockPath := filepath.Join(cfg.BorrowedDir, lockName)
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, rec, fmt.Errorf("agent: opening lock file: %w", err)
	}
	if err := lockFile(lock); err != nil {
		lock.Close()
		return nil, rec, err
	}

	a := &Agent{
		cfg:    cfg,
		lock:   lock,
		leases: lease.New(acct, cfg.Lease, lease.WithJournal(lease.NewFileJournal(cfg.JournalPath))),
	}

	restored, journalErr := a.leases.Restore(now)
	rec.Expired = restored.Dropped
	rec.Unfittable = restored.Unfittable
	if journalErr != nil {
		// An unreadable journal is recoverable, because everything it
		// described is regenerable, but the caller has to hear about it. The
		// manager has already reset itself, so reconciliation below removes
		// the extents that are now unaccounted for.
		if !errors.Is(journalErr, lease.ErrCorrupt) {
			a.Close()
			return nil, rec, journalErr
		}
	}

	r, reconcileErr := a.reconcile(now)
	rec.OrphanExtents = r.OrphanExtents
	rec.LeasesWithoutExtents = r.LeasesWithoutExtents
	rec.InvalidatedExtents = r.InvalidatedExtents
	rec.Leftovers = r.Leftovers

	// After reconciliation, so a probe never sees a fresh timestamp from an
	// agent that has not yet established what it already lent.
	if err := a.Heartbeat(now); err != nil {
		a.Close()
		return nil, rec, err
	}
	if reconcileErr != nil {
		a.Close()
		return nil, rec, reconcileErr
	}
	// A journal problem is recovered from rather than fatal, and it is reported
	// on the Reconciliation rather than as an error, so that a returned error
	// always means no agent and no lock. A caller writing the obvious thing,
	// returning early when err is non-nil, must not leak the node lock.
	rec.JournalRecovered = journalErr
	return a, rec, nil
}

// reconcile makes the journal and the disk agree.
//
// Both directions are corrected. An extent nothing accounts for is unlinked,
// since capacity nobody recorded lending has leaked. A lease whose extent is
// missing is dropped, since it describes capacity that is not there. Only the
// borrowed directory is touched: donated capacity is durable and is never
// deleted by this.
func (a *Agent) reconcile(now time.Time) (Reconciliation, error) {
	var rec Reconciliation

	entries, err := os.ReadDir(a.cfg.BorrowedDir)
	if err != nil {
		return rec, fmt.Errorf("agent: reading borrowed pool: %w", err)
	}

	// An extent found part-way through being discarded is one event with a
	// known cause and a known recovery. It is removed like anything else the
	// accounting does not claim, but it is reported only here: counting it
	// again as leaked capacity, and its lease again as one that lost its
	// extent, turns one interrupted reclaim into three problems on the
	// startup line.
	rec.InvalidatedExtents = invalidatedExtents(entries)
	interrupted := make(map[string]struct{}, len(rec.InvalidatedExtents))
	for _, name := range rec.InvalidatedExtents {
		interrupted[strings.TrimSuffix(name, invalidSuffix)] = struct{}{}
	}

	live := make(map[string]struct{})
	for _, l := range a.leases.Leases() {
		live[l.ID] = struct{}{}
	}

	onDisk := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		// Only the lock and heartbeat are live state. Skipping the rest would
		// put a killed heartbeat's litter beyond the one thing that cleans
		// this pool.
		if e.Name() == lockName || e.Name() == heartbeatName {
			continue
		}
		if strings.HasPrefix(e.Name(), agentFilePrefix) {
			if err := os.Remove(filepath.Join(a.cfg.BorrowedDir, e.Name())); err != nil {
				return rec, fmt.Errorf("agent: removing leftover %s: %w", e.Name(), err)
			}
			rec.Leftovers = append(rec.Leftovers, e.Name())
			continue
		}
		onDisk[e.Name()] = struct{}{}
		if _, ok := live[e.Name()]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(a.cfg.BorrowedDir, e.Name())); err != nil {
			return rec, fmt.Errorf("agent: unlinking orphan extent %s: %w", e.Name(), err)
		}
		if strings.HasSuffix(e.Name(), invalidSuffix) {
			continue // Already reported as an interrupted reclaim.
		}
		rec.OrphanExtents = append(rec.OrphanExtents, e.Name())
	}

	// Sorted rather than ranged over the map, so a reported list does not
	// change order between runs.
	missing := make([]string, 0, len(live))
	for id := range live {
		if _, ok := onDisk[id]; !ok {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	for _, id := range missing {
		// Dropped by identifier rather than by the reclaim ladder, which
		// chooses by cost and would release the wrong lease entirely.
		if _, err := a.leases.Release(id, now); err != nil {
			return rec, fmt.Errorf("agent: dropping lease %s with no extent: %w", id, err)
		}
		if _, ok := interrupted[id]; ok {
			continue // Explained by the interrupted reclaim already reported.
		}
		rec.LeasesWithoutExtents = append(rec.LeasesWithoutExtents, id)
	}
	return rec, nil
}

// Accounting reports the node's current capacity split.
func (a *Agent) Accounting() pool.Accounting { return a.leases.Accounting() }

// ExtentPath is where a lease's capacity lives on disk.
//
// Identifiers arrive from the control plane and become paths here, so one that
// could escape the borrowed directory is refused rather than cleaned. The
// entire design rests on blunt deletion being confined to that directory, and
// filepath.Join would quietly resolve a traversal instead of rejecting it.
func (a *Agent) ExtentPath(leaseID string) (string, error) {
	if err := validLeaseID(leaseID); err != nil {
		return "", err
	}
	return filepath.Join(a.cfg.BorrowedDir, leaseID), nil
}

// validLeaseID rejects identifiers that are not a single, ordinary file name.
func validLeaseID(id string) error {
	switch {
	case id == "", id == "." || id == "..":
		return fmt.Errorf("%w: %q is not a name", ErrBadLeaseID, id)
	case strings.HasPrefix(id, agentFilePrefix):
		return fmt.Errorf("%w: %q starts with %s, which is reserved for the agent's own files", ErrBadLeaseID, id, agentFilePrefix)
	case strings.HasSuffix(id, invalidSuffix):
		// Discarding an extent renames it to this suffix, and a rename
		// overwrites. A lease allowed to carry the suffix could therefore have
		// its extent destroyed by the release of the lease it shadows, which
		// leaves it accounted for with nothing on disk and reports nothing.
		return fmt.Errorf("%w: %q ends in %s, which is reserved for extents being discarded", ErrBadLeaseID, id, invalidSuffix)
	case strings.ContainsRune(id, os.PathSeparator), strings.ContainsRune(id, '/'):
		return fmt.Errorf("%w: %q contains a path separator", ErrBadLeaseID, id)
	case id != filepath.Clean(id):
		return fmt.Errorf("%w: %q is not a clean path element", ErrBadLeaseID, id)
	}
	return nil
}

// Close releases the node lock. An agent that has closed no longer owns the
// node's pool and must not be used.
func (a *Agent) Close() error {
	if a.lock == nil {
		return nil
	}
	err := unlockFile(a.lock)
	if cerr := a.lock.Close(); err == nil {
		err = cerr
	}
	a.lock = nil
	return err
}
