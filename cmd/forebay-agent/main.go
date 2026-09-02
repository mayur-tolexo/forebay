// Command forebay-agent takes charge of one node's capacity: it owns the pool
// directories, holds the lock that makes it the only writer, replays the lease
// journal and reconciles it against the disk.
//
// It does not serve data yet. The read path is not built, so the agent starts,
// reports what it found, and exits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
	"github.com/mayur-tolexo/forebay/internal/topology"
	"github.com/mayur-tolexo/forebay/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forebay-agent:", err)
		os.Exit(1)
	}
}

// run parses configuration, starts the agent and reports what startup had to
// correct. Errors are returned rather than exiting inline so that the deferred
// Close releases the node lock on every path.
func run() error {
	var (
		showVersion = flag.Bool("version", false, "print the build identity and exit")
		borrowed    = flag.String("borrowed-dir", "", "directory holding capacity lent revocably")
		journal     = flag.String("journal", "", "path to the lease journal")
		capacity    = flag.Int64("capacity-bytes", 0, "total capacity of the device")
		reserved    = flag.Int64("reserved-bytes", 0, "capacity held for everything that is not Forebay, measured when not given")
		reclaim     = flag.Duration("reclaim-within", 30*time.Second, "how long an elastic lease may take to return capacity")
		sysroot     = flag.String("sysroot", "/", "filesystem root to discover hardware from")
		rack        = flag.String("rack", "", "this node's rack, which cannot be discovered and must be declared")
		mountinfo   = flag.String("mountinfo", "/proc/self/mountinfo", "mount table used to find the device under the pools")
		liveness    = flag.Bool("liveness", false, "check whether the agent owning the pool is still making progress, and exit non-zero if not")
		staleAfter  = flag.Duration("stale-after", 60*time.Second, "how long without progress means the agent is wedged")
		watch       = flag.Bool("watch", false, "stay running, keeping free space above the headroom target")
		headroom    = flag.Int64("headroom-bytes", 0, "free space the agent keeps on top of what is committed, required by --watch")
		interval    = flag.Duration("watch-interval", 10*time.Second, "how often free space is polled")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("forebay-agent", version.String())
		return nil
	}

	// A separate invocation, since the process it judges may be wedged.
	// Exiting non-zero is what gets the holder killed, which frees the lock.
	if *liveness {
		if *borrowed == "" {
			return fmt.Errorf("liveness needs --borrowed-dir, which is where the heartbeat lives")
		}
		switch err := agent.CheckLiveness(*borrowed, *staleAfter, time.Now()); {
		case errors.Is(err, agent.ErrUnreadable):
			// Not a verdict. Failing the probe here kills a healthy agent on
			// every attempt and the restart never fixes the mount.
			fmt.Fprintln(os.Stderr, "forebay-agent: cannot judge liveness:", err)
			return nil
		case err != nil:
			return err
		}
		fmt.Println("forebay-agent: making progress")
		return nil
	}

	// Without a reclaim deadline every elastic grant is refused, so the agent
	// fails here rather than running as a node that lends nothing and cannot
	// explain why.
	if *reclaim <= 0 {
		return fmt.Errorf("reclaim-within must be positive, got %s", *reclaim)
	}
	leaseCfg := lease.DefaultConfig()
	leaseCfg.ReclaimWithin = *reclaim

	cfg := agent.Config{
		BorrowedDir: *borrowed,
		JournalPath: *journal,
		Lease:       leaseCfg,
	}
	node := topology.Discover(*sysroot).WithDeclaredRack(*rack)
	reportTopology(node)

	// The layout is checked before the filesystem is measured, so a pool that
	// was never configured, or a pair that will be rejected anyway, is
	// reported as what it is. Measuring first answers a question about some
	// other directory and reports that in place of the real problem.
	if err := cfg.Validate(); err != nil {
		return err
	}

	// Nothing is created before this point. The pool directories are measured
	// where they will be, not after making them, so a pair of directories the
	// agent is about to reject does not get written to disk first.
	borrowedFS := topology.DescribePool(*sysroot, *mountinfo, cfg.BorrowedDir)
	// What this filesystem can actually hand to Forebay, worked out once and
	// used by both paths below.
	deliverable, haveDeliverable := deliverableBytes(borrowedFS, cfg.BorrowedDir)

	// Capacity comes from the filesystem the pools actually live on, not from
	// summing every device. A node with four drives and pools on one of them
	// can lend what that one holds.
	acct := pool.Accounting{
		Capacity: pool.Bytes(*capacity),
		Reserved: pool.Bytes(*reserved),
	}
	if *capacity == 0 {
		fmt.Printf("pools on %s\n", borrowedFS)

		// An unknown never satisfies a requirement, and being local is a
		// requirement for lending. A filesystem whose device cannot be
		// identified might be a network volume, and offering it as
		// compute-local capacity is the one mistake this project cannot make.
		local, known := borrowedFS.Local.Known()
		switch {
		case known && !local:
			return fmt.Errorf("the pools are on %s, which is not local storage, so lending it would offer somebody else's capacity as compute-local", borrowedFS.Device)
		case !known:
			return fmt.Errorf("could not establish that the storage under %s is local, so it will not be lent; pass --capacity-bytes to override", cfg.BorrowedDir)
		}
		total, known := borrowedFS.TotalBytes.Known()
		if !known || total == 0 {
			return fmt.Errorf("could not measure the filesystem holding %s and no capacity was given, so there is nothing to manage", cfg.BorrowedDir)
		}
		acct.Capacity = pool.Bytes(total)

		switch {
		case haveDeliverable:
			if reserve := acct.Capacity - deliverable; reserve > acct.Reserved {
				// Raised rather than refused: the node is not misconfigured,
				// it is simply already using its disk, which is the normal
				// case.
				fmt.Printf("reserved raised to %s, the space this filesystem already holds for others\n", reserve)
				acct.Reserved = reserve
			}
		case acct.Reserved > 0:
			// The reserve could not be worked out, but an operator declaring
			// one is exactly what the refusal below asks for, so it has to be
			// honoured here or that advice is a dead end.
			fmt.Printf("keeping the declared compute reserve of %s: what this filesystem already holds for others could not be measured\n", acct.Reserved)
		default:
			return fmt.Errorf("could not work out how much of the filesystem holding %s is actually free, so the space already in use cannot be reserved; pass --reserved-bytes to declare it", cfg.BorrowedDir)
		}
	}
	if err := acct.Validate(); err != nil {
		return err
	}

	// The ceiling holds however capacity arrived. Discovery satisfies it by
	// construction, but an operator who passed --capacity-bytes has not been
	// checked against the disk at all, and the refusal that sends them here
	// asks for exactly that flag: without this, taking our own advice about
	// unprovable locality would switch off the guard against promising space
	// the filesystem does not have.
	if promised := acct.Capacity - acct.Reserved; haveDeliverable && promised > deliverable {
		return fmt.Errorf("this configuration promises %s but the filesystem holding %s can deliver %s; lower --capacity-bytes or raise --reserved-bytes",
			promised, cfg.BorrowedDir, deliverable)
	}

	a, rec, err := agent.Open(cfg, acct, time.Now())
	if err != nil {
		return err
	}
	defer a.Close()
	if rec.JournalRecovered != nil {
		// Recovered rather than fatal: everything the journal described is
		// regenerable, so the node started empty. Never silent, though.
		fmt.Fprintln(os.Stderr, "forebay-agent: recovered from a journal problem:", rec.JournalRecovered)
	}

	fmt.Println("forebay-agent", version.String())
	fmt.Printf("capacity %s  reserved %s  borrowed %s  free %s\n",
		acct.Capacity, acct.Reserved, a.Accounting().Borrowed, a.Accounting().Free())
	fmt.Printf("startup corrected: %d expired, %d no longer fit, %d orphan extents, %d leases without extents, %d interrupted reclaims, %d leftovers\n",
		len(rec.Expired), len(rec.Unfittable), len(rec.OrphanExtents), len(rec.LeasesWithoutExtents),
		len(rec.InvalidatedExtents), len(rec.Leftovers))
	if !agent.ReservesBlocks {
		// A development build sizes an extent without committing its blocks,
		// so the capacity it reports lending is not actually held. Saying so
		// is the difference between a caveat and a lie.
		fmt.Fprintln(os.Stderr, "warning: this build cannot reserve blocks, so borrowed capacity is not really held")
	}
	fmt.Fprintln(os.Stderr, "no serving path yet: see docs/rfcs/0007-fast-tier-data-path.md")
	if !*watch {
		return nil
	}
	return watchPressure(a, cfg, borrowedFS, *sysroot, *mountinfo, pool.Bytes(*headroom), *interval)
}

