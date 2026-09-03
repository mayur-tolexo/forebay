// Command forebay-harm answers whether reclaiming borrowed capacity hurts the
// job that owns the node.
//
// RFC-0001's first kill criterion is that it must not. This runs a steady
// writer against the device, reclaims real leases underneath it through the
// agent's own path, and reports what the writer achieved before, during and
// after, because a stall inside a total cannot be seen.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
	"github.com/mayur-tolexo/forebay/internal/workload"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forebay-harm:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		borrowed = flag.String("borrowed-dir", "", "the pool directory, on the filesystem under test")
		journal  = flag.String("journal", "", "the lease journal")
		writeDir = flag.String("write-dir", "", "where the competing writer writes, defaulting to the pool's filesystem")
		capacity = flag.Int64("capacity-bytes", 0, "capacity the agent may lend")
		lend     = flag.Int64("lend-bytes", 4<<30, "how much to lend out and then take back")
		leases   = flag.Int("leases", 4, "how many leases that is spread across")
		block    = flag.Int64("write-block", 1<<20, "the competing writer's block, a multiple of 4096")
		limit    = flag.Int64("write-limit", 8<<30, "how large the writer's file grows before it rewrites from the start")
		interval = flag.Duration("interval", 200*time.Millisecond, "how often the writer banks progress")
		settle   = flag.Duration("settle", 5*time.Second, "how long the writer runs before anything is done to it")
		after    = flag.Duration("after", 5*time.Second, "how long it runs once the reclaim is done")
		writers  = flag.Int("writers", 1, "how many writers hold the device at once, since a device the workload does not saturate cannot show contention")
		control  = flag.Bool("control", false, "lend the capacity but never take it back, to see what the phases look like when nothing happened")
	)
	flag.Parse()

	switch {
	case *borrowed == "" || *journal == "":
		return fmt.Errorf("--borrowed-dir and --journal are required")
	case *capacity <= 0:
		return fmt.Errorf("--capacity-bytes is required")
	case *lend <= 0 || *leases <= 0:
		return fmt.Errorf("--lend-bytes and --leases must be positive")
	}
	if *writeDir == "" {
		*writeDir = *borrowed
	}

	now := time.Now()
	a, _, err := agent.Open(agent.Config{
		BorrowedDir: *borrowed,
		JournalPath: *journal,
		Lease: lease.Config{
			ReclaimWithin: time.Second,
		},
	}, pool.Accounting{Capacity: pool.Bytes(*capacity)}, now)
	if err != nil {
		return fmt.Errorf("opening the agent: %w", err)
	}
	defer a.Close()

	// Lent as several leases rather than one, since reclamation walks a ladder
	// and a single extent would measure one unlink.
	each := pool.Bytes(*lend / int64(*leases))
	for i := 0; i < *leases; i++ {
		id := fmt.Sprintf("harm-%d", i)
		// A term long enough that nothing expires during the run, since an
		// expiry would reclaim capacity this experiment means to take itself.
		if err := a.Grant(lease.Lease{ID: id, Class: lease.Opportunistic, Size: each, Term: time.Hour}, now); err != nil {
			return fmt.Errorf("granting %s: %w", id, err)
		}
	}
	fmt.Printf("lent %s across %d leases, on %s\n", pool.Bytes(*lend), *leases, *borrowed)
	fmt.Printf("%d writer(s), direct IO, so what is measured is the device rather than memory\n\n", *writers)

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		samples []workload.Sample
		err     error
	}
	done := make(chan outcome, *writers)
	for i := 0; i < *writers; i++ {
		go func(i int) {
			s, err := workload.Run(ctx, workload.Config{
				Dir: *writeDir, Name: fmt.Sprintf("workload-%d.dat", i),
				Block: *block, Interval: *interval, Limit: *limit,
			})
			done <- outcome{s, err}
		}(i)
	}

	start := time.Now()
	time.Sleep(*settle)

	// The real path, not an unlink: it chooses leases by class and ordinal,
	// invalidates each extent before removing it, and is timed against the
	// deadline the class promises.
	//
	// The control arm does everything else and skips this, since a phase table
	// with no run that had nothing done to it cannot tell an effect from the
	// spread the writer has anyway.
	var rec agent.Reclamation
	from := time.Since(start)
	to := from
	if !*control {
		var err error
		if rec, err = a.ReclaimCapacity(pool.Bytes(*lend), time.Now()); err != nil {
			cancel()
			<-done
			return fmt.Errorf("reclaiming: %w", err)
		}
		to = time.Since(start)
	}

	time.Sleep(*after)
	cancel()
	var all []workload.Sample
	for i := 0; i < *writers; i++ {
		got := <-done
		if got.err != nil {
			return got.err
		}
		all = append(all, got.samples...)
	}

	before, during, afterward := workload.Phases(all, from, to)

	// The agent's own measurement, taken on a monotonic clock inside the call.
	if *control {
		fmt.Printf("control: the capacity was lent and never taken back\n\n")
	} else {
		fmt.Printf("reclaimed %s in %s, with a shortfall of %s\n\n",
			rec.Result.Reclaimed, rec.Elapsed.Round(time.Microsecond), rec.Result.Shortfall)
	}
	fmt.Printf("%-12s %10s %12s %14s %10s\n", "phase", "intervals", "MiB/s", "worst write", "window")
	row := func(name string, s []workload.Sample, window time.Duration) {
		// Summed across writers, since one writer's median says nothing about
		// whether the device was busy.
		fmt.Printf("%-12s %10d %12.1f %14s %10s\n", name, len(s),
			workload.MedianRate(s)*float64(*writers),
			workload.WorstStall(s).Round(time.Microsecond), window.Round(time.Millisecond))
	}
	row("before", before, from)
	row("during", during, to-from)
	row("after", afterward, time.Since(start)-to)
	return nil
}
