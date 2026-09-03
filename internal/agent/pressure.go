package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mayur-tolexo/forebay/internal/pool"
)

// ErrNoHeadroom reports a watch with no target to keep free.
var ErrNoHeadroom = errors.New("agent: headroom target must be positive")

// Pressure is one observation of how much capacity compute needs back.
type Pressure struct {
	// Source names what noticed, so a reclaim can say why it happened.
	Source string
	// Need is the shortfall this observation implies.
	Need pool.Bytes
}

// Shortfall is the largest need, never their sum.
//
// A pod's declared request shows up in polled free space once it starts
// writing, so adding the two double counts and reclaims cache the node did not
// need to lose.
func Shortfall(observed ...Pressure) Pressure {
	worst := Pressure{Source: "none"}
	for _, o := range observed {
		if o.Need > worst.Need {
			worst = o
		}
	}
	return worst
}

// WatchConfig tunes the pressure loop.
type WatchConfig struct {
	// Headroom is a fixed floor kept free on top of what is already committed.
	// It has no defensible default, so there is none: a watch with neither
	// this nor HeadroomFor is refused rather than guessed.
	Headroom pool.Bytes
	// HeadroomFor is how long the node may be behind, and is the floor
	// expressed the way it was measured. What has to be covered is what a
	// workload writes between two polls, which is a rate rather than a size:
	// the same drive was measured sixty times apart depending on whether its
	// write cache was spent, so a byte count set in one state is wrong in the
	// other.
	HeadroomFor time.Duration
	// MinHeadroom is the floor under the floor, required with HeadroomFor. A
	// node writing nothing would otherwise keep nothing free, and the next
	// burst arrives before the next poll does.
	MinHeadroom pool.Bytes
	// Interval is how often free space is polled. It trades reclaim latency
	// against idle cost.
	Interval time.Duration
}

// Validate rejects a watch that could not keep a floor.
func (c WatchConfig) Validate() error {
	switch {
	case c.Headroom <= 0 && c.HeadroomFor <= 0:
		return ErrNoHeadroom
	case c.Headroom > 0 && c.HeadroomFor > 0:
		return errors.New("agent: a fixed headroom and a duration are two floors, configure one")
	case c.HeadroomFor > 0 && c.MinHeadroom <= 0:
		return fmt.Errorf("agent: a headroom of %s needs a minimum, since a node writing nothing would keep nothing free", c.HeadroomFor)
	case c.Interval <= 0:
		return fmt.Errorf("agent: watch interval must be positive, got %s", c.Interval)
	}
	return nil
}

// rateEstimator turns successive observations into what a workload is
// consuming, which is not the same as what free space did.
//
// Free space also rises when the agent reclaims and falls when it grants, so
// between two polls it moves by what the workload took less what the agent
// gave back. The agent's own effect is the change in what it has lent, so
// subtracting that leaves the workload's.
//
// This assumes the filesystem shows a reclaim by the next poll. Measured on an
// idle filesystem it does, arriving before the first reading taken after the
// unlink returns; under concurrent writing the measurement cannot attribute
// what it sees, so the assumption holds where the node is quiet and is untested
// where it is not. Where it fails the space given up in the accounting has not
// arrived, the difference is attributed to the workload, and the floor comes
// out too large, which is the safe direction of being wrong.
type rateEstimator struct {
	free     pool.Bytes
	borrowed pool.Bytes
	at       time.Time
	have     bool
}

// observe records a sample, returning bytes a second since the previous one
// and whether there was one to compare against.
//
// Never negative: a workload that deleted more than it wrote is not consuming,
// and a floor sized from a negative rate would be the minimum anyway.
func (r *rateEstimator) observe(free, borrowed pool.Bytes, now time.Time) (float64, bool) {
	prev, prevBorrowed, at, had := r.free, r.borrowed, r.at, r.have
	r.free, r.borrowed, r.at, r.have = free, borrowed, now, true
	if !had {
		return 0, false
	}
	elapsed := now.Sub(at).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	took := float64(prev-free) + float64(prevBorrowed-borrowed)
	if took < 0 {
		took = 0
	}
	return took / elapsed, true
}

// target is the floor this pass has to keep, in bytes.
//
// A configured size is used as it stands. A duration is multiplied by what the
// workload is consuming, and never falls below the minimum, which also covers
// the first pass of a run: it has nothing to difference against and so has no
// rate.
func (c WatchConfig) target(rate float64, known bool) pool.Bytes {
	if c.Headroom > 0 {
		return c.Headroom
	}
	if !known {
		return c.MinHeadroom
	}
	if want := pool.Bytes(rate * c.HeadroomFor.Seconds()); want > c.MinHeadroom {
		return want
	}
	return c.MinHeadroom
}

// FreeSpace reports what is currently free on the pools' filesystem.
type FreeSpace func() (pool.Bytes, error)

// Source is something that can see compute wanting its capacity back.
//
// Free space is polled by the watch itself. A source is anything that knows
// sooner: a Kubernetes adapter watching pods, and later a Slurm one. The agent
// has no opinion about where an observation came from, which is what keeps an
// orchestrator's types out of it.
type Source interface {
	// Name says which observation this is, so a reclaim can say what drove it.
	Name() string
	// Observe reports the shortfall this source sees, given what is free now.
	Observe(ctx context.Context, cfg WatchConfig, available pool.Bytes) (pool.Bytes, error)
}

