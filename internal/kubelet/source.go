package kubelet

import (
	"context"
	"fmt"
	"strings"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

// Source turns what the kubelet knows into the shortfall the agent acts on.
//
// It reports what pods have asked for and not yet written, because that is the
// part free space cannot see: a pod holding a 50 GiB request and one byte on
// disk looks like nothing until it writes.
type Source struct {
	client *Client
	// capacity is the size of the filesystem pods are charged against. A pod
	// asking for more than that is asking for something the filesystem cannot
	// give, whatever the request says.
	//
	// It is a looser bound than the kubelet's own, which admits against
	// allocatable and so refuses a pod somewhat before this does. Allocatable
	// is not on offer here: it lives on the Node object, and reading that
	// would put the API server back on the reclaim path. This is the closest
	// thing the node itself reports.
	capacity int64
}

// NewSource builds a pressure source over one node's kubelet.
//
// The capacity is the size of the filesystem the kubelet charges pods against.
// Without it there is no way to tell a large request from an impossible one,
// and the API server hands out impossible ones: it clamps an over-large
// request to the largest signed 64-bit value rather than refusing it.
func NewSource(c *Client, capacity int64) (*Source, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("kubelet: a pod source needs the size of the filesystem pods are charged against, got %d", capacity)
	}
	return &Source{client: c, capacity: capacity}, nil
}

// Name says which observation this is.
func (s *Source) Name() string { return "pod requests" }

// Observe reports the shortfall once the pods on this node write what they
// asked for.
//
// Only live pods count. A finished or failed pod will write nothing more, and
// reclaiming against its request would take a job's cache for a demand that no
// longer exists.
func (s *Source) Observe(ctx context.Context, cfg agent.WatchConfig, available pool.Bytes) (pool.Bytes, error) {
	pods, unread := s.client.Pods(ctx)
	if unread != nil && len(pods) == 0 {
		return 0, unread
	}
	var unwritten int64
	var impossible []string
	for _, p := range pods {
		if !p.Live {
			continue
		}
		// A pod asking for more than the whole filesystem is not a demand
		// anyone is going to meet, and the scheduler would not have admitted
		// one that meant it. It is what the API server leaves behind when it
		// clamps an over-large request instead of refusing it, so it is
		// dropped for the same reason an unreadable request is: it is not
		// evidence about what will be written. Counting it would reclaim
		// every lease on the node for a demand that does not exist.
		if p.Declared > s.capacity {
			impossible = append(impossible, fmt.Sprintf("%s/%s asks for %s, more than the %s filesystem it would be written to",
				p.Namespace, p.Name, pool.Bytes(p.Declared), pool.Bytes(s.capacity)))
			continue
		}
		unwritten = addSaturating(unwritten, p.Unwritten())
	}

	// What free space will be once they write it, measured against the target.
	need := cfg.Headroom - (available - pool.Bytes(unwritten))
	if err := join(unread, impossible); err != nil {
		// The need stands as a floor: the pods that were read still hold what
		// they hold, and dropping that because another pod was unreadable
		// would lose a real shortfall.
		return need, &agent.Partial{Err: err}
	}
	return need, nil
}

// join folds what could not be read and what could not be believed into one
// complaint, so a pass reports both or neither.
func join(unread error, impossible []string) error {
	switch {
	case unread != nil && len(impossible) > 0:
		return fmt.Errorf("%w; and %d pod(s) asking for more than the filesystem holds, so what they hold is not counted: %s",
			unread, len(impossible), strings.Join(impossible, "; "))
	case unread != nil:
		return unread
	case len(impossible) > 0:
		return fmt.Errorf("kubelet: %d pod(s) asking for more than the filesystem holds, so what they hold is not counted: %s",
			len(impossible), strings.Join(impossible, "; "))
	default:
		return nil
	}
}
