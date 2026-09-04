package fasttier

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNoCapacity reports a cache with nowhere to put a block.
var ErrNoCapacity = errors.New("fasttier: no slot free")

// ErrWouldEvict reports a prefetched block refused because taking it would
// have evicted one somebody read. It wraps ErrNoCapacity rather than replacing
// it, so a caller that only wants to know the tier was full still sees that,
// and a caller deciding whether to keep predicting can tell the two apart.
var ErrWouldEvict = errors.New("fasttier: a prediction may not evict")

// Config sizes the tier.
type Config struct {
	// BlockSize is the unit. Fixed, so accounting and eviction have one
	// currency: whole objects make a dataloader pull gigabytes to read one
	// shard, and shards vary by orders of magnitude between datasets.
	BlockSize int64
	// FirstReadLimit bounds the record of first reads. Too small and admission
	// never fires; the value is a measurement RFC-0018 owns.
	FirstReadLimit int
}

// where a resident block lives.
type placement struct {
	slab   *slab
	slot   int
	length int
	// used orders eviction within a lease. Without it the victim is whatever
	// a map iteration reaches first, so a block read a thousand times goes as
	// readily as one read twice.
	used uint64
	// gen identifies this occupancy of this slot, and never repeats.
	//
	// A read happens outside the lock, so the slot can be evicted and refilled
	// while the read is in flight. Comparing slab and slot afterwards is not
	// enough: a block evicted, its slot reused, and the same key re-admitted
	// to it would pass that check while the read saw the intervening content.
	gen uint64
}

// Cache is the node-local half of the fast tier.
//
// Peer fetch is absent deliberately: RFC-0007 designs the rack tier as
// removable and RFC-0018 owns whether it earns its place, so nothing here may
// assume it exists.
type Cache struct {
	cfg   Config
	mu    sync.Mutex
	slabs map[string]*slab // By lease.
	// order is the lease order reclamation would follow, cheapest first, as
	// the lease manager reports it. Eviction prefers what is leaving anyway.
	order    []string
	resident map[Key]placement
	first    *firstReads
	clock    uint64
	// generation stamps each occupancy of a slot, so a read can tell that what
	// it found is still what it was sent for.
	generation uint64
	hits       int
	misses     int
	// beforeRead runs between releasing the lock and reading, and is only set
	// by tests.
	beforeRead func()
}

// New builds a cache with no capacity. Capacity arrives as leases.
func New(cfg Config) (*Cache, error) {
	if cfg.BlockSize <= 0 {
		return nil, fmt.Errorf("fasttier: block size must be positive, got %d", cfg.BlockSize)
	}
	if cfg.FirstReadLimit < 0 {
		return nil, fmt.Errorf("fasttier: first-read limit cannot be negative, got %d", cfg.FirstReadLimit)
	}
	return &Cache{
		cfg:      cfg,
		slabs:    map[string]*slab{},
		resident: map[Key]placement{},
		first:    newFirstReads(cfg.FirstReadLimit),
	}, nil
}

// AddCapacity gives the cache a lease's extent to fill.
func (c *Cache) AddCapacity(lease, extent string) error {
	s, err := openSlab(lease, extent, c.cfg.BlockSize)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.slabs[lease]; exists {
		s.close()
		return fmt.Errorf("fasttier: lease %s is already held", lease)
	}
	c.slabs[lease] = s
	c.order = append(c.order, lease)
	return nil
}

