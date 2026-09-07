package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

var (
	// ErrOverran reports a checkpoint bigger than the reservation made for it.
	// RFC-0013 calls this the framework's error, and it is reported as one.
	ErrOverran = errors.New("checkpoint: larger than its reservation")
	// ErrNotStaged reports an upload asked for before anything was staged.
	ErrNotStaged = errors.New("checkpoint: nothing has been staged")
	// ErrRequest reports a request that names nothing, or names an
	// acknowledgement this project does not offer. No other node would
	// accept it either, so it is the writer's to fix rather than a reason to
	// try elsewhere.
	ErrRequest = errors.New("checkpoint: the request cannot be acted on")
)

// Node is what staging needs of the agent that owns this node's capacity.
//
// An interface because the agent is the authority on capacity and this is not:
// staging asks, and the answer it gets is the one the reclaim ladder will
// honour.
type Node interface {
	// GuaranteedFree is how much more the node may promise not to take back.
	GuaranteedFree() pool.Bytes
	// Grant reserves capacity and creates the extent it lives in.
	Grant(l lease.Lease, now time.Time) error
	// Release gives it back.
	Release(leaseID string, now time.Time) (pool.Bytes, error)
	// ExtentPath is where a lease's capacity lives on disk.
	ExtentPath(leaseID string) (string, error)
}

// Store is what staging needs of the durable backend.
//
// Only the streaming write. A checkpoint is on this node's disk by the time it
// is uploaded, and holding it in memory as well is the thing RFC-0006's
// write-stream capability was added to avoid.
type Store interface {
	WriteObjectFrom(ctx context.Context, object string, src io.ReadSeeker, size int64) error
}

// Config wires a stager to a node and a backend.
type Config struct {
	Node  Node
	Store Store
	// Term is how long a staging lease runs before it expires. It is a
	// backstop rather than a schedule: an upload that never finishes should
	// leave a lease an operator can see, and one that runs forever is a node
	// that has promised capacity to a job that is gone.
	Term time.Duration
	// Now is the clock, so a test does not wait.
	Now func() time.Time
}

// Stager stages checkpoints on one node and makes them durable.
type Stager struct{ cfg Config }

