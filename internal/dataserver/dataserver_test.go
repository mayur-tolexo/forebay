package dataserver_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/driver/filedriver"
	"github.com/mayur-tolexo/forebay/internal/dataserver"
	"github.com/mayur-tolexo/forebay/internal/fasttier"
	"github.com/mayur-tolexo/forebay/internal/prefetch"
)

const blockSize = 1 << 12

// serveWith builds a data server over a tier with capacity and a backend
// holding one object, delayed by the given amount so a test that cares which
// side is faster can decide it rather than measure an accident.
func serveWith(t *testing.T, object string, content []byte, cfg dataserver.Config, delay time.Duration) (*dataserver.Server, *fasttier.Cache) {
	t.Helper()
	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(store, object), content, 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := filedriver.New(store)
	if err != nil {
		t.Fatal(err)
	}
	var d driver.Driver = &slow{Driver: fd, delay: delay}
	back, err := driver.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	tier, err := fasttier.New(fasttier.Config{BlockSize: blockSize, FirstReadLimit: 128})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tier.Close() })
	extent := filepath.Join(t.TempDir(), "extent")
	if err := os.WriteFile(extent, make([]byte, 64*blockSize), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tier.AddCapacity("lease", extent); err != nil {
		t.Fatal(err)
	}
	srv, err := dataserver.New(tier, back, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return srv, tier
}

// serve is the ordinary case: no delay, no prefetching.
func serve(t *testing.T, object string, content []byte) (*dataserver.Server, *fasttier.Cache) {
	t.Helper()
	return serveWith(t, object, content, dataserver.Config{Backend: "store"}, 0)
}

// pattern is content whose every byte says where it is, so a misplaced slice
// shows up as a wrong value rather than as the right length of wrong bytes.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestAReadIsAnsweredFromTheBackendWhenTheTierIsEmpty(t *testing.T) {
	content := pattern(10 * blockSize)
	srv, _ := serve(t, "obj", content)

	got, err := srv.ReadRange(context.Background(), "t1", "obj", 0, 3*blockSize)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !bytes.Equal(got, content[:3*blockSize]) {
		t.Errorf("got %d bytes, not the object's first three blocks", len(got))
	}
}

func TestAReadThatStartsAndEndsMidBlockIsExact(t *testing.T) {
	// The grid is the tier's, not the read's, so this exercises the slicing
	// on both ends. Off-by-one here serves a job the wrong bytes silently.
	content := pattern(10 * blockSize)
	srv, _ := serve(t, "obj", content)

	const off, length = blockSize + 17, 2*blockSize + 5
	got, err := srv.ReadRange(context.Background(), "t1", "obj", off, length)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !bytes.Equal(got, content[off:off+length]) {
		t.Errorf("got %d bytes and they are not the range asked for", len(got))
	}
}

func TestASecondReadComesFromTheTier(t *testing.T) {
	// Admission is on the second read, so the first fetches and records and
	// the second admits. The third is the one that can hit.
	content := pattern(4 * blockSize)
	srv, _ := serve(t, "obj", content)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := srv.ReadRange(ctx, "t1", "obj", 0, blockSize); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if got := srv.Stats(); got.Hits == 0 {
		t.Errorf("no read came from the tier: %+v", got)
	}
}

func TestTheLastPartialBlockIsServedRatherThanRefused(t *testing.T) {
	// A whole-block fetch of the tail runs past the end of the object, which
	// the backend calls an error. That is ordinary, not a failure, and the
	// read has to answer.
	content := pattern(2*blockSize + 100)
	srv, _ := serve(t, "obj", content)

	got, err := srv.ReadRange(context.Background(), "t1", "obj", 2*blockSize, 100)
	if err != nil {
		t.Fatalf("reading the tail: %v", err)
	}
	if !bytes.Equal(got, content[2*blockSize:]) {
		t.Errorf("got %d bytes of the tail, want 100", len(got))
	}
	// The backend can say how large the object is, so the tail is fetched at
	// its true length rather than probed for and narrowed.
	if s := srv.Stats(); s.Fetched != 1 || s.Narrowed != 0 {
		t.Errorf("want the tail fetched once at its real size, got %+v", s)
	}
}

