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
	// Headroom is the floor kept free on top of what is already committed.
	// Sizing it trades a burst of writes beating the reclaim against lending
	// less than the node could, and it has no defensible default, so there is
	// none: a watch without one is refused rather than guessed.
	Headroom pool.Bytes
	// Interval is how often free space is polled. It trades reclaim latency
	// against idle cost.
	Interval time.Duration
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
	if cfg.Headroom <= 0 {
		return ErrNoHeadroom
	}
	if cfg.Interval <= 0 {
		return fmt.Errorf("agent: watch interval must be positive, got %s", cfg.Interval)
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
func (a *Agent) Step(ctx context.Context, cfg WatchConfig, free FreeSpace, now time.Time, sources ...Source) (Tick, error) {
	var t Tick
	available, err := free()
	if err != nil {
		return t, fmt.Errorf("agent: measuring free space: %w", err)
	}
	observed := []Pressure{{Source: "free space", Need: cfg.Headroom - available}}
	for _, s := range sources {
		need, err := observe(ctx, s, cfg, available)
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
