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
	"net"
	"net/http"
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
	"github.com/mayur-tolexo/forebay/internal/leaseapi"
	"github.com/mayur-tolexo/forebay/internal/metrics"
	"github.com/mayur-tolexo/forebay/internal/pool"
	"github.com/mayur-tolexo/forebay/internal/prefetch"
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
		showVersion    = flag.Bool("version", false, "print the build identity and exit")
		borrowed       = flag.String("borrowed-dir", "", "directory holding capacity lent revocably")
		journal        = flag.String("journal", "", "path to the lease journal")
		capacity       = flag.Int64("capacity-bytes", 0, "total capacity of the device")
		reserved       = flag.Int64("reserved-bytes", 0, "capacity held for everything that is not Forebay, measured when not given")
		reclaim        = flag.Duration("reclaim-within", 30*time.Second, "how long an elastic lease may take to return capacity")
		tenantCeiling  = flag.Int64("tenant-borrowed-bytes", 0, "the most capacity one tenant may hold on this node, of any class. Zero is unbounded, which is what a node serving one tenant wants")
		stagingCeiling = flag.Int64("tenant-guaranteed-bytes", 0, "the most of this node's guaranteed share one tenant may reserve for checkpoint staging. Bounded separately and more tightly, since guaranteed capacity denies itself to everyone else by construction")
		sysroot        = flag.String("sysroot", "/", "filesystem root to discover hardware from")
		rack           = flag.String("rack", "", "this node's rack, which cannot be discovered and must be declared")
		mountinfo      = flag.String("mountinfo", "/proc/self/mountinfo", "mount table used to find the device under the pools")
		drain          = flag.Bool("drain", false, "return what this node lent and exit, so it can be upgraded. Exits non-zero if something is still held, since a rolling upgrade should stop rather than read a log line")
		autonomy       = flag.Bool("autonomy", true, "let the node adapt what it may adapt, which today is its post-reclaim cooldown. Turning it off holds the configured values and stops nothing the node promised: it still reclaims and still expires")
		readySlow      = flag.Duration("ready-slow", 2*time.Second, "a read at or over this makes the node report itself not ready, since a node that is slow rather than dead keeps taking work nobody else can see it failing")
		readyWell      = flag.Duration("ready-recovered", 500*time.Millisecond, "reads must come back under this before the node reports itself ready again. Two bounds rather than one, or a marginal node flaps between ready and not")
		readyWindow    = flag.Duration("ready-window", 30*time.Second, "how far back readiness looks")
		liveness       = flag.Bool("liveness", false, "check whether the agent owning the pool is still making progress, and exit non-zero if not")
		staleAfter     = flag.Duration("stale-after", 60*time.Second, "how long without progress means the agent is wedged")
		watch          = flag.Bool("watch", false, "stay running, keeping free space above the headroom target")
		headroom       = flag.Int64("headroom-bytes", 0, "free space the agent keeps on top of what is committed, as a fixed size")
		headroomFor    = flag.Duration("headroom-for", 0, "how long the node may be behind, which the agent turns into bytes against the rate the workload is writing at. The measured form: a size set while a drive is fast is wrong once its cache is spent")
		minHeadroom    = flag.Int64("headroom-min-bytes", 0, "the floor under the floor, required with --headroom-for, since a node writing nothing would otherwise keep nothing free")
		interval       = flag.Duration("watch-interval", 10*time.Second, "how often free space is polled")
		kubeletHost    = flag.String("kubelet-host", "", "this node's address, to read pods bound to it. Without one the watch is reactive")
		kubeletPort    = flag.Int("kubelet-port", 10250, "the kubelet's port")
		tokenFile      = flag.String("kubelet-token-file", "", "service account token for the kubelet, defaulting to the pod's own")
		kubeletRoot    = flag.String("kubelet-root", "/var/lib/kubelet", "the kubelet's directory, used to check pods are charged against the filesystem the pools are on")
		metricsAddr    = flag.String("metrics-addr", "", "address to serve metrics on, which is an operator surface and carries tenant names, so it binds where only a scrape reaches it")
		leaseTokenFile = flag.String("lease-token-file", "", "file holding the token a control plane must present to propose or release a lease. Without one the lease endpoints are not served at all, since a node whose capacity anyone on the network can claim is a node anyone can fill")
		serveSocket    = flag.String("serve-socket", "", "path to listen on for reads, which is how something that speaks a storage protocol asks this agent for bytes")
		backendDir     = flag.String("backend-dir", "", "directory the durable backend serves objects from, read through the file driver")
		s3Endpoint     = flag.String("backend-s3-endpoint", "", "scheme and host of an S3-compatible durable backend, instead of --backend-dir. Credentials come from "+accessKeyEnv+" and "+secretKeyEnv)
		s3Bucket       = flag.String("backend-s3-bucket", "", "bucket the S3 backend serves objects from")
		s3Region       = flag.String("backend-s3-region", "", "region the S3 backend signs for, defaulting to us-east-1")
		tierBytes      = flag.Int64("tier-bytes", 0, "capacity to hold the fast tier, granted to this agent by itself in the absence of a control plane")
		blockBytes     = flag.Int64("tier-block-bytes", 1<<20, "the unit the fast tier is keyed in")
		firstReads     = flag.Int("tier-first-reads", 1<<16, "how many first reads are remembered, which decides whether admission on the second read fires at all")
		doPrefetch     = flag.Bool("prefetch", false, "predict what a reader will ask for next and fetch it ahead. Off by default, since the numbers below are guesses and a wrong prediction spends bandwidth on a node whose bandwidth feeds an accelerator")
		pfDepth        = flag.Int("prefetch-depth", prefetch.DefaultConfig().Depth, "how many blocks ahead of a confirmed reader to fetch")
		pfFloor        = flag.Float64("prefetch-accuracy", prefetch.DefaultConfig().MinAccuracy, "the share of recent predictions that must have been read for a stream to keep being predicted")
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
	leaseCfg.Autonomy = *autonomy
	leaseCfg.Quota = lease.Quota{
		Borrowed:   pool.Bytes(*tenantCeiling),
		Guaranteed: pool.Bytes(*stagingCeiling),
	}

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
	if *drain {
		return drainNode(a, time.Now())
	}

	// Registered before anything records, so a scrape of an idle agent still
	// shows the series exist: a metric that appears only once something
	// happened cannot be alerted on for not happening.
	reg := metrics.New()
	if err := metrics.Node(reg); err != nil {
		return err
	}
	ready, err := metrics.NewReadiness(*readySlow, *readyWell, *readyWindow)
	if err != nil {
		return err
	}

	var reads *serving
	if *serveSocket == "" {
		fmt.Fprintln(os.Stderr, "not serving: pass --serve-socket and a backend to answer reads")
	} else {
		var err error
		reads, err = serveReads(a, servingOptions{
			Socket: *serveSocket,
			Backend: backendOptions{
				Dir:      *backendDir,
				Endpoint: *s3Endpoint,
				Bucket:   *s3Bucket,
				Region:   *s3Region,
			},
			TierBytes:  pool.Bytes(*tierBytes),
			BlockBytes: *blockBytes,
			FirstReads: *firstReads,
			Metrics:    reg,
			Ready:      ready,
			Prefetch:   prefetchConfig(*doPrefetch, *pfDepth, *pfFloor),
		})
		if err != nil {
			return err
		}
		defer reads.stop()
	}
	if !*watch {
		if *serveSocket == "" {
			return nil
		}
		// Serving is a reason to stay up. Without this the agent opened the
		// socket, said so, and exited, taking the socket with it: a caller
		// that read the line and then dialled found nothing there.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		fmt.Println("forebay-agent: stopped, no longer answering reads")
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
	// Read before anything is served, so a node configured with a token file
	// it cannot read refuses to start rather than serving a lease endpoint
	// nobody can reach and nobody notices is unreachable.
	token, err := leaseToken(*leaseTokenFile)
	if err != nil {
		return err
	}
	if *metricsAddr != "" {
		stopMetrics, _, err := serveMetrics(*metricsAddr, reg, ready, reads, a, token)
		if err != nil {
			return err
		}
		defer stopMetrics()
	}

	watchCfg := agent.WatchConfig{
		Headroom:    pool.Bytes(*headroom),
		HeadroomFor: *headroomFor,
		MinHeadroom: pool.Bytes(*minHeadroom),
		Interval:    *interval,
	}
	return watchPressure(a, cfg, borrowedFS, *sysroot, *mountinfo, watchCfg, reads, reg, sources...)
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

// cacheGivenUp names what reclamation took from the tier, when it took any.
// Reported here rather than as it happens, since that is inside the window the
// reclaim deadline is measured over.
func cacheGivenUp(sv *serving, last *int64) string {
	if sv == nil {
		return ""
	}
	now := sv.Dropped()
	if now == *last {
		return ""
	}
	n := now - *last
	*last = now
	return fmt.Sprintf(", giving up %d cached blocks", n)
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
func watchPressure(a *agent.Agent, cfg agent.Config, fs topology.PoolStorage, sysroot, mountinfo string, watchCfg agent.WatchConfig, sv *serving, reg *metrics.Registry, sources ...agent.Source) error {
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
	fmt.Printf("watching %s, keeping %s free, polling every %s\n", device, describeHeadroom(watchCfg), watchCfg.Interval)
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
	var lastDropped int64
	report := func(t agent.Tick, err error) {
		// Recorded first and on every pass, whether or not anything happened.
		// A watch that has died and a cluster where nothing is happening
		// produce the same absence of events, and this counter is what tells
		// them apart: the alert is on a number that must move.
		record(reg, t)
		// The tier's own numbers are gauges, so they are read on the pass
		// rather than written on every block: what they describe is a level
		// rather than an event, and sampling a level at the watch's rate is
		// what a gauge is.
		recordTier(reg, sv)

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
			// The count belongs here most of all: this is the pass that could
			// not meet demand, and having emptied the cache trying is the
			// part an operator needs.
			fmt.Printf("%s wanted %s back, reclaimed %s%s, still %s short: the node is now where it would be with no lending at all\n",
				t.Observed.Source, t.Observed.Need, t.Reclaimed, cacheGivenUp(sv, &lastDropped), t.Shortfall)
		case t.Reclaimed > 0:
			// The target is printed with it when it moves, since the same free
			// space is enough one pass and short the next, and a reclaim
			// nobody can account for is one an operator turns off.
			fmt.Printf("%s wanted %s back, reclaimed %s%s%s\n",
				t.Observed.Source, t.Observed.Need, t.Reclaimed, cacheGivenUp(sv, &lastDropped),
				movingTarget(watchCfg, t))
		}
	}
	if err := a.Watch(ctx, watchCfg, free, report, sources...); err != nil {
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
		// Present and up are different answers, and an operator reading this
		// to decide whether a fast transport is available needs the second.
		if present {
			switch active, known := n.RDMAActive.Known(); {
			case !known:
				rdma += " (link state unknown)"
			case active:
				rdma += " (link up)"
			default:
				rdma += " (no port up)"
			}
		}
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

// describeHeadroom says which floor is being kept, since a duration and a size
// are the same field to an operator reading one line of output.
func describeHeadroom(cfg agent.WatchConfig) string {
	if cfg.HeadroomFor > 0 {
		return fmt.Sprintf("%s of writing, at least %s", cfg.HeadroomFor, cfg.MinHeadroom)
	}
	return cfg.Headroom.String()
}

// movingTarget names the floor and the rate behind it, and says nothing when
// the floor is a fixed size that the operator already knows.
func movingTarget(cfg agent.WatchConfig, t agent.Tick) string {
	if cfg.HeadroomFor <= 0 {
		return ""
	}
	return fmt.Sprintf(", keeping %s for %s of writing at %s/s",
		t.Target, cfg.HeadroomFor, pool.Bytes(t.Rate))
}

// serveMetrics starts the scrape endpoint, returning how to stop it.
//
// Out of the IO path entirely: a read does not consult it and does not stop
// when it stops, which is why a failure here is reported and does not end the
// agent.
// leaseToken reads the token a control plane must present, trimming the
// newline a file written by an operator or mounted from a secret carries.
func leaseToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the lease token: %w", err)
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		// Refused rather than treated as absent. An empty token file is an
		// operator who meant to set one, and serving the endpoint open would
		// be the opposite of what they asked for.
		return "", fmt.Errorf("the lease token in %s is empty", path)
	}
	return token, nil
}

// serveMetrics returns where it is listening as well as how to stop it. A
// caller that asked for port zero cannot otherwise find out.
func serveMetrics(addr string, reg *metrics.Registry, ready *metrics.Readiness, reads *serving, a *agent.Agent, token string) (func(), string, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("serving metrics on %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	// On the same listener as the metrics, because an orchestrator that could
	// reach one and not the other would draw a conclusion from the wrong
	// silence.
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		if ok, why := ready.Ready(time.Now()); !ok {
			// The reason on the body, since a probe failure with no reason
			// sends an operator to the logs of a node that is answering.
			http.Error(w, why, http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ready")
	})
	// Only when there is a read path. A node that serves nothing holds
	// nothing, and an endpoint answering an empty list would be indistinguish-
	// able from one whose tier is genuinely empty.
	if reads != nil {
		mux.Handle("/residency", residencyHandler(reads.residency))
	}
	// Served only when a token is set. A lease endpoint that granted disk to
	// anything that could reach the port would hand a node's capacity to the
	// first thing on the network to ask.
	if token != "" {
		mux.Handle("/leases", leaseapi.Handler(a, token))
		mux.Handle("/leases/", leaseapi.Handler(a, token))
		mux.Handle("/capacity", leaseapi.Handler(a, token))
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "forebay-agent: metrics stopped:", err)
		}
	}()
	fmt.Printf("serving metrics on %s/metrics and readiness on %s/ready\n", l.Addr(), l.Addr())
	if reads != nil {
		fmt.Printf("reporting residency on %s/residency, which a controller turns into node labels\n", l.Addr())
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "not accepting leases: pass --lease-token-file for a control plane to propose them")
	} else {
		fmt.Printf("accepting lease proposals on %s/leases\n", l.Addr())
	}
	return func() { srv.Close() }, l.Addr().String(), nil
}

