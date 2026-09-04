// Package residency turns how much of a dataset a node holds into something
// stable enough to publish as a node label.
//
// RFC-0022 chooses labels over a scheduler plugin so that nothing in the
// placement path calls Forebay. The cost of that choice is that the signal has
// to survive being written to the API server, and a cache under reclamation
// pressure moves continuously: a label carrying a percentage would be
// rewritten on every admission and eviction, for every dataset on every node.
package residency

import (
	"fmt"
	"hash/fnv"
)

// Level is how much of a dataset a node or rack holds, coarsely.
//
// Three levels rather than ten, because a scheduler weighing an affinity does
// not need precision it cannot act on: what it needs is whether this would be
// a warm start, a partial one, or a cold one.
type Level int

const (
	// None is not worth scheduling towards.
	None Level = iota
	// Some is a partial warm start.
	Some
	// Most is a warm start.
	Most
)

// rise and fall are the boundaries in each direction. They differ on purpose:
// a node whose residency sits at a threshold would otherwise rewrite its
// labels every time a block arrived or left.
var (
	rise = map[Level]float64{Some: 0.25, Most: 0.75}
	fall = map[Level]float64{Some: 0.20, Most: 0.70}
)

// String is the label value. None is empty, because a node holding nothing
// carries no label rather than a label saying so.
func (l Level) String() string {
	switch l {
	case Some:
		return "some"
	case Most:
		return "most"
	default:
		return ""
	}
}

// Next returns the level to publish, given the level already published and
// what the node now holds.
//
// Rising and falling use different thresholds, so crossing a boundary and
// crossing back are not the same number.
func Next(current Level, fraction float64) Level {
	switch {
	case fraction >= rise[Most]:
		return Most
	case fraction >= rise[Some]:
		// Already at Most and still above its fall line: stay, since coming
		// down needs to cross the lower threshold rather than the upper one.
		if current == Most && fraction >= fall[Most] {
			return Most
		}
		return Some
	case fraction >= fall[Some]:
		// Between the two lines: hold whatever was published rather than
		// choosing, which is the whole of the hysteresis.
		if current >= Some {
			return current
		}
		return None
	default:
		return None
	}
}

// prefix namespaces the label keys this project writes.
const prefix = "forebay.io/"

// Key is the label key for one dataset's residency.
//
// Hashed because a Kubernetes label key's name is bounded at 63 characters and
// a tenant and dataset name together are not. A collision reports one
// dataset's residency for another, which is a worse scheduling hint and never
// a wrong read: nothing on the read path consults a label.
func Key(tenant, dataset string) string {
	h := fnv.New64a()
	// Separated, so that a tenant and dataset whose names run together cannot
	// collide with a different pair that concatenates the same way.
	h.Write([]byte(tenant))
	h.Write([]byte{0})
	h.Write([]byte(dataset))
	return fmt.Sprintf("%sresident-%016x", prefix, h.Sum64())
}

// RackKey is the label key for a rack's residency, which is what a
// gang-scheduled job matches.
//
// A rack that holds the data on one node out of eight is not a warm start for
// a job that needs all eight, so scoring nodes for a gang scores the wrong
// unit.
func RackKey(tenant, dataset string) string {
	return Key(tenant, dataset) + "-rack"
}

// Fraction is how much of a dataset is resident, guarding the arithmetic
// rather than trusting the caller.
//
// A dataset of no bytes is not resident: reporting it as fully held would make
// every empty dataset a warm start everywhere.
func Fraction(resident, total int64) (float64, error) {
	switch {
	case total <= 0:
		return 0, fmt.Errorf("residency: a dataset of %d bytes has no residency to report", total)
	case resident < 0:
		return 0, fmt.Errorf("residency: %d resident bytes is not a quantity", resident)
	case resident > total:
		// Held rather than clamped silently: it means the caller's idea of the
		// dataset's size disagrees with what the tier holds, and a scheduling
		// hint built on that is worth refusing.
		return 0, fmt.Errorf("residency: %d bytes resident of a %d byte dataset", resident, total)
	}
	return float64(resident) / float64(total), nil
}
