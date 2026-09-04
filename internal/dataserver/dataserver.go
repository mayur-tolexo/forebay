// Package dataserver answers a byte range of an immutable object.
//
// It is the half of the access layer RFC-0008 puts on the node: the fast tier
// where the bytes are resident, and the durable backend where they are not.
// The miss is absorbed here rather than passed on. RFC-0007 answers a revoked
// read with a miss its caller must re-issue and says the caller is this layer,
// because the caller above this one is an unmodified NFS client and there is
// no way to tell it to try again. What the client sees is a slower read.
package dataserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/internal/efficiency"
	"github.com/mayur-tolexo/forebay/internal/fasttier"
	"github.com/mayur-tolexo/forebay/internal/metrics"
	"github.com/mayur-tolexo/forebay/internal/prefetch"
)

// ErrRefused marks a request this will not answer, as opposed to one it
// could not answer.
//
// The two need different replies. A caller that asked for something
// nonsensical has to be told so, and a caller whose backend was briefly down
// has to be told something else, or it retries the first for ever and gives up
// on the second.
var ErrRefused = errors.New("dataserver: refused")

// Server serves one backend through one fast tier.
type Server struct {
	tier    *fasttier.Cache
	backend *driver.Backend
	// name scopes cached blocks to the backend they came from, since the same
	// object name in two backends is two different objects.
	name     string
	maxRead  int64
	idle     time.Duration
	exchange time.Duration
	block    int64

	mu    sync.Mutex
	stats Stats
	// score answers whether the tier is earning its capacity, from the same
	// reads the counters above come from. Guarded by mu with them, because a
	// scoreboard is not safe for concurrent use and this is the one place that
	// writes it.
	score *efficiency.Scoreboard
	// detect predicts what a reader will ask for next, and is nil when
	// prefetching is off. Guarded by mu, since a detector is not safe for
	// concurrent use.
	detect *prefetch.Detector
	// ahead carries predictions to the worker that fetches them, so no read
	// ever waits on one.
	ahead chan fasttier.Key
	// reg and ready are how this path becomes visible. Both are nil when
	// nothing is watching, which is the case for a server built in a test that
	// is not about observability.
	reg   *metrics.Registry
	ready *metrics.Readiness
}

// Stats is what the read path did, for an operator deciding whether the tier
// is earning its capacity.
type Stats struct {
	// Blocks is how many blocks were answered, whatever answered them.
	Blocks int64
	// Hits came from the tier.
	Hits int64
	// Fetched were read whole from the backend.
	Fetched int64
	// Narrowed were read from the backend as less than a whole block, which
	// happens at the end of an object and cannot be admitted.
	Narrowed int64
	// Admitted were placed in the tier, which is a second read of a block.
	Admitted int64
	// NotAdmitted is an admission that failed. The read still answered, so
	// this is the only place it shows.
	NotAdmitted int64
	// Prefetched were fetched ahead of any read and placed in the tier.
	Prefetched int64
	// PrefetchRefused were fetched and declined because placing them would
	// have evicted a block somebody read. This is the rule working rather than
	// a fault, and it is counted apart from the ones that are.
	PrefetchRefused int64
	// PrefetchFailed were predicted and could not be fetched, which for a
	// stride running past the end of an object is ordinary.
	PrefetchFailed int64
	// PrefetchDropped were predicted and never attempted, because fetching was
	// not keeping up with predicting.
	PrefetchDropped int64
}

// DefaultMaxRead bounds a single read when nothing else says to.
//
// Generous against an NFS client, whose rsize is a megabyte at most, and small
// enough that a caller asking for a terabyte is refused rather than answered
// by the kernel killing this process.
const DefaultMaxRead = 64 << 20

// Config is what a data server needs beyond the tier and the backend.
type Config struct {
	// Backend names the store the blocks came from, and scopes them in the
	// tier: the same object name in two backends is two different objects.
	Backend string
	// Idle bounds how long a caller may go without asking. Zero means
	// DefaultIdle.
	Idle time.Duration
	// Exchange bounds serving one request and delivering its answer, and
	// starts when the request arrives rather than when the wait for it did.
	// Zero means DefaultExchangeBudget.
	Exchange time.Duration
	// Metrics is where the read path publishes what it did. Nil means it
	// publishes nothing, and the series RFC-0017 names then read as flat zeros
	// rather than as absent, which is why the agent always passes one.
	Metrics *metrics.Registry
	// Ready is fed each read's service time, which is the signal RFC-0015
	// names as the one that separates a node that is slow from one that is
	// dead. Nil means nothing is deciding readiness from reads.
	Ready *metrics.Readiness
	// Prefetch turns on predicting what a reader will ask for next. Nil means
	// off, which is what a caller that has not measured its own workload
	// should choose: RFC-0011's depth and accuracy floor are guesses, and a
	// prediction costs bandwidth on a node whose bandwidth feeds an
	// accelerator.
	Prefetch *prefetch.Config
	// MaxRead bounds one read. Zero means DefaultMaxRead.
	//
	// The bound is on what is asked for, not on what exists, because the
	// answer is sized before the object is: a read allocates its own length
	// and only then discovers the object is smaller.
	MaxRead int64
}

