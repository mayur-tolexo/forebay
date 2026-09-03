// Command forebay-bench runs the crossover experiment from RFC-0018.
//
// It answers where node-local bandwidth stops beating a node's achievable
// share of backend fan-out, and it reports three arms rather than two, because
// the tier is reached through a socket the backend arm need not cross and two
// end numbers cannot say how much of the difference is locality.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/driver/s3driver"
	"github.com/mayur-tolexo/forebay/internal/bench"
	"github.com/mayur-tolexo/forebay/internal/dataserver"
)

const (
	accessKeyEnv = "FOREBAY_S3_ACCESS_KEY"
	secretKeyEnv = "FOREBAY_S3_SECRET_KEY"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forebay-bench:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		socket   = flag.String("socket", "", "the agent's read socket, for the two arms that cross it")
		tenant   = flag.String("tenant", "t1", "tenant the socket arms present")
		endpoint = flag.String("s3-endpoint", "", "scheme and host of the durable backend")
		bucket   = flag.String("s3-bucket", "", "bucket the backend serves from")
		region   = flag.String("s3-region", "", "region the backend signs for")
		object   = flag.String("object", "", "object every arm reads")
		size     = flag.Int64("size", 0, "how many bytes that object holds")
		block    = flag.Int64("block", 1<<20, "request size, the same for every arm")
		workers  = flag.String("workers", "1,2,4,8,16,32", "concurrency points to sweep")
		repeat   = flag.Int("repeat", 3, "runs per point, reported as the median")
		cold     = flag.String("cold-objects", "", "comma separated objects the tier has never held, one per repeat, for the third arm")
		extent   = flag.String("evict-extent", "", "the tier's extent, whose page cache is dropped before each measured tier run so the tier is read from the device")
	)
	flag.Parse()

	switch {
	case *socket == "":
		return fmt.Errorf("--socket is required, since two of the three arms cross it")
	case *endpoint == "" || *bucket == "":
		return fmt.Errorf("--s3-endpoint and --s3-bucket are required")
	case *object == "" || *size <= 0:
		return fmt.Errorf("--object and --size are required")
	}
	points, err := parseWorkers(*workers)
	if err != nil {
		return err
	}
	coldObjects := splitList(*cold)

	// As many connections as the widest sweep point, or the transport's own
	// limit of two idle per host throttles the backend arm and flatters the
	// tier. A comparison that handicaps the baseline is not a comparison.
	widest := points[len(points)-1]
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = widest
	transport.MaxConnsPerHost = 0

	d, err := s3driver.New(s3driver.Config{
		Endpoint:   *endpoint,
		Bucket:     *bucket,
		Region:     *region,
		AccessKey:  os.Getenv(accessKeyEnv),
		SecretKey:  os.Getenv(secretKeyEnv),
		HTTPClient: &http.Client{Timeout: 5 * time.Minute, Transport: transport},
	})
	if err != nil {
		return err
	}
	backend, err := driver.Open(d)
	if err != nil {
		return err
	}

	conditions(os.Stdout, *object, *size, *block, *repeat, widest, len(coldObjects), len(points), *extent)
	fmt.Printf("%-34s %8s %10s %12s %18s\n", "arm", "workers", "MiB/s", "elapsed", "checksum")

	ctx := context.Background()
	used := 0
	for _, w := range points {
		plan := bench.Plan{Object: *object, Size: *size, Block: *block, Workers: w}

		if err := report(runRepeats(ctx, "backend, in process", plan, *repeat, func() ([]bench.Reader, func(), error) {
			return backendReaders(backend, w), func() {}, nil
		}, "")); err != nil {
			return err
		}

		tierArm := "tier, through the agent"
		if *extent != "" {
			tierArm = "tier, through the agent, evicted"
		}
		if err := report(runRepeats(ctx, tierArm, plan, *repeat, func() ([]bench.Reader, func(), error) {
			return socketReaders(*socket, *tenant, w, *block)
		}, *extent)); err != nil {
			return err
		}

		// Both cold arms, or the socket's price is read off a warm arm against
		// a cold one and is really the store's cache. One object per run,
		// consumed across the whole sweep: reusing one at the next point would
		// read what the previous point just warmed.
		for _, arm := range []struct {
			name string
			open func() ([]bench.Reader, func(), error)
		}{
			{"backend, in process, cold", func() ([]bench.Reader, func(), error) {
				return backendReaders(backend, w), func() {}, nil
			}},
			{"backend, through the agent, cold", func() ([]bench.Reader, func(), error) {
				return socketReaders(*socket, *tenant, w, *block)
			}},
		} {
			if len(coldObjects)-used < *repeat {
				continue
			}
			var runs []bench.Result
			for i := 0; i < *repeat; i++ {
				p := plan
				p.Object = coldObjects[used]
				used++
				rs, closeAll, err := arm.open()
				if err != nil {
					return err
				}
				r, err := bench.Run(ctx, arm.name, rs, p)
				closeAll()
				if err != nil {
					return err
				}
				runs = append(runs, r)
			}
			if err := report(bench.Median(runs), nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// conditions prints what each arm carried, which RFC-0018 requires of any
// result claiming to be about locality.
func conditions(w io.Writer, object string, size, block int64, repeat, widest, cold, points int, extent string) {
	fmt.Fprintf(w, "object %s, %d bytes, %d byte blocks, %d runs per point, median reported\n",
		object, size, block, repeat)
	fmt.Fprintln(w, "both arms read every byte once, on the same block grid, interleaved across workers")
	fmt.Fprintf(w, "no compression on either side: the object is stored and served as written\n")
	fmt.Fprintf(w, "the backend arm may open %d connections, so the transport does not cap it below the sweep\n", widest)
	fmt.Fprintln(w, "the tier arm is warm, having read the object twice before it is measured")
	if extent == "" {
		fmt.Fprintf(w, "the tier arm reads an extent through the page cache, so a working set of %d bytes\n"+
			"  that fits in memory measures cache and NVMe together and is an upper bound on NVMe alone\n", size)
	} else {
		fmt.Fprintf(w, "the tier arm's extent is evicted from the page cache before each measured run, and the\n"+
			"  run is refused unless the eviction took, so the tier is read from the device\n")
	}
	if cold == 0 {
		fmt.Fprintln(w, "no cold arms: pass --cold-objects to separate the socket's cost from the tier's benefit")
	} else {
		fmt.Fprintf(w, "the two cold arms read %d objects nothing has read before, one per run, so the socket's\n"+
			"  price is taken between two cold arms rather than off a warm one\n", cold)
		if need := repeat * points * 2; cold < need {
			fmt.Fprintf(w, "that is fewer than the %d this sweep needs, so the arm stops when they run out\n", need)
		}
	}
	fmt.Fprintln(w)
}

// runRepeats measures one arm at one point, warming it first so the tier arm
// is measured warm rather than on its own first read.
func runRepeats(ctx context.Context, arm string, plan bench.Plan, repeat int, open func() ([]bench.Reader, func(), error), extent string) (bench.Result, error) {
	// Twice, because the tier admits a block on its second read.
	for i := 0; i < 2; i++ {
		rs, closeAll, err := open()
		if err != nil {
			return bench.Result{}, err
		}
		_, err = bench.Run(ctx, arm, rs, plan)
		closeAll()
		if err != nil {
			return bench.Result{}, err
		}
	}
	var runs []bench.Result
	for i := 0; i < repeat; i++ {
		// After warming, so what is dropped is the cache the warming filled.
		if extent != "" {
			before, after, err := evict(extent)
			if err != nil {
				return bench.Result{}, err
			}
			if after*20 > before {
				return bench.Result{}, fmt.Errorf("evicting %s left %d of %d pages resident, so the run would not be cold",
					extent, after, before)
			}
		}
		rs, closeAll, err := open()
		if err != nil {
			return bench.Result{}, err
		}
		r, err := bench.Run(ctx, arm, rs, plan)
		closeAll()
		if err != nil {
			return bench.Result{}, err
		}
		runs = append(runs, r)
	}
	return bench.Median(runs), nil
}

// report prints one row.
func report(r bench.Result, err error) error {
	if err != nil {
		return err
	}
	fmt.Printf("%-34s %8d %10.1f %12s %18x\n",
		r.Arm, r.Workers, r.Rate(), r.Elapsed.Round(time.Millisecond), r.Checksum)
	return nil
}

// backendReaders gives every worker the same driver, which pools connections
// up to the limit set above.
func backendReaders(b *driver.Backend, n int) []bench.Reader {
	out := make([]bench.Reader, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// socketReaders opens one connection per worker, since one connection carries
// one exchange at a time.
func socketReaders(socket, tenant string, n int, maxReply int64) ([]bench.Reader, func(), error) {
	var (
		out    = make([]bench.Reader, n)
		closes []func() error
	)
	for i := 0; i < n; i++ {
		// The block size, since that is the largest reply this asks for and the
		// bound sizes the caller's own memory.
		c, err := dataserver.Dial("unix", socket, dataserver.ClientConfig{MaxReply: maxReply})
		if err != nil {
			for _, f := range closes {
				_ = f()
			}
			return nil, nil, fmt.Errorf("dialling the agent: %w", err)
		}
		out[i] = tenantReader{client: c, tenant: tenant}
		closes = append(closes, c.Close)
	}
	return out, func() {
		for _, f := range closes {
			_ = f()
		}
	}, nil
}

// tenantReader adapts the socket client, which needs a tenant the driver does
// not.
type tenantReader struct {
	client *dataserver.Client
	tenant string
}

func (t tenantReader) ReadRange(_ context.Context, object string, offset, length int64) ([]byte, error) {
	return t.client.ReadRange(t.tenant, object, offset, length)
}

// parseWorkers reads the sweep, in order, so the widest point is the last.
func parseWorkers(s string) ([]int, error) {
	var out []int
	for _, f := range splitList(s) {
		n, err := strconv.Atoi(f)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("worker count %q is not a positive number", f)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no worker counts")
	}
	sort.Ints(out)
	return out, nil
}

// splitList reads a comma separated flag, dropping empties.
func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
