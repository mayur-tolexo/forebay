// Command forebay-agent takes charge of one node's capacity: it owns the pool
// directories, holds the lock that makes it the only writer, replays the lease
// journal and reconciles it against the disk.
//
// It does not serve data yet. The read path is not built, so the agent starts,
// reports what it found, and exits.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
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
		donated     = flag.String("donated-dir", "", "directory holding durable data, never reclaimed")
		journal     = flag.String("journal", "", "path to the lease journal")
		capacity    = flag.Int64("capacity-bytes", 0, "total capacity of the device")
		compute     = flag.Int64("compute-bytes", 0, "capacity reserved for the workload on this node")
		donatedSize = flag.Int64("donated-bytes", 0, "capacity permanently given to durable storage")
		reclaim     = flag.Duration("reclaim-within", 30*time.Second, "how long an elastic lease may take to return capacity")
		sysroot     = flag.String("sysroot", "/", "filesystem root to discover hardware from")
		rack        = flag.String("rack", "", "this node's rack, which cannot be discovered and must be declared")
		mountinfo   = flag.String("mountinfo", "/proc/self/mountinfo", "mount table used to find the device under the pools")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("forebay-agent", version.String())
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
		DonatedDir:  *donated,
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
	donatedFS := topology.DescribePool(*sysroot, *mountinfo, cfg.DonatedDir)

	if err := requireOneFilesystem(borrowedFS, donatedFS); err != nil {
		return err
	}

	// What this filesystem can actually hand to Forebay, worked out once and
	// used by both paths below.
	deliverable, haveDeliverable := deliverableBytes(borrowedFS, cfg.BorrowedDir, cfg.DonatedDir)

	// Capacity comes from the filesystem the pools actually live on, not from
	// summing every device. A node with four drives and pools on one of them
	// can lend what that one holds.
	acct := pool.Accounting{
		Capacity: pool.Bytes(*capacity),
		Compute:  pool.Bytes(*compute),
		Donated:  pool.Bytes(*donatedSize),
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
			if reserve := acct.Capacity - deliverable; reserve > acct.Compute {
				// Raised rather than refused: the node is not misconfigured,
				// it is simply already using its disk, which is the normal
				// case.
				fmt.Printf("compute reserve raised to %s, the space this filesystem already holds for others\n", reserve)
				acct.Compute = reserve
			}
		case acct.Compute > 0:
			// The reserve could not be worked out, but an operator declaring
			// one is exactly what the refusal below asks for, so it has to be
			// honoured here or that advice is a dead end.
			fmt.Printf("keeping the declared compute reserve of %s: what this filesystem already holds for others could not be measured\n", acct.Compute)
		default:
			return fmt.Errorf("could not work out how much of the filesystem holding %s is actually free, so the space already in use cannot be reserved; pass --compute-bytes to declare it", cfg.BorrowedDir)
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
	if promised := acct.Capacity - acct.Compute - acct.Donated; haveDeliverable && promised > deliverable {
		return fmt.Errorf("this configuration promises %s but the filesystem holding %s can deliver %s; lower --capacity-bytes or --donated-bytes, or raise --compute-bytes",
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
	fmt.Printf("capacity %s  compute %s  donated %s  borrowed %s  free %s\n",
		acct.Capacity, acct.Compute, acct.Donated, a.Accounting().Borrowed, a.Accounting().Free())
	fmt.Printf("startup corrected: %d expired, %d no longer fit, %d orphan extents, %d leases without extents\n",
		len(rec.Expired), len(rec.Unfittable), len(rec.OrphanExtents), len(rec.LeasesWithoutExtents))
	fmt.Fprintln(os.Stderr, "no serving path yet: see docs/rfcs/0007-fast-tier-data-path.md")
	return nil
}

// requireOneFilesystem refuses a configuration whose two pools sit on
// different devices.
//
// One capacity figure is kept for both pools, so one filesystem has to hold
// both. Split them and the donated bytes are deducted from a device that never
// held them, while the device that did is never measured at all. Refusing is
// better than keeping books that cannot be right.
//
// A device that could not be identified is not a mismatch. On the discovery
// path an unidentified device has already been refused for not being provably
// local, and on the override path the operator has taken the numbers on.
func requireOneFilesystem(borrowed, donated topology.PoolStorage) error {
	b, d := borrowed.Device, donated.Device
	if b == "" || d == "" || b == d {
		return nil
	}
	return fmt.Errorf("the borrowed pool is on %s and the donated pool is on %s, and one capacity figure cannot describe both; put them on the same filesystem", b, d)
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