// prefetchConfig turns the flags into a configuration, or into nothing.
//
// Nil rather than a zeroed configuration, since the data server reads nil as
// off and a zeroed one as a configuration it must refuse.
func prefetchConfig(on bool, depth int, floor float64) *prefetch.Config {
	if !on {
		return nil
	}
	cfg := prefetch.DefaultConfig()
	cfg.Depth, cfg.MinAccuracy = depth, floor
	return &cfg
}

// recordTier publishes what the tier holds and whether it is earning it.
//
// A node that is not serving publishes no sample for them, rather than zeros.
// The series are still declared, since RFC-0017 registers the whole set up
// front, but a gauge nobody has set emits no value: a node with no tier has
// not saved nothing, it has no tier, and those are different answers.
func recordTier(reg *metrics.Registry, sv *serving) {
	if sv == nil {
		return
	}
	_ = reg.Set(metrics.TierBytes, nil, float64(sv.Resident()))
	saved, covered := sv.Saved()
	_ = reg.Set(metrics.TierSavedSeconds, nil, saved.Seconds())
	_ = reg.Set(metrics.TierSavingCover, nil, covered)
}

// record puts one watch pass into the metrics.
//
// Errors are dropped rather than returned. A metric that could not be recorded
// must not stop a reclaim: the watch's job is giving compute its disk back,
// and failing that to report a number would be the wrong way round.
func record(reg *metrics.Registry, t agent.Tick) {
	if reg == nil {
		return
	}
	_ = reg.Add(metrics.WatchPasses, nil, 1)
	_ = reg.Set(metrics.HeadroomBytes, nil, float64(t.Target))
	if t.Reclaimed > 0 {
		// Labelled by the class that was actually taken, since only elastic
		// promises a deadline and reading both against it would judge
		// opportunistic capacity by a promise it never made.
		class := "opportunistic"
		if t.Bounded {
			class = "elastic"
		}
		_ = reg.Observe(metrics.ReclaimSeconds, metrics.Labels{"class": class}, t.Elapsed.Seconds())
	}
	if t.Shortfall > 0 {
		_ = reg.Add(metrics.ReclaimShortfall, nil, float64(t.Shortfall))
	}
}