// New builds a data server over a tier and a backend.
//
// The block size comes from the tier rather than the config. Two answers to
// that question is one too many: a block written at a size the tier does not
// use is a block nothing will find again.
func New(tier *fasttier.Cache, backend *driver.Backend, cfg Config) (*Server, error) {
	switch {
	case tier == nil:
		return nil, errors.New("dataserver: no fast tier")
	case backend == nil:
		return nil, errors.New("dataserver: no backend")
	case cfg.Backend == "":
		return nil, errors.New("dataserver: the backend needs a name, since it scopes what the tier holds")
	case cfg.MaxRead < 0:
		return nil, fmt.Errorf("dataserver: a read bound of %d is not a size", cfg.MaxRead)
	case cfg.Idle < 0:
		return nil, fmt.Errorf("dataserver: an idle bound of %s is not a duration to wait", cfg.Idle)
	case cfg.Exchange < 0:
		return nil, fmt.Errorf("dataserver: an exchange bound of %s is not a duration to wait", cfg.Exchange)
	}
	if cfg.MaxRead == 0 {
		cfg.MaxRead = DefaultMaxRead
	}
	if cfg.Idle == 0 {
		cfg.Idle = DefaultIdle
	}
	if cfg.Exchange == 0 {
		cfg.Exchange = DefaultExchangeBudget
	}
	srv := &Server{
		tier: tier, backend: backend,
		name: cfg.Backend, maxRead: cfg.MaxRead,
		idle: cfg.Idle, exchange: cfg.Exchange, block: tier.BlockSize(),
		score: efficiency.New(),
		reg:   cfg.Metrics,
		ready: cfg.Ready,
	}
	if cfg.Prefetch != nil {
		detect, err := prefetch.New(*cfg.Prefetch)
		if err != nil {
			return nil, err
		}
		srv.detect, srv.ahead = detect, make(chan fasttier.Key, aheadQueue)
	}
	return srv, nil
}

// ReadRange answers length bytes of object from offset.
//
// A range reaching past the end of the object is an error, the same answer the
// backend gives. That is not the miss this layer absorbs: a caller asking for
// bytes that do not exist has made a different mistake from one whose cached
// bytes went away.
func (s *Server) ReadRange(ctx context.Context, tenant, object string, offset, length int64) ([]byte, error) {
	switch {
	case tenant == "":
		return nil, fmt.Errorf("%w: no tenant, and blocks are not shared between them", ErrRefused)
	case object == "":
		return nil, fmt.Errorf("%w: no object", ErrRefused)
	case offset < 0 || length < 0:
		return nil, fmt.Errorf("%w: %d bytes from %d is not a range", ErrRefused, length, offset)
	case length == 0:
		return nil, nil
	case offset > (1<<63-1)-length:
		return nil, fmt.Errorf("%w: %d bytes from %d runs past what an offset can count", ErrRefused, length, offset)
	case length > s.maxRead:
		// Refused rather than failed: asking for more than a read may ask for
		// is the caller's mistake and will not come right on a retry.
		// Refused before anything is allocated. The answer is sized from the
		// length asked for and the object's real size is not known until the
		// backend is asked, so a large enough number reaches the allocator
		// first and the process does not survive it.
		return nil, fmt.Errorf("%w: %d bytes is more than the %d a single read may ask for", ErrRefused, length, s.maxRead)
	}

	// Counted around the whole read rather than around a block, because what a
	// caller waited for is the read, and readiness is a claim about what a
	// caller experienced.
	began := time.Now()
	s.inFlight(1)
	defer func() {
		s.inFlight(-1)
		s.answered(time.Since(began))
	}()

	end := offset + length
	out := make([]byte, 0, length)
	// Blocks are aligned to the tier's grid, not to the read, so a read that
	// starts mid-block still fetches and caches the whole block it lands in.
	for start := offset / s.block * s.block; start < end; start += s.block {
		// Checked per block rather than only in the backend, because a read
		// answered entirely from the tier never reaches the backend and would
		// otherwise run to the end whatever the caller has decided.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("dataserver: reading %s: %w", object, err)
		}
		data, err := s.blockAt(ctx, tenant, object, start/s.block, end)
		if err != nil {
			return nil, err
		}
		// A block shorter than the unit is the last one, because only whole
		// blocks and exact tails are ever admitted. So a read wanting more
		// than it holds is a read past the end of the object, and it has to
		// be answered as the range error the backend would have given.
		//
		// Checked here rather than only where a block is fetched, since a
		// resident block never reaches that code and the tail becoming
		// resident is the point of asking a backend for object sizes at all.
		if held := int64(len(data)); held < s.block && end > start+held {
			return nil, fmt.Errorf("%w: %d bytes from %d, %s ends at %d", driver.ErrRange, length, offset, object, start+held)
		}
		lo, hi := int64(0), int64(len(data))
		if offset > start {
			lo = offset - start
		}
		if end-start < hi {
			hi = end - start
		}
		out = append(out, data[lo:hi]...)
	}
	if int64(len(out)) != length {
		// The rule the driver contract applies to a backend, applied here to
		// this layer: a caller that asked for a range and got fewer bytes
		// cannot tell that from truncation. Nothing should reach this, since
		// only whole blocks are admitted and a narrowed fetch is exactly what
		// the read needs, but that is an invariant rather than a check.
		return nil, fmt.Errorf("dataserver: assembled %d bytes for a %d byte read of %s", len(out), length, object)
	}
	return out, nil
}

