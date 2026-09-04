package metrics

import (
	"fmt"
	"sync"
	"time"
)

// Readiness answers whether the read path has been answering quickly enough.
//
// RFC-0015's worst failure is a component that is slow rather than dead: it
// passes every liveness probe while every client waits on it. So readiness is
// computed from observed service time and never from a request that succeeded,
// because a request that succeeded slowly is the failure.
type Readiness struct {
	// fail is the service time at which a node stops taking work, and recover
	// the lower one at which it starts again. Two bounds rather than one,
	// because a node that crosses a single bound alternately is removed and
	// restored repeatedly, and every removal moves work that then moves back.
	fail, recover time.Duration
	// window is how much recent history decides it. Long enough that one slow
	// read does not remove a node, short enough that a node which recovered is
	// not held out by what it did minutes ago.
	window time.Duration

	mu      sync.Mutex
	samples []sample
	unready bool
	since   time.Time
}

type sample struct {
	at   time.Time
	took time.Duration
}

// NewReadiness builds one, refusing bounds that could not settle.
func NewReadiness(fail, recover, window time.Duration) (*Readiness, error) {
	switch {
	case fail <= 0 || recover <= 0:
		return nil, fmt.Errorf("metrics: readiness needs both bounds, got fail %s and recover %s", fail, recover)
	case recover >= fail:
		return nil, fmt.Errorf("metrics: recovering at %s and failing at %s is one bound, so a marginal node would flap between them", recover, fail)
	case window <= 0:
		return nil, fmt.Errorf("metrics: readiness needs a window, got %s", window)
	}
	return &Readiness{fail: fail, recover: recover, window: window}, nil
}

// Observe records how long one read took.
func (r *Readiness) Observe(took time.Duration, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, sample{at: now, took: took})
	r.trim(now)
	r.decide(now)
}

// trim drops what has fallen out of the window.
func (r *Readiness) trim(now time.Time) {
	cut := now.Add(-r.window)
	keep := 0
	for _, s := range r.samples {
		if s.at.After(cut) {
			r.samples[keep] = s
			keep++
		}
	}
	r.samples = r.samples[:keep]
}

// decide moves between ready and unready, against whichever bound applies.
//
// An empty window returns to ready, and this is load bearing rather than a
// convenience. A node taken out of service stops being sent reads, so it stops
// producing the samples that would show it had recovered: requiring evidence
// of health to return would make every removal permanent. Coming back on an
// absence of evidence is safe because the same measurement removes it again
// within one window if it is still slow.
func (r *Readiness) decide(now time.Time) {
	if len(r.samples) == 0 {
		if r.unready {
			r.unready, r.since = false, now
		}
		return
	}
	var worst time.Duration
	for _, s := range r.samples {
		if s.took > worst {
			worst = s.took
		}
	}
	switch {
	case !r.unready && worst >= r.fail:
		r.unready, r.since = true, now
	case r.unready && worst < r.recover:
		r.unready, r.since = false, now
	}
}

// Ready reports whether the node should be taking work, and why not.
//
// A node with nothing recent to judge is ready: a quiet node is not a slow one,
// and holding it out until it proves otherwise would keep an idle cluster
// permanently out of service.
func (r *Readiness) Ready(now time.Time) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trim(now)
	r.decide(now)
	if !r.unready {
		return true, ""
	}
	var worst time.Duration
	for _, s := range r.samples {
		if s.took > worst {
			worst = s.took
		}
	}
	return false, fmt.Sprintf("the slowest read in the last %s took %s, at or over the %s bound, since %s ago",
		r.window, worst.Round(time.Millisecond), r.fail, now.Sub(r.since).Round(time.Second))
}