func TestAWholeObjectWithAPartialTailReadsBackExactly(t *testing.T) {
	// The case that catches a tail served at block length rather than object
	// length, which appends bytes the object does not have.
	content := pattern(3*blockSize + 7)
	srv, _ := serve(t, "obj", content)

	got, err := srv.ReadRange(context.Background(), "t1", "obj", 0, int64(len(content)))
	if err != nil {
		t.Fatalf("reading the whole object: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("read back %d bytes, want %d", len(got), len(content))
	}
}

func TestAReadPastTheEndIsStillAnError(t *testing.T) {
	// The miss this layer absorbs is capacity going away. A caller asking for
	// bytes that do not exist has made a different mistake, and hiding it
	// would return short data that reads as truncation.
	//
	// Both sides of the object's end are covered. An overrun starting at a
	// block boundary beyond the object is a different path from one starting
	// inside the last block, and knowing the object's size is what makes the
	// second answerable rather than left to a length mismatch further up.
	for _, c := range []struct {
		name           string
		size           int
		offset, length int64
	}{
		{"whole blocks past the end", blockSize, 0, 4 * blockSize},
		{"overrun inside the tail block", 2*blockSize + 100, 2 * blockSize, 200},
		{"overrun by one byte", 2*blockSize + 100, 2 * blockSize, 101},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Both with and without object-size, since the capability decides
			// which path supplies a block, and cold as well as warm, since a
			// resident block is supplied by neither. Everything here was once
			// checked cold only, and the answer was right until the tier
			// started working.
			for _, hide := range []bool{false, true} {
				// Warm only where the tail can become resident, which is what
				// object-size is for: without it the tail is never admitted,
				// so there is no warm state to reach and asserting one would
				// pass by doing nothing.
				for _, warm := range []bool{false, true} {
					if warm && hide {
						continue
					}
					srv, _ := countingServer(t, "obj", pattern(c.size), hide)
					ctx := context.Background()
					if warm {
						// Three reads inside the object: the second admits
						// and the third hits.
						for i := 0; i < 3; i++ {
							if _, err := srv.ReadRange(ctx, "t1", "obj", c.offset, int64(c.size)-c.offset); err != nil {
								t.Fatalf("warming: %v", err)
							}
						}
						if srv.Stats().Hits == 0 {
							t.Fatal("nothing became resident, so warm proves nothing")
						}
					}
					_, err := srv.ReadRange(ctx, "t1", "obj", c.offset, c.length)
					if !errors.Is(err, driver.ErrRange) {
						t.Errorf("object-size hidden=%v warm=%v: reading past the end returned %v, want a range error", hide, warm, err)
					}
				}
			}
		})
	}
}

func TestRevokedCapacityBecomesAFetchAndNeverAnError(t *testing.T) {
	// The whole point of absorbing the miss. The tier loses the block while
	// the client is reading, and the client sees a slower read.
	content := pattern(4 * blockSize)
	srv, tier := serve(t, "obj", content)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := srv.ReadRange(ctx, "t1", "obj", 0, blockSize); err != nil {
			t.Fatal(err)
		}
	}
	if srv.Stats().Hits == 0 {
		t.Fatal("nothing was resident, so this proves nothing about losing it")
	}
	tier.Revoke("lease")

	got, err := srv.ReadRange(ctx, "t1", "obj", 0, blockSize)
	if err != nil {
		t.Fatalf("a read after the capacity went away failed: %v", err)
	}
	if !bytes.Equal(got, content[:blockSize]) {
		t.Error("the bytes after revocation are not the object's")
	}
}

func TestBlocksAreNotSharedBetweenTenants(t *testing.T) {
	// Identical content in two tenants is two blocks. Sharing them would say
	// that one tenant holds what the other does.
	content := pattern(2 * blockSize)
	srv, _ := serve(t, "obj", content)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := srv.ReadRange(ctx, "t1", "obj", 0, blockSize); err != nil {
			t.Fatal(err)
		}
	}
	before := srv.Stats().Hits
	if _, err := srv.ReadRange(ctx, "t2", "obj", 0, blockSize); err != nil {
		t.Fatal(err)
	}
	if srv.Stats().Hits != before {
		t.Error("a second tenant read the first tenant's cached block")
	}
}

func TestAnEmptyReadAsksTheBackendNothing(t *testing.T) {
	srv, _ := serve(t, "obj", pattern(blockSize))
	got, err := srv.ReadRange(context.Background(), "t1", "obj", 0, 0)
	if err != nil || got != nil {
		t.Errorf("zero-length read = %v, %v; want nothing and no error", got, err)
	}
	if s := srv.Stats(); s.Blocks != 0 {
		t.Errorf("Blocks = %d, want no block touched", s.Blocks)
	}
}

