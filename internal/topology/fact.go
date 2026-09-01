// Package topology models what a node's hardware looks like, and is careful
// about the difference between a fact, a fact somebody asserted, and no fact
// at all.
//
// On real hardware most of what this package wants is unavailable: a probe of
// a GPU node found NUMA affinity reported as -1 on every PCI device and no
// rack label of any kind. Representing that honestly, rather than defaulting
// it to something plausible, is the point of the package.
package topology

// Provenance says where a fact came from, and therefore what may be done with
// it. The zero value is Unknown, so a fact nobody filled in is never mistaken
// for a fact somebody established.
type Provenance uint8

const (
	// Unknown means asked and not answered. It is not a value.
	Unknown Provenance = iota
	// Discovered means the node described itself, which is trusted about the
	// node.
	Discovered
	// Declared means an operator said so, which is the only source for
	// anything above the node such as a rack.
	Declared
)

// String names the provenance.
func (p Provenance) String() string {
	switch p {
	case Discovered:
		return "discovered"
	case Declared:
		return "declared"
	default:
		return "unknown"
	}
}

// Fact is a value together with where it came from.
//
// The value is unexported so that reading it requires acknowledging whether it
// exists. A kernel returning -1 for NUMA affinity is not saying zero, and code
// that reads it as a number places every device as though it shared one NUMA
// node, which is the defect this type exists to make unwriteable.
type Fact[T any] struct {
	value T
	prov  Provenance
}

// Known returns the value and whether there is one. A fact that is not known
// yields the zero value, which the caller must not use.
func (f Fact[T]) Known() (T, bool) {
	if f.prov == Unknown {
		var zero T
		return zero, false
	}
	return f.value, true
}

// Provenance reports where the fact came from.
func (f Fact[T]) Provenance() Provenance { return f.prov }

// Or returns the value if known and the fallback otherwise. It exists for
// display and reporting, never for placement: a fallback used in a placement
// decision is exactly the invented value this package refuses.
func (f Fact[T]) Or(fallback T) T {
	if v, ok := f.Known(); ok {
		return v
	}
	return fallback
}

// DiscoveredValue records something the node said about itself.
func DiscoveredValue[T any](v T) Fact[T] { return Fact[T]{value: v, prov: Discovered} }

// DeclaredValue records something an operator asserted.
func DeclaredValue[T any](v T) Fact[T] { return Fact[T]{value: v, prov: Declared} }

// UnknownValue records that the question was asked and not answered.
func UnknownValue[T any]() Fact[T] { return Fact[T]{} }
