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

// Tick is what one pass of the watch did.
type Tick struct {
	// Observed is the pressure that drove this tick, or a zero need.
	Observed Pressure
	// Reclaimed is what was actually returned.
	Reclaimed pool.Bytes
	// Shortfall is what compute still needs and the node could not supply.
	// Non-zero means the node is in the state it would have been in with no
	// lending at all, and the operator has to be able to see that.
	Shortfall pool.Bytes
}

// Watch keeps free space above the headroom target until ctx is done, calling
// report after every pass.
//
// Polling is the only input built. RFC-0004 wants three, and the two that
// would give warning before a workload writes anything need Kubernetes, so
// this watch is reactive: it learns about pressure once the space has already
// gone.
//
// A pass that fails does not end the watch. Abandoning it would leave the node
// unwatched exactly when it may be under pressure, and a failure that persists
// stops the heartbeat, which is what liveness is for.
func (a *Agent) Watch(ctx context.Context, cfg WatchConfig, free FreeSpace, report func(Tick, error)) error {
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
		report(a.Step(cfg, free, time.Now()))
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
func (a *Agent) Step(cfg WatchConfig, free FreeSpace, now time.Time) (Tick, error) {
	var t Tick
	available, err := free()
	if err != nil {
		return t, fmt.Errorf("agent: measuring free space: %w", err)
	}
	if short := cfg.Headroom - available; short > 0 {
		t.Observed = Pressure{Source: "free space", Need: short}
	}
	t.Observed = Shortfall(t.Observed)

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
