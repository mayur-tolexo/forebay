// Package checkpoint says what an acknowledgement means and refuses a staging
// request that would make a reclaim into a data loss.
//
// RFC-0013 opens by naming its own danger: a checkpoint reported complete
// before it is durable is a correctness problem dressed as a performance win,
// and the difference only shows when a node is lost. So there are two words,
// the safe one is the default, and the fast one names what it costs.
package checkpoint

import (
	"errors"
	"fmt"

	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

// Ack is what a writer waits for.
type Ack string

const (
	// Durable means the backend has the bytes and its guarantee applies. The
	// default, because a user who has not chosen gets the one that cannot
	// lose their work.
	Durable Ack = "durable"
	// Staged means the bytes are on this node in capacity nobody may take.
	// They survive the process and a restart of the agent, and they do not
	// survive the node.
	Staged Ack = "staged"
)

// Survives says what an acknowledgement withstands, in the words the document
// uses, so a message to a user is the same as the document they read.
func (a Ack) Survives() (survives, lost string, err error) {
	switch a {
	case Durable:
		return "whatever the backend survives", "whatever the backend does not", nil
	case Staged:
		return "the process, and a restart of the agent", "the node", nil
	default:
		return "", "", fmt.Errorf("checkpoint: %q is not an acknowledgement this project offers, which are %s and %s",
			a, Durable, Staged)
	}
}

// WithDefault fills an unstated policy with the safe one.
func (a Ack) WithDefault() Ack {
	if a == "" {
		return Durable
	}
	return a
}

var (
	// ErrRevocable refuses staging into capacity somebody can take back.
	ErrRevocable = errors.New("checkpoint: staging may not use revocable capacity")
	// ErrTooLarge refuses a checkpoint bigger than the node may promise.
	ErrTooLarge = errors.New("checkpoint: larger than this node's guaranteed share")
)

// Reservation is what staging asks of a node before it writes a byte.
type Reservation struct {
	// Bytes is the whole checkpoint rather than the part written so far. A
	// rank that learns it cannot stage after it has stopped computing has
	// learned it at the worst moment.
	Bytes pool.Bytes
	// Class is how reclaimable the capacity is, and staging accepts one.
	Class lease.Class
}

// Check answers whether a node may stage this, given what it can guarantee.
//
// A checkpoint being staged is the only copy of itself, so the capacity
// holding it is capacity nobody may reclaim. That is the one exception to the
// rule that borrowed data is regenerable, and it is why staging takes a
// guaranteed lease rather than the plentiful class.
func Check(r Reservation, guaranteedFree pool.Bytes) error {
	if r.Bytes <= 0 {
		return fmt.Errorf("checkpoint: a reservation of %s stages nothing", r.Bytes)
	}
	if r.Class != lease.Guaranteed {
		// Not downgraded to what is available. A reclaim promises a deadline,
		// and copying a checkpoint to the backend does not fit inside one, so
		// the promise would break exactly when it was tested.
		return fmt.Errorf("%w: %s capacity can be taken back mid-checkpoint, and the bytes are the only copy",
			ErrRevocable, r.Class)
	}
	if r.Bytes > guaranteedFree {
		// Refused rather than partially reserved: a rank that can stage half
		// its checkpoint has to write through anyway, having wasted the wait.
		return fmt.Errorf("%w: asked for %s and %s is available",
			ErrTooLarge, r.Bytes, guaranteedFree)
	}
	return nil
}