// drainNode returns what this node lent, so it can be upgraded.
//
// An upgrade is a crash whose moment the operator chose, and everything a
// restart needs already exists. What drain adds is not waiting for terms to
// expire, which are hours, when somebody is patching a security hole.
func drainNode(a *agent.Agent, now time.Time) error {
	before := a.Accounting().Borrowed
	if before == 0 {
		fmt.Println("nothing lent, so nothing to return")
		return nil
	}

	rec, err := a.ReclaimCapacity(before, now)
	fmt.Printf("returned %s of %s in %s\n", rec.Result.Reclaimed, before, rec.Elapsed.Round(time.Millisecond))
	return drainOutcome(rec, err, a.Leases())
}

// drainOutcome decides whether a node is drained, kept apart from the draining
// so every way it can end is reachable in a test rather than only the two an
// agent can be driven into.
func drainOutcome(rec agent.Reclamation, err error, remaining []lease.Lease) error {
	// A missed deadline is a promise the node broke rather than capacity it
	// kept, and the drain itself still happened. Anything else stopped it.
	if err != nil && !errors.Is(err, agent.ErrDeadlineMissed) {
		return fmt.Errorf("draining: %w", err)
	}
	if rec.Result.Err != nil {
		return fmt.Errorf("draining stopped part way: %w", rec.Result.Err)
	}
	return stillHeld(remaining)
}

// stillHeld says what a drain could not return, and separates the two reasons,
// because they call for opposite things from an operator.
//
// A guaranteed lease is checkpoint staging, whose bytes are the only copy of
// themselves until they are durable: a drain that took one would destroy a
// job's progress while reporting success, so the wait is the right answer.
// Anything else is a lease the ladder was supposed to take, which is a fault
// and must not be reported as a checkpoint the operator would wait on forever.
func stillHeld(remaining []lease.Lease) error {
	var staging, stuck pool.Bytes
	for _, l := range remaining {
		if l.Class == lease.Guaranteed {
			staging += l.Size
			continue
		}
		stuck += l.Size
	}
	switch {
	case stuck > 0:
		return fmt.Errorf("still holding %s the drain should have been able to take, and %s of checkpoint staging", stuck, staging)
	case staging > 0:
		// Non-zero, and in the words an operator needs: the rollout has to stop,
		// and the reason is a promise this node made rather than a fault.
		return fmt.Errorf("still holding %s of checkpoint staging, whose bytes are the only copy of themselves. "+
			"Wait for it to become durable and drain again", staging)
	}
	return nil
}