func TestABadConfigurationIsRefused(t *testing.T) {
	// A backend with no name would let two backends' blocks collide in the
	// tier, which serves one object's bytes for another's.
	tier, err := fasttier.New(fasttier.Config{BlockSize: blockSize, FirstReadLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer tier.Close()
	fd, err := filedriver.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	back, err := driver.Open(fd)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		tier *fasttier.Cache
		back *driver.Backend
		cfg  dataserver.Config
	}{
		{"no tier", nil, back, dataserver.Config{Backend: "store"}},
		{"no backend", tier, nil, dataserver.Config{Backend: "store"}},
		{"no name", tier, back, dataserver.Config{}},
		{"negative read bound", tier, back, dataserver.Config{Backend: "store", MaxRead: -1}},
	} {
		if _, err := dataserver.New(c.tier, c.back, c.cfg); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

func TestATierWithNoCapacityStillServesAndSaysSo(t *testing.T) {
	// Admission failing is not a reason to fail a read: the bytes are already
	// in hand. But an admission that never succeeds is invisible unless it is
	// counted, and a tier that holds nothing looks exactly like a cold one.
	content := pattern(2 * blockSize)
	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(store, "obj"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := filedriver.New(store)
	if err != nil {
		t.Fatal(err)
	}
	back, err := driver.Open(fd)
	if err != nil {
		t.Fatal(err)
	}
	// No AddCapacity, so there is nowhere to put a block.
	tier, err := fasttier.New(fasttier.Config{BlockSize: blockSize, FirstReadLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer tier.Close()
	srv, err := dataserver.New(tier, back, dataserver.Config{Backend: "store"})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		got, err := srv.ReadRange(ctx, "t1", "obj", 0, blockSize)
		if err != nil {
			t.Fatalf("read %d failed because nothing could be cached: %v", i, err)
		}
		if !bytes.Equal(got, content[:blockSize]) {
			t.Fatalf("read %d returned the wrong bytes", i)
		}
	}
	s := srv.Stats()
	if s.NotAdmitted == 0 {
		t.Errorf("admission never succeeded and nothing recorded it: %+v", s)
	}
	if s.Hits != 0 {
		t.Errorf("Hits = %d with no capacity to hold anything", s.Hits)
	}
}

func TestEveryBlockIsCountedOnceUnderExactlyOneHeading(t *testing.T) {
	// Blocks is the total, and a block is served by the tier, by a whole
	// fetch or by a narrowed one. A counter that does not add up is worse
	// than no counter, because it is believed.
	content := pattern(3*blockSize + 9)
	srv, _ := serve(t, "obj", content)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := srv.ReadRange(ctx, "t1", "obj", 0, int64(len(content))); err != nil {
			t.Fatal(err)
		}
	}
	s := srv.Stats()
	if s.Blocks != s.Hits+s.Fetched+s.Narrowed {
		t.Errorf("Blocks = %d, but hits+fetched+narrowed = %d: %+v", s.Blocks, s.Hits+s.Fetched+s.Narrowed, s)
	}
	if s.Fetched == 0 || s.Admitted == 0 || s.Hits == 0 {
		t.Errorf("reading a whole object three times left a heading empty: %+v", s)
	}
}

func TestAnEnormousReadIsRefusedRatherThanAllocated(t *testing.T) {
	// Asking for a terabyte of a four-kilobyte object used to size the answer
	// from the number asked for, reach the allocator before the backend, and
	// end with the kernel killing the process. The bound is on what is asked
	// for, because the object's real size is not known until the backend has
	// been asked and by then the memory is committed.
	srv, _ := serve(t, "obj", pattern(blockSize))

	_, err := srv.ReadRange(context.Background(), "t1", "obj", 0, 1<<40)
	if err == nil {
		t.Fatal("a terabyte read of a four-kilobyte object was accepted")
	}
	if !strings.Contains(err.Error(), "single read") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
	if s := srv.Stats(); s.Blocks != 0 {
		t.Errorf("Blocks = %d, want the read refused before any block was touched", s.Blocks)
	}
}

func TestTheReadBoundIsConfigurable(t *testing.T) {
	// An NFS client reads a megabyte at most, but a Go caller pulling a whole
	// shard is a different shape, so the bound belongs to the deployment.
	content := pattern(4 * blockSize)
	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(store, "obj"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := filedriver.New(store)
	if err != nil {
		t.Fatal(err)
	}
	back, err := driver.Open(fd)
	if err != nil {
		t.Fatal(err)
	}
	tier, err := fasttier.New(fasttier.Config{BlockSize: blockSize, FirstReadLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer tier.Close()
	srv, err := dataserver.New(tier, back, dataserver.Config{Backend: "store", MaxRead: blockSize})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := srv.ReadRange(context.Background(), "t1", "obj", 0, blockSize); err != nil {
		t.Errorf("a read at the bound was refused: %v", err)
	}
	if _, err := srv.ReadRange(context.Background(), "t1", "obj", 0, blockSize+1); err == nil {
		t.Error("a read past the bound was accepted")
	}
}

func TestACancelledReadStopsEvenWhenEverythingIsResident(t *testing.T) {
	// A read answered entirely from the tier never reaches the backend, so
	// the backend's own context handling never sees it. Without a check here
	// a large resident read runs to the end whatever the caller decided.
	content := pattern(8 * blockSize)
	srv, _ := serve(t, "obj", content)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := srv.ReadRange(ctx, "t1", "obj", 0, int64(len(content))); err != nil {
			t.Fatal(err)
		}
	}
	if srv.Stats().Hits == 0 {
		t.Fatal("nothing is resident, so this proves nothing about the cached path")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.ReadRange(cancelled, "t1", "obj", 0, int64(len(content))); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled read returned %v, want it to stop", err)
	}
}

// counting wraps a driver and counts round trips, optionally hiding that it
// can say how large an object is.
type counting struct {
	driver.Driver
	hideSize bool
	calls    int
}

func (c *counting) Declare() driver.Declaration {
	d := c.Driver.Declare()
	if !c.hideSize {
		return d
	}
	var kept []driver.Capability
	for _, cap := range d.Capabilities {
		if cap != driver.ObjectSize {
			kept = append(kept, cap)
		}
	}
	d.Capabilities = kept
	return d
}

func (c *counting) SizeOf(ctx context.Context, o string) (int64, error) {
	if c.hideSize {
		return 0, fmt.Errorf("%w: %s", driver.ErrNotSupported, driver.ObjectSize)
	}
	c.calls++
	return c.Driver.SizeOf(ctx, o)
}

func (c *counting) ReadRange(ctx context.Context, o string, off, n int64) ([]byte, error) {
	c.calls++
	return c.Driver.ReadRange(ctx, o, off, n)
}

// countingServer serves one object through a driver that counts round trips.
func countingServer(t *testing.T, object string, content []byte, hideSize bool) (*dataserver.Server, *counting) {
	t.Helper()
	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(store, object), content, 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := filedriver.New(store)
	if err != nil {
		t.Fatal(err)
	}
	c := &counting{Driver: fd, hideSize: hideSize}
	back, err := driver.Open(c)
	if err != nil {
		t.Fatal(err)
	}
	tier, err := fasttier.New(fasttier.Config{BlockSize: blockSize, FirstReadLimit: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tier.Close() })
	extent := filepath.Join(t.TempDir(), "extent")
	if err := os.WriteFile(extent, make([]byte, 64*blockSize), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tier.AddCapacity("lease", extent); err != nil {
		t.Fatal(err)
	}
	srv, err := dataserver.New(tier, back, dataserver.Config{Backend: "store"})
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func TestAnObjectSmallerThanABlockIsCached(t *testing.T) {
	// An object smaller than one block is entirely tail, so the carve-out for
	// the last partial block swallows the whole object. Datasets of many
	// small files are exactly that shape, and they are the workload this tier
	// exists for: without object-size such an object is never cached and
	// every read goes to the backend twice.
	content := pattern(100)
	srv, c := countingServer(t, "small", content, false)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		got, err := srv.ReadRange(ctx, "t1", "small", 0, 100)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("read %d returned the wrong bytes", i)
		}
	}
	s := srv.Stats()
	if s.Hits == 0 || s.Admitted == 0 {
		t.Errorf("a sub-block object was never cached: %+v", s)
	}
	// Two misses that each cost a failed whole-block probe, a size and a
	// fetch, and then nothing.
	if c.calls > 9 {
		t.Errorf("%d backend round trips for five reads, want the tier to take over", c.calls)
	}
}

func TestWithoutObjectSizeTheTailStillServesAndIsNotCached(t *testing.T) {
	// The fallback has to keep working, because read-range is the whole
	// mandatory core and a backend may offer nothing else.
	content := pattern(100)
	srv, _ := countingServer(t, "small", content, true)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		got, err := srv.ReadRange(ctx, "t1", "small", 0, 100)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("read %d returned the wrong bytes", i)
		}
	}
	s := srv.Stats()
	if s.Narrowed != 3 || s.Hits != 0 {
		t.Errorf("want every read narrowed and nothing cached, got %+v", s)
	}
}

// contradicting refuses a whole block and then reports a size that would make
// that block whole, which is what a driver pointed at a mutable object looks
// like from here.
type contradicting struct{ content []byte }

func (c *contradicting) Declare() driver.Declaration {
	return driver.Declaration{
		Contract:     1,
		Capabilities: []driver.Capability{driver.ReadRange, driver.ObjectSize},
	}
}
func (c *contradicting) SizeOf(context.Context, string) (int64, error) {
	return int64(len(c.content)), nil
}
func (c *contradicting) ReadRange(_ context.Context, _ string, off, n int64) ([]byte, error) {
	if off == blockSize && n == blockSize {
		return nil, fmt.Errorf("%w: past the end", driver.ErrRange)
	}
	if off+n > int64(len(c.content)) {
		return nil, fmt.Errorf("%w: past the end", driver.ErrRange)
	}
	return c.content[off : off+n], nil
}
func (c *contradicting) WriteObject(context.Context, string, []byte) error {
	return driver.ErrNotSupported
}
func (c *contradicting) DeleteObject(context.Context, string) error { return driver.ErrNotSupported }
func (c *contradicting) SnapshotObject(context.Context, string) (string, error) {
	return "", driver.ErrNotSupported
}
func (c *contradicting) CloneObject(context.Context, string, string) error {
	return driver.ErrNotSupported
}

// contradictSize is how large contradicting claims its object is, in blocks.
// Two makes the refused block exactly whole, which is the boundary the guard
// has to treat the same as more than whole.
func TestABackendThatContradictsItselfIsNamed(t *testing.T) {
	// A size saying the block is full, after a whole-block read of that same
	// range was refused for running past the end, is the backend disagreeing
	// with itself. Fetching it anyway returns more than a block, and the
	// caller is left to work out from a length mismatch that the fault was
	// not its own.
	for _, blocks := range []int{2, 6} {
		t.Run(fmt.Sprintf("%d blocks", blocks), func(t *testing.T) {
			assertContradictionNamed(t, blocks)
		})
	}
}

// assertContradictionNamed reads an object the backend claims is the given
// number of blocks while refusing a whole read of its second block.
func assertContradictionNamed(t *testing.T, blocks int) {
	t.Helper()
	back, err := driver.Open(&contradicting{content: pattern(blocks * blockSize)})
	if err != nil {
		t.Fatal(err)
	}
	tier, err := fasttier.New(fasttier.Config{BlockSize: blockSize, FirstReadLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer tier.Close()
	extent := filepath.Join(t.TempDir(), "extent")
	if err := os.WriteFile(extent, make([]byte, 32*blockSize), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tier.AddCapacity("lease", extent); err != nil {
		t.Fatal(err)
	}
	srv, err := dataserver.New(tier, back, dataserver.Config{Backend: "store"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = srv.ReadRange(context.Background(), "t1", "obj", 0, 2*blockSize)
	if err == nil {
		t.Fatal("a backend contradicting itself was served anyway")
	}
	if !strings.Contains(err.Error(), "refused a whole block") {
		t.Errorf("the error does not name the backend as the problem: %v", err)
	}
}

func TestAReadWithBadArgumentsIsRefused(t *testing.T) {
	// The empty tenant is the one that matters. Blocks are scoped by tenant
	// and two tenants sharing one would say that each holds what the other
	// does, so the guard making an empty tenant impossible is part of that
	// boundary rather than tidiness.
	srv, _ := serve(t, "obj", pattern(2*blockSize))
	ctx := context.Background()
	const max = int64(math.MaxInt64)

	for _, c := range []struct {
		name           string
		tenant, object string
		offset, length int64
	}{
		{"no tenant", "", "obj", 0, 16},
		{"no object", "t1", "", 0, 16},
		{"negative offset", "t1", "obj", -1, 16},
		{"negative length", "t1", "obj", 0, -1},
		{"offset and length overflow", "t1", "obj", max - 8, 16},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := srv.ReadRange(ctx, c.tenant, c.object, c.offset, c.length); err == nil {
				t.Error("accepted")
			}
		})
	}
	if s := srv.Stats(); s.Blocks != 0 {
		t.Errorf("Blocks = %d, want every refusal to happen before a block is touched", s.Blocks)
	}
}

// failing refuses every read after the first n, which is a backend going away
// mid-request rather than an object ending.
type failing struct {
	driver.Driver
	afterReads int
	reads      int
	failSize   bool
}

func (f *failing) SizeOf(ctx context.Context, o string) (int64, error) {
	if f.failSize {
		return 0, errors.New("backend unreachable")
	}
	return f.Driver.SizeOf(ctx, o)
}

func (f *failing) ReadRange(ctx context.Context, o string, off, n int64) ([]byte, error) {
	f.reads++
	if f.reads > f.afterReads {
		return nil, errors.New("backend unreachable")
	}
	return f.Driver.ReadRange(ctx, o, off, n)
}

// failingServer serves an object through a backend that stops answering.
func failingServer(t *testing.T, content []byte, afterReads int, failSize bool) *dataserver.Server {
	t.Helper()
	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(store, "obj"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := filedriver.New(store)
	if err != nil {
		t.Fatal(err)
	}
	back, err := driver.Open(&failing{Driver: fd, afterReads: afterReads, failSize: failSize})
	if err != nil {
		t.Fatal(err)
	}
	tier, err := fasttier.New(fasttier.Config{BlockSize: blockSize, FirstReadLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tier.Close() })
	srv, err := dataserver.New(tier, back, dataserver.Config{Backend: "store"})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestABackendFailureIsReportedRatherThanAbsorbed(t *testing.T) {
	// The miss this layer absorbs is capacity going away. A backend that
	// cannot answer is a different thing, and serving a short read or an
	// empty one in its place would be the truncation this layer exists to
	// avoid.
	content := pattern(2*blockSize + 100)
	for _, c := range []struct {
		name       string
		afterReads int
		failSize   bool
		offset     int64
	}{
		{"whole-block fetch fails", 0, false, 0},
		{"size lookup fails", 1, true, 2 * blockSize},
		{"exact tail fetch fails", 1, false, 2 * blockSize},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := failingServer(t, content, c.afterReads, c.failSize)
			_, err := srv.ReadRange(context.Background(), "t1", "obj", c.offset, 100)
			if err == nil {
				t.Fatal("a backend that could not answer was absorbed")
			}
			if errors.Is(err, driver.ErrRange) {
				t.Errorf("a backend failure was reported as a bad range: %v", err)
			}
		})
	}
}

// slow wraps a driver so a backend read takes a knowable amount of time,
// because the scoreboard's whole question is which side is faster and an
// in-memory fake answers it by accident.
type slow struct {
	driver.Driver
	delay time.Duration
}

func (s *slow) ReadRange(ctx context.Context, object string, offset, length int64) ([]byte, error) {
	time.Sleep(s.delay)
	return s.Driver.ReadRange(ctx, object, offset, length)
}

// serveSlow is serve with a backend that takes its time.
func serveSlow(t *testing.T, object string, content []byte, delay time.Duration) *dataserver.Server {
	t.Helper()
	srv, _ := serveWith(t, object, content, dataserver.Config{Backend: "store"}, delay)
	return srv
}

// TestTheReadPathScoresItself is what turns RFC-0024's scoreboard from a
// package into an answer: the reads it needs are the ones this path already
// makes, and it is the only place that knows which side served them.
func TestTheReadPathScoresItself(t *testing.T) {
	const object = "shard"
	srv := serveSlow(t, object, pattern(8*blockSize), 5*time.Millisecond)
	ctx := context.Background()

	// Two backend reads of one block, which is what admission on second read
	// costs, and then a hit.
	for i := 0; i < 3; i++ {
		if _, err := srv.ReadRange(ctx, "t1", object, 0, blockSize); err != nil {
			t.Fatal(err)
		}
	}

	got := srv.Efficiency()
	if got.Covered != 1 {
		t.Errorf("covered %d hits, want the one the tier served", got.Covered)
	}
	if got.Uncovered != 0 {
		t.Errorf("uncovered %d hits, though a miss of that size was recorded", got.Uncovered)
	}
	if got.Saved <= 0 {
		t.Errorf("saved %s against a backend delayed by 5ms", got.Saved)
	}
	if got.Spread < 0 {
		t.Errorf("spread %s is not a duration", got.Spread)
	}
}

// TestAnUnservedSizeIsUncovered covers the honesty the scoreboard is for: a
// hit the node has no comparable miss for is counted and not estimated.
func TestAnUnservedSizeIsUncovered(t *testing.T) {
	const object = "shard"
	srv := serveSlow(t, object, pattern(8*blockSize), time.Millisecond)
	ctx := context.Background()

	// Two whole-block reads to get one block resident, then read a short slice
	// of it. The slice is a tier hit of a size nothing missed at.
	for i := 0; i < 2; i++ {
		if _, err := srv.ReadRange(ctx, "t1", object, 0, blockSize); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.ReadRange(ctx, "t1", object, 0, 16); err != nil {
		t.Fatal(err)
	}

	got := srv.Efficiency()
	if got.Covered+got.Uncovered == 0 {
		t.Fatal("no tier hits were scored at all")
	}
	// Whatever the split, the fraction is reported rather than assumed whole.
	if f := got.CoveredFraction(); f < 0 || f > 1 {
		t.Errorf("covered fraction %v is not a share", f)
	}
}

// TestAReadServedEntirelyFromTheBackendSavesNothing keeps a node whose tier
// never answered from reporting a saving it did not make.
func TestAReadServedEntirelyFromTheBackendSavesNothing(t *testing.T) {
	const object = "shard"
	srv := serveSlow(t, object, pattern(4*blockSize), time.Millisecond)

	// One read per block, so nothing is ever read twice and nothing is
	// admitted.
	for i := int64(0); i < 4; i++ {
		if _, err := srv.ReadRange(context.Background(), "t1", object, i*blockSize, blockSize); err != nil {
			t.Fatal(err)
		}
	}
	got := srv.Efficiency()
	if got.Saved != 0 {
		t.Errorf("saved %s with no tier hit at all", got.Saved)
	}
	if got.Covered != 0 || got.Uncovered != 0 {
		t.Errorf("scored %d covered and %d uncovered hits with no hits", got.Covered, got.Uncovered)
	}
}

// waitFor polls until a condition holds, since prefetching happens off the
// read path and a test that asserted immediately would be asserting that it
// happens on it.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// TestASequentialReaderIsGotAheadOf is the point of the detector: a reader
// walking an object should find blocks already resident that it never read.
func TestASequentialReaderIsGotAheadOf(t *testing.T) {
	const object = "shard"
	cfg := prefetch.DefaultConfig()
	srv, tier := serveWith(t, object, pattern(32*blockSize),
		dataserver.Config{Backend: "store", Prefetch: &cfg}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, err := net.Listen("unix", socketPath(t))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); srv.Serve(ctx, l) }()
	defer func() { cancel(); wg.Wait() }()

	// Three blocks in a row confirms the stride, and the fourth read is where
	// predictions start being issued.
	for i := int64(0); i < 4; i++ {
		if _, err := srv.ReadRange(ctx, "t1", object, i*blockSize, blockSize); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "a block the reader has not reached to be resident", func() bool {
		return srv.Stats().Prefetched > 0
	})

	// The block ahead of the reader is there, and putting it there did not
	// count as a read anybody made.
	ahead := fasttier.Key{Tenant: "t1", Block: fasttier.BlockRef{Backend: "store", Object: object, Index: 5}}
	if !tier.Resident(ahead) {
		t.Error("the block after the reader is not resident, though something was prefetched")
	}
}

// TestPrefetchIsOffUnlessAsked matters because RFC-0011's depth and accuracy
// floor are guesses, and a prediction costs bandwidth on a node whose
// bandwidth feeds an accelerator.
func TestPrefetchIsOffUnlessAsked(t *testing.T) {
	const object = "shard"
	srv, _ := serve(t, object, pattern(32*blockSize))

	for i := int64(0); i < 8; i++ {
		if _, err := srv.ReadRange(context.Background(), "t1", object, i*blockSize, blockSize); err != nil {
			t.Fatal(err)
		}
	}
	got := srv.Stats()
	if got.Prefetched+got.PrefetchRefused+got.PrefetchFailed+got.PrefetchDropped != 0 {
		t.Errorf("a server with no prefetch configured predicted something: %+v", got)
	}
}

// TestAPredictionPastTheEndIsOrdinary covers the stride that runs off the end
// of an object, which a detector following a difference cannot know about.
func TestAPredictionPastTheEndIsOrdinary(t *testing.T) {
	const object = "shard"
	cfg := prefetch.DefaultConfig()
	// Two blocks only, so predictions run past the end almost immediately.
	srv, _ := serveWith(t, object, pattern(3*blockSize),
		dataserver.Config{Backend: "store", Prefetch: &cfg}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, err := net.Listen("unix", socketPath(t))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); srv.Serve(ctx, l) }()
	defer func() { cancel(); wg.Wait() }()

	for i := int64(0); i < 3; i++ {
		if _, err := srv.ReadRange(ctx, "t1", object, i*blockSize, blockSize); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "a prediction past the end of the object", func() bool {
		return srv.Stats().PrefetchFailed > 0
	})
	// Reads still work, because nothing was waiting on any of it.
	if _, err := srv.ReadRange(ctx, "t1", object, 0, blockSize); err != nil {
		t.Errorf("a read after failed predictions: %v", err)
	}
}

// settleFor gives background work time to do the thing a test is asserting it
// does not do. There is no state to poll for here: the assertion is that
// nothing happened, and nothing happening has no edge to wait on.
func settleFor(d time.Duration) { time.Sleep(d) }

// TestAResidentBlockIsNotFetchedAgain covers the check that keeps prefetching
// from paying for what it already has. A detector following a stride predicts
// blocks without knowing which are resident, so the saving is entirely in the
// skip.
func TestAResidentBlockIsNotFetchedAgain(t *testing.T) {
	const object = "shard"
	cfg := prefetch.DefaultConfig()
	cfg.Depth = 2
	srv, tier := serveWith(t, object, pattern(32*blockSize),
		dataserver.Config{Backend: "store", Prefetch: &cfg}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, err := net.Listen("unix", socketPath(t))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); srv.Serve(ctx, l) }()
	defer func() { cancel(); wg.Wait() }()

	// Walk far enough that the detector is confirmed and has fetched ahead.
	for i := int64(0); i < 8; i++ {
		if _, err := srv.ReadRange(ctx, "t1", object, i*blockSize, blockSize); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "the reader to be got ahead of", func() bool { return srv.Stats().Prefetched > 0 })

	// The blocks prefetch placed are the resident ones: a block read once on
	// demand is not admitted, and nothing predicts backwards, so the early
	// indices of the walk are still absent.
	var held []int64
	for i := int64(1); i < 6; i++ {
		k := fasttier.Key{Tenant: "t1", Block: fasttier.BlockRef{Backend: "store", Object: object, Index: i}}
		if tier.Resident(k) {
			held = append(held, i)
		}
	}
	if len(held) < 3 {
		t.Fatalf("only %d blocks were got ahead of, so a second walk proves nothing", len(held))
	}

	// Walking them again predicts blocks already held. A fetch of one would
	// come back, find the tier already has it, and be counted as dropped, so
	// that counter is what separates the skip from the wasted round trip.
	// Predictions past the resident region are fetched and admitted, and show
	// up as prefetched rather than dropped.
	for _, i := range held {
		if _, err := srv.ReadRange(ctx, "t1", object, i*blockSize, blockSize); err != nil {
			t.Fatal(err)
		}
	}
	// Nothing should happen, which is what has to be waited for rather than
	// polled for: the worker has a dozen predictions to consider and each one
	// it acts on would be a read of the store.
	settleFor(200 * time.Millisecond)
	if got := srv.Stats().PrefetchDropped; got != 0 {
		t.Errorf("%d predictions were fetched and then found already resident", got)
	}
}

// TestPredictingNeverBlocksTheReadPath is the bound on the queue. Nothing
// drains it here, so it fills and every prediction after that is dropped, and
// the reads have to keep going regardless.
func TestPredictingNeverBlocksTheReadPath(t *testing.T) {
	const object = "shard"
	cfg := prefetch.DefaultConfig()
	cfg.Depth = 16
	// No Serve, so no worker: the queue fills and stays full.
	srv, _ := serveWith(t, object, pattern(256*blockSize),
		dataserver.Config{Backend: "store", Prefetch: &cfg}, 0)

	for i := int64(0); i < 64; i++ {
		if _, err := srv.ReadRange(context.Background(), "t1", object, i*blockSize, blockSize); err != nil {
			t.Fatalf("read %d with a full prediction queue: %v", i, err)
		}
	}
	if got := srv.Stats().PrefetchDropped; got == 0 {
		t.Error("a queue nothing drains dropped no predictions, so nothing bounded it")
	}
	if got := srv.Stats().Prefetched; got != 0 {
		t.Errorf("%d blocks were prefetched with no worker running", got)
	}
}