// SizeOf reports how large an object is, which an NFS server has to answer
// before a client will read anything.
//
// Asked of the backend rather than of the tier, because a size the tier could
// answer is a size for the blocks it happens to hold rather than for the
// object, and a getattrs answered from a partial cache is a truncated file.
func (s *Server) SizeOf(ctx context.Context, tenant, object string) (int64, error) {
	switch {
	case tenant == "":
		return 0, fmt.Errorf("%w: no tenant, and objects are not shared between them", ErrRefused)
	case object == "":
		return 0, fmt.Errorf("%w: no object", ErrRefused)
	}
	size, err := s.backend.SizeOf(ctx, object)
	if err != nil {
		return 0, err
	}
	if size < 0 {
		return 0, fmt.Errorf("%w: %s reported %d bytes", ErrRefused, object, size)
	}
	return size, nil
}

// blockAt returns one block, from the tier if it is there and from the backend
// if it is not.
//
// wantEnd is where the read stops, used only when the block runs past the end
// of the object.
func (s *Server) blockAt(ctx context.Context, tenant, object string, index, wantEnd int64) ([]byte, error) {
	k := fasttier.Key{
		Tenant: tenant,
		Block:  fasttier.BlockRef{Backend: s.name, Object: object, Index: index},
	}
	s.predict(tenant, object, index)

	began := time.Now()
	if data, ok := s.tier.Read(k); ok {
		s.record(func(st *Stats) { st.Blocks++; st.Hits++ })
		s.measure(efficiency.Tier, int64(len(data)), time.Since(began))
		s.delivered(efficiency.Tier, int64(len(data)))
		return data, nil
	}

	start := index * s.block
	began = time.Now()
	data, err := s.backend.ReadRange(ctx, object, start, s.block)
	switch {
	case err == nil:
		s.record(func(st *Stats) { st.Blocks++; st.Fetched++ })
		// Timed before admission, which is this node's own bookkeeping rather
		// than part of what the backend cost, and counted whatever the size:
		// a miss is the evidence a hit of the same size is estimated against.
		s.measure(efficiency.Backend, int64(len(data)), time.Since(began))
		s.delivered(efficiency.Backend, int64(len(data)))
		s.admit(k, data)
		return data, nil
	case !errors.Is(err, driver.ErrRange):
		return nil, err
	}

	// The whole block runs past the end of the object, which is ordinary for
	// the last one.
	//
	// Where the backend can say how large the object is, the exact tail is
	// fetched and admitted like any other block, because it is a whole block:
	// there is no shorter one behind it. Where it cannot, the read asks for
	// what it needs and that answer is not admitted, since a short answer
	// cannot be told from the object being shorter than the caller believed
	// and a partial block would be served whole to the next reader. An object
	// smaller than one block is entirely tail, so without object-size such an
	// object is never cached and costs two requests on every read: this one,
	// which fails, and the narrowed one after it.
	if s.backend.Supports(driver.ObjectSize) {
		return s.tailBySize(ctx, k, object, start, wantEnd)
	}
	narrow := wantEnd - start
	if narrow <= 0 || narrow >= s.block {
		return nil, err
	}
	began = time.Now()
	data, err = s.backend.ReadRange(ctx, object, start, narrow)
	if err != nil {
		return nil, err
	}
	s.record(func(st *Stats) { st.Blocks++; st.Narrowed++ })
	// Recorded like any other backend read. The tier can never serve this one,
	// so it will never be a hit of that size, but it is still evidence of what
	// this backend costs at that size.
	s.measure(efficiency.Backend, int64(len(data)), time.Since(began))
	s.delivered(efficiency.Backend, int64(len(data)))
	return data, nil
}

