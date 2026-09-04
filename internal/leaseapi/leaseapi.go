// Package leaseapi is the wire between a control plane that proposes leases
// and a node agent that decides whether they are real.
//
// RFC-0005 puts the authority at the node: a grant is a proposal, accepted
// only if the node's own accounting says the capacity exists. That inversion
// is what this protocol has to preserve, so a proposal here is a request that
// can be refused for reasons the node alone knows, and a refusal carries which
// reason it was.
//
// The types live in one package because the agent serves them and the control
// plane sends them. Two copies of a wire format drift, and the drift shows up
// as a node refusing something nobody meant to propose.
package leaseapi

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

// Proposal is a control plane asking a node to lend capacity.
type Proposal struct {
	// ID identifies the lease. A control plane that retries after a timeout
	// sends the same one, which is what makes a proposal idempotent.
	ID string `json:"id"`
	// Tenant is who the capacity is for, and is what a quota is counted
	// against. A node under a quota refuses a proposal that does not name one.
	Tenant string `json:"tenant"`
	// Class is how fast the capacity can be taken back.
	Class string `json:"class"`
	// Bytes is how much is asked for.
	Bytes int64 `json:"bytes"`
	// Term is how long the node is asked to hold it, as a duration rather than
	// an expiry: a control plane with a skewed clock cannot extend a lease
	// past what the node agreed to.
	Term string `json:"term"`
}

// Lease turns a proposal into what the node's own manager accepts.
func (p Proposal) Lease() (lease.Lease, error) {
	class, err := lease.ParseClass(p.Class)
	if err != nil {
		return lease.Lease{}, err
	}
	term, err := time.ParseDuration(p.Term)
	if err != nil {
		return lease.Lease{}, fmt.Errorf("leaseapi: term %q: %w", p.Term, err)
	}
	if p.Bytes <= 0 {
		return lease.Lease{}, fmt.Errorf("leaseapi: %d bytes is not capacity to lend", p.Bytes)
	}
	return lease.Lease{
		ID: p.ID, Tenant: p.Tenant, Class: class,
		Size: pool.Bytes(p.Bytes), Term: term,
	}, nil
}

// Decision is what the node did with a proposal.
//
// A refusal is an answer rather than an error: the control plane asked a
// question the node is the authority on, and "no, because this node is
// churning" is the node working correctly.
type Decision struct {
	// Granted says the capacity now exists on that node.
	Granted bool `json:"granted"`
	// Already says the node was already holding this lease, which is what a
	// retried proposal gets. Reported apart from a fresh grant so a control
	// plane can tell a retry that worked from a first attempt that did.
	Already bool `json:"already,omitempty"`
	// Reason is why not, in the node's own words. Empty when granted.
	Reason string `json:"reason,omitempty"`
	// Refusal names the kind of no, so a control plane can act on it without
	// parsing prose: somewhere else may have room, and waiting helps a node
	// that is in cooldown.
	Refusal Refusal `json:"refusal,omitempty"`
}

// Refusal is why a node said no, in the few kinds a caller can act on.
type Refusal string

const (
	// NoCapacity means this node does not have the space. Another might.
	NoCapacity Refusal = "no-capacity"
	// Backing off means the node is churning or in its post-reclaim cooldown.
	// The same proposal may succeed later, and hammering makes it worse.
	BackingOff Refusal = "backing-off"
	// OverQuota means the tenant has as much as it may hold here.
	OverQuota Refusal = "over-quota"
	// Malformed means the proposal could not be read, which no retry fixes.
	Malformed Refusal = "malformed"
	// Unavailable means the node could not answer, which says nothing about
	// whether it would have.
	Unavailable Refusal = "unavailable"
)

// Capacity is what a node says about its own pool.
//
// RFC-0005 makes the control plane's view of fleet capacity a cache of this,
// always slightly stale, and says the reporting has to admit that rather than
// present it as exact. So the answer carries when the node measured it.
type Capacity struct {
	Bytes      int64     `json:"bytes"`
	Reserved   int64     `json:"reserved"`
	Borrowed   int64     `json:"borrowed"`
	Free       int64     `json:"free"`
	MeasuredAt time.Time `json:"measuredAt"`
}

// Age is how stale this reading is, which is the number a planner should
// consider rather than the timestamp.
func (c Capacity) Age(now time.Time) time.Duration { return now.Sub(c.MeasuredAt) }

// encode writes a value as the body of an answer.
func encode(v any) ([]byte, error) { return json.Marshal(v) }

// decode reads one.
func decode(b []byte, v any) error { return json.Unmarshal(b, v) }
