package fasttier

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const blockSize = 1 << 10

// extent makes a preallocated file of n blocks, standing in for a lease's
// capacity.
func extent(t *testing.T, blocks int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "extent")
	if err := os.WriteFile(p, make([]byte, int64(blocks)*blockSize), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

// newCache builds a cache holding one lease of n blocks.
func newCache(t *testing.T, blocks, firstReadLimit int) *Cache {
	t.Helper()
	c, err := New(Config{BlockSize: blockSize, FirstReadLimit: firstReadLimit})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddCapacity("lease-a", extent(t, blocks)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func key(n int) Key {
	return Key{Tenant: "t1", Block: BlockRef{Backend: "s3", Object: "ds@v1", Index: int64(n)}}
}

func body(n int) []byte { return []byte(fmt.Sprintf("block-%d", n)) }

// seed makes a block resident the way the read path does: a miss offers the
// bytes and is refused, and the next miss admits them.
func seed(t *testing.T, c *Cache, k Key, data []byte) {
	t.Helper()
	c.Read(k)
	if admitted, err := c.Admit(k, data, false); err != nil || admitted {
		t.Fatalf("seeding %s: first offer admitted=%v err=%v", k, admitted, err)
	}
	c.Read(k)
	if admitted, err := c.Admit(k, data, false); err != nil || !admitted {
		t.Fatalf("seeding %s: second offer admitted=%v err=%v", k, admitted, err)
	}
}

func TestNothingIsAdmittedOnFirstRead(t *testing.T) {
	// A cache that admits on first touch is emptied by any single sequential
	// pass over a large dataset, which is exactly what a training epoch looks
	// like from below.
	c := newCache(t, 8, 64)
	for i := range 8 {
		if _, hit := c.Read(key(i)); hit {
			t.Fatalf("block %d was resident before anything admitted it", i)
		}
		admitted, err := c.Admit(key(i), body(i), false)
		if err != nil {
			t.Fatal(err)
		}
		if admitted {
			t.Errorf("block %d was admitted on its first read", i)
		}
	}
	if _, _, resident := c.Stats(); resident != 0 {
		t.Errorf("%d blocks resident after a sequential pass, want none", resident)
	}
}

func TestTheSecondReadAdmits(t *testing.T) {
	c := newCache(t, 8, 64)
	k := key(1)
	c.Read(k)
	if admitted, _ := c.Admit(k, body(1), false); admitted {
		t.Fatal("admitted on the first read")
	}
	// Second pass, as the next epoch would.
	c.Read(k)
	admitted, err := c.Admit(k, body(1), false)
	if err != nil {
		t.Fatal(err)
	}
	if !admitted {
		t.Fatal("a block read twice was not admitted")
	}
	got, hit := c.Read(k)
	if !hit {
		t.Fatal("an admitted block was not resident")
	}
	if string(got) != string(body(1)) {
		t.Errorf("read %q, want %q", got, body(1))
	}
}

func TestPrefetchBypassesTheSecondReadRule(t *testing.T) {
	// A manifest saying a job will read these shards arrives before the first
	// read, which is better evidence than a second access.
	c := newCache(t, 8, 64)
	admitted, err := c.Admit(key(1), body(1), true)
	if err != nil {
		t.Fatal(err)
	}
	if !admitted {
		t.Error("a prefetched block was refused")
	}
}

func TestARecordTooSmallAdmitsNothing(t *testing.T) {
	// The failure this rule exists to prevent, reached from the other side. If
	// the record cannot span the gap between two reads of the same block, every
	// read looks like a first one.
	c := newCache(t, 8, 2)
	k := key(0)
	c.Read(k)
	c.Admit(k, body(0), false)
	// Other traffic pushes the record past its bound before the second read,
	// as a further epoch of a dataset larger than the record would.
	for i := 1; i <= 5; i++ {
		c.Read(key(i))
		c.Admit(key(i), body(i), false)
	}
	c.Read(k)
	if admitted, _ := c.Admit(k, body(0), false); admitted {
		t.Error("admitted despite the record having forgotten the first read")
	}
	if _, _, resident := c.Stats(); resident != 0 {
		t.Errorf("%d resident, want nothing admitted", resident)
	}
}

func TestBlocksAreNotSharedBetweenTenants(t *testing.T) {
	// Identical content, different tenants. Sharing would reveal that two
	// tenants hold the same bytes.
	c := newCache(t, 8, 64)
	a := Key{Tenant: "t1", Block: BlockRef{Backend: "s3", Object: "ds@v1", Index: 0}}
	b := Key{Tenant: "t2", Block: BlockRef{Backend: "s3", Object: "ds@v1", Index: 0}}
	seed(t, c, a, body(0))
	if _, hit := c.Read(b); hit {
		t.Error("a second tenant read the first tenant's block")
	}
}

func TestRevokingALeaseDropsEveryBlockItHeld(t *testing.T) {
	// Reclamation takes a whole lease, so every block it holds dies together.
	// That is why the cost of a reclaim arrives as a burst rather than one read.
	c := newCache(t, 8, 64)
	for i := range 4 {
		seed(t, c, key(i), body(i))
	}
	if dropped := c.Revoke("lease-a"); dropped != 4 {
		t.Errorf("revoking dropped %d blocks, want 4", dropped)
	}
	for i := range 4 {
		if _, hit := c.Read(key(i)); hit {
			t.Errorf("block %d survived the revocation of the lease holding it", i)
		}
	}
}

func TestAReaderSeesAMissRatherThanAnError(t *testing.T) {
	// The rule the whole design turns on: not an error, not stale bytes.
	c := newCache(t, 8, 64)
	k := key(1)
	seed(t, c, k, body(1))
	// Revoke the slab without going through Revoke, as an agent invalidating
	// an extent before unlinking it would.
	c.slabs["lease-a"].revoke()

	got, hit := c.Read(k)
	if hit {
		t.Error("a revoked block was served")
	}
	if got != nil {
		t.Errorf("read returned %q from revoked capacity", got)
	}
}

func TestEvictionPrefersCapacityThatIsLeavingAnyway(t *testing.T) {
	// Evicting from a lease that is staying throws away something the tier
	// could have kept. The lease manager already orders leases by what it
	// would reclaim first, and the tier asks rather than deciding.
	c, err := New(Config{BlockSize: blockSize, FirstReadLimit: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.AddCapacity("staying", extent(t, 1)); err != nil {
		t.Fatal(err)
	}
	if err := c.AddCapacity("leaving", extent(t, 1)); err != nil {
		t.Fatal(err)
	}
	c.ReclaimOrder([]string{"leaving", "staying"})

	// Two blocks fill both slabs; the leaving one is filled first.
	for i := range 2 {
		seed(t, c, key(i), body(i))
	}
	// A third block must evict, and must take from the lease that is leaving.
	seed(t, c, key(2), body(2))
	stayingHolds := 0
	for _, p := range c.resident {
		if p.slab.lease == "staying" {
			stayingHolds++
		}
	}
	if stayingHolds != 1 {
		t.Errorf("the staying lease holds %d blocks, want its one kept", stayingHolds)
	}
}

func TestACacheWithNoCapacityRefusesRatherThanPretends(t *testing.T) {
	c, err := New(Config{BlockSize: blockSize, FirstReadLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	k := key(0)
	c.Read(k)
	c.Admit(k, body(0), false) // First read, recorded and refused.
	c.Read(k)
	if _, err := c.Admit(k, body(0), false); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("Admit with no capacity = %v, want ErrNoCapacity", err)
	}
}

func TestABlockLargerThanTheUnitIsRefused(t *testing.T) {
	c := newCache(t, 2, 8)
	if _, err := c.Admit(key(0), make([]byte, blockSize+1), true); err == nil {
		t.Error("a block larger than the unit was accepted")
	}
}

func TestAnExtentTooSmallForOneBlockIsRefused(t *testing.T) {
	c, err := New(Config{BlockSize: blockSize, FirstReadLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := filepath.Join(t.TempDir(), "tiny")
	if err := os.WriteFile(p, make([]byte, blockSize/2), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := c.AddCapacity("tiny", p); err == nil {
		t.Error("an extent holding no whole block was accepted")
	}
}

func TestAFailedReadReturnsItsSlot(t *testing.T) {
	// A read can fail for reasons other than revocation, and losing the slot
	// each time starves the tier of capacity it was granted, one intermittent
	// device error at a time, while every event looks like an ordinary miss.
	c := newCache(t, 2, 64)
	seed(t, c, key(0), body(0))
	s := c.slabs["lease-a"]
	free := len(s.free)

	s.file.Close() // Any read now fails without the slab being revoked.
	if _, hit := c.Read(key(0)); hit {
		t.Fatal("a read succeeded against a closed extent")
	}
	if len(s.free) != free+1 {
		t.Errorf("free slots = %d after a failed read, want %d", len(s.free), free+1)
	}
	if _, still := c.resident[key(0)]; still {
		t.Error("the block is still resident after its read failed")
	}
}

func TestARevokedSlabDoesNotHandBackSlots(t *testing.T) {
	// The opposite case: a revoked slab is being unlinked whole, so returning
	// its slots would let the cache hand out space that is going away.
	c := newCache(t, 2, 64)
	seed(t, c, key(0), body(0))
	s := c.slabs["lease-a"]
	free := len(s.free)
	s.revoke()
	c.Read(key(0))
	if len(s.free) != free {
		t.Errorf("a revoked slab returned a slot: free went %d to %d", free, len(s.free))
	}
}

func TestEvictionTakesTheLeastRecentlyUsed(t *testing.T) {
	// Within a lease the victim used to be whatever a map iteration reached
	// first, so a block read a thousand times went as readily as one read
	// twice. RFC-0007 says the choice is the tier's.
	c := newCache(t, 2, 64)
	seed(t, c, key(0), body(0))
	seed(t, c, key(1), body(1))
	// Keep the first one warm; the second is now the oldest.
	if _, hit := c.Read(key(0)); !hit {
		t.Fatal("setup: block 0 not resident")
	}
	seed(t, c, key(2), body(2)) // Must evict something.

	if _, hit := c.Read(key(0)); !hit {
		t.Error("the recently used block was evicted")
	}
	if _, hit := c.Read(key(1)); hit {
		t.Error("the least recently used block survived")
	}
}

func TestReadsDoNotSerialiseOnOneLock(t *testing.T) {
	// Concurrent readers on a GPU node are the expected case. Holding the
	// cache lock across the disk read would queue them all behind each other
	// on the layer whose whole purpose is being fast.
	c := newCache(t, 32, 256)
	for i := range 16 {
		seed(t, c, key(i), body(i))
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 50 {
				if _, hit := c.Read(key(round % 16)); !hit {
					t.Error("a resident block missed under concurrent reads")
					return
				}
			}
		}()
	}
	wg.Wait()
	if hits, _, _ := c.Stats(); hits < 8*50 {
		t.Errorf("hits = %d, want at least %d", hits, 8*50)
	}
}

func TestRevokingUnderConcurrentReadsIsAlwaysAMiss(t *testing.T) {
	// The reader must never see an error or stale bytes, including when the
	// capacity goes while reads are in flight outside the lock.
	c := newCache(t, 32, 256)
	for i := range 16 {
		seed(t, c, key(i), body(i))
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 100 {
				got, hit := c.Read(key(round % 16))
				if hit && len(got) == 0 {
					t.Error("a hit returned no bytes")
					return
				}
			}
		}()
	}
	c.Revoke("lease-a")
	wg.Wait()
	if _, _, resident := c.Stats(); resident != 0 {
		t.Errorf("%d blocks resident after the lease was revoked", resident)
	}
}

func TestAHitNeverAnswersWithAnotherBlocksBytes(t *testing.T) {
	// Reading outside the lock lets a slot be evicted and refilled while the
	// read is in flight. Serving what came back would answer for one object
	// with the content of another, which nothing downstream could detect: the
	// bytes are valid, they just belong to something else.
	c := newCache(t, 1, 64) // One slot, so anything admitted takes this one.
	seed(t, c, key(0), body(0))

	var once sync.Once
	c.beforeRead = func() {
		// Exactly what a concurrent admission does to the slot, without going
		// back through the cache and re-entering this hook.
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			p := c.resident[key(0)]
			delete(c.resident, key(0))
			c.generation++
			c.resident[key(1)] = placement{
				slab: p.slab, slot: p.slot, length: len(body(1)),
				used: c.clock, gen: c.generation,
			}
			if err := p.slab.write(p.slot, body(1), c.cfg.BlockSize); err != nil {
				t.Error(err)
			}
		})
	}

	if got, hit := c.Read(key(0)); hit {
		t.Errorf("a hit returned %q for a block whose slot had been refilled", got)
	}
	// The block that took the slot reads as itself.
	c.beforeRead = nil
	if got, hit := c.Read(key(1)); !hit || string(got) != string(body(1)) {
		t.Errorf("block 1 = %q/%v, want its own bytes", got, hit)
	}
}
