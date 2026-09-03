// Command forebay-headroom measures how much free space the agent has to keep
// on top of what it lent, which RFC-0018 owns and nobody had measured.
//
// The watch refuses to run without that number, so an operator deploying the
// agent today supplies a guess. What decides it is how much a workload can take
// between the space going and the reclaim putting it back, so that is what this
// measures: the target is set just under the space that is already free, a
// writer takes a few gigabytes, and the deficit it opens is the answer.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
	"github.com/mayur-tolexo/forebay/internal/topology"
	"github.com/mayur-tolexo/forebay/internal/workload"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forebay-headroom:", err)
		os.Exit(1)
	}
}

// options is what a run needs, so the measurement can be driven without a
// command line and exercised without a device under it.
type options struct {
	Borrowed, Journal      string
	Capacity, Lend, Budget int64
	// TargetUnder is how far below free space the target sits, which is the
	// distance the writers cross before the watch owes a reclaim.
	TargetUnder              int64
	Leases, Writers          int
	Block                    int64
	Interval, Sample, RunFor time.Duration
}

// report is what a run found.
type report struct {
	Start, Target, Deficit pool.Bytes
	Below                  time.Duration
	Samples                int
	// Wrote is what the writers actually achieved, summed. A deficit means
	// nothing without it: the same watch looks faultless against a device
	// that has fallen to a tenth of its rate.
	Wrote float64
	// Took is how many bytes they took in total. Reaching the budget means
	// they stopped taking before the watch was done, so the deficit is this
	// run's configuration rather than the watch's response.
	Took pool.Bytes
}

func run() error {
	var (
		borrowed  = flag.String("borrowed-dir", "", "the pool directory")
		journal   = flag.String("journal", "", "the lease journal")
		sysroot   = flag.String("sysroot", "/", "root the filesystem is measured under")
		mountinfo = flag.String("mountinfo", "/proc/self/mountinfo", "where mounts are read from")
		capacity  = flag.Int64("capacity-bytes", 0, "capacity the agent may lend")
		lend      = flag.Int64("lend-bytes", 8<<30, "how much is lent out, and so available to reclaim")
		leases    = flag.Int("leases", 16, "how many leases that is spread across")
		budget    = flag.Int64("budget-bytes", 4<<30, "how much the writers take in total, which bounds what this costs the node and has to outlast the watch's response")
		under     = flag.Int64("target-under", 1<<30, "how far under the free space the target sits, which is the distance the writers cross before the watch is owed a reclaim")
		writers   = flag.Int("writers", 4, "how many writers take it at once")
		block     = flag.Int64("write-block", 1<<20, "one write, a multiple of 4096")
		interval  = flag.Duration("interval", time.Second, "how often the watch polls free space")
		sample    = flag.Duration("sample", 20*time.Millisecond, "how often this measures free space, which must be finer than the watch")
		runFor    = flag.Duration("run-for", 25*time.Second, "how long to run")
	)
	flag.Parse()

	o := options{
		Borrowed: *borrowed, Journal: *journal,
		Capacity: *capacity, Lend: *lend, Budget: *budget, TargetUnder: *under,
		Leases: *leases, Writers: *writers, Block: *block,
		Interval: *interval, Sample: *sample, RunFor: *runFor,
	}
	if err := checkFlags(o); err != nil {
		return err
	}
	free := func() (pool.Bytes, error) {
		s := topology.DescribePool(*sysroot, *mountinfo, o.Borrowed)
		available, ok := s.AvailableBytes.Known()
		if !ok {
			return 0, fmt.Errorf("the filesystem holding %s did not say how much is free", o.Borrowed)
		}
		return pool.Bytes(available), nil
	}

	got, err := measure(context.Background(), o, free)
	if err != nil {
		return err
	}
	return got.writeTo(os.Stdout, o)
}

// writeTo states the conditions before the number, since a headroom figure
// without the poll interval it was taken at is not a headroom figure.
func (r report) writeTo(w io.Writer, o options) error {
	if _, err := fmt.Fprintf(w, "%s free after lending %s, so the target is %s and %d writer(s) take %s\n",
		r.Start, pool.Bytes(o.Lend), r.Target, o.Writers, pool.Bytes(o.Budget)); err != nil {
		return err
	}
	fmt.Fprintf(w, "watch polls every %s, this samples every %s\n\n", o.Interval, o.Sample)
	fmt.Fprintf(w, "%-28s %s\n", "worst deficit below target", r.Deficit)
	fmt.Fprintf(w, "%-28s %s\n", "time spent below target", r.Below.Round(time.Millisecond))
	fmt.Fprintf(w, "%-28s %d\n", "samples", r.Samples)
	fmt.Fprintf(w, "%-28s %.0f MiB/s\n", "the writers achieved", r.Wrote)
	fmt.Fprintf(w, "%-28s %s of %s\n", "they took", r.Took, pool.Bytes(o.Budget))
	if r.Took >= pool.Bytes(o.Budget) {
		fmt.Fprintf(w, "\nthey reached the budget, so they stopped taking before the run ended and this\ndeficit is bounded by the budget rather than by the watch. Raise --budget-bytes.\n")
	}
	fmt.Fprintf(w, "\nheadroom has to cover what a workload takes before the watch puts it back,\nso on this device at this poll interval that is %s.\n", r.Deficit)
	return nil
}

