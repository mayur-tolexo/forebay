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
	"time"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
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
	acct := pool.Accounting{
		Capacity: pool.Bytes(*capacity),
		Compute:  pool.Bytes(*compute),
		Donated:  pool.Bytes(*donatedSize),
	}
	if err := acct.Validate(); err != nil {
		return err
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
