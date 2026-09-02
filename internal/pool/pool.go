// Package pool tracks how one node's local NVMe is divided, and enforces the
// arithmetic that division has to satisfy.
package pool

import (
	"errors"
	"fmt"
)

// Bytes is a capacity quantity. Signed rather than unsigned so that a
// subtraction that goes negative is a detectable defect instead of a very
// large positive number.
type Bytes int64

// Common capacity units, for callers building Accounting values.
const (
	KiB Bytes = 1 << 10
	MiB Bytes = 1 << 20
	GiB Bytes = 1 << 30
	TiB Bytes = 1 << 40
)

// String renders a capacity in the largest unit that keeps it readable.
func (b Bytes) String() string {
	switch {
	case b >= TiB:
		return fmt.Sprintf("%.2fTiB", float64(b)/float64(TiB))
	case b >= GiB:
		return fmt.Sprintf("%.2fGiB", float64(b)/float64(GiB))
	case b >= MiB:
		return fmt.Sprintf("%.2fMiB", float64(b)/float64(MiB))
	case b >= KiB:
		return fmt.Sprintf("%.2fKiB", float64(b)/float64(KiB))
	default:
		return fmt.Sprintf("%dB", int64(b))
	}
}

// Errors returned when a node's capacity does not add up, or cannot satisfy a
// request. Callers distinguish these with errors.Is.
var (
	ErrNegative     = errors.New("pool: negative capacity")
	ErrOvercommit   = errors.New("pool: pools exceed device capacity")
	ErrInsufficient = errors.New("pool: insufficient free capacity")
	// ErrOverRelease is returning more than was ever lent. It is distinct from
	// ErrNegative because the input is perfectly valid and the accounting is
	// what has gone wrong, and callers need to tell those apart.
	ErrOverRelease = errors.New("pool: returning more than is borrowed")
)

// Accounting is one node's view of its own capacity, split three ways.
//
// The node agent owns this rather than the control plane, because the agent is
// the only party that can see the device. A control plane view is a cache of
// it and may be stale.
type Accounting struct {
	// Capacity is the total the device provides.
	Capacity Bytes
	// Reserved is what this filesystem already holds for everything that is
	// not Forebay: the operating system, container images, the workload, and
	// any durable data donated to another store that happens to live here. It
	// is measured rather than declared, and it is never lent.
	//
	// One term rather than several, because Forebay treats them identically
	// and cannot tell them apart anyway: they are all bytes on the device that
	// belong to somebody else.
	Reserved Bytes
	// Borrowed is lent revocably and holds only regenerable data.
	Borrowed Bytes
}

// Free reports capacity belonging to no pool. It can go negative when the
// accounting is inconsistent, which Validate reports as a defect rather than
// silently clamping, since capacity arithmetic that does not balance is a bug.
func (a Accounting) Free() Bytes {
	return a.Capacity - a.Reserved - a.Borrowed
}

// Validate reports whether the three pools plus free space still equal device
// capacity. A discrepancy is surfaced rather than corrected: in a system whose
// promise is about capacity, arithmetic that does not balance is a defect.
func (a Accounting) Validate() error {
	// Ordered rather than ranged over a map, so that a node with two bad
	// fields always reports the same one and the message is testable.
	for _, f := range []struct {
		name string
		v    Bytes
	}{
		{"capacity", a.Capacity}, {"reserved", a.Reserved},
		{"borrowed", a.Borrowed},
	} {
		if f.v < 0 {
			return fmt.Errorf("%w: %s is %d", ErrNegative, f.name, f.v)
		}
	}
	if a.Free() < 0 {
		return fmt.Errorf("%w: %s allocated of %s",
			ErrOvercommit, a.Reserved+a.Borrowed, a.Capacity)
	}
	return nil
}

// CanLend reports whether n more bytes could be lent without overcommitting.
func (a Accounting) CanLend(n Bytes) bool {
	return n >= 0 && a.Free() >= n
}

// Lend records n bytes as borrowed, refusing rather than overcommitting.
//
// This is where the agent declines a control-plane grant it cannot honour. The
// control plane proposes; only the agent has the arithmetic to accept.
func (a *Accounting) Lend(n Bytes) error {
	if n < 0 {
		return fmt.Errorf("%w: cannot lend %d", ErrNegative, n)
	}
	if !a.CanLend(n) {
		return fmt.Errorf("%w: need %s, free %s", ErrInsufficient, n, a.Free())
	}
	a.Borrowed += n
	return nil
}

// Return releases n borrowed bytes back to free space.
func (a *Accounting) Return(n Bytes) error {
	if n < 0 {
		return fmt.Errorf("%w: cannot return %d", ErrNegative, n)
	}
	if n > a.Borrowed {
		return fmt.Errorf("%w: returning %s of %s borrowed", ErrOverRelease, n, a.Borrowed)
	}
	a.Borrowed -= n
	return nil
}

// Reclaimable is the most that could ever be handed back to compute: every
// borrowed byte. Donated capacity is excluded because it is never reclaimed,
// which is what bounds how far a node can be recovered under pressure.
func (a Accounting) Reclaimable() Bytes {
	return a.Borrowed
}
