// Package intent turns what a user declared into what a backend must be able
// to do, and answers whether one can.
//
// Nine words and a table, which is the whole design: RFC-0009 opens by naming
// how intent systems fail, and both halves of that failure are vocabulary
// problems. A vague word means different things to two users; an expressive
// one is a configuration file with better marketing.
package intent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mayur-tolexo/forebay/driver"
)

// Durability says what losing it costs.
type Durability string

// Latency says where it should be read from.
type Latency string

// Cost says what may be spent holding it.
type Cost string

const (
	// DurabilityNone is scratch: losing it costs a refetch.
	DurabilityNone Durability = "none"
	// DurabilityBackend is the store's own guarantee, which is what the user
	// bought, and is the default.
	DurabilityBackend Durability = "backend"
	// DurabilityReplicated needs the store to keep more than one copy and to
	// say so.
	DurabilityReplicated Durability = "replicated"
	// DurabilityRackTolerant needs that, and a fleet that can say which rack a
	// node is in.
	DurabilityRackTolerant Durability = "rack-tolerant"

	// LatencyBestEffort reads it from wherever it is, and is the default.
	LatencyBestEffort Latency = "best-effort"
	// LatencyCached asks for the fast tier, which is borrowed capacity.
	LatencyCached Latency = "cached"

	// CostCheapest refuses to spend borrowed capacity on it.
	CostCheapest Cost = "cheapest"
	// CostBalanced is the default.
	CostBalanced Cost = "balanced"
)

// Intent is what a user declared. A zero value is the default intent, which is
// deliberate: a dataset that says nothing gets the store's own durability and
// no borrowed capacity, which is how the system behaves when Forebay is not
// installed.
type Intent struct {
	Durability Durability `json:"durability,omitempty"`
	Latency    Latency    `json:"latency,omitempty"`
	Cost       Cost       `json:"cost,omitempty"`
}

// WithDefaults fills what was not declared.
func (i Intent) WithDefaults() Intent {
	if i.Durability == "" {
		i.Durability = DurabilityBackend
	}
	if i.Latency == "" {
		i.Latency = LatencyBestEffort
	}
	if i.Cost == "" {
		i.Cost = CostBalanced
	}
	return i
}

// Fleet is what the resolution needs to know about the cluster itself, as
// against about a backend. Kept separate because an intent can be
// unsatisfiable for a reason no backend could fix.
type Fleet struct {
	// KnowsRacks says topology can name the rack a node is in. RFC-0003 is
	// allowed not to find it, and an intent that needs it is unsatisfiable
	// where it did not.
	KnowsRacks bool
}

// Validate rejects a declaration outside the vocabulary or contradicting
// itself.
//
// A contradiction is refused rather than resolved by precedence, because
// picking one of two things a user asked for, silently, is the degradation
// RFC-0001 forbids.
func (i Intent) Validate() error {
	i = i.WithDefaults()
	switch i.Durability {
	case DurabilityNone, DurabilityBackend, DurabilityReplicated, DurabilityRackTolerant:
	default:
		return fmt.Errorf("intent: %q is not a durability this project publishes, which are %s",
			i.Durability, list(DurabilityNone, DurabilityBackend, DurabilityReplicated, DurabilityRackTolerant))
	}
	switch i.Latency {
	case LatencyBestEffort, LatencyCached:
	default:
		return fmt.Errorf("intent: %q is not a latency this project publishes, which are %s",
			i.Latency, list(LatencyBestEffort, LatencyCached))
	}
	switch i.Cost {
	case CostCheapest, CostBalanced:
	default:
		return fmt.Errorf("intent: %q is not a cost this project publishes, which are %s",
			i.Cost, list(CostCheapest, CostBalanced))
	}
	if i.Cost == CostCheapest && i.Latency == LatencyCached {
		return fmt.Errorf("intent: %s and %s are two different requests: one refuses to spend borrowed capacity and the other asks for it",
			CostCheapest, LatencyCached)
	}
	return nil
}

// list renders a vocabulary for an error, sorted so the message is stable.
func list[T ~string](vs ...T) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// Unsatisfiable says an intent cannot be met, and by what.
//
// Both causes are carried, because a user asked for something and did not get
// it: which of a backend or a fleet's own blindness was responsible is the
// operator's problem, and a message naming only one sends somebody to change a
// backend over a topology they cannot see.
type Unsatisfiable struct {
	// Missing are capabilities the backend does not declare.
	Missing []driver.Capability
	// Fleet is what the cluster itself could not offer.
	Fleet []string
}

func (u *Unsatisfiable) Error() string {
	var parts []string
	if len(u.Missing) > 0 {
		names := make([]string, len(u.Missing))
		for i, c := range u.Missing {
			names[i] = string(c)
		}
		parts = append(parts, "the backend does not declare "+strings.Join(names, " or "))
	}
	if len(u.Fleet) > 0 {
		parts = append(parts, strings.Join(u.Fleet, ", and "))
	}
	return "intent: unsatisfiable: " + strings.Join(parts, ", and ")
}

// Needs reports the capabilities a declaration requires of a backend.
//
// Exported because a control plane choosing between backends needs to ask what
// an intent would want before it has one to ask.
func (i Intent) Needs() []driver.Capability {
	switch i.WithDefaults().Durability {
	case DurabilityReplicated, DurabilityRackTolerant:
		return []driver.Capability{driver.Replicate}
	default:
		// Every backend has the mandatory core, so backend durability asks for
		// nothing beyond existing, and none asks for less than that.
		return nil
	}
}

// Resolve answers whether a backend and a fleet can satisfy a declaration.
//
// A nil error means it can be met as declared. Anything else names what is
// missing, and an invalid declaration is a different error from an
// unsatisfiable one: the first is the user's to fix and the second is not.
func Resolve(i Intent, b *driver.Backend, f Fleet) error {
	if err := i.Validate(); err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("intent: no backend to resolve against")
	}
	i = i.WithDefaults()

	var u Unsatisfiable
	for _, c := range i.Needs() {
		if !b.Supports(c) {
			u.Missing = append(u.Missing, c)
		}
	}
	if i.Durability == DurabilityRackTolerant && !f.KnowsRacks {
		// Not treated as satisfied by assuming an unknown rack is its own,
		// which would meet the intent by assuming the thing it guarantees.
		u.Fleet = append(u.Fleet, "this fleet cannot say which rack a node is in")
	}
	if len(u.Missing) == 0 && len(u.Fleet) == 0 {
		return nil
	}
	return &u
}
