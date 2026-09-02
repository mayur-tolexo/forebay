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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/kubelet"
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
		kubeletHost = flag.String("kubelet-host", "", "this node's address, to read pods bound to it. Without one the watch is reactive")
		kubeletPort = flag.Int("kubelet-port", 10250, "the kubelet's port")
		tokenFile   = flag.String("kubelet-token-file", "", "service account token for the kubelet, defaulting to the pod's own")
		kubeletRoot = flag.String("kubelet-root", "/var/lib/kubelet", "the kubelet's directory, used to check pods are charged against the filesystem the pools are on")
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
	sources := optionalSources(kubeletOptions{
		Host:        *kubeletHost,
		Port:        *kubeletPort,
		TokenFile:   *tokenFile,
		Root:        *kubeletRoot,
		BorrowedDir: cfg.BorrowedDir,
		Pools:       borrowedFS,
	})
	return watchPressure(a, cfg, borrowedFS, *sysroot, *mountinfo, pool.Bytes(*headroom), *interval, sources...)
}

// kubeletOptions is what the pod input needs to reach the kubelet and to prove
// it is watching the filesystem the pools are on.
//
// A struct rather than five positional strings: transposing the kubelet's
// directory and the pool's would compile and compare the wrong pair of paths,
// which is the check this has already been got wrong once.
type kubeletOptions struct {
	Host        string
	Port        int
	TokenFile   string
	Root        string
	BorrowedDir string
	Pools       topology.PoolStorage
}

// optionalSources builds the inputs the watch can have on top of free space.
//
// The pod input is proved once here so that a healthy start is quiet. Leaving
// it all to the background would mean the first pass always found it
// unfinished, and an operator who sees a degraded input on every restart
// learns to skim past the one that matters.
func optionalSources(opts kubeletOptions) []agent.Source {
	if opts.Host == "" {
		return nil
	}
	p := &pendingSource{opts: opts}
	p.attempt(startupProbe)
	return []agent.Source{p}
}

// buildTimeout is what proving the pod input gets, which is deliberately more
// than a watch pass would give it.
//
// The check reads the node filesystem from the kubelet, and on a node with
// hundreds of pods that response is large. Bounding it by the pass would tie
// the budget to how often the watch polls, and a first attempt that could not
// finish in time would retry for ever against the same too-small budget.
const buildTimeout = 15 * time.Second

// startupProbe is what the first attempt gets, before the watch begins.
//
// Short, because the agent should not wait on the kubelet to start guarding
// the node, and it does not have to: a first attempt that does not finish in
// time costs only that the pod input starts a pass or two later. Long enough
// that it will normally succeed, since the check measured 121ms to 247ms
// against a real kubelet.
const startupProbe = 2 * time.Second

// pendingSource is the pod input before it has been proved usable.
//
// Building it needs the kubelet, and the kubelet is not always there when the
// agent starts: on a reboot the two come up together and the agent may well be
// first. Exiting would leave the node unwatched through exactly the minutes
// when image pulls are filling the disk, and building it once and giving up
// would mean a kubelet that arrives a second late is never used.
//
// So the attempt is made in the background and repeated until it succeeds,
// off the watch's own timing. A pass never waits on the kubelet handshake, and
// a failure is reported the way any other source failure is, which is what
// stops a missing input from looking like a quiet cluster: an input that was
// asked for and is not working says so on every pass, rather than once at
// startup and never again.
type pendingSource struct {
	opts kubeletOptions

	// mu guards the rest. The watch asks its sources from one goroutine, but
	// the attempts run on another, and Step is exported to anyone who wants
	// to drive a pass themselves.
	mu       sync.Mutex
	built    agent.Source
	lastErr  error
	building bool
}

// Name says which observation this is, whether or not it is working yet.
func (p *pendingSource) Name() string { return "pod requests" }

// Observe asks the source if there is one, and otherwise reports why not
// while an attempt to build one runs behind it.
func (p *pendingSource) Observe(ctx context.Context, cfg agent.WatchConfig, available pool.Bytes) (pool.Bytes, error) {
	p.mu.Lock()
	built, lastErr := p.built, p.lastErr
	if built == nil && !p.building {
		p.building = true
		go p.attempt(buildTimeout)
	}
	p.mu.Unlock()

	switch {
	case built != nil:
		return built.Observe(ctx, cfg, available)
	case lastErr != nil:
		return 0, lastErr
	default:
		return 0, errors.New("still checking the kubelet is watching the filesystem the pools are on")
	}
}