// watchPressure runs the agent until it is signalled, keeping free space above
// the headroom target.
//
// Free space is re-measured each pass rather than derived from the accounting,
// because the point is to catch writes by workloads that told nobody.
func watchPressure(a *agent.Agent, cfg agent.Config, fs topology.PoolStorage, sysroot, mountinfo string, headroom pool.Bytes, interval time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	free := func() (pool.Bytes, error) {
		s := topology.DescribePool(sysroot, mountinfo, cfg.BorrowedDir)
		available, ok := s.AvailableBytes.Known()
		if !ok {
			return 0, fmt.Errorf("the filesystem holding %s did not say how much is free", cfg.BorrowedDir)
		}
		return pool.Bytes(available), nil
	}

	device := fs.Device
	if device == "" {
		device = "an unidentified device"
	}
	fmt.Printf("watching %s, keeping %s free, polling every %s\n", device, headroom, interval)
	fmt.Fprintln(os.Stderr, "only free space is polled: the two inputs that would give warning before a workload writes need RFC-0014")
	report := func(t agent.Tick, err error) {
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, "forebay-agent: pass failed:", err)
		case t.Shortfall > 0:
			fmt.Printf("%s wanted %s back, reclaimed %s, still %s short: the node is now where it would be with no lending at all\n",
				t.Observed.Source, t.Observed.Need, t.Reclaimed, t.Shortfall)
		case t.Reclaimed > 0:
			fmt.Printf("%s wanted %s back, reclaimed %s\n", t.Observed.Source, t.Observed.Need, t.Reclaimed)
		}
	}
	if err := a.Watch(ctx, agent.WatchConfig{Headroom: headroom, Interval: interval}, free, report); err != nil {
		return err
	}
	fmt.Println("forebay-agent: stopped, lock released")
	return nil
}