// minObserve is the least time a source is given to answer, however short the
// interval. Half of a brisk interval is not enough to read anything over a
// network, and a bound that no source can meet turns every pass degraded while
// looking like the sources are broken.
const minObserve = time.Second

// observe asks one source, bounded by a fraction of the interval. A source
// that hangs would otherwise hold up the reclaim decision for free space too,
// which is the input most likely to still be right when a source is
// unreachable.
func observe(ctx context.Context, s Source, cfg WatchConfig, available pool.Bytes) (pool.Bytes, error) {
	if cfg.Interval <= 0 {
		return s.Observe(ctx, cfg, available)
	}
	bound := cfg.Interval / 2
	if bound < minObserve {
		bound = minObserve
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	return s.Observe(ctx, cfg, available)
}

// Partial wraps an error a source returns alongside a need it could still
// work out. The need is a floor rather than the whole answer, so the watch
// acts on it and reports what was missed, which is better than discarding a
// real shortfall because part of the source was unreadable.
type Partial struct{ Err error }

// Error reports what could not be read.
func (p *Partial) Error() string { return p.Err.Error() }

// Unwrap gives up the underlying error.
func (p *Partial) Unwrap() error { return p.Err }

// Tick is what one pass of the watch did.
type Tick struct {
	// Observed is the pressure that drove this tick, or a zero need.
	Observed Pressure
	// Reclaimed is what was actually returned.
	Reclaimed pool.Bytes
	// Degraded names sources that could not be read this pass. The watch
	// continues on what it has, and says which eye it lost.
	Degraded []string
	// Shortfall is what compute still needs and the node could not supply.
	// Non-zero means the node is in the state it would have been in with no
	// lending at all, and the operator has to be able to see that.
	Shortfall pool.Bytes
	// Target is the floor this pass kept, which moves when it is configured as
	// a duration. Reported because a reclaim is otherwise unexplainable: the
	// same free space is fine at one moment and short at the next.
	Target pool.Bytes
	// Rate is what the workload was observed to consume, in bytes a second,
	// and is zero on the first pass, which has nothing to difference.
	Rate float64
}

// Watch keeps free space above the headroom target until ctx is done, calling
// report after every pass.
//
// Free space is always polled. Sources add the inputs that give warning before
// a workload writes anything, and with none the watch is reactive: it learns
// about pressure once the space has already gone.
//
// A pass that fails does not end the watch. Abandoning it would leave the node
// unwatched exactly when it may be under pressure, and a failure that persists
// stops the heartbeat, which is what liveness is for.
func (a *Agent) Watch(ctx context.Context, cfg WatchConfig, free FreeSpace, report func(Tick, error), sources ...Source) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if report == nil {
		return errors.New("agent: watch needs somewhere to report, or a reclaim happens in silence")
	}
	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()
	for {
		report(a.Step(ctx, cfg, free, time.Now(), sources...))
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Step runs one pass: measure, reclaim what is missing, and report progress.
//
// The heartbeat is written last, so a pass that failed partway does not claim
// the agent is making progress.
//
// It remembers the previous pass, since a floor configured as a duration is
// sized from what changed between two of them, so it belongs to one loop and
// two goroutines calling it would difference each other's samples.
func (a *Agent) Step(ctx context.Context, cfg WatchConfig, free FreeSpace, now time.Time, sources ...Source) (Tick, error) {
	var t Tick
	available, err := free()
	if err != nil {
		return t, fmt.Errorf("agent: measuring free space: %w", err)
	}
	// The workload's rate, not what free space did: a reclaim raises free
	// space, and reading that as consumption would shrink the floor in the
	// pass that had just proved it too small.
	rate, known := a.rate.observe(available, a.Accounting().Borrowed, now)
	t.Target = cfg.target(rate, known)
	t.Rate = rate
	// Sources are given the floor this pass settled on rather than the
	// configuration, since one of them reads it to work out its own shortfall
	// and two floors in one pass would reclaim to whichever it consulted.
	effective := cfg
	effective.Headroom = t.Target
	observed := []Pressure{{Source: "free space", Need: t.Target - available}}
	for _, s := range sources {
		need, err := observe(ctx, s, effective, available)
		if err != nil {
			t.Degraded = append(t.Degraded, fmt.Sprintf("%s: %v", s.Name(), err))
			// A partial read still carries a need worth acting on. Anything
			// else saw nothing, and one source failing does not blind the
			// others, so the watch drops it and keeps going on free space.
			var partial *Partial
			if !errors.As(err, &partial) {
				continue
			}
		}
		observed = append(observed, Pressure{Source: s.Name(), Need: need})
	}
	t.Observed = Shortfall(observed...)

	if t.Observed.Need > 0 {
		rec, err := a.ReclaimCapacity(t.Observed.Need, now)
		t.Reclaimed = rec.Result.Reclaimed
		t.Shortfall = rec.Result.Shortfall
		// A missed deadline is a broken promise rather than a failed pass, and
		// the capacity came back either way, so the watch reports it and
		// continues rather than stopping the node's only reclaim path.
		if err != nil && !errors.Is(err, ErrDeadlineMissed) {
			return t, err
		}
	}
	return t, a.Heartbeat(now)
}
