package dataserver

import (
	"context"
	"errors"
	"time"

	"github.com/mayur-tolexo/forebay/internal/efficiency"
	"github.com/mayur-tolexo/forebay/internal/fasttier"
	"github.com/mayur-tolexo/forebay/internal/prefetch"
)

// aheadQueue bounds how many predictions wait to be fetched.
//
// Bounded so that predicting can never slow a read down: a full queue drops
// the prediction, which costs a miss that would have happened anyway. An
// unbounded one would trade the thing prefetch exists to protect for the thing
// it is trying to provide.
const aheadQueue = 64

// predict tells the detector what was just read and queues what it says comes
// next.
//
// The detector is not safe for concurrent use and this is the only place that
// drives it, so it is held under the same lock as the counters beside it. The
// work it does is a map lookup and a subtraction, which is why that is
// affordable on the read path and fetching is not.
func (s *Server) predict(tenant, object string, index int64) {
	if s.detect == nil {
		return
	}
	s.mu.Lock()
	next := s.detect.Observe(prefetch.Block{Stream: tenant + "/" + object, Index: index}, time.Now())
	s.mu.Unlock()

	for _, b := range next {
		k := fasttier.Key{
			Tenant: tenant,
			Block:  fasttier.BlockRef{Backend: s.name, Object: object, Index: b.Index},
		}
		select {
		case s.ahead <- k:
		default:
			// The queue is full, which means fetching is not keeping up with
			// predicting. Dropped rather than waited on, because waiting here
			// is waiting on the read path.
			s.record(func(st *Stats) { st.PrefetchDropped++ })
		}
	}
}

// fetchAhead fetches predicted blocks until its context is done.
//
// One worker rather than several. The point is to be ahead of a reader rather
// than to saturate the backend, and a pool of fetchers competing with the
// reads they are meant to help is the failure this is most likely to become.
func (s *Server) fetchAhead(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case k := <-s.ahead:
			s.fetchOne(ctx, k)
		}
	}
}

// fetchOne fetches one predicted block and offers it to the tier.
//
// Nothing here fails a read, because no read is waiting on it. Each outcome is
// counted instead, and they are counted apart because they mean different
// things: a tier with no room is prefetch working as designed, and a backend
// that would not answer is not.
func (s *Server) fetchOne(ctx context.Context, k fasttier.Key) {
	// Asked without touching recency. Read would count a hit nobody had and
	// make the block look recently used, which is what eviction orders by.
	if s.tier.Resident(k) {
		return
	}

	began := time.Now()
	data, err := s.backend.ReadRange(ctx, k.Block.Object, k.Block.Index*s.block, s.block)
	if err != nil {
		// A prediction past the end of an object arrives here as a range
		// error, which is ordinary rather than a fault: a detector following a
		// stride does not know where the object stops.
		s.record(func(st *Stats) { st.PrefetchFailed++ })
		return
	}
	// Recorded like any other backend read. It is evidence of what this
	// backend costs at this size, which is what the scoreboard uses misses
	// for, and speculative reads come from the same distribution as demanded
	// ones.
	s.measure(efficiency.Backend, int64(len(data)), time.Since(began))

	switch admitted, err := s.tier.Admit(k, data, true); {
	case admitted:
		s.record(func(st *Stats) { st.Prefetched++ })
	case errors.Is(err, fasttier.ErrWouldEvict):
		// The rule working, not a problem: a prediction may take space nothing
		// is using and may never take space from a block somebody read.
		s.record(func(st *Stats) { st.PrefetchRefused++ })
	default:
		s.record(func(st *Stats) { st.PrefetchDropped++ })
	}
}