// New builds one.
func New(cfg Config) (*Stager, error) {
	switch {
	case cfg.Node == nil:
		return nil, errors.New("checkpoint: staging needs a node to reserve capacity from")
	case cfg.Store == nil:
		return nil, errors.New("checkpoint: staging needs a backend to make bytes durable in")
	case cfg.Term <= 0:
		return nil, fmt.Errorf("checkpoint: a staging lease of %s would expire before it was written", cfg.Term)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Stager{cfg: cfg}, nil
}

// Request is one checkpoint.
type Request struct {
	// ID names the staging lease, and is what an operator sees holding
	// capacity if the upload never completes.
	ID string
	// Tenant is who the capacity is counted against.
	Tenant string
	// Object is where the bytes go in the durable backend.
	Object string
	// Bytes is the whole checkpoint, reserved before the first byte is
	// written. RFC-0013 reserves for the whole rather than the part written
	// so far, because a rank that learns it cannot stage after it has stopped
	// computing has learned it at the worst moment.
	Bytes pool.Bytes
	// Ack is what the writer waits for. Empty means the safe one.
	Ack Ack
}

// Checkpoint is one staged checkpoint and what became of it.
type Checkpoint struct {
	// Ack is what was actually achieved when Stage returned.
	ack Ack
	// done closes when the upload has finished, one way or the other.
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

// Ack reports what the writer was given.
func (c *Checkpoint) Ack() Ack { return c.ack }

// Wait blocks until the bytes are durable, and reports what stopped them.
//
// For a durable acknowledgement this has already happened and Wait returns at
// once. For a staged one it is how a writer that wants to know eventually can
// find out, without having waited for it.
func (c *Checkpoint) Wait(ctx context.Context) error {
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finish records how the upload ended, once.
func (c *Checkpoint) finish(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
}

// Stage reserves capacity, writes the checkpoint into it, and makes it durable
// according to the policy asked for.
//
// The order is the one RFC-0013 argues for. Reserving happens before the first
// byte, so a rank that cannot stage learns it before it has stopped computing
// and can write straight through instead. Reserving during the write means a
// rank stops, starts, fails part way, and then does the slow thing anyway with
// the checkpoint half written.
func (s *Stager) Stage(ctx context.Context, r Request, src io.Reader) (*Checkpoint, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	ack := r.Ack.WithDefault()
	if _, _, err := ack.Survives(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRequest, err)
	}

	// Asked before anything is granted, so the refusal names what was
	// available rather than what the lease manager happened to say.
	if err := Check(Reservation{Bytes: r.Bytes, Class: lease.Guaranteed}, s.cfg.Node.GuaranteedFree()); err != nil {
		return nil, err
	}

	now := s.cfg.Now()
	l := lease.Lease{
		ID:     r.ID,
		Tenant: r.Tenant,
		// Guaranteed and never anything else. A checkpoint being staged is
		// the only copy of itself, so a reclaim taking it would break the
		// constraint the whole project rests on, in the one place where the
		// data is worth the most.
		Class: lease.Guaranteed,
		Size:  r.Bytes,
		Term:  s.cfg.Term,
	}
	if err := s.cfg.Node.Grant(l, now); err != nil {
		return nil, fmt.Errorf("checkpoint: reserving %s for %s: %w", r.Bytes, r.ID, err)
	}

	path, err := s.cfg.Node.ExtentPath(r.ID)
	if err != nil {
		s.release(r.ID, "the extent could not be named")
		return nil, err
	}
	written, err := stageTo(path, src, int64(r.Bytes))
	if err != nil {
		// Released, because there is nothing worth keeping: a checkpoint that
		// overran its reservation or failed to write is not a fallback for
		// anything, and holding capacity for it would hold it forever.
		s.release(r.ID, "the checkpoint could not be staged")
		return nil, err
	}
	if written == 0 {
		// Also released. Holding capacity is for a staged checkpoint that is
		// the only copy of itself, and nothing was staged: a writer that
		// declared a size and sent no bytes would otherwise leave a lease
		// nobody will ever finish with.
		s.release(r.ID, "nothing was staged")
		return nil, fmt.Errorf("%w: %s reserved %s and sent none", ErrNotStaged, r.ID, r.Bytes)
	}

	c := &Checkpoint{ack: ack, done: make(chan struct{})}
	upload := func(ctx context.Context) {
		err := s.durable(ctx, r, path, written)
		if err == nil {
			// Released only now. RFC-0013 is explicit that staging capacity
			// is held until the bytes are durable and not before, because
			// until then it holds the only copy.
			s.release(r.ID, "the checkpoint is durable")
		}
		c.finish(err)
	}

	if ack == Durable {
		upload(ctx)
		if err := c.Wait(ctx); err != nil {
			return nil, err
		}
		return c, nil
	}
	// Staged: the writer has its acknowledgement and the upload carries on
	// without it. The context is deliberately not the caller's, which is
	// about to be cancelled by a writer that has been told it can stop
	// waiting.
	go upload(context.WithoutCancel(ctx))
	return c, nil
}

// durable uploads what was staged.
//
// The staged file is reopened rather than kept open across the acknowledgement,
// so a staged checkpoint holds capacity and a file descriptor for as long as
// the upload takes rather than for as long as the writer lives.
func (s *Stager) durable(ctx context.Context, r Request, path string, written int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("checkpoint: reading what was staged for %s: %w", r.ID, err)
	}
	defer f.Close()

	if err := s.cfg.Store.WriteObjectFrom(ctx, r.Object, io.NewSectionReader(f, 0, written), written); err != nil {
		// Not released. The bytes are still the only copy, and giving the
		// capacity back would let a reclaim take them. An operator sees a
		// lease that is not shrinking, which is RFC-0013's own answer to an
		// upload that never completes.
		return fmt.Errorf("checkpoint: making %s durable: %w", r.Object, err)
	}
	return nil
}

// release gives staging capacity back, reporting rather than returning a
// failure: every caller of this is already on its way somewhere else, and a
// lease that could not be released is the node's problem rather than the
// writer's.
func (s *Stager) release(id, why string) {
	if _, err := s.cfg.Node.Release(id, s.cfg.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint: releasing %s because %s: %v\n", id, why, err)
	}
}

// check rejects a request that names nothing.
func (r Request) check() error {
	switch {
	case r.ID == "":
		return fmt.Errorf("%w: no id, which is what holds the capacity", ErrRequest)
	case r.Object == "":
		return fmt.Errorf("%w: nowhere durable for %s to go", ErrRequest, r.ID)
	}
	return nil
}

// syncFile flushes the staged extent to the device.
//
// A variable so a test can see that it was called. Whether the bytes reached
// the platter is not something a test can observe, and dropping the call is
// something an edit can do silently, so what is pinned is the call.
var syncFile = func(f *os.File) error { return f.Sync() }

// stageTo writes a checkpoint into the extent reserved for it.
//
// One byte past the reservation is read, so a checkpoint larger than what was
// reserved is caught rather than truncated into it. RFC-0013 calls that the
// framework's error and requires it to be reported as one: a checkpoint
// silently cut to fit is a restart that reads half a model.
func stageTo(path string, src io.Reader, size int64) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("checkpoint: opening the staged extent: %w", err)
	}
	defer f.Close()

	n, err := io.Copy(f, io.LimitReader(src, size+1))
	if err != nil {
		return 0, fmt.Errorf("checkpoint: staging: %w", err)
	}
	if n > size {
		return 0, fmt.Errorf("%w: reserved %s and the checkpoint is larger", ErrOverran, pool.Bytes(size))
	}
	// Flushed before anything is acknowledged. A staged acknowledgement says
	// the bytes survive a restart of the agent, and bytes in the page cache
	// do not.
	if err := syncFile(f); err != nil {
		return 0, fmt.Errorf("checkpoint: flushing what was staged: %w", err)
	}
	return n, nil
}