// Revoke makes a lease's blocks unreadable and forgets them.
//
// Called before the agent unlinks the extent. Reclamation is not the tier's
// choice, so this cannot refuse: every block the lease held dies together,
// which is why the cost of a reclaim arrives as a burst rather than one read.
func (c *Cache) Revoke(lease string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.slabs[lease]
	if !ok {
		return 0
	}
	s.revoke()
	dropped := 0
	for k, p := range c.resident {
		if p.slab == s {
			delete(c.resident, k)
			dropped++
		}
	}
	delete(c.slabs, lease)
	for i, l := range c.order {
		if l == lease {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	s.close()
	return dropped
}

// ReclaimOrder tells the cache which leases are cheapest to lose, so eviction
// can prefer capacity that is leaving anyway over capacity that is staying.
//
// A hint rather than a rule: the lease manager already orders leases this way,
// and the tier asks rather than deciding.
func (c *Cache) ReclaimOrder(leases []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order = append(c.order[:0:0], leases...)
}

// Read returns a block if it is resident.
//
// On a miss the caller fetches from the backend and offers the bytes to Admit,
// which is where the read is recorded. Reading does not record on its own, so
// a caller that never offers the bytes never influences admission.
func (c *Cache) Read(k Key) ([]byte, bool) {
	c.mu.Lock()
	p, ok := c.resident[k]
	if !ok {
		c.misses++
		c.mu.Unlock()
		// The read is not recorded here. Admit records it, and recording in
		// both places made every first read look like a second one, which is
		// admission on first touch wearing this rule's name.
		return nil, false
	}
	c.clock++
	p.used = c.clock
	c.resident[k] = p
	gen := p.gen
	c.mu.Unlock()

	// Outside the lock. Holding it across the read would serialise every
	// reader on this node through one mutex, on the layer whose whole purpose
	// is being fast.
	if c.beforeRead != nil {
		// A seam for the test that proves a refilled slot is caught. The race
		// is microseconds wide otherwise, so a test without it would pass
		// whether or not the guard exists.
		c.beforeRead()
	}
	data, err := p.slab.read(p.slot, p.length, c.cfg.BlockSize)

	c.mu.Lock()
	defer c.mu.Unlock()
	cur, still := c.resident[k]
	if err != nil {
		// Revoked underneath the reader, or the device failed: a miss, never
		// an error. The caller refetches, which costs one backend read.
		if still && cur.gen == gen {
			delete(c.resident, k)
			// The slot goes back only if the slab survives. A revoked slab is
			// being unlinked whole, and returning slots to it would hand them
			// out again.
			if !p.slab.revoked.Load() {
				p.slab.release(p.slot)
			}
		}
		c.misses++
		return nil, false
	}
	if !still || cur.gen != gen {
		// The slot was refilled while the read was in flight, so these bytes
		// belong to some other block. Serving them would answer for one object
		// with the content of another, which nothing downstream could detect.
		c.misses++
		return nil, false
	}
	c.hits++
	return data, true
}

// Admit offers a block to the cache.
//
// It is kept on the second read, or when something asked for it ahead of time.
// Admitting on first touch would let one sequential pass over a large dataset
// empty the cache, which is what a training epoch looks like from below.
func (c *Cache) Admit(k Key, data []byte, prefetched bool) (bool, error) {
	if int64(len(data)) > c.cfg.BlockSize {
		return false, fmt.Errorf("fasttier: block is %d bytes, the unit is %d", len(data), c.cfg.BlockSize)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, already := c.resident[k]; already {
		return false, nil
	}
	// Prefetch bypasses the rule: a manifest saying a job will read these
	// shards arrives before the first read, which is better evidence than a
	// second access.
	if !prefetched && !c.first.sawBefore(k) {
		return false, nil
	}
	slab, slot, err := c.placeLocked(prefetched)
	if err != nil {
		return false, err
	}
	if err := slab.write(slot, data, c.cfg.BlockSize); err != nil {
		slab.release(slot)
		return false, err
	}
	c.clock++
	c.generation++
	c.resident[k] = placement{
		slab: slab, slot: slot, length: len(data), used: c.clock, gen: c.generation,
	}
	c.first.forget(k)
	return true, nil
}

// placeLocked finds a slot, evicting if it has to.
func (c *Cache) placeLocked(prefetched bool) (*slab, int, error) {
	for _, lease := range c.order {
		if s, ok := c.slabs[lease]; ok {
			if slot, took := s.take(); took {
				return s, slot, nil
			}
		}
	}
	// A prediction may take space nothing is using and may never take space
	// from a block somebody read. RFC-0011 makes that the rule which keeps a
	// wrong prediction costing bandwidth instead of costing a block that was
	// about to be read, and it binds hardest here: a full tier means a busy
	// node, which is exactly where predictions are least reliable.
	if prefetched {
		return nil, 0, fmt.Errorf("%w: %w", ErrNoCapacity, ErrWouldEvict)
	}
	if !c.evictOneLocked() {
		return nil, 0, ErrNoCapacity
	}
	for _, lease := range c.order {
		if s, ok := c.slabs[lease]; ok {
			if slot, took := s.take(); took {
				return s, slot, nil
			}
		}
	}
	return nil, 0, ErrNoCapacity
}

// evictOneLocked frees a slot, preferring a lease that is leaving anyway.
//
// Eviction is the tier's own choice and reclamation is not, which is why they
// are separate. They meet only here, as a preference.
func (c *Cache) evictOneLocked() bool {
	for _, lease := range c.order {
		s, ok := c.slabs[lease]
		if !ok {
			continue
		}
		// Least recently used within that lease, so the choice is the tier's
		// rather than a map iteration's.
		var victim Key
		var oldest uint64
		found := false
		for k, p := range c.resident {
			if p.slab != s {
				continue
			}
			if !found || p.used < oldest {
				victim, oldest, found = k, p.used, true
			}
		}
		if found {
			s.release(c.resident[victim].slot)
			delete(c.resident, victim)
			return true
		}
	}
	return false
}

// Held identifies one dataset's blocks in the tier, without the block index.
// It is what residency is counted per, since a scheduler is told about a
// dataset rather than about a block.
type Held struct {
	Tenant, Backend, Object string
}

// HeldBlocks reports how many blocks of each object are resident.
//
// A count rather than a share, because the tier does not know how large an
// object is: it holds blocks and never asked what they were part of. Turning
// the count into a share needs the object's size, which the backend knows.
func (c *Cache) HeldBlocks() map[Held]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[Held]int, len(c.resident))
	for k := range c.resident {
		out[Held{Tenant: k.Tenant, Backend: k.Block.Backend, Object: k.Block.Object}]++
	}
	return out
}

// Resident reports whether a block is held, without touching its recency.
//
// A prefetcher needs to know before it fetches, and asking with Read would
// count a hit nobody had and make the block look recently used, which is the
// signal eviction orders by.
func (c *Cache) Resident(k Key) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.resident[k]
	return ok
}

// BlockSize is the unit the cache is keyed in.
//
// A caller that splits a read into blocks has to use this number and not one
// of its own: a block admitted at a different size is a block that can never
// be found again, and one larger than this is refused.
func (c *Cache) BlockSize() int64 { return c.cfg.BlockSize }

// Stats reports what the tier has done, so a hit rate is observable rather
// than inferred.
func (c *Cache) Stats() (hits, misses, resident int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, len(c.resident)
}

// Close releases every extent the cache holds.
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	for _, s := range c.slabs {
		if cerr := s.close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	c.slabs = map[string]*slab{}
	c.resident = map[Key]placement{}
	return err
}