// measure lends the capacity, puts the target just under what is free, and
// watches how far a workload drives it under before the watch puts it back.
//
// free is read from the watch and from the tracker at once, so it has to be
// safe for concurrent use.
func measure(ctx context.Context, o options, free func() (pool.Bytes, error)) (report, error) {
	now := time.Now()
	a, _, err := agent.Open(agent.Config{
		BorrowedDir: o.Borrowed,
		JournalPath: o.Journal,
		Lease:       lease.Config{ReclaimWithin: time.Second},
	}, pool.Accounting{Capacity: pool.Bytes(o.Capacity)}, now)
	if err != nil {
		return report{}, fmt.Errorf("opening the agent: %w", err)
	}
	defer a.Close()

	each := pool.Bytes(o.Lend / int64(o.Leases))
	for i := 0; i < o.Leases; i++ {
		id := fmt.Sprintf("headroom-%d", i)
		if err := a.Grant(lease.Lease{ID: id, Class: lease.Elastic, Size: each, Term: time.Hour}, now); err != nil {
			return report{}, fmt.Errorf("granting %s: %w", id, err)
		}
	}

	start, err := free()
	if err != nil {
		return report{}, err
	}
	target, err := targetFor(start, o.TargetUnder)
	if err != nil {
		return report{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, o.RunFor)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Reported into nothing but the pass itself: what is measured here is
		// free space, and printing every pass would compete with the writers.
		_ = a.Watch(ctx, agent.WatchConfig{Headroom: target, Interval: o.Interval}, free, func(agent.Tick, error) {})
	}()

	rates := make(chan float64, o.Writers)
	taken := make(chan int64, o.Writers)
	for i := 0; i < o.Writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Limit is the budget each writer owns, so the file grows to that
			// and is then rewritten, which stops the cost growing with time.
			s, _ := workload.Run(ctx, workload.Config{
				Dir: o.Borrowed, Name: fmt.Sprintf("taker-%d.dat", i),
				Block: o.Block, Interval: 200 * time.Millisecond, Limit: o.Budget / int64(o.Writers),
			})
			var took int64
			for _, x := range s {
				took += x.Bytes
			}
			rates <- workload.MedianRate(s)
			taken <- took
		}(i)
	}

	deficit, below, samples := track(ctx, free, target, o.Sample)
	wg.Wait()
	close(rates)
	close(taken)
	var wrote float64
	for r := range rates {
		wrote += r
	}
	var took int64
	for t := range taken {
		took += t
	}
	return report{Start: start, Target: target, Deficit: deficit, Below: below,
		Samples: samples, Wrote: wrote, Took: pool.Bytes(took)}, nil
}

// targetFor puts the line a fixed distance under what is free.
//
// Its own knob rather than a share of the budget: the distance decides how soon
// the writers cross, and the budget decides how long they keep taking after
// that. Tying them together makes the experiment unrunnable on a slow device,
// where the crossing never arrives before the budget is spent.
//
// Refused when it would put the line at or below zero, since the watch would
// reject that as no headroom and report a configuration error rather than a
// filesystem with less free space than was asked for.
func targetFor(start pool.Bytes, under int64) (pool.Bytes, error) {
	target := start - pool.Bytes(under)
	if target <= 0 {
		return 0, fmt.Errorf("a target %s under the %s that is free leaves nothing",
			pool.Bytes(under), start)
	}
	return target, nil
}

// checkFlags rejects a configuration that cannot measure what it claims to.
func checkFlags(o options) error {
	switch {
	case o.Borrowed == "" || o.Journal == "":
		return fmt.Errorf("--borrowed-dir and --journal are required")
	case o.Capacity <= 0 || o.Lend <= 0 || o.Budget <= 0 || o.TargetUnder <= 0:
		return fmt.Errorf("--capacity-bytes, --lend-bytes, --budget-bytes and --target-under must be positive")
	case o.TargetUnder >= o.Budget:
		return fmt.Errorf("a target %s under free space is not crossed by writers taking %s in total",
			pool.Bytes(o.TargetUnder), pool.Bytes(o.Budget))
	case o.Leases <= 0:
		return fmt.Errorf("--leases must be positive")
	case o.Lend > o.Capacity:
		return fmt.Errorf("lending %s of %s is more than the node has",
			pool.Bytes(o.Lend), pool.Bytes(o.Capacity))
	case o.Sample >= o.Interval:
		return fmt.Errorf("sampling every %s cannot see a watch that polls every %s", o.Sample, o.Interval)
	}
	return nil
}

// track samples free space until ctx is done, reporting how far below the
// target it fell and for how long.
//
// Sampled far more often than the watch polls, since the deficit is opened and
// closed between two of the watch's own passes and a sampler at the watch's
// rate would report the number it wants to hear.
func track(ctx context.Context, free func() (pool.Bytes, error), target pool.Bytes, every time.Duration) (pool.Bytes, time.Duration, int) {
	tick := time.NewTicker(every)
	defer tick.Stop()

	var (
		worst  pool.Bytes
		below  time.Duration
		count  int
		lastAt time.Time
	)
	for {
		select {
		case <-ctx.Done():
			return worst, below, count
		case at := <-tick.C:
			got, err := free()
			if err != nil {
				continue
			}
			count++
			if got < target {
				if d := target - got; d > worst {
					worst = d
				}
				if !lastAt.IsZero() {
					below += at.Sub(lastAt)
				}
				lastAt = at
			} else {
				lastAt = time.Time{}
			}
		}
	}
}