// deliverableBytes is how much of the pools' filesystem Forebay can actually
// be given: what is free on it, plus what Forebay already holds there.
//
// It exists because a filesystem's size is not the agent's to lend. Measured
// on a real node, the disk was 1.83 TiB and 559 GiB of it was already held by
// the operating system, container images and other workloads. An agent that
// treats the total as its own offers half a terabyte that does not exist, and
// filling it fills the node's root filesystem, which takes the kubelet and
// every pod on the node down with it. Forebay must never leave a node worse
// off than a node without Forebay, and this is the arithmetic that keeps that
// true.
//
// What Forebay already holds is added back because that space is ours to hand
// out again. Free space alone would shrink the ceiling by everything currently
// lent, and a node would forget a little more of its own capacity on every
// restart.
//
// The second return is false when the filesystem did not say enough to work it
// out, which is a refusal on the discovery path and a warning on the override
// path, never a guess.
func deliverableBytes(fs topology.PoolStorage, pools ...string) (pool.Bytes, bool) {
	available, ok := fs.AvailableBytes.Known()
	if !ok {
		return 0, false
	}
	ours, ok := topology.OccupiedBytes(pools...).Known()
	if !ok {
		return 0, false
	}
	return pool.Bytes(available + ours), true
}

// reportTopology prints what the machine said about itself, including what it
// declined to say. An unknown is printed as unknown rather than as a plausible
// number, because a reader deciding whether placement can work needs to see
// which facts are missing.
func reportTopology(n topology.Node) {
	rack := "unknown"
	if r, ok := n.Rack.Known(); ok {
		rack = fmt.Sprintf("%s (%s)", r, n.Rack.Provenance())
	}
	numa := "unknown"
	if c, ok := n.NUMANodes.Known(); ok {
		numa = strconv.Itoa(c)
	}
	rdma := "unknown"
	if present, ok := n.RDMA.Known(); ok {
		rdma = strconv.FormatBool(present)
	}
	fmt.Printf("topology: rack %s, NUMA nodes %s, RDMA %s\n", rack, numa, rdma)

	for _, a := range n.Accelerators {
		affinity := "unknown"
		if v, ok := a.NUMA.Known(); ok {
			affinity = strconv.Itoa(v)
		}
		fmt.Printf("  accelerator %s  %s %s  NUMA %s\n", a.PCIAddress, a.VendorName, a.Device, affinity)
	}
	if len(n.Accelerators) == 0 {
		fmt.Println("  no accelerators identified")
	}
	for _, d := range n.Disks {
		note := ""
		if local, known := d.Local.Known(); !known {
			note = "  (locality unknown, not counted)"
		} else if !local {
			note = "  (not local, not counted)"
		}
		fmt.Printf("  disk %-12s %10s%s\n", d.Name, pool.Bytes(d.SizeBytes.Or(0)), note)
	}
}