// tailBySize fetches the last block of an object whose size the backend can
// report, which makes it a whole block and so one the tier can keep.
func (s *Server) tailBySize(ctx context.Context, k fasttier.Key, object string, start, wantEnd int64) ([]byte, error) {
	size, err := s.backend.SizeOf(ctx, object)
	if err != nil {
		return nil, err
	}
	length := size - start
	switch {
	// wantEnd is where the read stops and this block starts before it, so a
	// block beginning past the end of the object arrives here as an overrun
	// and needs no case of its own.
	case wantEnd > size:
		// The read wants bytes the object does not have. Knowing the size is
		// what makes that answerable here, and it has to be answered as the
		// range error the backend would have given: a caller that cannot tell
		// its own bad range from this layer failing has no way to react, and
		// above this layer that caller has to map the answer to a status.
		return nil, fmt.Errorf("%w: %d bytes from %d, %s holds %d", driver.ErrRange, wantEnd-start, start, object, size)
	case length >= s.block:
		// Reached only after a whole-block read of this same range was
		// refused for running past the end, so a size saying the block is
		// whole is the backend contradicting itself, and a size saying it is
		// more than whole equally so. Refused here rather than fetched: at
		// exactly a block this would re-issue the call that just failed, and
		// beyond one it would return more than a block and leave the caller
		// to work out from a length mismatch that the fault was not its own.
		return nil, fmt.Errorf("dataserver: %s refused a whole block at %d and then reported %d bytes, which would make that block whole", object, start, size)
	}
	began := time.Now()
	data, err := s.backend.ReadRange(ctx, object, start, length)
	if err != nil {
		return nil, err
	}
	s.record(func(st *Stats) { st.Blocks++; st.Fetched++ })
	s.measure(efficiency.Backend, int64(len(data)), time.Since(began))
	s.delivered(efficiency.Backend, int64(len(data)))
	s.admit(k, data)
	return data, nil
}

// admit offers a block to the tier.
//
// A failure here does not fail the read. The bytes are already in hand and the
// caller asked for bytes, so turning a caching problem into a client-visible
// error would be the wrong trade; it is counted instead, because an admission
// that never succeeds is invisible otherwise.
func (s *Server) admit(k fasttier.Key, data []byte) {
	switch admitted, err := s.tier.Admit(k, data, false); {
	case err != nil:
		s.record(func(st *Stats) { st.NotAdmitted++ })
	case admitted:
		s.record(func(st *Stats) { st.Admitted++ })
	}
}

// inFlight moves the gauge of reads being answered right now, which is the
// one number that says whether a quiet node is idle or stuck.
func (s *Server) inFlight(delta float64) {
	if s.reg == nil {
		return
	}
	_ = s.reg.Add(metrics.ReadsInFlight, nil, delta)
}

// answered publishes how long one read took, to the histogram an operator
// reads and to the readiness that decides whether this node should be sent
// more.
//
// Both from the same number on purpose. A node judged ready by one measurement
// and reported slow by another gives an operator two answers and no way to
// choose.
func (s *Server) answered(took time.Duration) {
	if s.reg != nil {
		_ = s.reg.Observe(metrics.ReadSeconds, nil, took.Seconds())
	}
	if s.ready != nil {
		s.ready.Observe(took, time.Now())
	}
}

// delivered publishes bytes answered and where they came from, which is the
// only label that distinguishes the tier earning its capacity from the backend
// doing the work.
//
// Called where a caller was waiting and not from the prefetch worker, whose
// bytes are fetched on nobody's behalf. They are still evidence of what the
// backend costs, which is why the scoreboard takes them and this does not.
func (s *Server) delivered(src efficiency.Source, bytes int64) {
	if s.reg == nil {
		return
	}
	where := "backend"
	if src == efficiency.Tier {
		where = "tier"
		_ = s.reg.Add(metrics.TierHits, nil, 1)
	}
	_ = s.reg.Add(metrics.ReadBytes, metrics.Labels{"source": where}, float64(bytes))
}

// measure records one read for the scoreboard.
//
// Every read that reaches here returned bytes, since a zero-length read is
// answered before any block is fetched. A read of nothing would need no guard
// even so: the scoreboard buckets by size, so it would be evidence only about
// other reads of nothing.
func (s *Server) measure(src efficiency.Source, bytes int64, took time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.score.Record(efficiency.Read{Source: src, Bytes: bytes, Took: took})
}

// Efficiency reports what the tier saved, with the account of itself that
// RFC-0024 requires travelling with it.
func (s *Server) Efficiency() efficiency.Estimate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.score.Estimate()
}

// record updates the counters under the lock.
func (s *Server) record(f func(*Stats)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.stats)
}

// Stats reports what the read path has done.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}
