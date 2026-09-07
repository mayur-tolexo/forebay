// Package volumes carries CSI volume requests from the node plugin to the
// agent, and turns them into the third input to the pressure watch.
//
// RFC-0004 calls a CSI request the explicit case: watching pods gives warning
// before a workload writes, polling free space catches what that missed, and a
// volume request is compute asking for space in so many words. The watch takes
// the largest of the three and never their sum.
//
// The direction is one way, which RFC-0014 is explicit about. The node plugin
// reports and never asks: a mount that waited on a local storage daemon would
// fail a pod for a reason the pod has nothing to do with. So nothing here can
// refuse a volume, and a report that does not arrive costs the agent an
// observation rather than the pod its mount.
package volumes

import (
	"context"
	"sync"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

// Registry is what the node has been asked to serve.
//
// It is the adapter's memory: the node plugin sends events, and the watch asks
// on its own schedule for the total. The two do not meet, so something has to
// hold the state between them.
type Registry struct {
	mu sync.Mutex
	// byID rather than a running total, because a repeated publish of the
	// same volume is one volume. The orchestrator repeats a publish it did
	// not hear the answer to, and a total that added each time would climb
	// with the retries.
	byID map[string]int64
}

// NewRegistry builds an empty one.
func NewRegistry() *Registry { return &Registry{byID: map[string]int64{}} }

// Published records a volume now served on this node.
//
// A size of zero is kept rather than dropped. It is a volume that was asked
// for and whose size is not known, and forgetting it would make the node look
// like it had been asked for nothing.
func (r *Registry) Published(id string, bytes int64) {
	if id == "" {
		return
	}
	if bytes < 0 {
		bytes = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[id] = bytes
}

// Unpublished forgets one.
func (r *Registry) Unpublished(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
}

// Requested reports how many volumes are served here and how much they name.
func (r *Registry) Requested() (count int, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.byID {
		// Saturating, because the sizes come from a control plane and a
		// wrapped total would read as almost no demand at all.
		if bytes > (1<<63-1)-b {
			return len(r.byID), 1<<63 - 1
		}
		bytes += b
	}
	return len(r.byID), bytes
}

// Source turns the registry into an observation for the pressure watch.
type Source struct{ reg *Registry }

// NewSource builds one over a registry.
func NewSource(r *Registry) *Source { return &Source{reg: r} }

// Name says which observation this is, so a reclaim can say what drove it.
func (s *Source) Name() string { return "volume requests" }

// Observe reports the shortfall the volumes on this node imply.
//
// A dataset mounted here is read through the access layer, and the fast tier
// on this node is what makes that read local. So the space a volume request
// asks for is the space its dataset needs to be resident in, measured the same
// way the kubelet source measures a pod's declared request: what free space
// will be once the demand is met, against the floor.
//
// This is an upper bound, and deliberately. A job may read a fraction of what
// it mounted, in which case the tier never holds all of it and the node kept
// more free than it had to. The watch takes the largest observation and never
// the sum, so an over-count here cannot add to what free space already sees;
// it can only reclaim earlier than necessary, which RFC-0004 prefers to
// reclaiming later than possible.
func (s *Source) Observe(ctx context.Context, cfg agent.WatchConfig, available pool.Bytes) (pool.Bytes, error) {
	_, bytes := s.reg.Requested()
	return cfg.Headroom - (available - pool.Bytes(bytes)), ctx.Err()
}