// attempt proves the input usable, on its own budget rather than the watch's.
func (p *pendingSource) attempt(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	src, err := podSource(ctx, p.opts)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.building = false
	p.built, p.lastErr = src, err
}

// podSource builds the pod input, refusing one that would be watching a
// different filesystem from the one the pools are on.
//
// Pods are charged for ephemeral storage against the kubelet's filesystem. If
// the pools live on a second device, a pod writing everything it asked for
// takes nothing from Forebay, and acting on that signal would reclaim a job's
// cache for pressure that cannot reach it. The two filesystems are told apart
// by size, which is checked once here rather than every pass.
func podSource(ctx context.Context, opts kubeletOptions) (agent.Source, error) {
	// Identity first, and first in the order it runs as well as in the order
	// it is trusted. Sizes match on any two drives of the same model, which
	// is what a GPU node usually has, so a size check alone can pass on the
	// wrong device. Two stat calls settle it, and settling it here means a
	// node whose pools are on another device refuses without ever asking the
	// kubelet: this runs on every pass, and that answer is never going to
	// change.
	same, unreachable := topology.SameFilesystem(opts.Root, opts.BorrowedDir)
	if unreachable == nil && !same {
		return nil, fmt.Errorf("%s and %s are on different filesystems, so what pods are charged for says nothing about the space Forebay lends", opts.Root, opts.BorrowedDir)
	}

	token, err := kubelet.TokenFromFile(opts.TokenFile)
	if err != nil {
		return nil, err
	}
	client := kubelet.New(opts.Host, opts.Port, token)
	capacity, _, err := client.NodeFS(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not check the kubelet is watching the same filesystem as the pools: %w", err)
	}

	switch {
	case unreachable == nil:
		fmt.Println("pods are charged against the filesystem the pools are on, checked by device")
	default:
		// Falling back rather than refusing, because the agent not being able
		// to see the kubelet's directory is a packaging detail and not
		// evidence about the device. Size is the weaker answer and is
		// reported as one, so nobody reads it as identity.
		total, ok := opts.Pools.TotalBytes.Known()
		if !ok {
			return nil, fmt.Errorf("the filesystem holding the pools could not be measured and %w, so there is no way to tell pods are charged against it", unreachable)
		}
		if int64(total) != capacity {
			// Exact bytes as well as the rounded size, because two
			// filesystems that differ by less than the display precision
			// otherwise produce a refusal naming the same size twice. The
			// unreachable path is named too, so it is clear the stronger
			// check did not run.
			return nil, fmt.Errorf("%w, so the device could not be compared; and the kubelet charges pods against a %s filesystem (%d bytes) while the pools are on a %s one (%d bytes), so pod requests say nothing about the space Forebay lends",
				unreachable, pool.Bytes(capacity), capacity, pool.Bytes(total), total)
		}
		fmt.Printf("%v, so pods are only known to be charged against a filesystem of the same size as the pools', not the same one\n", unreachable)
	}
	return kubelet.NewSource(client, capacity)
}

// watchPressure runs the agent until it is signalled, keeping free space above
// the headroom target.
//
// Free space is re-measured each pass rather than derived from the accounting,
// because the point is to catch writes by workloads that told nobody.
func watchPressure(a *agent.Agent, cfg agent.Config, fs topology.PoolStorage, sysroot, mountinfo string, headroom pool.Bytes, interval time.Duration, sources ...agent.Source) error {
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
	if len(sources) == 0 {
		fmt.Fprintln(os.Stderr, "only free space is polled, so pressure is noticed once the space has gone: pass --kubelet-host to watch pods too")
	} else {
		for _, s := range sources {
			fmt.Printf("also watching %s\n", s.Name())
		}
	}
	// A source that stays broken would otherwise repeat itself every pass, so
	// only a change is worth printing. It is repeated when it recovers, which
	// is the transition an operator is waiting for.
	var lastDegraded string
	report := func(t agent.Tick, err error) {
		if d := strings.Join(t.Degraded, "; "); d != lastDegraded {
			switch {
			case d == "":
				fmt.Println("all inputs readable again")
			default:
				fmt.Fprintln(os.Stderr, "forebay-agent: degraded, continuing on what is left:", d)
			}
			lastDegraded = d
		}
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
	if err := a.Watch(ctx, agent.WatchConfig{Headroom: headroom, Interval: interval}, free, report, sources...); err != nil {
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
